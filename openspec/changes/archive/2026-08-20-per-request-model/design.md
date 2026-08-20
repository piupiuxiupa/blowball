# Design: per-request-model

## Context

模型与思考等级目前由 `agents.<name>.model` / `thinking` / `reasoning_effort` 在启动时定死，请求侧不可选；compaction 阈值绑定全局单值 `openai.max_context_tokens`。`LLMRequest.Model/Thinking/ReasoningEffort` 字段已存在，缺的只是"目录配置 → 请求参数 → 校验 → 覆盖 agent 配置"这一层。openai-go 无独立思考开关（`shared.ReasoningEffort` 是 string 别名，`openai_client.go:202` 裸 cast），思考的开关就是发不发 `reasoning_effort`。

## Goals / Non-Goals

**Goals:**
- config 定义模型目录（name / max_context_tokens / thinking），单网关共享
- 请求参数选模型与思考等级，三 agent 一致覆盖，400 快速失败
- compaction 阈值、summary 模型、turn_usage 记录全部跟随本次请求所选模型
- `GET /api/v1/models` 列表端点
- 目录未配置 = 字节级现状（零行为变化惯例）

**Non-Goals:**
- 不做多网关/多供应商（per-model base_url/api_key），schema 留缝不实现
- 不做 per-model 请求参数差异（如 glm thinking_budget 约束）——网关适配问题，出现实际故障再议
- 不做 per-agent 差异化选择（三 agent 强制一致）
- 不动 title 的独立模型配置

## Decisions

### D1: 目录配置与零行为变化缺省

```yaml
openai:
  models:
    - name: gpt-5
      max_context_tokens: 400000   # 正整数，必填
      thinking: true               # 该模型支持 reasoning
    - name: glm-4.7
      max_context_tokens: 200000
      thinking: false
  default_model: gpt-5             # 缺省条目；未配置时取第一条
```

`models` 未配置（空列表）→ 合成隐式目录 `[{name: openai.model, max_context_tokens: openai.max_context_tokens}]`，且请求参数一律 400（目录显式配置才开放传参）——现状完全不变。目录内 `name` 唯一（校验重复失败）；`default_model` 必须指向目录内条目。

*备选*：保留 `openai.max_context_tokens` 作为目录条目缺省值的回退。否决——双来源读心，显式目录条目必填更清晰；legacy 字段只在隐式目录里用。

### D2: 请求参数解析与派生规则（handler 侧解析，构建侧应用）

```
model          目录内名称；未知 → 400 INVALID_MODEL；缺省 → default_model
reasoning_effort  none|low|medium|high|xhigh|max
                 对 thinking:false 模型仅允许 none/缺省 → 400 INVALID_EFFORT
```

解析产物是**已解析的覆盖三元组**，规则一张表：

| 请求 | 三 agent 的 model | 三 agent 的 thinking/effort |
|---|---|---|
| 无 model、无 effort | 各自 config（现状） | 各自 config（现状） |
| 仅 effort | default_model | effort（按 default 模型 gate） |
| 仅 model | 所选模型 | 模型目录派生：`thinking ? medium : none` |
| model + effort | 所选模型 | effort |

即：**请求一旦指定 model，三 agent 的思考配置完全由目录/effort 决定**（agent 级 `thinking`/`reasoning_effort` 在该 turn 不参与），避免"换了非思考模型还沿用 agent 的 effort"这类陈旧配置错误。请求未指定 model 时 agent 配置照旧。

thinking 布尔派生：`model.thinking && effort != "none"`。effort=none → 不发 `reasoning_effort`、走 `max_tokens`+`temperature` 分支；effort≠none → 发 effort、走 `max_completion_tokens` 分支（`openai_client.go:201-213` 现有分支原样复用，只是触发条件改为派生值）。

max_tokens 数值仍取各 agent 配置（只覆盖模型与思考维度，不动配额）。

