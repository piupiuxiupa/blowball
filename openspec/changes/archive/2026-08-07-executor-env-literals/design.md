## Context

`bash` 沙箱的环境变量今天由 `internal/tool/executor/bwrap.go:buildBwrapArgs` 构造,流程是:`--clearenv` → `filterEnv(cfg.AllowedEnvPatterns)`(从 `os.Environ()` 按 glob 放行)→ 强制覆盖 `PYTHONPATH`(前置 `/workspace/.pip`)、`HOME`(强制 `sandboxHome`)、`PATH`(前置 `$HOME/.local/bin`)→ 逐项 `--setenv`。`filterEnv` 定义在 `internal/tool/executor/env.go:12`。

关键约束:

- **只能放行宿主已有变量,不能设定字面值**。operator 想注入 `PIP_INDEX_URL`/代理等公共配置,只能「宿主 export + allowlist 名字」,config 不是事实源。
- **强制层已存在且必须最后胜出**:`HOME` 必须指向 `sandboxHome`(tmpfs 挂在那),否则 `$HOME` 解析落空(见既有 bwrap.go D2/D3 注释);`PATH`/`PYTHONPATH` 是前置不变量。
- config loader 在 `yaml.Unmarshal` **之前**对整份 YAML 原文跑 `expandEnv`(`internal/config/config.go:804`,`${VAR}`/`${VAR:default}`),故**任何字符串字段**的 value 都已支持宿主环境展开 —— 这是一个对设计有利的既有事实。

本次改动面很小:在 `filterEnv` 与强制层之间插入一层 `env` 字面值 merge。

## Goals / Non-Goals

**Goals:**

- 让 operator 能在 `tools.executor.bash.env`(map)以 `KEY: value` 注入字面环境变量到每个 bash 沙箱,覆盖镜像源/代理等公共配置场景。
- 固定三层 env 优先级,保证既有 load-bearing 不变量(HOME/PATH/PYTHONPATH)不被破坏。
- 复用既有 `${VAR}` 全局展开,使秘钥可引用宿主环境而不硬编码。
- fail-fast 校验:保留键 `HOME` 与非法 key 名在 config load 阶段报错。

**Non-Goals:**

- **不做 per-user env**。`env` 是 operator 全局配置,所有用户共享。per-user 秘钥走 user-MCP 那条 workspace 居住路径(`data/{userID}/workspace/.blowball/...`),不在本次范围。
- 不改 `allowed_env_patterns` 语义(保留 glob + 动态宿主透传)。
- 不改 Landlock / bwrap 目录策略 / PYTHONPATH 注入实现 / `.pip` 落盘位置。
- 不为 `env` value 提供转义 `${...}` 的机制(沿用全局 `expandEnv` 既有行为,与其它字段一致;字面 `${X}` 会被展开,这是全 config 既有行为,非本次引入)。

## Decisions

### D1. 形状选 map,不选 list

`env` 用 YAML map(`env: { FOO: bar }`),Go `map[string]string` 原生反序列化,无需切分;value 含 `:`(如 `PATH: "/a:/b"`)无歧义。

- *备选(list of `"KEY=VALUE"` / `"KEY:VALUE"`)*:被否。需按首个 `=`/`:` 切分,value 含分隔符要小心;env 天然是 map,list 只是伪 map。用户已确认选 map。

### D2. 三层优先级:host allowlist < env 字面值 < 强制不变量

```
最高 ↑  强制层(bwrap.go 代码拥有):
         HOME = sandboxHome
         PATH ← 前置 $HOME/.local/bin
         PYTHONPATH ← 前置 /workspace/.pip
       ─────────────────────────────────────────────
       env 字面值层(新增,cfg.Env):
         operator 的 KEY:value,覆盖 host allowlist
       ─────────────────────────────────────────────
最低 ↓  host allowlist 层(filterEnv):
         按 glob 放行宿主变量
```

落地:`env := filterEnv(cfg.AllowedEnvPatterns); for k,v := range cfg.Env { env[k] = v }; /* 强制层照旧 */`。强制层在 `env` 之后应用,故 HOME/PATH/PYTHONPATH 不变量天然保住。

- *理由*:这是改动最小、且不破坏既有 load-bearing 不变量的方案。`env` 覆盖 host allowlist 符合「config 是事实源」;强制层始终胜出符合「load-bearing 不变量不可被 operator 配置」。
- `PATH`/`PYTHONPATH` 是「前置」不变量:operator 写 `PATH: /opt/bin` → 结果 `/home/blowball/.local/bin:/opt/bin`(tools bin 仍前置);`PYTHONPATH: /x` → `/workspace/.pip:/x`。允许 operator 贡献,代码仍前置,故不把它们列为保留键。

