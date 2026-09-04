## Context

`webfetch` is a host-network built-in tool executed outside the bwrap network sandbox. Its current implementation calls `io.ReadAll` and returns the complete response body verbatim. That behavior provides excellent fidelity for small documents but has no defense against multi-megabyte responses, markup noise, or accidental binary downloads.

Mainstream agent implementations commonly use two independent bounds: one on bytes accepted from the network and a smaller bound on content injected into model context. They also extract readable HTML content rather than returning the source markup unchanged.

## Goals / Non-Goals

**Goals:**

- Bound retained response bytes with a configurable download limit (default 5 MiB).
- Convert HTML and XHTML responses to compact Markdown-oriented text.
- Bound extracted content with a configurable output limit (default 100 KiB).
- Expose machine-readable truncation metadata and an in-body truncation notice.
- Preserve redirect, timeout, method, header, status, and response-header behavior.

**Non-Goals:**

- No response cache, SSRF policy, JavaScript rendering, or binary file download support.
- No model-backed map-reduce digestion in this change. A truncation flag alone cannot recover omitted bytes; a future implementation must persist complete chunks (or support ranged requests) before dispatching concurrent small-model workers.
- No attempt to produce byte-perfect HTML-to-Markdown conversion; the extraction prioritizes stable, useful headings, links, images, lists, code, and readable text.

## Decisions

### Two independent limits

`max_download_bytes` protects process memory and bounds network transfer; `max_output_bytes` protects the agent context after extraction. They intentionally have different defaults (5 MiB and 100 KiB). Download truncation is represented separately from overall truncation so operators and models can distinguish an upstream cut from the context cap.

Reading uses `io.LimitReader(limit + 1)`. The extra byte makes truncation deterministic without a second read, after which the buffer is cut to the configured limit.

### Extract before final truncation

HTML is parsed with `golang.org/x/net/html` and charset detection. Scripts, styles, templates, and non-content head metadata are omitted. The lightweight renderer emits Markdown-style headings, links, images, list items, code blocks, tables, and normalized text.

The final cap is applied only after extraction. This keeps a 4 MiB HTML page with 40 KiB of readable article text intact while still bounding pathological output. UTF-8 truncation backs up to a rune boundary where possible.

### Metadata and marker

The result carries:

- `converted_from_html`: whether extraction was applied;
- `content_bytes`: extracted byte count before the output cap;
- `download_truncated`: whether the network response exceeded the download cap;
- `truncated`: whether either the download or output cap affected the returned body.

When `truncated` is true, the body also ends with a short bracketed notice. The structured fields support tooling; the text marker makes the limit visible to the model without requiring it to inspect metadata.

### Configuration semantics

As with `timeout` and `max_redirects`, defaults are resolved inside the tool when values are zero or negative. This preserves the existing no-defaults configuration block and keeps directly constructed test configurations simple.

## Risks / Trade-offs

- [The lightweight HTML renderer is not Turndown-complete] → It preserves common semantic content and always falls back to readable text; non-HTML responses are not transformed.
- [A download cut can end HTML mid-tag] → `x/net/html` recovers a document from partial markup, and `download_truncated` plus the body marker disclose the cut.
- [A fixed output cap can omit relevant tail content] → The cap is configurable and explicitly marked; future chunk digestion can use the metadata as an trigger, but must retain or refetch the omitted content.
- [`golang.org/x/net` becomes a direct dependency] → It is already in the module graph, Go-standard-adjacent, and required for robust charset-aware HTML parsing.

## Migration Plan

The default limits activate when operators upgrade. Existing small HTML/text responses remain usable but HTML bodies become extracted Markdown rather than source markup. Operators needing prior fidelity can raise both limits; exact prior raw-HTML behavior is intentionally not guaranteed because source markup is not model-efficient content.

Rollback is a code rollback; no persisted schema or migration is introduced.

## Open Questions

- Should a future large-document mode spill complete extracted chunks to per-user workspace files and fan them out to a small model? If so, the manifest needs byte ranges, content hashes, worker concurrency, per-worker token/output caps, and a deterministic merge/failure policy.
