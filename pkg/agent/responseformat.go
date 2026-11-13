package agent

import (
	"encoding/json"
	"strings"
)

// buildResponseFormatPayload wraps a raw JSON Schema into OpenAI's
// response_format wire shape for a generic sub-agent's terminal round.
func buildResponseFormatPayload(schemaJSON, agentName string) json.RawMessage {
	if strings.TrimSpace(schemaJSON) == "" {
		return nil
	}
	payload := map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   sanitizeSchemaName(agentName),
			"strict": true,
			"schema": json.RawMessage(schemaJSON),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return raw
}

// sanitizeSchemaName reduces a label to OpenAI's json_schema name charset.
func sanitizeSchemaName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "result"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
