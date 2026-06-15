// Package cursor provides encode/decode helpers for opaque pagination tokens.
//
// A page_token encodes the tuple (msg_time, msg_index, id) of the last message
// on the current page. Clients pass the token back to retrieve the next page.
// The encoded form is a JSON object compressed with gzip and encoded as
// URL-safe base64, making the token opaque, compact, and safe to pass in URLs.
package cursor

import (
	"bytes"
	"compress/gzip"
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

	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(jsonBytes); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("cursor encode gzip: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("cursor encode gzip close: %w", err)
	}

	return base64.URLEncoding.EncodeToString(buf.Bytes()), nil
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

	r, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor decode gzip: %w", err)
	}
	defer r.Close()

	var c Cursor
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return Cursor{}, fmt.Errorf("cursor decode json: %w", err)
	}
	return c, nil
}
