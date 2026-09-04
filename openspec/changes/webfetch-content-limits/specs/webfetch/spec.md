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
