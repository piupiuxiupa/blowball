package webfetch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

const Name = "webfetch"

var schemaFetch = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "Absolute http(s) URL to fetch (must include the scheme)."
    },
    "method": {
      "type": "string",
      "description": "HTTP method to use (GET, POST, etc.). Defaults to GET.",
      "enum": ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]
    },
    "headers": {
      "type": "object",
      "description": "Optional HTTP headers as key-value pairs.",
      "additionalProperties": { "type": "string" }
    }
  },
  "required": ["url"],
  "additionalProperties": false
}`)

// fetchArgs decodes the model-supplied tool arguments.
type fetchArgs struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

// RegisterAll registers the webfetch tool against r when enabled in cfg.
func RegisterAll(r *tool.Registry, cfg config.WebfetchConfig) {
	if !cfg.Enabled {
		return
	}

	spec := &tool.ToolSpec{
		Name: Name,
		Description: "Fetch an external URL and return `{url, status_code, headers, body}`: `url` is the final URL after " +
			"redirects, `status_code` is the HTTP status int, `headers` maps each response header to its value(s), and " +
			"`body` is the **full** response as text (no truncation, no size cap). **`url` MUST be an absolute http(s) URL " +
			"including the scheme.** Follows redirects up to a configurable limit (default 10) and uses the configured " +
			"timeout (default 30s). **The body is NOT decoded for binary content** — non-text bytes pass through as-is " +
			"(often garbled), so prefer this for HTML/text, not for downloading binaries. On a non-2xx response the result " +
			"still carries `status_code` and `headers` (including any `Location`), so **you SHOULD retry with the resolved " +
			"URL or adjusted method/headers.** A timeout, transport failure, or exceeded redirect cap returns an error " +
			"(carrying the last redirect `Location` if any) instead of a result.",
		ParametersJSON: schemaFetch,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a fetchArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("webfetch: parse args: %w", err)
			}
			return Fetch(a.URL, a.Method, a.Headers, cfg.Timeout, cfg.MaxRedirects)
		},
	}
	if err := r.Register(spec); err != nil {
		panic(fmt.Sprintf("webfetch register: %v", err))
	}
}
