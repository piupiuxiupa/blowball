## Why

`webfetch` currently reads and returns every response byte verbatim. A large or markup-heavy page can consume excessive memory and push the agent context toward its token limit, while scripts, styles, and boilerplate dilute useful content.

## What Changes

- Add an operator-configurable download cap that bounds response bytes retained by `webfetch`.
- Convert HTML responses to a compact, Markdown-oriented text representation before returning them to the model.
- Add an operator-configurable extracted-content cap and machine-readable truncation metadata.
- Retain existing redirect, timeout, method, header, and error-recovery behavior.
- Document the new knobs in `config.example.yaml`.

The change does not add model-backed content digestion in this step. Large-content fan-out to a small model is a subsequent design decision because it needs persisted chunks, worker budgets, and merge/failure semantics.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `webfetch`: Bounded response downloads, HTML extraction, output truncation, and truncation metadata.

## Impact

- `internal/tool/webfetch`: response reading, HTML extraction, result schema, and tool description.
- `internal/config`: `tools.webfetch` fields.
- `config.example.yaml`: examples for the new fields.
- Tests and the `webfetch` OpenSpec delta.
