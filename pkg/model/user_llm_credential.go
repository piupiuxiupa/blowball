package model

import "time"

// UserLLMCredential mirrors the user_llm_credentials table
// (migration 018_user_llm_credentials.sql, user-llm-token capability).
// It stores the per-user gateway token that replaces the global
// openai.api_key for every LLM call attributable to the user; base_url and
// the model catalog remain deployment-global.
type UserLLMCredential struct {
	UserID     string    `db:"user_id"     json:"user_id"`
	APIKey     string    `db:"api_key"     json:"-"` // secret: never JSON-serialized
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}
