package model

import "time"

// Raw-log kind values. These match the kind column on the llm_raw_log table.
// A new LLM call produces a request row and a response row (success) or an
// error row (failure); all rows of a call share call_id and seq, ordered by
// frame_index within the call. chunk remains a historical kind for rows written
// by deployments that captured individual SSE frames.
const (
	RawKindRequest  = "request"
	RawKindChunk    = "chunk"
	RawKindResponse = "response"
	RawKindError    = "error"
)

// LLMRawLog mirrors the `llm_raw_log` table (migration 011_llm_raw_log.sql).
// One row is one raw payload captured around a single LLM call: the as-sent
// request params (kind=request), the stitched chat.completion-equivalent
// response (kind=response), or the gateway's error body (kind=error).
// Historical rows may instead contain a single SSE frame's verbatim wire bytes
// (kind=chunk).
//
// model / finish_reason / http_status / duration_ms are materialized
// redundantly from raw for filter-without-parse queries. The table carries no
// sessions FK and is never cleaned: rows survive session deletion as orphans
// (see the llm-raw-capture spec).
type LLMRawLog struct {
	ID           int64     `db:"id"            json:"id"`
	CallID       string    `db:"call_id"       json:"call_id"`
	Seq          int       `db:"seq"           json:"seq"`
	FrameIndex   int       `db:"frame_index"   json:"frame_index"`
	TraceID      string    `db:"trace_id"      json:"trace_id"`
	SessionID    string    `db:"session_id"    json:"session_id"`
	UserID       string    `db:"user_id"       json:"user_id"`
	Agent        string    `db:"agent"         json:"agent"`
	Kind         string    `db:"kind"          json:"kind"`
	Model        string    `db:"model"         json:"model"`
	FinishReason string    `db:"finish_reason" json:"finish_reason"`
	HTTPStatus   int       `db:"http_status"   json:"http_status"`
	DurationMS   int64     `db:"duration_ms"   json:"duration_ms"`
	Raw          string    `db:"raw"           json:"raw"`
	MsgTime      time.Time `db:"msg_time"      json:"msg_time"`
	UpdateTime   time.Time `db:"update_time"   json:"update_time"`
}
