package bfl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// NOTE: BFL has not yet published official API docs for flux-3-video (early
// access as of Aug 2026). This adaptor is a best-effort implementation based
// on the BFL dashboard playground UI (prompt, aspect_ratio, duration,
// resolution, generate_audio, safety_tolerance, start/end frame images) and
// the submit+poll job shape already confirmed for BFL's image endpoints
// (POST returns {id, polling_url}; GET polling_url with header x-key returns
// {status, result}). Field names for this specific model may need
// correction once official docs are available.

const modelFlux3Video = "flux-3-video"

// ============================
// Request / Response structures
// ============================

type requestPayload struct {
	Prompt          string `json:"prompt"`
	AspectRatio     string `json:"aspect_ratio,omitempty"`
	Duration        int    `json:"duration,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	GenerateAudio   *bool  `json:"generate_audio,omitempty"`
	SafetyTolerance int    `json:"safety_tolerance,omitempty"`
	Seed            int    `json:"seed,omitempty"`
	StartImage      string `json:"start_image,omitempty"`
	EndImage        string `json:"end_image,omitempty"`
}

type submitResponse struct {
	ID         string `json:"id"`
	PollingURL string `json:"polling_url"`
}

type pollResponse struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Result   json.RawMessage `json:"result"`
	Progress *float64        `json:"progress"`
}

type pollResult struct {
	Sample string `json:"sample"`
}

// ============================
// Adaptor implementation
// ============================

type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	baseURL     string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	if err := relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate); err != nil {
		return err
	}
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return service.TaskErrorWrapper(err, "get_task_request_failed", http.StatusBadRequest)
	}
	action := constant.TaskActionTextGenerate
	switch len(req.Images) {
	case 0:
		if req.Image != "" {
			action = constant.TaskActionGenerate
		}
	case 1:
		action = constant.TaskActionGenerate
	default:
		action = constant.TaskActionFirstTailGenerate
	}
	info.Action = action
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, err
	}

	generateAudio := true
	body := requestPayload{
		Prompt:          req.Prompt,
		AspectRatio:     taskcommon.DefaultString(req.Size, "16:9"),
		Duration:        taskcommon.DefaultInt(req.Duration, 5),
		Resolution:      "hd",
		GenerateAudio:   &generateAudio,
		SafetyTolerance: 2,
	}

	switch info.Action {
	case constant.TaskActionFirstTailGenerate:
		body.StartImage = req.Images[0]
		body.EndImage = req.Images[1]
	case constant.TaskActionGenerate:
		if len(req.Images) > 0 {
			body.StartImage = req.Images[0]
		} else {
			body.StartImage = req.Image
		}
	}

	if err := taskcommon.UnmarshalMetadata(req.Metadata, &body); err != nil {
		return nil, errors.Wrap(err, "unmarshal metadata failed")
	}

	data, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	baseURL := a.baseURL
	if baseURL == "" {
		baseURL = constant.ChannelBaseURLs[constant.ChannelTypeBFL]
	}
	return fmt.Sprintf("%s/v1/%s", baseURL, modelFlux3Video), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-key", info.ApiKey)
	return nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}

	var sResp submitResponse
	if err := common.Unmarshal(responseBody, &sResp); err != nil {
		taskErr = service.TaskErrorWrapper(errors.Wrap(err, fmt.Sprintf("%s", responseBody)), "unmarshal_response_failed", http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(sResp.PollingURL) == "" {
		taskErr = service.TaskErrorWrapperLocal(fmt.Errorf("submit response missing polling_url: %s", responseBody), "submit_failed", http.StatusBadGateway)
		return
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.CreatedAt = time.Now().Unix()
	ov.Model = info.OriginModelName
	c.JSON(http.StatusOK, ov)

	// The upstream task ID is the polling_url itself (BFL's job status is
	// only reachable via that URL, not a predictable path built from `id`).
	return sResp.PollingURL, responseBody, nil
}

// FetchTask polls a BFL job. body["task_id"] is the polling_url stored as
// the upstream task ID by DoResponse.
func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	pollingURL, ok := body["task_id"].(string)
	if !ok || pollingURL == "" {
		return nil, fmt.Errorf("invalid task_id")
	}

	req, err := http.NewRequest(http.MethodGet, pollingURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-key", key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) GetModelList() []string {
	return []string{modelFlux3Video}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "bfl"
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	taskInfo := &relaycommon.TaskInfo{}

	var pResp pollResponse
	if err := common.Unmarshal(respBody, &pResp); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response body")
	}

	if pResp.Progress != nil {
		taskInfo.Progress = fmt.Sprintf("%d%%", int(*pResp.Progress*100))
	}

	switch strings.ToLower(strings.TrimSpace(pResp.Status)) {
	case "ready":
		taskInfo.Status = model.TaskStatusSuccess
		if len(pResp.Result) > 0 {
			var result pollResult
			if err := common.Unmarshal(pResp.Result, &result); err == nil {
				taskInfo.Url = result.Sample
			}
		}
	case "pending", "request accepted", "queued", "queueing":
		taskInfo.Status = model.TaskStatusInProgress
	case "error", "request moderated", "content moderated", "task not found":
		taskInfo.Status = model.TaskStatusFailure
		taskInfo.Reason = pResp.Status
	default:
		return nil, fmt.Errorf("unknown task status: %s", pResp.Status)
	}

	return taskInfo, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	var pResp pollResponse
	if err := common.Unmarshal(originTask.Data, &pResp); err != nil {
		return nil, errors.Wrap(err, "unmarshal bfl task data failed")
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.CompletedAt = originTask.UpdatedAt

	if len(pResp.Result) > 0 {
		var result pollResult
		if err := common.Unmarshal(pResp.Result, &result); err == nil && result.Sample != "" {
			openAIVideo.SetMetadata("url", result.Sample)
		}
	}

	if taskFailed := strings.EqualFold(pResp.Status, "Error") || strings.EqualFold(pResp.Status, "Request Moderated") || strings.EqualFold(pResp.Status, "Content Moderated"); taskFailed {
		openAIVideo.Error = &dto.OpenAIVideoError{
			Message: pResp.Status,
			Code:    pResp.Status,
		}
	}

	return common.Marshal(openAIVideo)
}
