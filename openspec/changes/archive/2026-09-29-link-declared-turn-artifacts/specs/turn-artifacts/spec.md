## MODIFIED Requirements

### Requirement: Turn 末产物检测
系统 SHALL 在每个 turn 结束时，仅从当前 turn 顶层 assistant 内容中的产物链接提取本轮产物候选。系统 SHALL NOT 遍历用户 workspace，也 SHALL NOT 使用文件 mtime 或 turn 开始时间判断产物归属。链接路径 SHALL 经 URL 解码并校验为工作区相对路径；绝对路径、路径穿越、query/fragment、`tmp/`、`.blowball/` 及隐藏路径 SHALL 被排除。同一路径重复出现 SHALL 只保留一个候选，候选顺序 SHALL 确定。只有存在、可读取、非空的常规文件 SHALL 记为产物；目录、symlink、缺失文件和空文件 SHALL NOT 记为产物。

#### Scenario: Linked file written by xizhi tool is detected
- **WHEN** 一个 turn 中 agent 通过 xizhi 写工具新建 `reports/a.docx`，且最终回复包含 `[a.docx](blowball://workspace/reports/a.docx)`
- **THEN** turn 结束时 `reports/a.docx` 被检测为本轮产物

#### Scenario: Linked file written via bash sandbox is detected
- **WHEN** 一个 turn 中 agent 通过 bash 在 `/workspace/out/` 生成文件，且最终回复以产物链接引用这些文件
- **THEN** turn 结束时这些被链接的常规非空文件被检测为本轮产物

#### Scenario: Unlinked generated file is not an artifact
- **WHEN** 一个 turn 生成了 `reports/a.docx` 但最终回复没有包含指向它的产物链接
- **THEN** `reports/a.docx` 不出现在本轮产物集合中

#### Scenario: tmp, reserved, hidden, and invalid links excluded
- **WHEN** 最终回复包含指向 `tmp/x.txt`、`.blowball/x`、隐藏路径、绝对路径或路径穿越的链接
- **THEN** 这些路径均不出现在本轮产物集合中

#### Scenario: Duplicate links deduplicated
- **WHEN** 最终回复中多个产物链接解析到同一工作区相对路径
- **THEN** 本轮产物集合中该路径只出现一次

#### Scenario: Missing or unusable target skipped
- **WHEN** 产物链接指向缺失文件、目录、symlink 或空文件
- **THEN** 该链接不产生产物事件，且 turn 正常完成

#### Scenario: Concurrent sessions do not share artifacts
- **WHEN** 同一用户的 Session A 和 Session B 并发运行，A 生成并链接 `reports/a.docx`，B 生成并链接 `reports/b.docx`
- **THEN** A 的产物集合只包含 `reports/a.docx`，B 的产物集合只包含 `reports/b.docx`

#### Scenario: Existing file linked as deliverable
- **WHEN** 当前 turn 最终回复链接一个已存在且内容未变化的常规非空文件
- **THEN** 该文件按产物规则处理；若已有逐字节相同的最新版本，则复用该 `version_id`
