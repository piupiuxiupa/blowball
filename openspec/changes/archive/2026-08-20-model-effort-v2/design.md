# model-effort-v2 — 设计

## Context

per-request-model-selection(已实现待归档)落地后的现状:

```
openai.model ──────────┐  TitleService 模型 / 隐式目录条目名 / DefaultModelName 回退
openai.models[] ───────┤  请求可选目录(thinking bool 门禁 effort)
openai.default_model ──┘
agents.<name>.model            各 agent 自带模型
agents.<name>.thinking (bool)  + reasoning_effort(仅 thinking:true;未设补 none)
请求 model/reasoning_effort → D2 四行表 → ModelOverride{Model, Thinking, ReasoningEffort}
wire: Thinking? → reasoning_effort + max_completion_tokens;else → max_tokens(+temperature,恒 0 死代码)
```

痛点:(1) 三处来源、四行解析、双路径(显式/隐式目录)代码与 spec 双倍;(2) 请求级 `effort=none` 翻成 `Thinking=false` → 参数整个不发,glm 网关上的思考型模型按自身默认等级继续思考;(3) agent 间模型/思考可漂移,与"一 turn 一模型"的意图相悖。

本部署 operator 即唯一用户,接受一次 breaking 配置迁移。前置:先归档 `turn-detach-resume`、`per-request-model`(本设计基于归档后语义)。

## Goals / Non-Goals

**Goals:**

- 模型与 effort 各只剩一条配置来源(目录 + 两个 default 字段),请求参数按双轴覆盖
- `reasoning_effort: "none"` 在思考型条目上显式下发(B2),让兼容网关真正关掉思考
- 隐式目录/零行为变化机制整体移除,单一代码路径
- 残留旧字段在 load 时报迁移错误(fail-fast,不静默)
- GET /models 增量暴露 `default_reasoning_effort`

**Non-Goals:**

- 不做 per-agent effort 差异化(明确放弃,见 Trade-offs)
- 不做配置兼容垫片/自动迁移(旧字段不自动翻译,报错指路)
- 不改 `max_tokens` 配额、tools、prompt、retry 等 agent 能力面
- 不改 wire 的其他部分(watchdog、raw-capture、重试管线不动)
- 目录条目的 `thinking` 字段保留(模型能力标记,非开关)

## Decisions

### D1 配置面:目录必填,五个字段定乾坤

```yaml
openai:
  models:                     # 必填,≥1 条;校验同现状(唯一名/正整数窗口/default_model 在目录内)
    - name: glm-5.2
      max_context_tokens: 200000
      thinking: true
  default_model: glm-5.2           # 未设 → 第一条(同现状)
  default_reasoning_effort: high   # 新增;none|low|medium|high|xhigh|max;未设 → none
  title_model: gpt-4o-mini         # 原 openai.model;未设 → 回退 default 条目;不必属于目录
  # 删除:openai.model、顶层 openai.max_context_tokens
agents:
  <name>:
    # 删除:model / thinking / reasoning_effort
    # 保留:name / system_prompt / max_tokens / tools / mcp / skills / max_rounds / output_schema / retry
```

- **目录必填在 config load 校验**(非 role 感知):`seed` 子命令也走 `config.Load`,但 seed 与 serve 共用同一份 config.yaml,operator 配好目录后无实际影响;比 api_key 那种 serve-time role 感知检查简单。
- `default_reasoning_effort` 未设 → `none`:保持旧配置"默认不开思考"的精神(thinking 默认 false ≈ none)。
- `title_model` 未设回退 default 条目:标题生成骑默认模型,想省钱就显式填;它只是发给网关的名字,**不要求**出现在目录里(标题调用走非思考分支,`max_tokens`,无 `reasoning_effort`)。
- 备选与否决:`title_model` 设为必填——否决,强制填一个冗余字段没有收益;保留 `openai.model` 双语义——否决,正是本次要消灭的歧义。

### D2 解析坍缩:两条独立轴,无特例

```
model  = 请求.model  || default 条目(default_model || 第一条)
effort = 请求.effort || openai.default_reasoning_effort
门禁:entry.thinking == false → effort = none
     - 请求显式非 none   → 400 INVALID_EFFORT(同现状)
     - 全局默认非 none   → 钳为 none + WARN(无法在 load 时穷举:默认条目可思考、其他条目不可)
output_schema 冲突:任一 agent 带 output_schema 且解析后 effort ≠ none → 400(同现状)
```

- D2 旧四行表的"仅 model 接管 thinking / 派生 medium"特例消失:不存在 agent 级配置可接管,无参数与仅 model 对 effort 是同一条路(全局默认)。
- 备选与否决:model-only 仍硬编码 `medium`——否决,`default_reasoning_effort` 已是"部署默认等级"的自然来源,`medium` 是隐藏魔法值。
- 配置级 fail-fast:任一 agent 带 `output_schema` 且 `default_reasoning_effort ≠ none` → load 失败(否则无参数 turn 会静默踩结构化输出 × reasoning 冲突);运行时请求级 400 门禁保持。

### D3 B2 wire 家族:形态跟条目能力走

