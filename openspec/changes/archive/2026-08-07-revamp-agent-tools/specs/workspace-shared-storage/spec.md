## MODIFIED Requirements

### Requirement: executor 沙箱在共享文件系统上的访问前置条件
当 `storage.workspace.backend` 为 `shared` 且 `bash` executor 工具启用时，共享文件系统 SHALL 以允许非挂载者 UID 访问的方式挂载（JuiceFS `--allow-other`，并要求 `/etc/fuse.conf` 开启 `user_allow_other`），使 bwrap 用户命名空间内映射 UID 能读写绑定进 `/workspace` 的工作空间。系统 SHALL 在 `bash` executor 启用且 `backend` 为 `shared` 时，启动期执行一次轻量 `bwrap` 自检（绑定工作空间子目录并完成一次写/删探针）以提前暴露该前置条件缺失。（`python`/`pip_install` 专用执行器已移除，executor 现仅含 `bash`。）

#### Scenario: 沙箱可访问共享工作空间
- **WHEN** `backend` 为 `shared`、`bash` executor 启用，且 JuiceFS 以 `--allow-other` 挂载、`user_allow_other` 已开启
- **THEN** agent 调用 `bash` 时，bwrap 内对 `/workspace` 的读写正常，不出现 EACCES

#### Scenario: allow_other 缺失被启动自检捕获
- **WHEN** `backend` 为 `shared`、`bash` executor 启用，但挂载未带 `--allow-other` 或未开 `user_allow_other`
- **THEN** 启动期 bwrap 自检失败（探针写入 EACCES），系统 fatal 退出并提示检查 FUSE 访问选项，而非等到运行期 agent 调用才报错

### Requirement: executor tmp 与 pip 产物跨节点共享
当 `storage.workspace.backend` 为 `shared` 时，`bash` executor 创建于工作空间内的 `tmp/` 与 `.pip/` 子目录 SHALL 随工作空间落于共享文件系统，从而跨节点共享：一个节点 agent 经 `bash` 运行 `python3 -m pip install --target /workspace/.pip` 安装的包，其他节点 agent 经 `bash` 运行 `python3` SHALL 能在无需重新安装的情况下导入（`PYTHONPATH=/workspace/.pip` 指向共享路径）。(`pip_install`/`python` 专用工具已移除，装包与导入统一经 `bash`；`PYTHONPATH` 注入不变量保持不变。)

#### Scenario: 跨节点复用已安装的 Python 包
- **WHEN** 节点 A 的 agent 调用 `bash` 执行 `python3 -m pip install --target /workspace/.pip numpy` 到共享 `/workspace/.pip`
- **AND** 节点 B 的 agent 随后调用 `bash` 执行 `python3 -c "import numpy"`
- **THEN** 节点 B 的导入成功（包来自共享 `.pip`），无需在节点 B 再次安装

#### Scenario: 跨节点临时文件可见
- **WHEN** 节点 A 的 agent 经 `bash` 写入 `/tmp/hello.txt`（映射到共享 `workspace/tmp/hello.txt`）
- **THEN** 节点 B 的 `xizhi_read_file`（路径 `tmp/hello.txt`）可读到该内容
