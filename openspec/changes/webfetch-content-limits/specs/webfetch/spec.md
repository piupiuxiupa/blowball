## ADDED Requirements

### Requirement: Bounded webfetch response and model content
`webfetch` SHALL bound response bytes retained from the network with `tools.webfetch.max_download_bytes` (default 5 MiB) and SHALL bound extracted content returned to the model with `tools.webfetch.max_output_bytes` (default 100 KiB). Zero or negative values for either field SHALL fall back to the corresponding default. A response that exceeds the download cap SHALL still return its available status, headers, and truncated content rather than failing solely because of size.

#### Scenario: Response exceeds the download cap
- **WHEN** `webfetch` receives a response body larger than `max_download_bytes`
- **THEN** the tool retains at most `max_download_bytes` response bytes
- **AND** the result reports `download_truncated: true` and `truncated: true`
- **AND** the returned body ends with a visible truncation notice

#### Scenario: Download cap defaults
- **WHEN** `max_download_bytes` is omitted, zero, or negative
- **THEN** the tool uses a 5 MiB download cap

#### Scenario: Extracted content exceeds the output cap
- **WHEN** content extracted from a response is larger than `max_output_bytes`
- **THEN** the returned body is cut to at most `max_output_bytes`
- **AND** the result reports `content_bytes` as the pre-cap extracted size and `truncated: true`
- **AND** the returned body ends with a visible truncation notice

#### Scenario: Output cap defaults
- **WHEN** `max_output_bytes` is omitted, zero, or negative
- **THEN** the tool uses a 100 KiB output cap

### Requirement: HTML responses are extracted as Markdown-oriented text
For HTML or XHTML responses, `webfetch` SHALL parse the response with charset detection, omit script, style, template, and non-content head metadata, and return a Markdown-oriented representation containing readable text and common semantic elements such as headings, links, images, lists, code, and tables. The result SHALL report `converted_from_html: true`. Non-HTML response bodies SHALL be returned as text without HTML extraction.

#### Scenario: HTML scripts and styles are omitted
- **WHEN** an HTML response contains readable text plus `<script>` and `<style>` elements
- **THEN** the returned body contains the readable text
- **AND** the returned body does not contain the script or style contents
- **AND** the result reports `converted_from_html: true`

#### Scenario: HTML semantics are retained
- **WHEN** an HTML response contains a heading and an anchor with an absolute or page-relative `href`
- **THEN** the returned Markdown-oriented body represents the heading level and links to the resolved URL

#### Scenario: Non-HTML content is not converted
- **WHEN** a response has a non-HTML text content type
- **THEN** the tool returns the text body without applying HTML extraction
- **AND** the result reports `converted_from_html: false`

### Requirement: Threshold-gated small-model content digestion
When `tools.webfetch.digest.enabled` is true, `webfetch` SHALL decide whether to invoke the configured digest model from extracted `content_bytes`, not from raw response size. Content at or below `threshold_bytes` SHALL be returned directly with no digest model call. Content above `threshold_bytes` and at or below `single_shot_max_bytes` SHALL use one digest model call. Larger content SHALL be split into bounded chunks and analyzed concurrently up to `concurrency` before being reduced. The tool SHALL accept an optional `objective` argument that tells the digest model what information to retain.

#### Scenario: Small content does not invoke the digest model
- **WHEN** digesting is enabled and extracted `content_bytes` is at or below `threshold_bytes`
- **THEN** the result has `processing_mode: "direct"` and no digest model call is made

#### Scenario: Medium content uses one model call
- **WHEN** extracted `content_bytes` is above `threshold_bytes` and at or below `single_shot_max_bytes`
- **THEN** the digest model is called once with the full extracted content and objective
- **AND** the result reports a single-shot digest

#### Scenario: Large content uses bounded concurrent map-reduce
- **WHEN** extracted `content_bytes` is above `single_shot_max_bytes`
- **THEN** the content is split into chunk files of at most `chunk_bytes`
- **AND** at most `max_chunks` chunks are accepted
- **AND** at most `concurrency` digest model calls run simultaneously
- **AND** successful chunk analyses are reduced in original order
- **AND** the result reports chunk counts, analyzed coverage, and digest status

#### Scenario: Digest cost is bounded
- **WHEN** extracted content would require more than `max_chunks` chunks
- **THEN** the tool does not expand the worker fan-out
- **AND** it falls back to bounded direct content with a skipped digest result

#### Scenario: Digest failure preserves the fetch result
- **WHEN** a digest model call fails or returns no usable content
- **THEN** `webfetch` still returns the fetched status, headers, and bounded direct content
- **AND** the digest metadata reports the failure
