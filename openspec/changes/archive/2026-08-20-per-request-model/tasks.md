# Tasks: per-request-model

## 1. 配置层：模型目录

- [x] 1.1 `OpenAIConfig` 增加 `models` 目录（`name`/`max_context_tokens`/`thinking`）与 `default_model`；加载校验：目录内 name 唯一、max_context_tokens 正整数（raw 值 shadow-check 防 yaml 截断，沿用 max_context_tokens 惯例）、default_model 指向目录条目
- [x] 1.2 实现目录解析 API：显式目录直读；未配置时合成隐式目录（`openai.model` + legacy `openai.max_context_tokens`）并提供"请求参数是否可用"判定
- [x] 1.3 `config.example.yaml` 增加 models/default_model 注释示例；配置层测试（校验失败各分支、隐式目录合成、legacy 字段仍生效）

## 2. 请求参数解析与校验（handler 层）

- [x] 2.1 `sendMessageRequest` 增加 `model`/`reasoning_effort`；实现设计 D2 的解析规则表：解析为覆盖三元组；错误分支返回 400 `INVALID_MODEL` / `INVALID_EFFORT`；隐式目录下携带参数 400；任一 agent 配置 `output_schema` 且 effort≠none → 400
- [x] 2.2 解析单元测试：规则表全分支（无参/仅 effort/仅 model/双参 × 目录 thinking 真假 × 非法取值）

## 3. Agent 工厂覆盖

- [x] 3.1 `AgentFactory.Build` 签名增加已解析覆盖参数；工厂克隆三 agent 的 `AgentConfig` 并统一覆盖 `Model`/`Thinking`/`ReasoningEffort`（thinking 派生 = `model.thinking && effort != none`）；`max_tokens`/工具/提示词不受影响
- [x] 3.2 验证 LLM 请求分支：effort=none 不发 `reasoning_effort` 且走 `max_tokens`+`temperature`；effort≠none 走 `max_completion_tokens`（复用现有 `openai_client.go` 分支，仅触发条件改派生值）
- [x] 3.3 agent 工厂与三 agent 覆盖一致性测试（fake client 断言三次调用的 model/effort 相同；未带参数时 agent 配置照旧）

## 4. Compaction 联动

- [x] 4.1 `CompactionService` 阈值与 summary 模型按 turn 传入：`ShouldCompact(tokens, limit)`、`Compact` 的 `SummaryModel` 取本 turn 所选模型；turn-start 检查用当前请求模型的 `0.8 × max_context_tokens` 比较上一 turn 的原始 `context_tokens`；mid-turn `RoundHook` 闭包捕获本 turn limit
- [x] 4.2 compaction 测试：跨模型切换阈值触发/不触发、隐式目录回退 legacy 值、目录未配置时与现状逐字节一致

## 5. turn_usage 模型列

- [x] 5.1 迁移 `015_turn_usage_model.sql`（`model VARCHAR(64) NULL`；存量库手动应用惯例写入 CLAUDE.md）；store 层与 `buildTurnUsage` 写入解析后模型名
- [x] 5.2 持久化测试（model 列写入、存量 NULL 容忍）

## 6. 模型列表端点

- [x] 6.1 实现 `GET /api/v1/models` handler（返回目录 name/max_context_tokens/thinking + default；隐式目录返回合成条目）并注册到 api 分区路由（JWT）
- [x] 6.2 端点测试（显式目录、隐式目录、401、agent 角色不注册）

## 7. API 契约与文档

- [x] 7.1 更新 `api/openapi.yaml`：请求体 `model`/`reasoning_effort` 参数与 400 `INVALID_MODEL`/`INVALID_EFFORT`、`GET /api/v1/models`、`turn_usage` 语义说明；注明前端仓需重跑 `npm run generate-api`
- [x] 7.2 更新 `CLAUDE.md`：`openai.models` 配置说明、请求参数语义（含"指定 model 即全接管思考配置"规则）、端点表、迁移 015 手动应用说明、归档顺序依赖（turn-detach-resume 在前）

## 8. 回归验证

- [x] 8.1 集成测试：带参请求三 agent 一致使用所选模型（fake LLM 断言）；未配置目录 + 带参 400；`make test` 与 `make lint` 全绿
