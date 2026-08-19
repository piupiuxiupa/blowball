# Delta: llm-raw-capture

## ADDED Requirements

### Requirement: Watchdog abort is captured as an error row

When the LLM stream idle watchdog aborts a captured call, the sink SHALL emit a
`kind=error` row (not the partial `kind=response` row reserved for local
context cancellation) with `http_status=0` and the typed error message as the
raw body — carrying the configured idle, frames received, and model so the gap
is diagnosable without parsing chunks. The row MUST retain the call's
`call_id`/`seq` and take `frame_index = last frame + 1`, preserving the
"response/error last" ordering contract. Local context cancellation SHALL
continue to produce the partial `kind=response` row, unchanged.

#### Scenario: Idle-timeout abort produces an error row with diagnostics

- **WHEN** a captured streaming call is aborted by the idle watchdog
- **THEN** the call's final row is `kind=error` with `http_status=0`, a raw body
  naming the idle timeout plus frames received and model, ordered after the
  last `kind=chunk` row of the same `call_id`

#### Scenario: Client disconnect remains a partial response row

- **WHEN** a captured streaming call ends because the caller's context was
  canceled
- **THEN** the call's final row is the existing stitched partial
  `kind=response` row, with no error row added
