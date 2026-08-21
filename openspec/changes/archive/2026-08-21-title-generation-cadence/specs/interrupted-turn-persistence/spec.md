# Delta: interrupted-turn-persistence

## REMOVED Requirements

### Requirement: Title generation runs on interrupted first turn
**Reason**: 标题生成已改为用户发送消息时触发(title-generation-cadence,session-run claim 成功后、orchestrator 启动前),标题在 turn 开始前已生成或已在 flight——中断(取消/失败)发生时标题路径已经走完,不存在"中断的首轮 turn 需要用部分 assistant 内容补触发"的场景;turn 结束后的持久化路径不再触发标题生成。
**Migration**: 无需迁移——发送时触发对中断与正常 turn 行为一致;旧路径(首轮结束后用部分 assistant 内容触发)随 `persistEvents` 的 `isFirstTurn` 分支一并删除。
