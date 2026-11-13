package config

import (
	"fmt"
	"strings"
	"testing"
)

// cataloglessBaseYAML is validBaseYAML minus the openai.models entry, so the
// catalog tests below splice their own (a preset catalog would collide with
// the spliced one on a duplicate mapping key).
const cataloglessBaseYAML = `
openai:
  api_key: sk-test
%s
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "super-secret"
  expire: 7d
agents:
  confucius:
    name: Confucius
    system_prompt: "you are confucius"
`

// loadWithModels splices an openai block suffix into the minimal loadable
// config and loads it.
func loadWithModels(t *testing.T, openaiExtra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(cataloglessBaseYAML, openaiExtra)))
}

const twoModelCatalog = `
  models:
    - name: gpt-5
      max_context_tokens: 400000
      max_completion_tokens: 16384
      thinking: true
    - name: glm-4.7
      max_context_tokens: 200000
      max_completion_tokens: 8192
      thinking: false
`

// TestLoad_ModelCatalog_Valid loads a two-entry catalog with an explicit
// default_model and checks every field round-trips (per-request-model-selection
// spec: "目录配置校验" — a well-formed catalog loads).
func TestLoad_ModelCatalog_Valid(t *testing.T) {
	cfg, err := loadWithModels(t, twoModelCatalog+"  default_model: glm-4.7\n")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := len(cfg.OpenAI.Models); got != 2 {
		t.Fatalf("OpenAI.Models has %d entries, want 2", got)
	}
	first := cfg.OpenAI.Models[0]
	if first.Name != "gpt-5" || first.MaxContextTokens != 400000 || first.MaxCompletionTokens != 16384 || !first.Thinking {
		t.Errorf("Models[0] = %+v, want {gpt-5 400000 16384 true}", first)
	}
	second := cfg.OpenAI.Models[1]
	if second.Name != "glm-4.7" || second.MaxContextTokens != 200000 || second.MaxCompletionTokens != 8192 || second.Thinking {
		t.Errorf("Models[1] = %+v, want {glm-4.7 200000 8192 false}", second)
	}
	if cfg.OpenAI.DefaultModel != "glm-4.7" {
		t.Errorf("OpenAI.DefaultModel = %q, want glm-4.7", cfg.OpenAI.DefaultModel)
	}
}

// TestLoad_ModelCatalog_Required: model-effort-v2 makes the catalog
// mandatory — an empty catalog fails load instead of synthesizing an implicit
// single-entry one (spec: "未配置目录时加载失败").
func TestLoad_ModelCatalog_Required(t *testing.T) {
	_, err := loadWithModels(t, "")
	if err == nil {
		t.Fatal("Load accepted a missing openai.models catalog; want validation error")
	}
	if !strings.Contains(err.Error(), "openai.models") || !strings.Contains(err.Error(), "required") {
		t.Errorf("error %q does not state the catalog is required", err)
	}

	// default_model without a catalog fails on the missing catalog itself.
	_, err = loadWithModels(t, "  default_model: gpt-5\n")
	if err == nil {
		t.Fatal("Load accepted default_model without a catalog; want validation error")
	}
	if !strings.Contains(err.Error(), "openai.models") {
		t.Errorf("error %q does not name openai.models", err)
	}
}

// TestLoad_ModelCatalog_DefaultOmittedTakesFirst: an unset default_model
// resolves to the first catalog entry (spec: "未配置时取目录第一项").
func TestLoad_ModelCatalog_DefaultOmittedTakesFirst(t *testing.T) {
	cfg, err := loadWithModels(t, twoModelCatalog)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.DefaultModel != "" {
		t.Errorf("OpenAI.DefaultModel = %q, want empty (unset)", cfg.OpenAI.DefaultModel)
	}
	if got := cfg.DefaultModelName(); got != "gpt-5" {
		t.Errorf("DefaultModelName() = %q, want gpt-5 (first entry)", got)
	}
}

// TestLoad_ModelCatalog_DuplicateNameRejected: duplicate names fail load
// (spec: "目录内 name 唯一").
func TestLoad_ModelCatalog_DuplicateNameRejected(t *testing.T) {
	_, err := loadWithModels(t, `
  models:
    - name: gpt-5
      max_context_tokens: 400000
      max_completion_tokens: 16384
    - name: gpt-5
      max_context_tokens: 200000
      max_completion_tokens: 8192
`)
	if err == nil {
		t.Fatal("Load accepted a duplicate catalog name; want validation error")
	}
	if !strings.Contains(err.Error(), "openai.models") {
		t.Errorf("error %q does not name openai.models", err)
	}
}

// TestLoad_ModelCatalog_EmptyNameRejected: a nameless entry fails load.
func TestLoad_ModelCatalog_EmptyNameRejected(t *testing.T) {
	_, err := loadWithModels(t, `
  models:
    - max_context_tokens: 400000
`)
	if err == nil {
		t.Fatal("Load accepted an empty catalog name; want validation error")
	}
}

