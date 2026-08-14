package bfl

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

const (
	pollInterval = 500 * time.Millisecond
	pollTimeout  = 5 * time.Minute
)

type Adaptor struct {
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", errors.New("bfl adaptor: relay info is nil")
	}
	if info.ChannelBaseUrl == "" {
		info.ChannelBaseUrl = constant.ChannelBaseURLs[constant.ChannelTypeBFL]
	}
	requestPath := info.RequestURLPath
	if requestPath == "" {
		return info.ChannelBaseUrl, nil
	}
	return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, requestPath, info.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	if info == nil {
		return errors.New("bfl adaptor: relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("bfl adaptor: api key is required")
	}
	channel.SetupApiRequestHeader(info, c, req)
	// The outgoing body is always JSON built by this adaptor, regardless of
	// how the client sent it (e.g. multipart/form-data for image edits), so
	// force the content type instead of copying it from the client request.
	req.Set("Content-Type", "application/json")
	req.Set("Accept", "application/json")
	req.Set("x-key", info.ApiKey)
	return nil
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	if info == nil {
		return nil, errors.New("bfl adaptor: relay info is nil")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		if v := c.PostForm("prompt"); strings.TrimSpace(v) != "" {
			request.Prompt = v
		}
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("bfl adaptor: prompt is required")
	}

	modelName := strings.TrimSpace(info.UpstreamModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(request.Model)
	}
	if modelName == "" {
		modelName = ModelFluxPro11
	}
	info.UpstreamModelName = modelName
	info.RequestURLPath = "/v1/" + modelName

	payload := map[string]any{
		"prompt": request.Prompt,
	}

	if size := strings.TrimSpace(request.Size); size != "" {
		if aspectRatioModels[modelName] {
			if ratio, ok := sizeToAspectRatio(size); ok {
				payload["aspect_ratio"] = ratio
			}
		} else if w, h, ok := parseSize(size); ok {
			payload["width"] = normalizeBflDimension(w)
			payload["height"] = normalizeBflDimension(h)
		}
	}

	if len(request.OutputFormat) > 0 {
		var outputFormat string
		if err := common.Unmarshal(request.OutputFormat, &outputFormat); err == nil && strings.TrimSpace(outputFormat) != "" {
			payload["output_format"] = outputFormat
		}
	}

	if strings.EqualFold(request.Quality, "hd") || strings.EqualFold(request.Quality, "high") {
		payload["prompt_upsampling"] = true
	}

	if info.RelayMode == relayconstant.RelayModeImagesEdits {
		imageBase64, err := readInputImageBase64(c)
		if err != nil {
			return nil, err
		}
		payload["input_image"] = imageBase64
	}

	if len(request.ExtraFields) > 0 {
		var extra map[string]any
		if err := common.Unmarshal(request.ExtraFields, &extra); err != nil {
			return nil, fmt.Errorf("bfl adaptor: failed to decode extra_fields: %w", err)
		}
		for key, val := range extra {
			payload[key] = val
		}
	}

	for key, raw := range request.Extra {
		if raw == nil {
			continue
		}
		var val any
		if err := common.Unmarshal(raw, &val); err != nil {
			return nil, fmt.Errorf("bfl adaptor: failed to decode extra field %s: %w", key, err)
		}
		payload[key] = val
	}

	return payload, nil
}

// DoRequest submits one or more BFL generation jobs (BFL always returns
// exactly one image per job, so an OpenAI-style request for n>1 images is
// fanned out into n sequential submit+poll cycles) and hands the aggregated
// result to DoResponse as a synthetic 200 response.
func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	bodyBytes, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, fmt.Errorf("bfl adaptor: read request body failed: %w", err)
	}

	imageN := 1
	if info != nil {
		if req, ok := info.Request.(*dto.ImageRequest); ok {
			if n := lo.FromPtrOr(req.N, uint(0)); n > 0 {
				imageN = int(n)
			}
		}
	}

	samples := make([]string, 0, imageN)
	for i := 0; i < imageN; i++ {
		sample, err := a.generateOne(c, info, bodyBytes)
		if err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}

	resultBytes, err := common.Marshal(aggregatedResult{Samples: samples})
	if err != nil {
		return nil, fmt.Errorf("bfl adaptor: encode aggregated result failed: %w", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(resultBytes)),
	}, nil
}

