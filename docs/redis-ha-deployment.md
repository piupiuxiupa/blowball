# Redis 高可用部署(哨兵模式)— 部署 / 演练 / 回退手册

配套配置文件:`deploy/redis-ha/`(redis-master.conf、redis-replica.conf、
sentinel.conf、两个 systemd 单元、docker-compose.ha.yml 演练环境)。

前置阅读:

- `docs/redis-first-persistence.md` —— 消息走 Redis 写后落盘(write-behind),
  **Redis 是消息的唯一同步持久层**;
- `docs/shared-storage.md` —— `storage.workspace.backend: shared` 模式下,
  这套 Redis 同时是 **JuiceFS 的元数据引擎**(§2)。

## 0. TL;DR

- 方案:**Sentinel 哨兵模式**。3 台机器,1 主 2 从 + 3 个 sentinel 进程
  (每台机器跑 1 个 redis-server + 1 个 sentinel)。
- **不要上 Redis Cluster(分片)**:blowball 的写后落盘协议与分片槽位不兼容,
  直接接入会报 CROSSSLOT 错误(§1);JuiceFS 虽支持 Cluster,但会把一个文件
  系统的全部元数据固定在单个实例上,分片对元数据零收益。
- 该 Redis 承担**两个角色**:blowball 数据面(消息队列 + 缓存)与 JuiceFS
  元数据引擎(§2)。生产推荐**同机双实例**隔离故障域;小规模可共享一个实例
  (不同 DB 索引),但必须遵守 §2.1 的规则。
- 三条红线:所有节点 `appendonly yes`;`appendfsync always`(元数据在,
  丢一秒 = 数据块与索引可能不一致);`maxmemory-policy noeviction`
  (blowball 队列是业务数据,JuiceFS 也硬性要求 noeviction)。
- blowball 当前是单地址直连客户端(`redis.addr`,
  `internal/store/redis/redis.go:43`)。要吃满"自动故障转移",需要一次约 30
  行的客户端改造(§5 路线 A)或前置 VIP(§5 路线 B)。**不做这一步,
  哨兵集群仍然提供"数据不丢 + 可手工切换",但主从切换后写路径会持续报错,
  直到人工干预。**

## 1. 为什么是哨兵,而不是 Redis Cluster

blowball 的消息写后落盘协议大量使用"多 key 原子操作",而 Redis Cluster 把
key 散列到 16384 个槽,一条命令/事务里的 key 跨槽即报 `CROSSSLOT` 错误:

| 代码位置 | 操作 | 分片下的结果 |
|---|---|---|
| `internal/store/redis/msgqueue.go:56` `AppendMessagesDual` | MULTI/EXEC 事务同时写 `msgs:{session_id}`(读缓存)+ `msgs:buffer`(全局 ingest 队列) | 两个 key 几乎必然落在不同槽 → CROSSSLOT,**每条消息写入直接失败** |
| `internal/store/redis/msgqueue.go:89` `MoveMessageToProcessing` | `LMOVE msgs:buffer msgs:processing` 原子认领(flusher 消费) | 两 key 不同槽 → CROSSSLOT,队列永远无法消费 |
| `internal/store/redis/msgqueue.go:158` `RecoverProcessingToBuffer` | `LMOVE msgs:processing msgs:buffer`(进程崩溃恢复) | 同上 |

理论上可以给队列 key 加 hash tag(如 `msgs:{q}:buffer` / `msgs:{q}:processing`)
修掉 LMOVE 的跨槽问题,但双写事务无法这样修:`msgs:{session_id}` 是按会话
散列的缓存 key,和全局队列 key 永远不同槽,只能把事务拆成两条独立命令 —
丢掉"两写同成败"的语义,而 `AppendMessagesDual` 的注释明确说明调用方的
MySQL 回退路径依赖"pipeline 失败 = 两边都没写"这个前提,拆开就要重新推理
半写状态。

