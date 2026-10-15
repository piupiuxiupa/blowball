package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestViewImageConfig_ResolveDefaults(t *testing.T) {
	got := ViewImageConfig{
		MaxImageBytes:   -1,
		MaxOutputTokens: -1,
		Timeout:         -time.Second,
	}.Resolve()
	if got.MaxImageBytes != DefaultViewImageMaxImageBytes ||
		got.MaxOutputTokens != DefaultViewImageMaxOutputToken ||
		got.Timeout != DefaultViewImageTimeout {
		t.Fatalf("Resolve() = %+v, want all documented defaults", got)
	}
}

func TestViewImageConfig_ResolveKeepsExplicitValues(t *testing.T) {
	got := ViewImageConfig{
		MaxImageBytes:   4096,
		MaxOutputTokens: 512,
		Timeout:         5 * time.Second,
	}.Resolve()
	if got.MaxImageBytes != 4096 || got.MaxOutputTokens != 512 || got.Timeout != 5*time.Second {
		t.Fatalf("Resolve() = %+v, want explicit values preserved", got)
	}
}

func TestLoad_ViewImageModelRequiredWhenEnabled(t *testing.T) {
	yaml := fmt.Sprintf(cataloglessBaseYAML, twoModelCatalog) + `
tools:
  xizhi:
    view_image:
      enabled: true
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("Load accepted an enabled view_image block without a model")
	}
	if !strings.Contains(err.Error(), "tools.xizhi.view_image.model") || !strings.Contains(err.Error(), "required") {
		t.Fatalf("error %q does not identify the missing vision model", err)
	}
}

func TestLoad_ViewImageModelMustExistInCatalog(t *testing.T) {
	yaml := fmt.Sprintf(cataloglessBaseYAML, twoModelCatalog) + `
tools:
  xizhi:
    view_image:
      enabled: true
      model: missing-model
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("Load accepted an unknown view_image model")
	}
	if !strings.Contains(err.Error(), "tools.xizhi.view_image.model") || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("error %q does not identify the invalid view_image model", err)
	}
}

func TestLoad_ViewImageDisabledSkipsModelValidation(t *testing.T) {
	yaml := fmt.Sprintf(cataloglessBaseYAML, twoModelCatalog) + `
tools:
  xizhi:
    view_image:
      enabled: false
      model: missing-model
`
	if _, err := Load(writeTempYAML(t, yaml)); err != nil {
		t.Fatalf("Load rejected a disabled view_image block: %v", err)
	}
}