### D3. `HOME` 保留键 → fail-fast 拒绝,不静默覆盖

今天 `bwrap.go` 静默把 `env["HOME"]` 覆写成 `sandboxHome`(无论它来自 `filterEnv` 还是将来来自 `cfg.Env`)。新增 `env` map 后,operator 显式写 `HOME` 却被静默忽略会令人意外。

- *方案(采用)*:config 校验阶段检测 `env` 含 `HOME` → 报错「HOME is reserved (forced to the synthetic sandbox home)」。符合本仓库 `ParseMounts`/`validate()` 的 fail-fast 风格。
- *备选(静默覆盖)*:被否,违反「显式配置应生效或显式报错」。

### D4. `${VAR}` 展开天然可用 —— 秘钥不硬编码

`config.go:804` 的 `expandEnv` 在 `Unmarshal` 前对全文生效,故 `env: { OPENAI_API_KEY: "${OPENAI_API_KEY}" }` 会在 load 时从宿主取值,字面秘钥永不落 config。`"${LOG_LEVEL:info}"` 提供默认值。

- 这恰好优雅覆盖「按精确名字透传单个宿主变量」的单变量场景(多变量/通配仍走 `allowed_env_patterns`)。
- 信任边界一致:`config.yaml` 本就持有 `openai.api_key`、`jwt.secret`;bash env 放 operator 级变量是同一层。文档引导「秘钥用 `${VAR}`,勿写字面」。

### D5. key 名校验 `^[A-Za-z_][A-Za-z0-9_]*$`,fail-fast

防手滑(如 `"foo bar"`、空 key、`"1ABC"`)。非法 → config load 报错。合法环境变量名本就如此,不收紧能力。

### D6. 与 `allowed_env_patterns` 并存,职责互补

- `allowed_env_patterns`:glob(`PYTHON*`)+「值随宿主走」的动态透传。适合 PATH/HOME/LANG/USER/TERM 这类因宿主而异的变量。
- `env`:精确字面值(config 拥有)。适合镜像源/代理/开关这类 operator 想固定的公共配置。
- 两者定义同一 key 时,`env` 胜出(D2)。文档建议:把 `PIP_INDEX_URL`/`npm_config_registry` 等从 patterns 迁到 `env`,让 config 成为单一事实源。

### D7. 全局 operator 级,非 per-user

`env` 注入到所有用户的 bash 沙箱。镜像源/代理/`NODE_USE_ENV_PROXY=1` 这类公共配置正合适。per-user 秘钥不在本次范围(见 Non-Goals)。

## Risks / Trade-offs

- **[秘钥进 config.yaml]** → 与 `jwt.secret`/`openai.api_key` 同信任边界。缓解:文档引导 `${VAR}` 引用宿主(D4),字面秘钥不落 config;`.blowball` 命名空间在 agent 路径已被 `xizhi.ValidatePath` 拒绝,agent 读不到 operator config。
- **[operator 经 env 覆盖宿主 PATH]** → 非风险。PATH 是前置不变量,`$HOME/.local/bin` 始终前置(D2);operator 的 PATH 值只影响后续条目。
- **[`--setenv` 顺序非确定]** → 非风险。env 变量无序,bwrap 在 exec 前一次性 setenv 全部,无变量依赖另一者先 set;且 `filterEnv` 已是 map 迭代(现状)。测试用排序后的集合/子集断言,不断言精确序列。
- **[无 BREAKING]** → 纯新增字段,默认空 = 零行为变化。老 config 无 `env` 块时 `buildBwrapArgs` 输出与现状一致。

## Migration Plan

1. 代码改动(见 tasks)后 `make build && make test && make lint`。
2. **(可选)配置迁移**:把原经「宿主 export + `allowed_env_patterns`」注入的 `PIP_INDEX_URL`/`npm_config_registry` 等迁到 `tools.executor.bash.env`;代理类变量(`HTTP_PROXY`/`http_proxy`/`HTTPS_PROXY`/`https_proxy`/`NO_PROXY`/`no_proxy`)双写大小写,且 `NO_PROXY` 须含镜像源自身主机名(否则访问内部镜像被外网代理拦截)。
3. **回滚**:纯代码 + 纯新增字段,回滚 commit 即可;无 schema/数据迁移,老 config 不受影响。
