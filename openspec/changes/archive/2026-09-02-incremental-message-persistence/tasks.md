# Tasks: incremental-message-persistence

## 1. 核心

- [x] 1.1 `message_stream.go`：tap 无条件创建（压缩 hook 仍按需装配）；`turnFlushState` 增加 `persistMu` 与持锁辅助
- [x] 1.2 新增增量持久化 goroutine：周期快照 → 截断 `merged[:closed]` 复用 `buildTurnMessages` → SaveMessagesBatch → 成功推进游标；失败 WARN 跳过
- [x] 1.3 round hook 的 flush 段与终局批接入同一互斥/停止顺序（终局前 stop-and-drain 增量 goroutine）

## 2. 测试

- [x] 2.1 单测：闭合语义（token 段不提前写、边界事件闭合即写）、序数/幂等键与终局一致、区间不重叠、增量失败游标不推进
- [x] 2.2 handler 集成：慢 turn 期间多批增量落库、终局批为纯后缀、全 turn 行恰好一次
- [x] 2.3 `go test -race ./...` + `go vet` 全绿