| 条目 | reasoning_effort | token 参数 | temperature |
|---|---|---|---|
| `thinking: true` | **永远发**(none 也发) | `max_completion_tokens` | 不发 |
| `thinking: false` | 永不发(effort 被钳 none) | `max_tokens` | 不发(现状恒 0,死参数随分支移除) |

- 满足核心诉求:glm 条目(thinking: true)收到显式 `reasoning_effort: "none"` 才会真正关闭思考。
- 备选与否决:
  - **A**(none 走 max_tokens 分支 + 附加参数)——none 在两个 wire 家族里语义分裂,且 glm 关思考后 `max_completion_tokens` 语义无着落;
  - **B**(所有条目永远发)——`thinking: false` 条目指向真 OpenAI 非思考模型(如 gpt-4o)时会 400 拒掉 `reasoning_effort` 参数,B2 保住这类条目。
- 已知限制(接受):`thinking: true` 条目指向不认识 `reasoning_effort: "none"` 的严格网关时会报错——这是 operator 的目录声明责任,目录声明了能力就要承担相应 wire 形态。

### D4 内部管线:Thinking 从配置字段降级为 wire-family 标记

- `AgentConfig` 删 `Model/Thinking/ReasoningEffort` 三字段;三 agent 构造 `LLMRequest` 时从 turn 级解析结果取模型与 effort。
- `ModelOverride{Model, ReasoningEffort}` 保留但**永远非零**——handler 每回合解析出确定值,Build 时盖到三个 agent 的 clone 上;`IsZero` 快路径与 `apply` 的条件分支删除。"override"语义实际退化为"turn 配置注入",保留结构只为不动 `AgentFactory.Build` 签名与既有测试面。
- wire-family 标记(`entry.Thinking` 的拷贝)随解析结果进入 `LLMRequest`,取代原 `Thinking` 的分支职责;`LLMRequest.ReasoningEffort` 在 thinking 家族内永不为空(none 即字面值)。
- compaction `SummaryModel` 的启动回退从"Confucius 配置模型"改为 `DefaultModelName()`;turn 内仍优先用本 turn 解析出的模型。
- TitleService 只改读 `TitleModel`(回退逻辑在装配处解析),调用形态不动。

### D5 迁移报错:残留字段显式拒绝

- 被删字段(`openai.model`、顶层 `openai.max_context_tokens`、`agents.<name>.model|thinking|reasoning_effort`)残留于 config.yaml 时 load 失败并输出指向本迁移的错误信息(如 `agents.confucius.model was removed: the model now comes from the openai.models catalog`)。
- 实现沿用 yaml 原始 shadow 扫描先例(分数截断检查):反序列化后对原始 yaml map 检查这些键。不做"未知字段全局严格模式"(会波及其他合法的自由字段),只点名被删键。
- 备选与否决:静默忽略——`thinking: true` 被忽略后 effort 落到全局默认(未设即 none),思考**静默关闭**,是必须拦下的脚枪。

### D6 /models 与 openapi:增量字段

- `GET /api/v1/models` 响应新增 `default_reasoning_effort`(顶层,与 `default` 并列);条目数组形状不变(`name/max_context_tokens/thinking`)。
- `api/openapi.yaml`:`reasoning_effort` 参数描述改为"`none` 显式发送到 provider(thinking 条目)";/models schema 加字段。前端重新生成 `openapi.d.ts`。

## Risks / Trade-offs

- [Per-agent effort 差异化永久放弃(如"Confucius xhigh、子 agent low 省钱")] → 明确接受;若未来需要,以"effort 也进目录条目/请求参数粒度"另立变更,不回退本设计。
- [output_schema 部署被强制 default=none(否则 load 失败),Confucius 陪 Liang 一起失去思考] → 结构化输出 × reasoning 本就互斥,turn 级统一后无法 per-agent 绕开;真需要时把结构化约束退回 prompt-only(既有降级路径)。
- [thinking:true 条目 × 严格网关不认 `reasoning_effort:"none"`] → operator 目录声明责任(D3);llm_raw_log 可复核实际 wire 形态。
- [breaking 迁移一次到位,无灰度] → 单 operator 部署,迁移映射表 + 残留字段显式报错兜底;回滚 = 回退二进制 + 还原旧 config.yaml(无 schema 变更,`turn_usage.model`/run meta 数据形状不变)。
- [`seed` 也要求目录(D1)] → seed 与 serve 共用 config.yaml;极端场景(仅 seeding 的裸配置)补一条最小目录即可,文档注明。

## Migration Plan

1. 归档 `turn-detach-resume` → 归档 `per-request-model`(主 spec 就位)。
2. 实施 + 测试(本变更)。
3. operator 一次性改配置:

```
openai.model: X                 → openai.title_model: X(若 X 只为标题)
openai.max_context_tokens: N    → 删,写进每条目录条目
agents.*.model                  → 删
agents.<n>.thinking: true       → 删;如需非默认等级,设 openai.default_reasoning_effort
agents.<n>.reasoning_effort: E  → 删;E 提升为 openai.default_reasoning_effort: E(若全局一致)
(新增)                          → openai.models: [...](必填)+ default_model
```

4. 重启;残留字段会直接报错指路。回滚:还原旧 config.yaml + 旧二进制。

## Open Questions

(无 —— explore 会话已定稿 B2 wire 家族与 model-only → 全局默认两项关键决策;其余小项均按上述默认落地。)
