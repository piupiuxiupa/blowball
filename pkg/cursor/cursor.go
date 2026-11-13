// Package cursor provides encode/decode helpers for opaque pagination tokens.
//
// A page_token encodes the tuple (msg_time, msg_index, id) of the last message
// on the current page. Clients pass the token back to retrieve the next page.
// The encoded form is the JSON object in URL-safe base64, making the token
// opaque and safe to pass in URLs.
package cursor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Cursor is the decoded pagination position. It is stable because id is the
// AUTO_INCREMENT primary key: no two rows share the same id.
type Cursor struct {
	MsgTime  time.Time `json:"msg_time"`
	MsgIndex int       `json:"msg_index"`
	ID       int64     `json:"id"`
}

// Encode returns an opaque base64 string for c. An empty Cursor encodes to an
// empty string, which callers treat as "first page".
func Encode(c Cursor) (string, error) {
	if c == (Cursor{}) {
		return "", nil
	}

	jsonBytes, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("cursor encode marshal: %w", err)
	}
	return base64.URLEncoding.EncodeToString(jsonBytes), nil
}

// Decode parses an opaque token back into a Cursor. An empty token returns a
// zero Cursor and no error, representing the first page.
func Decode(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}

	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor decode base64: %w", err)
	}

	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("cursor decode json: %w", err)
	}
	return c, nil
}