JuiceFS 侧的结论一致:官方虽支持 Redis Cluster,但为避免跨实例事务,一个
文件系统的全部元数据会通过 `{N}` hash-tag 前缀固定在**单个实例**上 ——
分片对元数据零收益,徒增一套拓扑。

结论:**当前代码接 Redis Cluster 直接报错;要分片需要重构写路径并削弱原子性,
而收益接近零** —— blowball 对 Redis 的使用只有三样:秒级中转队列
(`msgs:buffer`,默认 1s 内被 flusher 搬进 MySQL)、24h 读缓存
(`msgs:{sid}` / `session:{id}`)、5s 周期的原始 LLM 捕获缓冲
(`llm_raw:buffer`)。这个量级单主轻松扛住(§7 容量估算),瓶颈从来不在
Redis 吞吐。哨兵模式下所有 key 都在单一主节点上,上述协议**原样工作、
零代码改动**。

## 2. 双重角色:blowball 数据面 + JuiceFS 元数据引擎

`storage.workspace.backend: shared` 模式下,JuiceFS 的元数据引擎也跑在这套
Redis 上(`docs/shared-storage.md` §2.2)。两个角色对 Redis 的要求不同:

- **blowball 数据面**:队列丢 1s 可以用 MySQL 回退补救,缓存丢了可重建 —
  耐受度高;
- **JuiceFS 元数据**:元数据丢失/滞后 = **数据块(MinIO)与索引不一致**,
  文件系统层面的问题,没有回退层 —— 耐受度低,决定了 §4 的红线档位。

### 2.1 布局一:共享一个实例(小规模可接受)

blowball 用 `redis.db: 0`,JuiceFS 元数据用独立 DB 索引(shared-storage.md
的示例用 `/5`)。规则:

- `appendfsync always`(元数据红线,见 §4.1);
- `maxmemory` 按**两个角色之和**估算(§7),`noeviction` 本来就是双方要求;
- 清楚共享的代价:
  - **故障域合并**:Redis 打满内存 / 宕机 / failover 窗口,消息路径有
    MySQL 回退(§5.4),但**文件系统没有回退层** —— workspace、skills、
    OnlyOffice 落盘全部停摆;
  - **延迟互相干扰**:Redis 单线程,JuiceFS 元数据 op 在每次文件操作的
    关键路径上,与 blowball 的突发批量 RPUSH 共享同一事件循环,互相放大。

PoC / 小规模可接受;生产推荐布局二。

### 2.2 布局二:同机双实例(生产推荐)

每台机器跑两个 redis-server:**6379 = blowball 数据面**,**6380 = JuiceFS
元数据**(复制 redis-master.conf 改端口、独立 `dir`、独立密码可不同),
各自独立的 1 主 2 从复制。**同一组 3 个哨兵同时监控两个 master**
(`blowball-master` + `juicefs-master`,sentinel.conf 里有注释好的第二段
monitor 配置)—— 哨兵进程天然支持监控多个 master,不用再起一套。

收益:故障域与内存预算隔离;两边可按需取不同的 fsync 档位(6380 `always`,
6379 可退回 `everysec` 与 flush 窗口对齐);一边维护重启不影响另一边。
代价:每台多一个进程和一份内存。

### 2.3 JuiceFS 侧接入(哨兵 meta URL)

JuiceFS 客户端原生支持哨兵发现与自动跟随。meta URL 格式
(`shared-storage.md` §2.3–2.5 里所有 `redis://<meta-host>:6379/5` 处替换):

```
# 第一个名字是 sentinel master 名,后面是哨兵地址(共用一个哨兵端口),
# 末尾 /N 是 DB 索引。
# 布局一(共享实例,master 名 = blowball-master):
redis://:<REDIS_PASSWORD>@blowball-master,10.0.0.11,10.0.0.12,10.0.0.13:26379/5
# 布局二(双实例,master 名 = juicefs-master,指向 6380 那套的哨兵):
redis://:<REDIS_PASSWORD>@juicefs-master,10.0.0.11,10.0.0.12,10.0.0.13:26379/5
```

