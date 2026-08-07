## Why

当前 `bash` 沙箱的环境变量只有一条路径:`allowed_env_patterns` 按 glob 放行**宿主机进程里已经存在**的变量(`internal/tool/executor/env.go:filterEnv`)。它无法:

1. **设定字面值** —— 例如注入 `PIP_INDEX_URL`、`npm_config_registry`、`HTTP_PROXY` 等公共镜像源/代理配置,operator 今天必须先在宿主环境 `export` 这些变量、再 allowlist 对应名字。值不在 `config.yaml` 里,可观测性差,部署到新机器易漏配。
2. **覆盖宿主值** —— 一旦 allowlist `PATH`,沙箱拿到的就是宿主 PATH,operator 无法从 config 层改写。

operator 的典型诉求是把**公共配置**(pip/npm 镜像源、公司代理、`NODE_USE_ENV_PROXY=1` 等)统一收口进 config,作为单一事实源,所有用户的 `bash` 沙箱共享。本次新增 `tools.executor.bash.env`(key:value map)补上这个缺口。

## What Changes

- **新增 `tools.executor.bash.env`(map)**:operator 以 `KEY: value` 形式声明环境变量字面值,注入到每个 `bash` 沙箱。默认空 → 零行为变化。
- **三层 env 叠加优先级**:`host allowlist(filterEnv,底)` < `env 字面值(中)` < `强制不变量(顶:HOME、PATH 前置 $HOME/.local/bin、PYTHONPATH 前置 /workspace/.pip)`。`env` 覆盖宿主透传值;强制层始终最后应用、始终胜出,故现有 load-bearing 不变量不受影响。
- **`${VAR}` / `${VAR:default}` 天然可用**:config loader 在 `yaml.Unmarshal` 前对全文做 `expandEnv`(`internal/config/config.go:804`),故 `env` 的 value 自动支持从宿主环境取值 —— 秘钥可写 `OPENAI_API_KEY: "${OPENAI_API_KEY}"` 而不硬编码进 config。
- **`HOME` 保留键**:在 `env` 中声明 `HOME` → config load 直接报错(fail-fast),而非像今天 `bwrap.go` 那样静默覆盖。key 名校验 `^[A-Za-z_][A-Za-z0-9_]*$`,非法即报错。
- **与 `allowed_env_patterns` 并存**:patterns 保留 glob(`PYTHON*`)与「值随宿主走」的动态透传;`env` 设精确字面值。两者互补,不替换。

## Capabilities

### Modified Capabilities
- `executor-tools`:`Environment variable filtering` 需求由「仅过滤」扩展为「三层 env 构造(host 过滤 + 字面值注入 + 强制不变量)」并固定优先级;新增 `Operator-defined environment literals` 需求,描述 `env` map 的形状、`${VAR}` 展开、`HOME` 保留键与 key 名校验。

## Impact

- **代码**:`internal/config/config.go` 的 `ExecutorToolConfig` 增加 `Env map[string]string \`yaml:"env"\`` 字段,`validate()`(或 bash executor 配置的校验路径)增加 `HOME` 保留键与 key 名正则校验;`internal/tool/executor/bwrap.go` 的 `buildBwrapArgs` 在 `filterEnv` 之后、强制层之前插入 `for k,v := range cfg.Env { env[k] = v }` 一层 merge(可抽到 `env.go` 的 `mergeEnv`)。
- **配置 / 部署**:`config.example.yaml` 的 `tools.executor.bash` 段新增 `env:` 注释块 + 示例(以 pip/npm 镜像源 + 代理为示范,标注 value 支持 `${VAR}` 展开、`HOME` 不可用);把示例里原在 `allowed_env_patterns` 的 `PIP_INDEX_URL`/`npm_config_registry` 迁到 `env` 示范,并把 `allowed_env_patterns` 注释收紧为「值随宿主走的变量 + glob」。
- **文档**:`CLAUDE.md` 的「Executor tools (Linux only)」与「Important conventions / Sandbox directory configuration」相关段落补充 `env` 字段、三层优先级、`${VAR}` 与秘钥引导、`HOME` 保留键说明。
- **测试**:`internal/tool/executor/bwrap_test.go` 增三层优先级用例(`env` 覆盖 host allowlist;PATH/PYTHONPATH 前置胜出;无 `env` 块零回归);`internal/config/config_test.go` 增 `env` 校验用例(`HOME` 拒绝、非法 key 名拒绝、空 map 放行)与 `${VAR}` 展开用例;`env_test.go` 增 merge 行为。
- **依赖**:无新增第三方库(`map[string]string` 反序列化 + 标准库 `regexp` 校验)。
