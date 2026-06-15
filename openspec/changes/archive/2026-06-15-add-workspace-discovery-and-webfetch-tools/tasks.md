## 1. Dependencies and Configuration

- [x] 1.1 Add `github.com/bmatcuk/doublestar/v4` to `go.mod` and run `go mod tidy`
- [x] 1.2 Extend `internal/config/config.go`:
  - Add `ListFiles`, `Tree`, `GlobFiles` fields to `XizhiConfig`
  - Add `WebfetchConfig` struct with `Enabled` and `Timeout`
  - Add `Webfetch` field to `ToolsConfig`
- [x] 1.3 Update `config.yaml` and `config.example.yaml` with new `tools.xizhi.*` and `tools.webfetch` sections

## 2. Xizhi File Discovery Tools

- [x] 2.1 Implement `internal/tool/xizhi/list.go`:
  - `ListFiles(workspaceRoot, relPath string, includeHidden bool)`
  - Apply `validatePath`, reject non-directory targets
  - Return entries with name, type, and size
- [x] 2.2 Add unit tests for `xizhi_list_file` in `internal/tool/xizhi/list_test.go`
- [x] 2.3 Implement `internal/tool/xizhi/tree.go`:
  - `Tree(workspaceRoot, relPath string, depth int, includeHidden bool)`
  - Default depth 3, clamp to max 10
  - Return nested tree structure
- [x] 2.4 Add unit tests for `xizhi_tree` in `internal/tool/xizhi/tree_test.go`
- [x] 2.5 Implement `internal/tool/xizhi/glob.go`:
  - `GlobFiles(workspaceRoot, relPath, pattern string, includeHidden bool)`
  - Use `doublestar.Glob` with `NoFollow` to avoid symlink escapes
  - Return matched relative paths
- [x] 2.6 Add unit tests for `xizhi_glob_files` in `internal/tool/xizhi/glob_test.go`
- [x] 2.7 Update `internal/tool/xizhi/register.go`:
  - Add tool name constants `NameListFiles`, `NameTree`, `NameGlobFiles`
  - Add parameter schemas and arg structs
  - Register the three new tools in `RegisterAll` when enabled in config

## 3. Webfetch Tool

- [x] 3.1 Create `internal/tool/webfetch/fetch.go`:
  - Implement `Fetch(url, method string, headers map[string]string)` with 30s timeout and redirect following
  - Return final URL, status, headers, and text body
  - Handle invalid URL and timeout errors
- [x] 3.2 Add unit tests for `webfetch` in `internal/tool/webfetch/fetch_test.go` using an HTTP test server
- [x] 3.3 Add `internal/tool/webfetch/register.go` to register `webfetch` tool against a registry
- [x] 3.4 Update `cmd/server/main.go` to call `webfetch.RegisterAll(reg, cfg.Tools.Webfetch)` at startup

## 4. Agent Integration

- [x] 4.1 Update `internal/agent/orchestrator.go`:
  - Extend `isXizhiTool` to recognize `xizhi_list_files`, `xizhi_tree`, `xizhi_glob_files`
- [x] 4.2 Update `config.yaml` and `config.example.yaml` agent `tools` lists:
  - Add new tools to `confuse.tools`
  - Add new tools to `chongzhi.tools`
  - Add new tools to `liang.tools`

## 5. Verification

- [x] 5.1 Run `go test ./internal/tool/...` and ensure all new and existing tests pass
- [x] 5.2 Run `go build ./...` to verify compilation
- [x] 5.3 Start server and call `GET /api/v1/mcp/tools` to confirm new tools appear in the catalogue
- [x] 5.4 Run `go mod tidy` and commit any `go.mod`/`go.sum` changes
