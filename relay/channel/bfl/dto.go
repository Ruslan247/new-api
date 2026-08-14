package bfl

import "encoding/json"

// SubmitResponse is BFL's response to the initial job submission call
// (POST /v1/{model}).
type SubmitResponse struct {
	ID         string `json:"id"`
	PollingURL string `json:"polling_url"`
}

// PollResponse is BFL's response to a polling_url check
// (GET polling_url, header x-key).
type PollResponse struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Result   json.RawMessage `json:"result"`
	Progress *float64        `json:"progress"`
}

// PollResult is the shape of PollResponse.Result once Status == "Ready".
type PollResult struct {
	Sample string `json:"sample"`
}

// aggregatedResult is an internal representation built by DoRequest to hand
// off to DoResponse; it is never sent to or received from BFL directly. It
// exists because a single OpenAI-style image request can require multiple
// BFL submit+poll cycles (BFL always returns exactly one image per job).
type aggregatedResult struct {
	Samples []string `json:"samples"`
}