防御：任一 agent 配置了 `output_schema` 且解析结果 effort≠none → 400（配置级 `Thinking+OutputSchema` 约束的运行时对齐；当前部署 Liang 未配置 output_schema，此为纯防御）。

### D3: 覆盖落点 = `AgentFactory.Build` 增参

`Build(workspaceRoot, skillsDir, userID, overrides)`，`overrides` 为已解析三元组（空 = 现状）。工厂克隆三份 `AgentConfig` 后统一覆盖再构建 agent——与现有"每请求克隆 config + 重建 registry"的模式一致；`LLMRequest` 构造点（confucius.go/chongzhi.go/liang.go 的 `Model: c.cfg.Model`）零改动，覆盖在 cfg 层完成。title 服务不参与（独立配置，不跟随）。

### D4: compaction 阈值按 turn 传入

- `ShouldCompact(tokens, limit)`：limit 由调用侧传入 = 本次请求所选模型的 `max_context_tokens × 0.8`（隐式目录 = legacy 值）。
- turn-start 检查（`SendMessage`）：读上一 turn 的 `turn_usage.context_tokens`（原始尺寸，不掺模型），与**当前请求模型**的阈值比较——天然支持"上 turn 用 A 模型、本 turn 换 B 模型"。
- mid-turn `RoundHook` 闭包捕获本 turn 的 limit；`CompactionService.Compact` 的 `SummaryModel` 同样按 turn 传入（不再取启动时快照的 `s.confucius.Model`）。
- 目录未配置时全部回退 legacy 全局值，行为与现状逐字节一致。

### D5: `turn_usage.model` 列 + 迁移 015

`turn_usage` 加 `model VARCHAR(64)`（本 turn 实际解析后的模型名；存量行 NULL）。迁移 `015_turn_usage_model.sql`，沿用惯例：现有库手动应用，initdb 挂载自动覆盖新库。`buildTurnUsage` 带上解析后的模型名。

### D6: `GET /api/v1/models`

api 分区注册（与 `GET /skills` 同类：纯配置回显、无 agent 依赖；`RegisterRoutes` 联合注册自动覆盖 `all` 角色，agent 角色不挂）。响应：

```json
{"models": [{"name": "gpt-5", "max_context_tokens": 400000, "thinking": true}], "default": "gpt-5"}
```

隐式目录（未配置 models）同样返回单条合成条目，前端无需分叉。

### D7: 与 turn-detach-resume 的交叉

run meta 的 `model` 字段即本变更的解析结果（turn-detach-resume 已在 meta schema 中预留）。两个 change 独立可实施；`service-roles` 的 Role-scoped route ownership requirement 被两者先后修改，本 change 的 delta 基于 turn-detach-resume 版本叠加——**归档顺序必须 turn-detach-resume 在前**。

## Risks / Trade-offs

- [目录 `thinking` 标注错误（把非思考模型标 true）] → 网关 4xx，llm-raw-capture 有完整请求/错误体可查；不额外加探测。
- [effort=none 对 gpt-5 系发 `max_tokens`+`temperature` 可能被官方端点拒绝] → 当前部署走兼容网关（glm 系）无碍；官方端点场景留待实际使用暴露，届时加 per-model 参数覆盖（目录条目 schema 已可平滑扩展）。
- [跨模型切换的阈值抖动] → 原始 context_tokens + 按当前请求阈值比较，模型只影响判据不影响数据；压缩记录不感知模型。
- [请求参数与 agent 配置的双轨语义] → D2 的规则表把双轨收敛为"指定 model 即全接管"，并在 openapi 中写明。

## Migration Plan

配置可选（不配 = 现状）；DB 迁移 015 存量库手动应用（不加列也不影响功能——`model` 列仅成本核算用，写入失败不阻断）。回滚 = 前端停用参数 + 回退二进制；列留存无害。归档顺序：turn-detach-resume → per-request-model（service-roles delta 叠加依赖）。

## Open Questions

- 目录条目是否需要 `max_output_tokens` 等 per-model 参数位（当前不做，出现真实网关差异再扩展）。
