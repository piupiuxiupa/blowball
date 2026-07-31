## ADDED Requirements

### Requirement: Executor tool descriptions declare result shape, limits, and file-tool anti-pattern
`bash`、`python`、`pip_install` 的工具描述 SHALL 声明其结果结构为 `{output, exit_code, truncated}`（`output` 为合并的 stdout+stderr），并告知输出有上限（默认 64KB，超限截断并以 `...output truncated...` 标记、置 `truncated: true`）与超时（`bash`/`python` 默认 30s、`pip_install` 默认 120s）。`bash` 与 `python` 的描述 SHALL 包含一条反模式：工作区文件的读取/写入/搜索**不要**用 `cat`、`echo`/重定向、`find`、`grep`，而用 `xizhi_*` 专用工具（除非专用工具无法完成该任务），并以强指令词 `DO NOT` 标记。`python` 的描述 SHALL 声明 `code` 与 `file` 互斥（恰提供一个）。每个执行类工具描述 SHALL 对其致命约束（如 64KB 截断、`code`/`file` 互斥、勿跑代码）用加粗 + 大写强指令词（`MUST`/`DO NOT`/`IMPORTANT`/`REQUIRED`/`ONLY` 等）标记不少于 2 处（R4）。

#### Scenario: 执行类工具描述声明结果结构与上限
- **WHEN** `bash`、`python`、`pip_install` 工具被注册并渲染给模型
- **THEN** 各描述包含 `output`、`exit_code`、`truncated` 三个字段名，以及输出上限（64KB）与截断标记的说明

#### Scenario: bash/python 描述包含文件工具让位反模式
- **WHEN** `bash` 与 `python` 工具被注册并渲染给模型
- **THEN** 描述中以强指令词 `DO NOT` 标记让位反模式：文件读写/搜索用 `xizhi_*`，不要用 `cat`/`echo`/重定向/`find`/`grep`

#### Scenario: python 描述声明 code/file 互斥
- **WHEN** `python` 工具被注册并渲染给模型
- **THEN** 描述声明 `code` 与 `file` 恰提供一个（二者互斥）

#### Scenario: pip_install 描述声明用途边界
- **WHEN** `pip_install` 工具被注册并渲染给模型
- **THEN** 描述声明其用于安装依赖（解决 `ModuleNotFoundError`/`ImportError`），而非运行代码
