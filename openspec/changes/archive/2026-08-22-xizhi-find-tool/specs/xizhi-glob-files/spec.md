## REMOVED Requirements

### Requirement: Xizhi glob files tool
**Reason**: `xizhi_glob_files` 被 `xizhi_find` 整体替换:glob-only 语法弱于 regex、结果无类型信息(分不清文件/目录)、无分页、无类型过滤。`xizhi_find` 以 fd 语义(regex 匹配条目名、`type` 过滤、`max_depth`、分页)覆盖并超出原能力;两工具语义重叠,保留会造成模型选择负担。
**Migration**: config.yaml 三处改名——`tools.xizhi.glob_files` → `tools.xizhi.find`(旧 key 加载期拒绝并指向迁移);`agents.<name>.tools` 列表中的 `xizhi_glob_files` → `xizhi_find`(遗留旧名在启动时经工具注册表 unknown-tools 检查 fail-fast);`tools.timeouts` 中的 key 同步更名。调用习惯迁移:glob pattern `*.go` → regex `\.go$`;`src/**/*.go` → `path: "src", pattern: "\.go$"`。
