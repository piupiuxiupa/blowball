## 1. 核心实现

- [x] 1.1 `grepRun`(`internal/tool/xizhi/grep_engine.go`):stat 分支放宽——目录照旧;`info.Mode().IsRegular()` 进入单文件模式并在 `grepInput` 上携带单文件标记(`searchFile` = basename,`absPath` 不变);其余非目录条目返回 `"must be a file or directory"` 错误(替换旧文案)
- [x] 1.2 rg 引擎(`internal/tool/xizhi/grep_rg.go`):`cmd.Dir` 改为 `filepath.Dir(in.absPath)`(目录模式即原 absPath 自身、行为不变),搜索目标从硬编码 `"."` 改为目录模式 `"."` / 单文件模式文件名;确认 `--json` 的 `data.path.text` 在单文件模式下即 basename,`TrimPrefix("./")` 后无需额外映射
- [x] 1.3 Go 引擎(`internal/tool/xizhi/grep.go`):文件级 hidden 过滤对 `p == in.absPath` 的根文件自身豁免(单文件模式下显式目标不被 hidden 过滤,与 rg 对显式路径参数的行为对齐);确认 WalkDir 单文件路径只回调一次、扫描分支直接工作
- [x] 1.4 确认 glob 过滤、二进制跳过、context 行、分页/truncated 在单文件模式下的行为与目录模式一致(均为既有 post-filter/扫描逻辑,预期零改动,逐项核对)

## 2. 注册与描述

- [x] 2.1 `register.go` 中 `xizhi_grep` description:`path` 说明补充可为文件(仅搜索该文件)或目录

## 3. 测试

- [x] 3.1 单文件搜索用例:`path` 指向普通文件,返回匹配行、`file` 为 basename、结果形状与目录模式一致(Go 引擎)
- [x] 3.2 单文件模式边界用例:hidden 根文件仍被搜索(`include_hidden: false`)、glob 与 basename 不匹配返回空结果、二进制文件返回空结果不报错、`path` 指向不存在文件返回 not found
- [x] 3.3 rg 与 Go 双引擎一致性用例:同工作空间同参数(目录 + 单文件两模式)断言两引擎输出逐字段一致(测试需在无 rg 环境跑 Go 引擎、有 rg 环境跑 rg 引擎,或直接单测两引擎)
- [x] 3.4 更新引用旧错误文案 `is not a directory` 的既有断言
- [x] 3.5 `make test` + `make lint` 全绿

## 4. 文档

- [x] 4.1 CLAUDE.md 中 `xizhi_grep` 段落补一句 `path` 可为单文件(spill 落盘文件可直接 grep)
