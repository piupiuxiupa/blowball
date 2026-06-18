## 1. Add request debug logging in OpenAIClient.StreamChat

- [x] 1.1 Import `github.com/lush/blowball/internal/pkg/logger` and `github.com/lush/blowball/internal/pkg/trace` in `internal/agent/openai_client.go`.
- [x] 1.2 Add a helper to truncate message content and tool-call arguments to a bounded preview length.
- [x] 1.3 After building `params` in `StreamChat`, emit a `logger.L().Debug` entry with:
  - `trace_id` from `trace.FromContext(ctx)` (when present),
  - `model`,
  - `message_count`,
  - `messages` as a slice of `{role, content_preview}` objects,
  - `tools_count` and `tools_preview`,
  - `max_tokens`.

## 2. Add response debug logging in OpenAIClient.StreamChat

- [x] 2.1 After the streaming loop finishes and `resp` is populated, emit a `logger.L().Debug` entry with:
  - `trace_id` from `trace.FromContext(ctx)` (when present),
  - `finish_reason`,
  - `content_len` and `content_preview`,
  - `tool_calls` as a slice of `{name, arguments_preview}` objects,
  - `usage` (prompt_tokens, completion_tokens, total_tokens).

## 3. Verify and test

- [x] 3.1 Ensure `go build ./...` passes.
- [x] 3.2 Run existing `internal/agent` tests to confirm no regression.
- [x] 3.3 Start the server with `logging.level: debug` and send a chat message; confirm that `llm_request` and `llm_response` debug log lines appear in stdout.
- [x] 3.4 Verify that no API key or Authorization header is present in the debug output.
