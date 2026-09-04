## 1. Configuration and OpenSpec

- [x] 1.1 Add `max_download_bytes` and `max_output_bytes` to `WebfetchConfig`.
- [x] 1.2 Document both fields and their defaults in `config.example.yaml`.

## 2. webfetch implementation

- [x] 2.1 Read responses through a bounded reader and record download truncation without failing solely for size.
- [x] 2.2 Add charset-aware HTML extraction to Markdown-oriented text, omitting scripts/styles and preserving common semantic elements.
- [x] 2.3 Apply the post-extraction output cap on a UTF-8 boundary, add structured truncation metadata, and append a visible truncation marker.
- [x] 2.4 Pass configured limits through `RegisterAll` and update the tool description.

## 3. Tests and validation

- [x] 3.1 Test download truncation and metadata.
- [x] 3.2 Test HTML extraction, script/style removal, and semantic conversion.
- [x] 3.3 Test output truncation and defaults.
- [x] 3.4 Run `gofmt`, package tests, and the relevant broader test suite.

## 4. Threshold-gated model digestion

- [x] 4.1 Add `tools.webfetch.digest` configuration, normalized defaults, validation, and examples.
- [x] 4.2 Add a webfetch-owned digest service with direct/single-shot/map-reduce modes, transient chunk files, bounded concurrency, prompts, and result metadata.
- [x] 4.3 Refactor fetch execution to carry context and the optional objective, invoke digestion before the final output cap, and degrade gracefully on digest failure.
- [x] 4.4 Add the agent-side prompt adapter and wire it into server startup with a catalog-resolved digest model.
- [x] 4.5 Update the tool schema and description.

## 5. Digest tests and validation

- [x] 5.1 Test configuration normalization and validation.
- [x] 5.2 Test no model call below threshold, one call in single-shot mode, and bounded concurrent calls in map-reduce mode.
- [x] 5.3 Test chunk-file cleanup, partial failure, oversized-content fallback, and fetch degradation.
- [x] 5.4 Test the agent prompt adapter and registration objective passthrough.
- [x] 5.5 Run formatting, lint, focused tests, OpenSpec validation, and the full race suite.