func (a *Adaptor) generateOne(c *gin.Context, info *relaycommon.RelayInfo, body []byte) (string, error) {
	submitResp, err := a.submit(c, info, body)
	if err != nil {
		return "", err
	}

	pollResp, err := a.poll(c.Request.Context(), info, submitResp.PollingURL)
	if err != nil {
		return "", err
	}

	if !strings.EqualFold(pollResp.Status, "Ready") {
		return "", fmt.Errorf("bfl adaptor: generation status %q", pollResp.Status)
	}
	if len(pollResp.Result) == 0 {
		return "", errors.New("bfl adaptor: ready response missing result")
	}

	var result PollResult
	if err := common.Unmarshal(pollResp.Result, &result); err != nil {
		return "", fmt.Errorf("bfl adaptor: decode poll result failed: %w", err)
	}
	if strings.TrimSpace(result.Sample) == "" {
		return "", errors.New("bfl adaptor: poll result missing sample")
	}
	return result.Sample, nil
}

func (a *Adaptor) submit(c *gin.Context, info *relaycommon.RelayInfo, body []byte) (*SubmitResponse, error) {
	requestURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bfl adaptor: build submit request failed: %w", err)
	}
	header := req.Header
	if err := a.SetupRequestHeader(c, &header, info); err != nil {
		return nil, err
	}

	client, err := httpClientFor(info)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bfl adaptor: submit request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bfl adaptor: read submit response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bfl adaptor: submit failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var submitResp SubmitResponse
	if err := common.Unmarshal(respBody, &submitResp); err != nil {
		return nil, fmt.Errorf("bfl adaptor: decode submit response failed: %w", err)
	}
	if strings.TrimSpace(submitResp.PollingURL) == "" {
		return nil, errors.New("bfl adaptor: submit response missing polling_url")
	}
	return &submitResp, nil
}

