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
