package mcp

import (
	"encoding/base64"
)

// defaultAPIKeyHeader is the header used for api-key auth when the server entry
// does not name one explicitly.
const defaultAPIKeyHeader = "X-API-Key"

// authHeaders translates a server's static auth into the outbound HTTP headers
// injected by the manager's transport. It is the ONLY place secret values are
// materialized into request shape, so it is the single point to audit for leak
// invariant #3 (auth never reaches model-visible text): these headers travel
// process-internal transport→HTTP, never through a tool result or log.
//
// An unknown auth type yields no headers (defensive; Validate() rejects
// unknown types up front).
func authHeaders(auth Auth) map[string]string {
	switch auth.Type {
	case AuthBearer:
		if auth.Value == "" {
			return nil
		}
		return map[string]string{"Authorization": "Bearer " + auth.Value}
	case AuthAPIKey:
		if auth.Value == "" {
			return nil
		}
		h := auth.Header
		if h == "" {
			h = defaultAPIKeyHeader
		}
		return map[string]string{h: auth.Value}
	case AuthBasic:
		if auth.Username == "" && auth.Password == "" {
			return nil
		}
		cred := auth.Username + ":" + auth.Password
		return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))}
	}
	return nil
}

// redacted replaces a non-empty secret with the standard mask and leaves an
// empty value absent. Used by redactedAuth.
const redacted = "***"

// redactedAuth is the model-safe projection of an Auth value: it keeps the
// credential KIND (so the agent knows how the server authenticates) but masks
// or omits every secret-bearing field. It is the shape returned by
// mcp_list_servers and never contains a plaintext token/key/password.
type redactedAuth struct {
	Type   AuthType `json:"type"`
	Header string   `json:"header,omitempty"`
	// Value is present (as the redaction mask) only when a secret was supplied,
	// so the agent can tell "configured with a secret" from "no auth".
	Value string `json:"value,omitempty"`
}

// redactAuth returns the model-safe projection of auth. A configured secret is
// reported as the redaction mask; an empty secret is omitted entirely. The
// auth type and (for api-key) the header name are preserved because neither is
// secret and both help the agent reason about the server.
func redactAuth(auth Auth) redactedAuth {
	out := redactedAuth{Type: auth.Type, Header: auth.Header}
	switch auth.Type {
	case AuthBearer, AuthAPIKey:
		if auth.Value != "" {
			out.Value = redacted
		}
	case AuthBasic:
		if auth.Username != "" || auth.Password != "" {
			out.Value = redacted
		}
	}
	return out
}

// serverView is the model-safe projection of a Server for mcp_list_servers:
// name/url/transport/description plus a redacted auth, and the count (not the
// bodies) of cached tools so the agent knows the server is callable without the
// schema noise.
type serverView struct {
	Name        string       `json:"name"`
	URL         string       `json:"url"`
	Transport   string       `json:"transport"`
	Description string       `json:"description,omitempty"`
	Auth        redactedAuth `json:"auth"`
	Tools       int          `json:"tools"`
}

// serverViewFrom builds the redacted projection of s.
func serverViewFrom(s Server) serverView {
	return serverView{
		Name:        s.Name,
		URL:         s.URL,
		Transport:   s.Transport,
		Description: s.Description,
		Auth:        redactAuth(s.Auth),
		Tools:       len(s.Tools),
	}
}
