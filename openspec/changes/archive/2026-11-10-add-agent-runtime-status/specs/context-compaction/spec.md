## MODIFIED Requirements

### Requirement: The checkpoint records earlier tool activity
When tool invocations leave the retained tail, the system SHALL consolidate them into a bounded tool-activity section of the checkpoint. The ledger SHALL include tool names, invocation counts, success and failure counts, important parameters, and concise outcomes. It SHALL preserve exact identifiers, paths, commands, and error strings needed for continuation.

对于宿主工具名为 `mcp_call` 的调用，台账 SHALL 额外从该调用的原始 JSON arguments 解析 `server` 与 `tool`，在保留父级 `mcp_call` 总调用、成功和失败计数的同时，按 MCP server 分组记录每个 remote tool 的调用、成功和失败次数。合并键 SHALL 为 `(server, tool)`；不同 server 下的同名 tool SHALL 分开统计。其他 `mcp_*` 管理工具、普通工具及直接以独立工具名注册的 operator MCP 代理工具 SHALL 保持原有按工具名统计的方式。

父级 `mcp_call` 计数与子项明细 SHALL 表示同一批调用，顶层总调用数 SHALL 只累加父级条目，不得因子项而重复计数。参数缺失、JSON 无效或旧 checkpoint 只有父级总数时，无法归因的调用 SHALL 保留在未归因余量中；系统 SHALL NOT 猜测或伪造 server/tool 明细。再次压缩时，父级与 `(server, tool)` 子项 SHALL 分别按各自键确定性累加。

The checkpoint SHALL remain a single consolidated summary. Repeated compaction SHALL merge prior checkpoint material and newer activity rather than stacking duplicate section inventories. The tool-activity ledger itself SHALL be condensed as it grows so the checkpoint cannot expand without bound, but this reduction SHALL NOT drop any `(server, tool)` invocation counts or use a Top-N truncation that loses the requested distribution. The planner SHALL reduce retained tail material and optional examples before considering an irreducible candidate uncompactable. MCP detail entries SHALL NOT duplicate the parent entry's optional examples.

#### Scenario: Calls are counted by tool
- **WHEN** multiple calls to the same tool leave the retained tail
- **THEN** the checkpoint reports their aggregate count and success/failure split

#### Scenario: Important arguments survive
- **WHEN** an earlier tool call used a consequential path, command, or query
- **THEN** that identifier or parameter appears verbatim in the tool-activity summary

#### Scenario: Re-compaction merges activity
- **WHEN** a compacted session is compacted again after further tool use
- **THEN** one consolidated checkpoint contains the merged tool activity without duplicate section inventories

#### Scenario: mcp_call is broken down by server and remote tool
- **WHEN** the shadowed span contains 2 calls to `gildata/search`, 1 call to `gildata/quote`, and 1 call to `wiki/search`
- **THEN** the parent `mcp_call` entry reports 4 calls
- **AND THEN** the nested ledger reports `gildata/search=2`, `gildata/quote=1`, and `wiki/search=1`, each with the corresponding success/failure split

#### Scenario: Same remote tool name on different servers stays distinct
- **WHEN** two servers both expose a tool named `search` and each is invoked through `mcp_call`
- **THEN** the two `(server, tool)` entries are merged independently rather than collapsed by remote tool name alone

#### Scenario: Re-compaction merges nested MCP activity without double counting
- **WHEN** a prior checkpoint contains `mcp_call=3` with `gildata/search=2`, and the new shadowed span adds 1 `gildata/search` and 1 `wiki/search`
- **THEN** the merged parent reports 5 calls and the nested entries report `gildata/search=3` and `wiki/search=1`
- **AND THEN** the top-level total tool-call count increases only by the 2 new `mcp_call` invocations, not by the sum of parent and child entries

#### Scenario: Legacy or malformed calls remain unattributed
- **WHEN** a prior ledger contains only aggregate `mcp_call` counts, or a current `mcp_call` lacks parseable `server`/`tool` arguments
- **THEN** those calls remain included in the parent and top-level totals
- **AND THEN** the renderer or structured ledger preserves an unattributed residual instead of assigning them to an invented server or tool

#### Scenario: Budget reduction preserves every MCP count
- **WHEN** a compaction candidate exceeds its target request budget and contains many distinct `(server, tool)` entries
- **THEN** the planner reduces retained tail material and optional examples before publication
- **AND THEN** no server/tool count is silently dropped by a Top-N cap; an irreducible candidate follows the existing uncompactable-context path

#### Scenario: Non-mcp_call tools keep their existing identity
- **WHEN** `mcp_list_tools`, an ordinary built-in tool, or an operator MCP proxy tool appears in the shadowed span
- **THEN** it remains a normal top-level tool-activity entry and is not parsed as an `mcp_call` nested remote invocation