- `juicefs format` / `juicefs mount` / `blowball-juicefs.service` 的
  ExecStart 里统一用这个 URL;挂载了 JuiceFS 的**每台**机器都会在 failover
  后自动跟随新主,无需重挂。
- **不要**加 `?route-read=replica`(那是只读挂载把读路由到副本的优化;
  读写挂载必须保持读你所写)。

## 3. 拓扑

```
        ┌───────────── sentinel ×3(26379,互相发现,quorum = 2)─────────────┐
        │      同一组哨兵可同时监控 blowball-master 与 juicefs-master        │
        │                                                                    │
 node1 10.0.0.11            node2 10.0.0.12            node3 10.0.0.13
 redis-server :6379         redis-server :6379         redis-server :6379
 [初始主]                    replicaof node1            replicaof node1
 (可选 :6380 juicefs 元数据实例,布局二 —— 同样 1 主 2 从)
        └──────────── 复制 ────────┴────────── 复制 ──────────┘

   blowball(api / agent / all)           juicefs mount(每台挂载机)
   经哨兵发现当前主(§5 路线 A)         meta URL 哨兵格式(§2.3)
   或经 VIP(§5 路线 B)
```

- **3 台是下限**:sentinel 需要 ≥2 个节点同意才判死并切换(quorum=2,奇数个
  哨兵避免脑裂);2 个副本保证任意一台宕机后仍有冗余。
- 资源要求低:每台 2C / 2G 起步,磁盘按 AOF 写入量(开 `always` 后元数据
  实例的 fsync 压力主要看文件操作频率,普通 SSD 可扛)。
- 三台尽量跨机架/可用区,但保持低复制延迟(<10ms);跨城部署会拖慢
  failover 判定和复制,不建议。
- 端口:6379(数据面;布局二再加 6380)+ 26379(哨兵)在三台与 blowball /
  juicefs 挂载机之间互通,对外网封闭。

## 4. 配置要点(以及为什么)

### 4.1 持久化红线

- **`appendonly yes`(所有节点)** —— `msgs:buffer` 是消息的唯一同步层,
  裸重启的 Redis 会丢掉 flush 窗口里的消息。blowball 启动时会探测
  (`cmd/blowball/serve.go:350`,best-effort `CONFIG GET appendonly`),
  关着会打 WARN。
- **`appendfsync always`** —— JuiceFS 官方口径:`everysec` 性能更好但可能
  丢最后一秒写入;对元数据而言丢一秒 = 数据块与索引不一致的风险,所以本
  手册在有元数据的角色上取 `always`。代价可控:blowball 每 turn 只发一个
  MULTI(3 条命令),fsync 次数 ≈ turn 数 + 元数据写次数,普通 SSD 量级内。
  布局二里只服务 blowball 的 6379 实例可退回 `everysec`(丢失窗口与
  `messages.flush_interval` 的 1s 对齐,且有 MySQL 回退兜底)。
- RDB(`save 3600 1 300 100 60 10000`)保留为冷备/快照兜底,不影响 AOF;
  Redis 恢复时 AOF 优先于 RDB(AOF 更完整)。

### 4.2 内存红线

- **`maxmemory-policy noeviction`(所有节点)** —— 双重理由:blowball 的
  `msgs:buffer` / `msgs:processing` 是**业务数据不是缓存**,任何淘汰策略都
  会静默丢消息;JuiceFS 对元数据引擎**硬性要求** noeviction(启动时尝试
  `CONFIG SET` 自动纠正,失败只打 WARN —— 所以必须在配置里显式写死,不能
  依赖自动纠正)。
- 内存打满时 noeviction 的表现是**拒绝写入**而不是删数据。blowball 收到写
  错误会走同步直写 MySQL 的回退(§5.4);但 **JuiceFS 元数据写失败没有回退
  层 = 文件系统不可写,workspace 全停** —— 这是 maxmemory 必须留余量、
  必须配使用率告警(§9)的原因。
