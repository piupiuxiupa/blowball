# Tasks: add-llm-stream-idle-watchdog

## 1. Config surface

- [x] 1.1 `internal/config/config.go`：`OpenAIConfig` 新增 `StreamIdleTimeout time.Duration`（yaml `stream_idle_timeout`），零值=禁用；load 校验负值报错（duration 走 time.ParseDuration，先确认无 yaml.v3 截断问题再决定是否需要 max_context_tokens 式 raw 值影子检查）
- [x] 1.2 `config.example.yaml` 的 `openai:` 块补 `stream_idle_timeout` 注释示例：推荐 `2m`、零值/缺省=禁用、约束的是帧间隔与首帧（含 time-to-first-frame），不是总时长

## 2. StreamChat 看门狗

- [x] 2.1 `internal/agent/openai_client.go`：`OpenAIClient` 构造时带入 idle 值（`NewOpenAIClientWithSink` 与 from-client 构造器都补），`StreamChat` 在 idle>0 时派生 `callCtx, cancel := context.WithCancel(ctx)`，`NewStreaming` 改传 `callCtx`，启动看门狗 goroutine（buffered-1 reset channel + `time.Timer` + `atomic.Bool` fired 标志，`defer cancel()`），按 design D2
- [x] 2.2 读循环每接受一帧后非阻塞 kick 看门狗（`select { case reset <- struct{}{}: default: }`），任意帧都重置（空 delta / usage 帧 / reasoning 帧同权重），按 design D4
- [x] 2.3 新增 `var ErrStreamIdleTimeout`；读循环退出后按 design D3 三态分诊：父 `ctx.Err()` 优先（走既有 `capturePartial` 路径，不动）；其次 fired → `captureError` + 返回 `fmt.Errorf("%w: no frame for %s (frames=%d, model=%s)", ...)`
- [x] 2.4 触发时打一条 WARN 日志（agent 名取自 ctx、model、已收帧数、配置 idle），按 design D8

## 3. 测试

- [x] 3.1 `internal/agent` 看门狗测试（fake 流式 server 或 stub stream）：中途停摆 → 类型化错误且消息含帧数/model；首帧永不到 → 类型化错误；连续出帧总时长远超 idle → 正常完成；配置零值/缺省 → 行为与改动前逐字节一致
- [x] 3.2 取消优先级测试：父 ctx 与 timer 同时触发 → 返回 `context.Canceled` + partial capture，绝不返回 idle 错误
- [x] 3.3 goroutine 泄漏测试：正常完成、报错、父取消三种路径下看门狗均退出（goleak 式或按包内既有测试风格数 goroutine）
- [x] 3.4 重试分类契约测试：`isTransientError(wrap(ErrStreamIdleTimeout)) == true`，守护零改动重试集成所依赖的 "timeout" 子串契约
- [x] 3.5 config 校验测试：`stream_idle_timeout: -1s` 加载失败；缺省/`0` 加载成功且字段为零值
- [x] 3.6 raw capture 行测试：挂 sink 且看门狗触发时，末行 `kind=error`、`http_status=0`、`frame_index = 最后 chunk + 1`、raw 体含 idle/帧数/model；父取消路径仍是 partial `kind=response` 行

## 4. 文档与验证

- [x] 4.1 CLAUDE.md 的 Agent orchestration / Important conventions 段落补一句 `openai.stream_idle_timeout`（opt-in 帧间空闲上限、超时按 transient 分类进重试管道）
- [x] 4.2 `make lint && make test` 全绿；`go test ./test/integration/...` 不回归（fake LLM 不受影响）