// TestLoad_ModelCatalog_NonPositiveWindowRejected: catalog windows must be
// positive integers — 0 is NOT the legacy "disable" off switch here, it is a
// malformed declaration (spec: "非正整数 max_context_tokens ... SHALL 使配置
// 加载失败").
func TestLoad_ModelCatalog_NonPositiveWindowRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
	}{
		{"zero", "  models:\n    - name: m1\n      max_context_tokens: 0\n"},
		{"negative", "  models:\n    - name: m1\n      max_context_tokens: -100\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithModels(t, tc.extra)
			if err == nil {
				t.Fatalf("Load accepted %s catalog max_context_tokens; want validation error", tc.name)
			}
			if !strings.Contains(err.Error(), "openai.models[0].max_context_tokens") {
				t.Errorf("error %q does not name openai.models[0].max_context_tokens", err)
			}
		})
	}
}

// TestLoad_ModelCatalog_FractionalWindowRejected: yaml.v3 silently truncates
// fractions into int fields, so the loader shadow-checks each entry's raw
// value — 12.5 must fail load, not silently become 12.
func TestLoad_ModelCatalog_FractionalWindowRejected(t *testing.T) {
	_, err := loadWithModels(t, "  models:\n    - name: m1\n      max_context_tokens: 12.5\n")
	if err == nil {
		t.Fatal("Load accepted fractional catalog max_context_tokens 12.5; want an error")
	}
	if !strings.Contains(err.Error(), "openai.models[0].max_context_tokens") {
		t.Errorf("error %q does not name openai.models[0].max_context_tokens", err)
	}
}

// TestLoad_ModelCatalog_DefaultNotInCatalogRejected: default_model must name a
// catalog entry (spec: "default_model 不在目录内 ... SHALL 使配置加载失败").
func TestLoad_ModelCatalog_DefaultNotInCatalogRejected(t *testing.T) {
	_, err := loadWithModels(t, twoModelCatalog+"  default_model: gpt-99\n")
	if err == nil {
		t.Fatal("Load accepted default_model outside the catalog; want validation error")
	}
	if !strings.Contains(err.Error(), "openai.default_model") {
		t.Errorf("error %q does not name openai.default_model", err)
	}
}

// TestLoad_DefaultReasoningEffort: unset normalizes to "none"; every closed-set
// value loads; anything else fails load naming the field
// (agent-reasoning-configuration spec: "Configuration validation").
func TestLoad_DefaultReasoningEffort(t *testing.T) {
	cfg, err := loadWithModels(t, twoModelCatalog)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.DefaultReasoningEffort != "none" {
		t.Errorf("DefaultReasoningEffort = %q, want none (unset normalizes to none)", cfg.OpenAI.DefaultReasoningEffort)
	}

	for _, level := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		cfg, err := loadWithModels(t, twoModelCatalog+"  default_reasoning_effort: "+level+"\n")
		if err != nil {
			t.Fatalf("Load with default_reasoning_effort %s returned error: %v", level, err)
		}
		if cfg.OpenAI.DefaultReasoningEffort != level {
			t.Errorf("DefaultReasoningEffort = %q, want %s", cfg.OpenAI.DefaultReasoningEffort, level)
		}
	}

	_, err = loadWithModels(t, twoModelCatalog+"  default_reasoning_effort: ultra\n")
	if err == nil {
		t.Fatal("Load accepted default_reasoning_effort ultra; want validation error")
	}
	if !strings.Contains(err.Error(), "openai.default_reasoning_effort") {
		t.Errorf("error %q does not name openai.default_reasoning_effort", err)
	}
}

// TestTitleModelName: openai.title_model is optional, need not belong to the
// catalog, and falls back to the default catalog entry
// (per-request-model-selection spec: "标题模型缺省回退").
func TestTitleModelName(t *testing.T) {
	// Unset → default entry.
	cfg, err := loadWithModels(t, twoModelCatalog)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.TitleModelName(); got != "gpt-5" {
		t.Errorf("TitleModelName() = %q, want gpt-5 (default entry)", got)
	}

	// Set to a name outside the catalog → used verbatim (title calls go to
	// the gateway directly; the catalog only governs per-turn models).
	cfg, err = loadWithModels(t, twoModelCatalog+"  title_model: gpt-4o-mini\n")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.TitleModelName(); got != "gpt-4o-mini" {
		t.Errorf("TitleModelName() = %q, want gpt-4o-mini (verbatim, off-catalog allowed)", got)
	}
}

// TestModelCatalog_Resolution: ModelCatalog/DefaultModelName return the
// configured entries verbatim, and FindModelCatalogEntry resolves names
// case-sensitively with an ok flag.
func TestModelCatalog_Resolution(t *testing.T) {
	cfg, err := loadWithModels(t, twoModelCatalog+"  default_model: gpt-5\n")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	catalog := cfg.ModelCatalog()
	if len(catalog) != 2 || catalog[0].Name != "gpt-5" || catalog[1].Name != "glm-4.7" {
		t.Fatalf("ModelCatalog() = %+v, want the two configured entries verbatim", catalog)
	}
	if got := cfg.DefaultModelName(); got != "gpt-5" {
		t.Errorf("DefaultModelName() = %q, want gpt-5", got)
	}
	if e, ok := cfg.OpenAI.FindModelCatalogEntry("glm-4.7"); !ok || e.MaxContextTokens != 200000 || e.Thinking {
		t.Errorf("FindModelCatalogEntry(glm-4.7) = %+v ok=%v, want {glm-4.7 200000 false} ok=true", e, ok)
	}
	if _, ok := cfg.OpenAI.FindModelCatalogEntry("GPT-5"); ok {
		t.Error("FindModelCatalogEntry(GPT-5) ok=true, want false (case-sensitive)")
	}
}
