## Why

`webfetch` currently reads and returns every response byte verbatim. A large or markup-heavy page can consume excessive memory and push the agent context toward its token limit, while scripts, styles, and boilerplate dilute useful content.

## What Changes

- Add an operator-configurable download cap that bounds response bytes retained by `webfetch`.
- Convert HTML responses to a compact, Markdown-oriented text representation before returning them to the model.
- Add an operator-configurable extracted-content cap and machine-readable truncation metadata.
- Add opt-in, threshold-gated small-model digestion: small pages return directly, medium pages use one model call, and large pages use bounded concurrent chunk analysis.
- Retain existing redirect, timeout, method, header, and error-recovery behavior.
- Document the new knobs in `config.example.yaml`.

Model digestion is opt-in. It is triggered by extracted `content_bytes`, not raw HTML size, so ordinary small pages never incur model calls.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `webfetch`: Bounded response downloads, HTML extraction, output truncation, and truncation metadata.

## Impact

- `internal/tool/webfetch`: response reading, HTML extraction, digest chunking, result schema, and tool description.
- `internal/agent`: a narrow adapter from the digest prompt interface to the shared LLM client.
- `cmd/blowball`: webfetch digest wiring.
- `internal/config`: `tools.webfetch` and `tools.webfetch.digest` fields.
- `config.example.yaml`: examples for the new fields.
- Tests and the `webfetch` OpenSpec delta.
