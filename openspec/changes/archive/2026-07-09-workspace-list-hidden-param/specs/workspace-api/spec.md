## MODIFIED Requirements

### Requirement: List workspace files
系统 SHALL 提供接口列出用户工作空间中的文件和目录，并支持通过 `include_hidden` 参数控制是否返回隐藏文件和目录。

#### Scenario: List files in workspace root (hidden excluded by default)
- **WHEN** 用户发送 GET /api/v1/workspace/files
- **THEN** 系统返回 data/{user_uuid}/workspace/ 下的非隐藏文件和目录列表，隐藏条目（名称以 "." 开头）默认不出现

#### Scenario: List files including hidden entries
- **WHEN** 用户发送 GET /api/v1/workspace/files?include_hidden=true
- **THEN** 系统返回 data/{user_uuid}/workspace/ 下的全部文件和目录列表，包括隐藏条目

#### Scenario: List files in subdirectory (hidden excluded by default)
- **WHEN** 用户发送 GET /api/v1/workspace/files?path=src
- **THEN** 系统返回 data/{user_uuid}/workspace/src/ 下的非隐藏文件和目录列表

#### Scenario: List files in subdirectory including hidden entries
- **WHEN** 用户发送 GET /api/v1/workspace/files?path=src&include_hidden=true
- **THEN** 系统返回 data/{user_uuid}/workspace/src/ 下的全部文件和目录列表，包括隐藏条目

#### Scenario: List empty workspace
- **WHEN** 用户工作空间为空
- **THEN** 系统返回 HTTP 200，body 为空数组 []
