# Tasks: add-length-continuation

## 1. 配置面

- [x] 1.1 `internal/config/config.go`：`OpenAIConfig` 新增 `LengthContinue` 块（`ExpandStep int` yaml `expand_step`、`MaxRetries int` yaml `max_retries`），暴露解析后的策略（全零 = 禁用；任一非零启用，缺省兄弟取 8192 / 3）；load 校验负值报错（按 design D9）
- [x] 1.2 `config.example.yaml`：`openai:` 块补 `length_continue` 注释示例（`expand_step`/`max_retries`、缺省禁用、负值拒绝、预算算式 `max_tokens + N×expand_step`，推荐 `expand_step: 8192`、`max_retries: 3`）
- [x] 1.3 `internal/config` 测试：未配置/全零 → 禁用；仅 `expand_step` → 启用且 retries=3；负值 → 加载失败

## 2. 续写 helper

- [x] 2.1 新建 `internal/agent/lengthcontinue.go`：`runLLMRound` 按 design D3 签名实现——attempt 循环内调 `StreamChat`（onToken/onReasoning 透传调用方闭包）、`resp.FinishReason != "length"` 或续写次数用尽即返回 `roundResult{Resp, Content(累计), Usage(累计), LengthHit}`
- [x] 2.2 扩容算式（design D6）：第 N 次尝试 `req.MaxTokens = agentCfg.MaxTokens + N×ExpandStep`，只改 `req`，不回写 agent 配置或状态
- [x] 2.3 纯 content 截断脚手架（design D4）：`*round` 追加 assistant（本次尝试 content/reasoning）+ user 续写指令（英文常量，含"从中断处继续 / 不得重复 / 大文件分块多次写"）；指令留存 round 至 turn 结束
- [x] 2.4 tool_calls 截断分诊（design D5）：assistant（含半截 calls）入 round；逐 call `json.Valid(args)` —— 合法者经 `dispatch` 回调照常执行（真实结果入 round）；非法者追加合成 tool result（未执行·参数被长度限制截断·请分块重新发起）并发射 `tool_call`（args 净化 `{}`）+ `tool_result` 事件对
- [x] 2.5 耗尽路径（design D7）：`LengthHit=true` 时由调用方发射 `agent_error`（`length_exhausted`）+ `agent_end` 并以错误结束 turn；helper 返回累计 Content 作为 finalContent
- [x] 2.6 禁用短路：策略全零时 helper 退化为单次 `StreamChat` 直返，行为与改动前逐字节一致

## 3. 四个调用点接入

- [x] 3.1 `internal/agent/confucius.go` 主循环：`StreamChat` 调用换 `runLLMRound`（dispatch 回调接现有 `dispatchToolCalls` 子集）；终答分支 `finalContent` 取 `result.Content`；耗尽路径接 2.5；`total`/`byAgent` 累计 result.Usage
- [x] 3.2 `internal/agent/chongzhi.go` 与 `internal/agent/liang.go` 主循环：同 3.1 接入（leaf 无子代理，dispatch 回调为其 registry dispatch）
- [x] 3.3 `internal/agent/roundcap.go` `runWrapUpRound`：接入 helper（无 dispatch 回调）；最终尝试仍 tool_calls/空内容时维持既有 `round_cap_exhausted` 路径（design Risks 条目）
- [x] 3.4 `observeRoundContext` 语义（design D8）：各循环只以最终一次尝试的用量喂 `tmeta.observeRoundContext`（helper 需单独暴露最终尝试 usage 或在 roundResult 中区分）

## 4. xizhi append 模式

- [x] 4.1 `internal/tool/xizhi/write.go`：`WriteFile` 增加 mode 分支——`append` 为文件不存在则创建（含父目录）、存在则末尾追加；结果结构增加 `Appended`（`write` 模式=全文长度，`append`=追加长度），`Size` 保持写后总大小
- [x] 4.2 `internal/tool/xizhi/register.go`：`schemaWrite` 增加 `mode` 枚举（缺省 `"write"`）；`writeArgs` 解析 mode，非法值报参数错误；静态描述更新（结果结构含 `appended`、`mode: "append"` 语义、大内容分块多次写的模式性建议——预算数字留给任务 5 注入，静态文本不写死数字）
- [x] 4.3 `internal/tool/xizhi` 测试：append 到既有文件（原内容保留、size/appended 正确）、append 创建缺失文件、缺省 mode 行为与改动前一致、非法 mode 报错、workspace 外路径仍拒绝

## 5. 写入预算引导注入

- [x] 5.1 `internal/agent/tools.go`：tools[] 渲染层（`buildRegularToolsJSON`/`buildConfuciusToolsJSON` 路径）为 `xizhi_write_file`/`xizhi_modify_file` 的描述追加预算句——数字 `floor(agentCfg.MaxTokens×7/10)`，按 agent 现算（design D10；注册表共享，注入仅在 agent 构造层）
- [x] 5.2 注入测试：同仓库表两个 agent 配不同 `max_tokens` → 各自渲染描述中的数字分别为各自 70% 取整；工具列表不含写入工具的 agent（Liang）描述不含引导句；静态描述中的模式性建议在注入前已存在

## 6. 集成与不变量测试

- [x] 6.1 `internal/agent` helper 单测（fake LLMClient 脚本化 finish_reason 序列）：length→stop（一次续写、内容累计、MaxTokens 递增、round 脚手架形状）；连续 length×3 后 stop（耗尽前夜）；4 连 length → `length_exhausted` + agent_end + done.error + finalContent=累计；全空响应（thinking 耗尽）走续写；禁用时单次直返
- [x] 6.2 分诊路径单测：`[合法 call, 截断 call]` 混合响应 → 合法者 dispatch 且真实结果入 round、截断者合成结果 + 净化事件对、续写请求前每个 call 有 tool 应答；空串 args 归入截断路径；续写后模型重发完整 call 走正常 dispatch
- [x] 6.3 `max_rounds` 不受续写消耗：`max_rounds: 2` + 一轮内 2 次续写 → 仍计 1 轮，收尾回合行为不变
- [x] 6.4 用量记账测试：多次尝试用量全部入 total/by_agent；`context_tokens` 只等于最终尝试的 prompt+completion（design D8）
- [x] 6.5 `internal/handler` MergeEvents 不变量测试：同一 (agent, run_id) 的两次尝试 token 事件（中间无事件）合并为单行、内容按序拼接；`llm-length-continuation` 规格的"SSE 与持久化零改动"场景对应
- [x] 6.6 B-call-1 回传形状守护测试：断言发往网关的 assistant 消息携带半截 args 字符串 + 每个 tool_call 有 tool 应答（为 glm 网关实测前留契约锚点，见 design Risks）
- [x] 6.7 既有测试回归：`make test` 全绿（重点 `confucius_test.go`/`roundcap_test.go`/`subagent_run_test.go` 因循环改动受影响的面）

## 7. 文档

- [x] 7.1 `CLAUDE.md`：Agent orchestration 段补 length 续写能力概述（触发、扩容算式、B-call-1、耗尽、`openai.length_continue` 配置、写入预算引导与 append 模式）；Important conventions 的 config 清单补 `openai.length_continue`
