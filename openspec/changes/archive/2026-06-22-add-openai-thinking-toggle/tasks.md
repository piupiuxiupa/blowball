## 1. Configuration schema and validation

- [x] 1.1 Add `Thinking bool` and `ReasoningEffort string` fields to `AgentConfig` in `internal/config/config.go`.
- [x] 1.2 Add startup validation for `reasoning_effort` values (`low`, `medium`, `high`) in `AgentsConfig.validate` or a new helper method.
- [x] 1.3 Add unit tests in `internal/config/config_test.go` for valid/invalid reasoning configurations.

## 2. LLM request plumbing

- [x] 2.1 Add `Thinking bool` and `ReasoningEffort string` to `LLMRequest` in `internal/agent/agent.go`.
- [x] 2.2 Populate `Thinking` and `ReasoningEffort` from `cfg` in `Confuse.Run` request construction (`internal/agent/confuse.go`).
- [x] 2.3 Populate `Thinking` and `ReasoningEffort` from `cfg` in `Chongzhi.Run` request construction (`internal/agent/chongzhi.go`).
- [x] 2.4 Populate `Thinking` and `ReasoningEffort` from `cfg` in `Liang.Run` request construction (`internal/agent/liang.go`).

## 3. OpenAI client mapping

- [x] 3.1 In `OpenAIClient.StreamChat` (`internal/agent/openai_client.go`), set `params.ReasoningEffort` when `req.Thinking` is true.
- [x] 3.2 In `OpenAIClient.StreamChat`, map `req.MaxTokens` to `params.MaxCompletionTokens` when `req.Thinking` is true; keep `params.MaxTokens` when false.
- [x] 3.3 Ensure `params.Temperature` is not sent for reasoning requests (already guarded; add reasoning-aware test if needed).
- [x] 3.4 Optionally include `reasoning_effort` in `logLLMRequest` output for observability.

## 4. Tests and documentation

- [x] 4.1 Update `fakeLLMClient` in `internal/agent/fake_client_test.go` to capture `Thinking` and `ReasoningEffort`.
- [x] 4.2 Add unit tests for reasoning request shape in `internal/agent/chongzhi_test.go`, `liang_test.go`, and `confuse_test.go`.
- [x] 4.3 Add or update integration tests in `test/integration/` to verify reasoning effort is propagated through the orchestrator.
- [x] 4.4 Update `config.example.yaml` with `thinking` and `reasoning_effort` examples.
- [x] 4.5 Run `make test` and `make lint` and fix any failures.