- `maxmemory` 按公 ×2~3 余量设置(§7);三台一致(任何一台都可能被提升
  为主)。

### 4.3 复制与切换

- `masterauth` **在主节点上也要配** —— failover 后旧主被哨兵降级为副本,
  它需要凭 `masterauth` 对新主认证,漏配这一行是哨兵部署最常见的坑。
- `min-replicas-to-write 1` + `min-replicas-max-lag 10` —— 没有健康副本时
  主节点拒绝写入,缩小 failover 丢数据窗口。注意:哨兵 + Redis 是**异步
  复制**,已确认的写不保证全部到达副本 —— 该配置缩小但不消除切换丢写窗口,
  所以 failover 后对元数据跑一次 `juicefs fsck`(§8.2)是标准动作。
  代价:切换完成后的几秒内(副本重同步完成前)新主可能拒写,期间 blowball
  走 MySQL 回退、文件操作短暂报错。可用性优先的部署可去掉这两行。
- `sentinel down-after-milliseconds 5000` —— 5s 判死;加上仲裁和切换,
  端到端恢复约 10~20s。
- `sentinel parallel-syncs 1` —— 切换后一次只让一个副本重同步,保持切换
  期间至少一个副本在服务。
- `sentinel failover-timeout 30000` —— 切换重试的抑制窗口。

### 4.4 运行时可写性(容易踩)

- 哨兵运行时会**重写自己的 sentinel.conf**(记录纪元、已发现的副本),
  该文件必须对运行用户可写。
- 切换时哨兵会对各节点下发 `REPLICAOF` + `CONFIG REWRITE`,把拓扑变化
  落回各节点的 redis.conf —— redis.conf 也必须对运行用户可写(两个
  systemd 单元已用 `ReadWritePaths` 放开 `/etc/redis`)。

### 4.5 网络与安全

- `bind 127.0.0.1 <本机内网IP>` + `requirepass`;6379(/6380)/26379 仅在
  三台与 blowball / juicefs 挂载机之间放行。
- 多网卡 / NAT / 容器端口映射场景,哨兵和副本会把自己"看到的地址"广播出去,
  必须显式设置 `replica-announce-ip` / `sentinel announce-ip`,否则彼此
  发现到一个不可达的地址。直连内网部署不需要。
- 用 IP 而不是主机名配置 `sentinel monitor`(Redis ≥6.2 若必须用主机名,
  开 `sentinel resolve-hostnames yes`)。

## 5. blowball 接入

### 5.1 现状

`internal/store/redis/redis.go:43` 用 go-redis 的单地址客户端
(`redis.NewClient`,由 `cmd/blowball/serve.go:337` 以 `cfg.Redis.Addr`
装配)。把 `redis.addr` 指向初始主节点:**数据面协议完全兼容**,但主从
切换后,旧主被降级为只读副本,写命令持续返回
`READONLY You can't write against a read only replica`,直到人工改配置重启。

### 5.2 路线 A(推荐):客户端支持哨兵

go-redis v9 原生自带 `redis.NewFailoverClient`:启动时通过哨兵查询当前主,
并订阅 `+switch-master` 事件自动跟随切换。改造点约 30 行:

1. `internal/config/config.go` 的 `RedisConfig` 增加可选 `sentinel` 块
   (`master_name` / `addrs` / 可选哨兵自身密码),校验 `addr` 与
   `sentinel` 二选一;
2. `internal/store/redis/redis.go` 的 `New()` 分流:配置了 sentinel 就改用
   `redis.NewFailoverClient(&redis.FailoverOptions{ MasterName, SentinelAddrs,
   Password: <数据面密码>, DB: cfg.Redis.DB, ... })`;
