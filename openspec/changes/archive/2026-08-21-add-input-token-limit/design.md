# Design: add-input-token-limit

## Context

`SendMessage`（`internal/handler/message_stream.go:142`）当前对 `content` 仅做非空校验（148 行），之后进入 model/effort 解析 → ownership → 历史恢复 → run claim → 编排。全代码库没有 tokenizer：token 数只在事后由网关 usage 回报（compaction 的哲学是 "authoritative usage, never estimation"，`internal/agent/roundhook.go:12`）。因此事前检测只能是估算 —— 对防滥用红线而言足够。

另一个现状：该端点无请求体大小上限。`MaxBytesReader` 只在 upload（`internal/handler/workspace.go:161-175`）使用，那里已有 `errors.As(err, &maxErr)` → 413 的检测模式可复用。

## Goals / Non-Goals

**Goals:**

- 恶意/误操作超大输入在入口被拒，不占用 run 认领槽、不触发任何 store/LLM 调用。
- 字节上限（DoS 后备层）+ token 估算上限（语义层）双层防线。
- token 上限可通过配置调整（coding agent 产品粘贴大段代码是合理用法，5000 不应当死）。

**Non-Goals:**

- 不做精确 token 计数（不引入 tiktoken 等依赖；网关后是 glm，tokenizer 本就与 cl100k 不符）。
- 不限制 `PATCH` 标题长度、不改动文本写文件的 413 字节上限。
- 不做按用户/按 IP 的限流 —— 那是另一个能力。

## Decisions

### D1: 估算算法 —— CJK 感知启发式，handler 包内助手

```go
// tokens.go
func estimateTokens(s string) int {
    cjk, other := 0, 0
    for _, r := range s {
        if isCJK(r) { cjk++ } else { other++ }
    }
    return cjk + other/4
}
```

- `isCJK` 覆盖 Han/Hiragana/Katakana/Hangul 等常用 CJK 区块（`unicode.Is(unicode.Han, r)` 等）。
- 误差方向刻意偏高（中文 1 字 ≈ 1 token 高估了 glm 的 ~0.6-0.75；英文 4 字符/token 是准数），宁可保守拒绝。
- **备选**：`rune/1.5` 一刀切 —— 否决，纯英文高估 ~2.5×，7500 字符的正常英文（实际 ~1900 token）会被误杀；tiktoken-go —— 否决，重依赖且对 glm 不准。

### D2: 配置 —— `messages.max_input_tokens`，默认 5000，显式 0=关闭，负数拒绝

- 归属现有 `messages:` 块（`flush_interval`/`flush_batch_size` 旁），`MessagesConfig` 增加指针字段 `MaxInputTokens *int`。
- 语义（"默认开启 5000"与"显式 0 关闭"并用 plain int 无法共存 —— yaml 未配置与显式 0 同为零值，必须用指针区分，仓库已有 `*bool` nil→默认 先例如 `PipToolConfig.Network`）：
  - 未配置（nil）→ 生效上限 5000（默认开启）；
  - 显式 `0` → 关闭检测；
  - 正整数 → 生效上限；
  - 负数 → config 加载报错（对齐块内其他键"负数拒绝"惯例）。
- config 加载后归一出一个带符号整数（handler 拿到的 `limit <= 0` 即关闭），handler 不接触指针。

### D3: 字节上限 —— 常量 1MB，不进 config

- `const maxMessageBodyBytes = 1 << 20`（handler 包内），bind 前套 `http.MaxBytesReader(c.Writer, c.Request.Body, maxMessageBodyBytes)`。
- 5000 token 最坏形态（纯 CJK 3 字节/字）≈ 15KB，1MB 留 ~65× 余量 —— 它是防打爆内存的后备层，正常请求永远碰不到，因此不是运营旋钮，不进 config（upload 的 `MaxUploadBytes` 同为常量）。
- 检测：`ShouldBindJSON` 出错后 `errors.As(err, &maxErr)`（`*http.MaxBytesError`）→ `413 REQUEST_TOO_LARGE`。encoding/json 的 Decode 会把读错误原样透传，`errors.As` 可命中（与 upload 的 `ParseMultipartForm` 路径同模式）。
- **备选**：上限也进 config —— 否决，多一个无人调的旋钮；写死 const 简单诚实。

### D4: 校验次序与错误形状

```
① MaxBytesReader 包住 body        （bind 前）
② ShouldBindJSON → MaxBytesError → 413 REQUEST_TOO_LARGE
③ content 非空 → 400 BAD_REQUEST   （现状，不动）
④ estimateTokens(content) > MaxInputTokens
   → 400 {"error":{"code":"CONTENT_TOO_LONG",
         "message":"content exceeds the 5000-token limit (estimated 8231 tokens)"}}
⑤ model/effort 解析 → ⑥ ownership → ⑦ recover → ⑧ claim → …
```

- ④ 在 ③ 之后、⑤ 之前：纯内存计算，任何 store IO 之前，绝不占用 run 槽。
- 错误码命名对齐现有风格（`INVALID_MODEL`/`FILE_TOO_LARGE`）；message 带上限与估算值，前端可直接提示"请缩减输入"。
- 响应遵循统一错误格式（`{"error":{code,message}}`），`api-server` 的 Unified error response 需求无需变更。

### D5: 放置位置 —— 校验留在 handler 层，不下沉 service

token 检查是入口协议校验（同 `req.Content == ""`），属 handler 职责；不下沉 `SessionService`/`MessageService`。估算助手独立小文件 `internal/handler/tokens.go` + 同包测试，保持可单测。

## Risks / Trade-offs

- [估算偏差误拒边界输入（如实际 4800 token 被估成 5100）] → 误差方向已知且偏小（英文准、中文高估 ~1.4×）；运营可调 `max_input_tokens` 兜底；错误 message 带估算值便于排查。
- [`*int` 指针字段比 plain int 多一点接线样板] → 只在 config 加载处出现一次，换来"默认开启"与"显式关闭"两种语义精确共存，符合仓库 `*bool` 先例。
- [`errors.As` 依赖 json 透传读错误的实现细节] → upload 路径同模式已在线上；若某 gin 版本包装了错误，测试会立即暴露（用例覆盖超限 body → 413）。
- [1MB 常量未来可能想调] → 常量改 config 是机械改动，先不为假想需求增加面。

## Migration Plan

纯增量校验，无部署顺序要求；上线即默认开启 5000。回滚 = 还原代码（配置键残留无害）。前端 `npm run generate-api` 再生成类型以获得新错误码枚举（可选，错误码是字符串字面量，不生成也不破坏前端）。

## Open Questions

（无 —— 探索阶段已定：估算算法、配置语义、字节上限、覆盖面）
