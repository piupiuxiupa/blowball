## 1. Config 结构与校验

- [x] 1.1 `internal/config/config.go`:`ExecutorToolConfig` 增加 `Env map[string]string \`yaml:"env"\`` 字段(紧随 `AllowedEnvPatterns`)。
- [x] 1.2 `internal/config/config.go`:在 bash executor 配置的校验路径(`validate()` 或对应分支)增加遍历 `Bash.Env` 的校验:(a) key 名匹配 `^[A-Za-z_][A-Za-z0-9_]*$`,否则报错;(b) key == `HOME` → 报错「HOME is reserved (forced to synthetic sandbox home)」。fail-fast,对齐 `ParseMounts` 风格。
- [x] 1.3 `internal/config/config_test.go`:新增用例 —— `env` 合法放行;`HOME` 拒绝;非法 key 名(`"bad key"`、`""`、`"1ABC"`)拒绝;空 map 放行。

## 2. bwrap 三层 env 叠加

- [x] 2.1 `internal/tool/executor/bwrap.go`:`buildBwrapArgs` 在 `env := filterEnv(cfg.AllowedEnvPatterns)` 之后、PYTHONPATH/HOME/PATH 强制层之前,插入 `for k, v := range cfg.Env { env[k] = v }`(可抽到 `env.go` 的 `mergeEnv`)。PYTHONPATH/HOME/PATH 强制层保持不动,确保不变量不被破坏。
- [x] 2.2 `internal/tool/executor/bwrap_test.go`:新增用例(用排序后的 `--setenv` 集合或子集断言,避免 map 迭代顺序干扰)—— (a) `env.FOO=cfg` 且 host `FOO=host` 且 `FOO` 在 patterns → 沙箱 `FOO=cfg`;(b) `env.PATH=/opt/bin` → `--setenv PATH` 值以 `$HOME/.local/bin` 前置;(c) `env.PYTHONPATH=/x` → 值以 `/workspace/.pip` 前置;(d) 无 `env` 块时 `--setenv` 集合与现状一致(零回归);(e) 空 value(`env.FOO=""`)正常 setenv。
- [x] 2.3 `internal/tool/executor/env_test.go`:若抽 `mergeEnv`,补单测(merge 覆盖语义、不污染入参);否则在 bwrap_test 覆盖。

## 3. `${VAR}` 展开用例

- [x] 3.1 `internal/config/config_test.go`:断言 `env: { KEY: "${HOST_VAR}" }` 经 loader 展开后 `cfg.Bash.Env["KEY"]` == 宿主 `HOST_VAR`;`"${VAR:default}"` 缺省路径生效。(复用既有 `expandEnv`,仅需断言 `Env` 字段值也被展开。)

## 4. 配置示例与文档

- [x] 4.1 `config.example.yaml`:`tools.executor.bash` 段新增 `env:` 注释块 + 示例(以 `PIP_INDEX_URL`/`PIP_TRUSTED_HOST`/`npm_config_registry`/`HTTP_PROXY`+`http_proxy`/`HTTPS_PROXY`+`https_proxy`/`NO_PROXY`+`no_proxy`/`NODE_USE_ENV_PROXY: "1"` 为示范),注明 value 支持 `${VAR}` 展开、`HOME` 不可用、三层优先级一句话;把 `allowed_env_patterns` 注释收紧为「值随宿主走的变量 + glob」,并从其示例中移除 `PIP_INDEX_URL`/`npm_config_registry`(迁到 `env` 示范)。
- [x] 4.2 `CLAUDE.md`:「Executor tools (Linux only)」与「Important conventions / Sandbox directory configuration」相关段落补充 `tools.executor.bash.env` 字段、三层 env 优先级、`${VAR}` 与秘钥引导、`HOME` 保留键说明。

## 5. 验证

- [x] 5.1 `make build` 通过。
- [x] 5.2 `make test` 通过(含 `./internal/config/...`、`./internal/tool/executor/...`、`./test/integration/...`)。
- [x] 5.3 `make lint` 通过。
- [x] 5.4 `openspec validate executor-env-literals --strict` 通过。