3. 其余零改动:AOF 探测(`CONFIG GET` 会路由到当前主)、队列协议、缓存
   读写全部兼容。**不要**开 `RouteByLatency` / `RouteRandomly`(那会把
   读命令路由到副本,破坏缓存路径的读你所写语义)。

目标 `config.yaml` 形态:

```yaml
redis:
  password: "${REDIS_PASSWORD}"   # 数据面密码,${VAR} 展开与其他 secret 一致
  db: 0
  sentinel:
    master_name: blowball-master
    addrs:
      - 10.0.0.11:26379
      - 10.0.0.12:26379
      - 10.0.0.13:26379
    # password: ""                # 哨兵进程自身的认证(可选,见 §11)
```

### 5.3 路线 B(不改代码):Keepalived VIP 前置

不动 blowball,`redis.addr` 填一个浮动 VIP。在 node1/node2 上跑 keepalived,
用检查脚本探测"本机 redis 当前是否为 master"(`redis-cli -a ... info
replication` 里 `role:master`),是 master 的一侧持有 VIP;failover 完成后
VIP 自动漂到新主所在机器。**JuiceFS meta URL 仍用 §2.3 的哨兵格式,不走
VIP**(JuiceFS 客户端原生跟随哨兵,VIP 那套只服务 blowball 的单地址客户端)。

```
# /etc/keepalived/keepalived.conf(node1 priority 150,node2 priority 100)
vrrp_script chk_redis_master {
    script "/etc/keepalived/check_master.sh"
    interval 2
    fall 2   # 连续 2 次不是 master 才降优先级,避开切换瞬态
    rise 2
}
vrrp_instance VI_1 {
    state BACKUP            # 两台都 BACKUP,避免抢占
    interface eth0
    virtual_router_id 51
    priority 150
    advert_int 1
    authentication { auth_type PASS auth_pass <VRRP_PASSWORD> }
    virtual_ipaddress { 10.0.0.10/24 }
    track_script { chk_redis_master }
}
```

缺点:只覆盖两台机器(第三台无 VIP 可漂)、多一层要运维的组件、切换比
路线 A 慢且可能出现短暂的 VIP 与实际主不一致。**除非完全不能改代码,
优先路线 A。**

### 5.4 failover 窗口期 blowball 的行为(为什么消息不丢、但要盯文件系统)

哨兵切换的 10~20s 里,Redis 写路径必然报错。分角色看:

- **消息(有回退,不丢)**:turn 落盘时 `AppendMessagesDual` 的 pipeline
  报错 → 自动回退为同步直写 MySQL(idempotent INSERT)。消息不丢,只是该
  turn 落库路径变慢。读路径 `RecoverMessages` 遇到 Redis 错误 → 回源
  MySQL。flusher 的 `LMOVE` 认领失败按退避重试,队列驻留 Redis 直到恢复。
- **JuiceFS 文件系统(无回退,短暂停摆)**:窗口期元数据操作报错,文件
  工具(`xizhi_*`)、workspace API、OnlyOffice 落盘、executor 沙箱内的
  文件 IO 都会失败;JuiceFS 客户端自动跟随新主,窗口后自愈。**这是 Redis
  HA 对本系统最硬的依赖面** —— 消息有 MySQL 兜底,文件系统没有。
- **llm_raw 捕获**:staging 是 best-effort(`llm_raw:buffer` RPUSH 失败
  即丢该行),窗口内的原始捕获可能缺失 —— 观测数据,可接受。
- **唯一要避免的**:Redis failover 与 MySQL 维护窗重叠。两个依赖同时不可用
  时,消息写入失败会把错误抛给该 turn(用户会看到报错,而不是静默丢数据)。

## 6. 部署步骤(生产,三台机器)

以下以 node1 = 初始主为例;三台统一执行的部分标注【三台】。

1. 【三台】安装 Redis ≥ 7.0(发行版包或源码)与 redis 用户:

   ```bash
   sudo useradd --system --home /var/lib/redis --shell /usr/sbin/nologin redis
   sudo mkdir -p /var/lib/redis /etc/redis
   sudo chown -R redis:redis /var/lib/redis /etc/redis
   ```

