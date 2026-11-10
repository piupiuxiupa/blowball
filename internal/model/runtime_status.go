package model

// RuntimeStatus is a cache-only synthetic user message anchored to the durable
// transcript. It deliberately has no MySQL representation.
type RuntimeStatus struct {
	StatusID          string `json:"status_id"`
	Kind              string `json:"kind"`
	Content           string `json:"content"`
	Seq               int64  `json:"seq"`
	BeforeClientMsgID string `json:"before_client_msg_id,omitempty"`
	AfterToolCallID   string `json:"after_tool_call_id,omitempty"`
}