func (a *Adaptor) poll(ctx context.Context, info *relaycommon.RelayInfo, pollingURL string) (*PollResponse, error) {
	client, err := httpClientFor(info)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(pollTimeout)
	for {
		if time.Now().After(deadline) {
			return nil, errors.New("bfl adaptor: polling timed out")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollingURL, nil)
		if err != nil {
			return nil, fmt.Errorf("bfl adaptor: build poll request failed: %w", err)
		}
		req.Header.Set("x-key", info.ApiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("bfl adaptor: poll request failed: %w", err)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("bfl adaptor: read poll response failed: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bfl adaptor: poll failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		var pollResp PollResponse
		if err := common.Unmarshal(respBody, &pollResp); err != nil {
			return nil, fmt.Errorf("bfl adaptor: decode poll response failed: %w", err)
		}

		switch strings.ToLower(strings.TrimSpace(pollResp.Status)) {
		case "ready", "error", "request moderated", "content moderated", "task not found":
			return &pollResp, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func httpClientFor(info *relaycommon.RelayInfo) (*http.Client, error) {
	if info != nil && info.ChannelSetting.Proxy != "" {
		return service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	}
	return service.GetHttpClient(), nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if resp == nil {
		return nil, types.NewError(errors.New("bfl adaptor: empty response"), types.ErrorCodeBadResponse)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	_ = resp.Body.Close()

	var result aggregatedResult
	if err := common.Unmarshal(responseBody, &result); err != nil {
		return nil, types.NewError(fmt.Errorf("bfl adaptor: failed to decode response: %w", err), types.ErrorCodeBadResponseBody)
	}
	if len(result.Samples) == 0 {
		return nil, types.NewError(errors.New("bfl adaptor: empty generation output"), types.ErrorCodeBadResponse)
	}

	var imageReq *dto.ImageRequest
	if info != nil {
		if req, ok := info.Request.(*dto.ImageRequest); ok {
			imageReq = req
		}
	}
	wantsBase64 := imageReq != nil && strings.EqualFold(imageReq.ResponseFormat, "b64_json")

	imageResponse := dto.ImageResponse{
		Created: common.GetTimestamp(),
		Data:    make([]dto.ImageData, 0),
	}

	if wantsBase64 {
		converted, convErr := downloadImagesToBase64(result.Samples)
		if convErr != nil {
			return nil, types.NewError(convErr, types.ErrorCodeBadResponse)
		}
		for _, content := range converted {
			if content == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{B64Json: content})
		}
	} else {
		for _, url := range result.Samples {
			if url == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{Url: url})
		}
	}

	if len(imageResponse.Data) == 0 {
		return nil, types.NewError(errors.New("bfl adaptor: no usable image data"), types.ErrorCodeBadResponse)
	}

	responseBytes, err := common.Marshal(imageResponse)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("bfl adaptor: encode response failed: %w", err), types.ErrorCodeBadResponseBody)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(responseBytes)

	usage := &dto.Usage{}
	return usage, nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func downloadImagesToBase64(urls []string) ([]string, error) {
	results := make([]string, 0, len(urls))
	for _, url := range urls {
		if strings.TrimSpace(url) == "" {
			continue
		}
		_, data, err := service.GetImageFromUrl(url)
		if err != nil {
			return nil, fmt.Errorf("bfl adaptor: failed to download image from %s: %w", url, err)
		}
		results = append(results, data)
	}
	return results, nil
}

func readInputImageBase64(c *gin.Context) (string, error) {
	mf := c.Request.MultipartForm
	if mf == nil {
		if _, err := c.MultipartForm(); err != nil {
			return "", fmt.Errorf("bfl adaptor: parse multipart form failed: %w", err)
		}
		mf = c.Request.MultipartForm
	}
	if mf == nil || len(mf.File) == 0 {
		return "", errors.New("bfl adaptor: image file is required for edits")
	}

	var fileHeader *multipart.FileHeader
	for _, key := range []string{"image", "image[]"} {
		if files := mf.File[key]; len(files) > 0 {
			fileHeader = files[0]
			break
		}
	}
	if fileHeader == nil {
		for _, files := range mf.File {
			if len(files) > 0 {
				fileHeader = files[0]
				break
			}
		}
	}
	if fileHeader == nil {
		return "", errors.New("bfl adaptor: image file is required for edits")
	}

	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("bfl adaptor: failed to open image file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("bfl adaptor: failed to read image file: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// parseSize parses an OpenAI-style "WxH" size string.
func parseSize(size string) (width int, height int, ok bool) {
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// sizeToAspectRatio reduces an OpenAI-style "WxH" size string to a "W:H"
// aspect ratio string as accepted by BFL's aspect_ratio-based models.
func sizeToAspectRatio(size string) (string, bool) {
	w, h, ok := parseSize(size)
	if !ok {
		return "", false
	}
	g := gcd(w, h)
	if g == 0 {
		return "", false
	}
	return fmt.Sprintf("%d:%d", w/g, h/g), true
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// normalizeBflDimension clamps a pixel dimension to BFL's supported range
// ([256, 1440]) and rounds it to the nearest multiple of 32, as required by
// BFL's width/height-based models.
func normalizeBflDimension(value int) int {
	const (
		minDim = 256
		maxDim = 1440
		step   = 32
	)
	if value < minDim {
		value = minDim
	}
	if value > maxDim {
		value = maxDim
	}
	remainder := value % step
	if remainder != 0 {
		if remainder >= step/2 {
			value += step - remainder
		} else {
			value -= remainder
		}
	}
	if value < minDim {
		value = minDim
	}
	if value > maxDim {
		value = maxDim
	}
	return value
}

func (a *Adaptor) ConvertOpenAIRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertOpenAIRequest is not implemented")
}

func (a *Adaptor) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertRerankRequest is not implemented")
}

func (a *Adaptor) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertEmbeddingRequest is not implemented")
}

func (a *Adaptor) ConvertAudioRequest(*gin.Context, *relaycommon.RelayInfo, dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("bfl adaptor: ConvertAudioRequest is not implemented")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(*gin.Context, *relaycommon.RelayInfo, dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertOpenAIResponsesRequest is not implemented")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertClaudeRequest is not implemented")
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("bfl adaptor: ConvertGeminiRequest is not implemented")
}