2. 【三台】放置配置(替换占位符后):

   ```bash
   # node1: deploy/redis-ha/redis-master.conf  -> /etc/redis/redis.conf
   # node2/3: deploy/redis-ha/redis-replica.conf -> /etc/redis/redis.conf
   # 三台: deploy/redis-ha/sentinel.conf(各改本机 bind) -> /etc/redis/sentinel.conf
   sudo chown redis:redis /etc/redis/redis.conf /etc/redis/sentinel.conf
   sudo chmod 640 /etc/redis/*.conf        # 内含密码
   ```

   布局二(双实例):6380 元数据实例复制同一份 conf 改 `port 6380`、
   `dir /var/lib/redis-juicefs`、`pidfile`,并部署第二个 systemd 实例
   (`systemd` 模板单元或复制一份改路径);sentinel.conf 里取消
   `juicefs-master` 那段的注释。

3. 【三台】安装 systemd 单元并按序启动 —— 先三台 redis-server,全部健康
   后再起哨兵:

   ```bash
   sudo cp deploy/redis-ha/blowball-redis.service \
            deploy/redis-ha/blowball-redis-sentinel.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now blowball-redis
   # 三台 redis 就绪后:
   sudo systemctl enable --now blowball-redis-sentinel
   ```

4. 验证拓扑与哨兵视图:

   ```bash
   redis-cli -h 10.0.0.11 -a '<REDIS_PASSWORD>' INFO replication
   #   期望 role:master, connected_slaves:2
   redis-cli -h 10.0.0.12 -a '<REDIS_PASSWORD>' INFO replication
   #   期望 role:slave, master_link_status:up
   redis-cli -h 10.0.0.11 -p 26379 SENTINEL masters
   #   期望 flags 含 master,num-slaves=2,num-other-sentinels=2
   #   (布局二:两套 master 都应出现)
   redis-cli -h 10.0.0.11 -p 26379 SENTINEL get-master-addr-by-name blowball-master
   #   期望 10.0.0.11 6379
   redis-cli -h 10.0.0.11 -p 26379 SENTINEL ckquorum blowball-master
   #   期望 OK 2 usable sentinels. Quorum and failover authorization can be reached
   ```

5. JuiceFS 侧:首次按 `shared-storage.md` §2.3–2.5 部署,meta URL 用
   §2.3 的哨兵格式;已有部署改 `blowball-juicefs.service` 的 ExecStart 并
   `systemctl restart`(客户端重连后走哨兵发现)。

6. 接入 blowball(§5 路线 A 或 B),滚动重启各角色进程,发消息验证:
   `LLEN msgs:buffer` 在一个 flush 周期内回到 0,
   `messages` 表出现新行,启动日志无 AOF WARN;跨节点读写一个 workspace
   文件确认挂载健康。

## 7. 容量与内存规划

blowball 侧三个 key 族的量级(单条消息 JSON 约 1~2KB):

| Key | 常驻量 | 估算 |
|---|---|---|
| `msgs:buffer` / `msgs:processing` | 写入速率 × flush 间隔 | 100 msg/s × 1s ≈ 200KB;**MySQL 宕机时无界增长**(flusher 永不丢弃) |
| `msgs:{session_id}` | 24h TTL 累积 | 活跃会话数 × 会话消息数 × 2KB,是内存大头 |
| `session:{session_id}` | 24h TTL | 每会话一个 blob,量级远小于上者 |
| `llm_raw:buffer` | 并发 LLM 调用 × 5s flush 周期 | 每调用捕获上限 8MB,10 并发重度调用 ≈ 数十~数百 MB 峰值 |

JuiceFS 元数据(布局一共实例时计入同一 `maxmemory`;布局二单独预算):

