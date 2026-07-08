## 1. Prompt updates

- [x] 1.1 Add the workspace path-convention section to `internal/prompt/render.go` and remove it from agent base prompts in `config.yaml`/`config.example.yaml`.
- [x] 1.2 Verify the rendered system prompt contains the new section and does not reintroduce an absolute workspace path in `# Environment`.

## 2. Sandbox /tmp mapping

- [x] 2.1 Update `internal/tool/executor/bwrap.go` `buildBwrapArgs` to accept a workspace temp directory and bind it to `/tmp` instead of using `--tmpfs /tmp`.
- [x] 2.2 Update `internal/tool/executor/runner.go` `run()` to create `workspace/tmp/` on demand before invoking bwrap and pass it to `buildBwrapArgs`.
- [x] 2.3 Update `internal/tool/executor/bwrap_test.go` expected arguments to match the new `--bind <workspaceTmp> /tmp` behavior.

## 3. Xizhi error guidance

- [x] 3.1 Update `internal/tool/xizhi/validate.go` error messages for absolute paths and traversal to include relative-path examples.
- [x] 3.2 Update `internal/tool/xizhi/validate_test.go` to assert the guidance text is present in returned errors.

## 4. Verification

- [x] 4.1 Run `go test ./internal/tool/executor/...` and ensure all tests pass.
- [x] 4.2 Run `go test ./internal/tool/xizhi/...` and ensure all tests pass.
- [x] 4.3 Run `go test ./internal/prompt/...` if prompt rendering was touched.
- [x] 4.4 Run `make test` for full regression coverage.
