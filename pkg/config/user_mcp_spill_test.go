package config

import "testing"

// baseYAML is the minimal loadable config every user_mcp spill case builds on.
const userMCPSpillBaseYAML = `
openai:
  api_key: sk-test
  models:
    - name: gpt-4o-mini
      max_context_tokens: 128000
      max_completion_tokens: 8192
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
`

// TestLoad_UserMCPMaxInlineResultTokens covers the inline-result cap forms:
// unset → default 20000 (raw pointer stays nil), an explicit positive value
// verbatim, and an explicit 0 = automatic spill disabled (mcp-call-result-spill).
func TestLoad_UserMCPMaxInlineResultTokens(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantCap int
		wantSet bool
	}{
		{
			name:    "unset applies default",
			yaml:    userMCPSpillBaseYAML,
			wantCap: 20000,
			wantSet: false,
		},
		{
			name: "explicit positive wins",
			yaml: userMCPSpillBaseYAML + `
tools:
  user_mcp:
    max_inline_result_tokens: 5000
`,
			wantCap: 5000,
			wantSet: true,
		},
		{
			name: "explicit zero disables",
			yaml: userMCPSpillBaseYAML + `
tools:
  user_mcp:
    max_inline_result_tokens: 0
`,
			wantCap: 0,
			wantSet: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, tc.yaml)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if got := cfg.Tools.UserMCP.MaxInlineResultTokensOrDefault(); got != tc.wantCap {
				t.Errorf("MaxInlineResultTokensOrDefault() = %d, want %d", got, tc.wantCap)
			}
			if gotSet := cfg.Tools.UserMCP.MaxInlineResultTokens != nil; gotSet != tc.wantSet {
				t.Errorf("MaxInlineResultTokens set = %v, want %v", gotSet, tc.wantSet)
			}
		})
	}
}

// TestLoad_UserMCPMaxInlineResultTokensNegative verifies a negative cap fails
// config load (unset and explicit 0 are both meaningful; a negative can only
// be a mistake).
func TestLoad_UserMCPMaxInlineResultTokensNegative(t *testing.T) {
	path := writeTempYAML(t, userMCPSpillBaseYAML+`
tools:
  user_mcp:
    max_inline_result_tokens: -1
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded for negative max_inline_result_tokens, want validation error")
	}
}