- 官方近似值 **~300 字节/文件**(小文件、无 xattr;大文件多次修改、碎片、
  xattr、长文件名都会放大)。10 万个 workspace 文件 ≈ 30MB —— 通常远小于
  blowball 缓存,但**只增不减**(没有 TTL),按文件数长期增长规划。
- 实际用量看 `juicefs status <meta-url>`(inode 数 / 元数据大小)。

- `maxmemory` = 上述之和 × 2~3 余量,典型部署 2~4GB 起步。
- **打满 noeviction 的表现是拒绝写入**:blowball 降级直写 MySQL(安全),
  但 JuiceFS 元数据写失败 = 文件系统不可写(§4.2)—— 必须告警处理;
  配 `used_memory` 使用率监控(§9)。
- MySQL 故障是内存的最大风险源:队列只进不出,内存预算必须能扛住"MySQL
  修复时长 × 写入速率"。对 `LLEN msgs:buffer` 设增长告警(现有约定:
  持续增长 = MySQL 挂了)。

## 8. 故障演练

### 8.1 演练环境(单机 6 容器)

```bash
docker compose -f deploy/redis-ha/docker-compose.ha.yml up -d
# 观察初始状态:redis-1 是主
docker compose -f deploy/redis-ha/docker-compose.ha.yml exec sentinel-1 \
    redis-cli -p 26379 SENTINEL get-master-addr-by-name blowball-master
```

### 8.2 kill 主节点,观察切换

```bash
docker compose -f deploy/redis-ha/docker-compose.ha.yml stop redis-1
# 约 5s 判死 + 数秒切换;盯哨兵日志里的 +sdown / +odown / +switch-master
docker compose -f deploy/redis-ha/docker-compose.ha.yml logs -f sentinel-1 sentinel-2
# 确认新主
docker compose -f deploy/redis-ha/docker-compose.ha.yml exec sentinel-1 \
    redis-cli -p 26379 SENTINEL get-master-addr-by-name blowball-master
# 期望返回 redis-2(或 redis-3)
```

让旧主回来、验证它被自动降级为副本:

```bash
docker compose -f deploy/redis-ha/docker-compose.ha.yml start redis-1
docker compose -f deploy/redis-ha/docker-compose.ha.yml exec redis-1 \
    redis-cli -a ha-test-pass INFO replication | grep role   # 期望 role:slave
```

### 8.3 生产切换后的标准动作(JuiceFS 一致性检查)

哨兵 + Redis 是异步复制,已确认的元数据写不保证全部到达副本;`min-replicas-
to-write` 缩小但不消除这个窗口。**每次真实/演练 failover 后**:

```bash
juicefs fsck <哨兵格式的 meta-url>   # 检查元数据与数据块一致性
```

并在各挂载机上读写一个文件,确认 JuiceFS 客户端已跟随新主。

### 8.4 带着真实 blowball 演练

生产切换前至少演练一次完整链路:kill 当前主 → 观察 blowball 日志(窗口期
出现 Redis 写错误与 MySQL 直写回退,文件操作短暂报错)→ 切换完成后新消息
恢复正常写入、workspace 文件操作恢复 → `LLEN msgs:buffer` 回落 → 检查
MySQL 中窗口期 turn 的消息行齐全 → `juicefs fsck` 通过。

### 8.5 受控切换(维护用,不杀进程)

```bash
redis-cli -h <任意哨兵> -p 26379 SENTINEL failover blowball-master
```

## 9. 监控要点

