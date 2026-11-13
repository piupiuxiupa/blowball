package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWebfetchDigestConfig_ResolveDefaults(t *testing.T) {
	got := WebfetchDigestConfig{
		ThresholdBytes:     -1,
		SingleShotMaxBytes: -1,
		ChunkBytes:         -1,
		MaxChunks:          -1,
		Concurrency:        -1,
		Timeout:            -time.Second,
		ChunkOutputTokens:  -1,
		FinalOutputTokens:  -1,
	}.Resolve()

	if got.ThresholdBytes != DefaultWebfetchDigestThresholdBytes ||
		got.SingleShotMaxBytes != DefaultWebfetchDigestSingleShotMaxBytes ||
		got.ChunkBytes != DefaultWebfetchDigestChunkBytes ||
		got.MaxChunks != DefaultWebfetchDigestMaxChunks ||
		got.Concurrency != DefaultWebfetchDigestConcurrency ||
		got.Timeout != DefaultWebfetchDigestTimeout ||
		got.ChunkOutputTokens != DefaultWebfetchDigestChunkTokens ||
		got.FinalOutputTokens != DefaultWebfetchDigestFinalTokens {
		t.Fatalf("Resolve() = %+v, want all documented defaults", got)
	}
}

func TestWebfetchDigestConfig_ResolveKeepsSingleShotAboveThreshold(t *testing.T) {
	got := WebfetchDigestConfig{ThresholdBytes: 300 << 10}.Resolve()
	if got.SingleShotMaxBytes <= got.ThresholdBytes {
		t.Fatalf("SingleShotMaxBytes = %d, want > ThresholdBytes %d", got.SingleShotMaxBytes, got.ThresholdBytes)
	}
}

func TestLoad_WebfetchDigestModelMustExistInCatalog(t *testing.T) {
	yaml := fmt.Sprintf(cataloglessBaseYAML, twoModelCatalog) + `
tools:
  webfetch:
    enabled: true
    digest:
      enabled: true
      model: missing-model
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("Load accepted an unknown webfetch digest model")
	}
	if !strings.Contains(err.Error(), "tools.webfetch.digest.model") || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("error %q does not identify the invalid digest model", err)
	}
}
