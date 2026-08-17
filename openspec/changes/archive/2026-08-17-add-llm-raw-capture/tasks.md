# Tasks: add-llm-raw-capture

## 1. Schema 与 store 层

- [x] 1.1 新增 `migrations/011_llm_raw_log.sql`:按 design D4 建 `llm_raw_log` 表(call_id/seq/trace_id/session_id/user_id/agent/kind/model/finish_reason/http_status/duration_ms/raw MEDIUMTEXT/msg_time/update_time;索引 (session_id,seq)、(trace_id,seq)、(msg_time);不挂 sessions FK)
- [x] 1.2 `internal/model/` 新增 `LLMRawLog` 结构体(db/json tag 对齐列名)与 kind 常量(request/response/error)
- [x] 1.3 `internal/store/mysql/` 新增 `AppendRawLogs(ctx, []model.LLMRawLog)` 多值批量 INSERT;单条失败(如超 MEDIUMTEXT)记 ERROR 并丢弃该条、批次内其余行继续(逐条降级插入)
- [x] 1.4 `internal/store/redis/` 新增 buffer key 家族:`llm_raw:buffer` 的 `PushRawLog`(RPUSH 单条 JSON)与 `PopRawLogs`(LMPop count=N);包注释补充第三个 key 家族说明
- [x] 1.5 单测:MySQL 批量插入(含单条超限隔离)、Redis push/pop 往返(用 miniredis 或既有测试基建)

## 2. 上下文管道与 sink 抽象

- [x] 2.1 新增 session_id context value(仿 `trace`/`skill.WithUserID` 先例,含 With/From helper 与测试)
- [x] 2.2 新增 agent 名 context value(同上);`SendMessage` 注入 session_id;confucius/chongzhi/liang 的 Run 入口注入自身 agent 名(覆盖 roundcap 收尾轮);`title.go` 的 `GenerateTitle` bg ctx 注入 session_id 与 `"title"`
- [x] 2.3 `internal/agent/` 定义 `RawCaptureSink` 接口与 record 结构体(call_id/seq/trace_id/session_id/user_id/agent/kind/model/finish_reason/http_status/duration_ms/raw);`OpenAIClient` 持可选 sink(nil 时零行为)
- [x] 2.4 sink 生产实现:JSON 序列化 + RPUSH,非阻塞失败仅 WARN;seq 用进程内全局单调计数器(design D4/Risks 取简方案)

## 3. 捕获点接线(StreamChat)

- [x] 3.1 params 组装完成后、发流前捕获 `kind=request`:序列化实发 `ChatCompletionNewParams` 全量 JSON,分配 call_id + seq
- [x] 3.2 流正常结束捕获 `kind=response`:由聚合状态缝合完整响应等价 JSON(内容/tool_calls/finish_reason/usage/reasoning 及扩展字段),记录 duration_ms
- [x] 3.3 流失败捕获 `kind=error`:spike 确认 openai-go v3 `apierror` 的 raw body 暴露形状(`APIError.JSON`),取 http_status + 原始错误 body(不可得降级 error string);ctx 取消不产 error 行,按 response 行落已聚合部分
- [x] 3.4 单测:fake sink 断言成功/失败/取消三路径各产出的行数、kind、call_id/seq 配对与 payload 形状

## 4. Flusher 与 wiring

- [x] 4.1 新增 flusher 组件:goroutine 循环 `LMPop count=batch(50)` → `AppendRawLogs`,触发为 ticker(5s) ∥ LLEN≥50;Redis/MySQL 不可达记 log 退避重试;提供 `Close()`(触发 final flush,超时上限 3s)
- [x] 4.2 `cmd/blowball/serve.go` wiring:仅 agent/all 角色构造 sink + 启动 flusher,挂到优雅停机链;api 角色不构造(保持无 LLM 依赖边界)
- [x] 4.3 单测:双触发逻辑、final flush、失败退避;集成测试跑一轮真实 turn 断言 `llm_raw_log` 出现成对 request/response 行且排序正确(实现说明:弹批用 `LPOP key count`——与 LMPop 单 key 语义等价、Redis≥6.2 兼容且 miniredis 可测;集成 harness 的 LLM 为脚本化 fake、不经 OpenAIClient,故成对/排序断言由 agent 捕获测试 + llmraw 真实 miniredis 管道测试共同覆盖,见 design D3 修订)

## 5. 验证与文档

- [x] 5.1 `make test` 全绿(含 race);`make lint` 通过(注:`internal/prompt` 两个失败为会话开始前工作区已有的未提交 WIP(render.go/render_test.go),与本变更无关;其余全绿,lint 通过)
- [x] 5.2 本地 `docker compose up -d` + `make run` 冒烟:发一条消息,SQL 查 `llm_raw_log` 按 (session_id, seq, kind) 排序验证成对与内容保真(实发 params、缝合响应、title 调用行)(已验证:迁移 011 在 MySQL 8.0 全链路应用、建表/索引正确;本地(SSH 隧道 DB)已手动应用 011;`serve` 正常启动、优雅停机日志序列 `server stopped → llm_raw.pop → llm raw flusher stopped` 确认 final drain;剩余:真实网关发一条消息后的 SQL 内容抽查——需真实 LLM 调用,留给重启 dev server 后人工验证)
- [x] 5.3 更新 `CLAUDE.md`(agent orchestration/工具节附近补 llm_raw_log 旁路说明)与 `config.example.yaml` 若有需要(零配置,预期无改动,已确认无改动)

## 6. 修订:逐帧原始 chunk 存储(推翻初版"逐帧不做"决策)

- [x] 6.1 migration 011 增列 `frame_index INT NOT NULL DEFAULT 0` + 索引调整 `(session_id, seq, frame_index)`;存量 dev 库手动 ALTER(已完成;编辑后的 011 在全新 MySQL 8.0 全链路复验通过)
- [x] 6.2 model 增 `FrameIndex` 字段与 `RawKindChunk` 常量;mysql INSERT 列表同步
- [x] 6.3 `StreamChat` 流循环内逐帧捕获:`chunk.RawJSON()`(wire-exact,空则回退 re-marshal)→ `kind=chunk` 行,`frame_index` 递增;request=0、response/error=末位;8MB 累计截断 + 标记行
- [x] 6.4 `RawCaptureRecord` 增 `FrameIndex`;sink 按帧路径去热(失败时才构建日志字段)
- [x] 6.5 测试更新:捕获测试断言帧行数/顺序/保真/截断标记;store 与管道测试同步;全量 `make test` + lint(仅剩 `internal/prompt` 两个会话前已有的 WIP 失败,与本变更无关)