| 指标 / 命令 | 告警条件 |
|---|---|
| `LLEN msgs:buffer` / `LLEN msgs:processing` | 持续增长(MySQL 故障);processing 非 0 且不回落(flusher 卡死) |
| `INFO replication`(主) | `connected_slaves < 1`;从库 `master_repl_offset - slave_offset` 持续拉大 |
| `SENTINEL ckquorum blowball-master` | 非 OK(哨兵失联,此时丧失自动切换能力) |
| 哨兵日志事件 `+sdown/+odown/+switch-master` | 任何 switch-master 都应触发一次 §8.3 的 fsck |
| `INFO memory` 的 `used_memory` / maxmemory | > 70% 告警(noeviction 打满 = 消息降级 + **文件系统不可写**) |
| `juicefs status <meta-url>` | inode/元数据用量增长趋势(只增不减,纳入容量规划) |
| JuiceFS 挂载存活(`mountpoint -q {data-dir}/data`) | 未挂载 → page(`shared-storage.md` §5) |
| blowball 启动日志 | `redis appendonly is disabled` WARN(§4.1 红线被破) |
| blowball `msgflush flush failed` WARN(带 `queue_len`) | 持续出现即 MySQL 侧故障 |

## 10. 迁移 / 回退

从单机 Redis 迁到哨兵集群时,把 key 分成两类对待:

**blowball 侧(可弃,不用搬)**:

- `msgs:buffer` / `msgs:processing` —— 业务数据,**必须排空**才能切
  (`LLEN` 均为 0,并静默一个 flush 周期;同 `docs/redis-first-persistence.md`
  的回滚规则);
- `msgs:{sid}` / `session:{id}` —— 24h TTL 缓存,可整体丢弃,读路径自动
  回源 MySQL 重建;
- `llm_raw:buffer` —— 观测数据,排空为佳,未排空部分接受丢失。

**JuiceFS 元数据(持久业务数据,必须迁移)** —— 若旧单机 Redis 上已有
JuiceFS 元数据,绝不能弃,否则整个共享文件系统的索引就没了(数据块还在
MinIO,但没有索引不可用)。官方路径是 dump → load,需先停所有挂载(或
确保完全无写入):

```bash
# 1. 停写:停各机 juicefs 挂载与 blowball(shared-storage.md §3 的停写步骤)
# 2. dump 旧引擎
juicefs dump redis://:<OLD_PASSWORD>@<旧单机>:6379/5 /backup/meta.dump.json
# 3. load 进新集群的初始主(直接指 IP 最简单;DB 索引与布局对应)
juicefs load redis://:<REDIS_PASSWORD>@10.0.0.11:6379/5 /backup/meta.dump.json
# 4. load 后一致性检查
juicefs fsck redis://:<REDIS_PASSWORD>@10.0.0.11:6379/5
```

**步骤串联**:维护窗内停写 → 确认 blowball 队列排空 + dump/load 元数据 →
三台部署本方案并起集群(§6)→ JuiceFS meta URL 切到哨兵格式(§2.3)→
blowball 切到哨兵地址(§5)→ 滚动重启 → 验证发消息 + 跨节点文件读写。
旧单机 Redis 停服下线。

**回退**(回到单机或旧地址):同样先排空 blowball 队列;JuiceFS 元数据
dump/load 回旧引擎;再改 `redis.addr` 与 meta URL 指回单机、滚动重启。

**备份契约**(布局二对 6380 实例、布局一对整个实例):MinIO 数据桶与元数据
快照必须**成对、同时点**备份,单独一份不可恢复(见 `shared-storage.md` §4);
Redis 侧快照 AOF 优先。

## 11. 可选加固(按需)

- **哨兵进程自身认证**(Redis ≥6.2):给哨兵配 ACL 用户,blowball 侧对应
  `FailoverOptions.SentinelUsername` / `SentinelPassword`,防止未授权方查询
  拓扑或伪造切换指令。内网 + 防火墙场景可暂不做。
- **ACL 替代全局密码**:数据面用 ACL 用户限定命令集;注意 blowball 需要
  `CONFIG GET`(AOF 探测,失败只是跳过)与常规读写命令,JuiceFS 需要
  `CONFIG SET maxmemory-policy`(自动纠正 noeviction,失败仅 WARN)。
- **TLS**:内网明文 + 密码通常足够;合规要求高再开 `port 0` + TLS 监听。
