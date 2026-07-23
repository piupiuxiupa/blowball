# Shared workspace storage (MinIO-backed JuiceFS)

This runbook describes how to run blowball with `storage.workspace.backend: shared`,
where the per-user data root (`{data-dir}/data` — sessions warm tier, workspace,
per-user skills) lives on an operator-mounted shared POSIX filesystem instead of
each process's local disk. The reference implementation is **JuiceFS** with
**MinIO** as the data backend and a **dedicated metadata engine**.

Shared mode gives two things local mode cannot: **multi-node sharing** (api/agent
instances on different machines read/write one per-user data tree) and
**disaster-recovery backup** (MinIO provides replication/versioning/lifecycle).

> **Platform**: JuiceFS, bubblewrap, and Landlock are Linux-only. macOS/Windows
> dev machines must use `backend: local`. Shared mode is supported on Linux only.

---

## 1. Why this is transparent (zero-change surface)

The whole point of choosing JuiceFS over s3fs/direct-S3 is that it presents a
**real POSIX filesystem**, so every existing file operation in blowball is
unchanged — only the mount point under `{data-dir}/data` changes. The following
surfaces require **no code change** in shared mode (audit checklist for review):

| Surface | Why it is transparent |
|---|---|
| `internal/store/fs/*` (`UserWorkspace`, `sessionPath`, `EnsureUserDirs`, `UserSkills`) | All paths derived via `filepath.Join(root, ...)`. The root is `{data-dir}/data`; whatever is mounted there is used verbatim. |
| `internal/handler/workspace.go` (upload/download/content/delete/rename/OnlyOffice) | Plain `os.*` calls scoped by `xizhi.ValidatePath`. |
| `internal/handler/message_stream.go:151` (`workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")`) | Derives the workspace from `dataDir`; follows the mount. |
| `internal/tool/xizhi/*` (6 tools + `ValidatePath` symlink defense) | Real POSIX paths; `EvalSymlinks` + `os.SameFile` work on JuiceFS. |
| `internal/tool/executor/*` (`bwrap --bind {workspace} /workspace`, `runner.go`) | Binds a real directory; rename atomicity, random write, dir listing all hold on JuiceFS. |
| `onlyOfficePersist` atomic `os.Rename` (`workspace.go:806`) | JuiceFS `rename` is atomic (s3fs is NOT — that is why s3fs was rejected). |

The only code added by the `add-minio-workspace-storage` change is:

- `internal/config/config.go` — the `storage.workspace.backend` knob + validation.
- `internal/storage/` — the shared-mode startup mount health check.
- `internal/tool/executor/probe.go` — the shared-mode bwrap `--allow-other` self-check.
- `cmd/blowball/serve.go` — wiring those two checks into `setupRuntime`.

---

## 2. Deployment runbook (new cluster, `shared` mode)

### 2.1 MinIO bucket

Create a bucket for JuiceFS data chunks, e.g. `blowball-juicefs`. Grant an
access key / secret key pair dedicated to JuiceFS (not reused by other tenants).

### 2.2 Dedicated metadata engine

JuiceFS metadata is its availability lifeline: if the engine is down, the
filesystem is unreadable. **Do not** reuse blowball's session-cache Redis for
this. Provision a **dedicated** metadata store:

- **Production**: a HA Redis (Sentinel or Cluster), or TiKV.
- **Dev**: a separate Redis DB index on an existing instance is acceptable.

Example meta URL (dedicated Redis DB index `5`, distinct from blowball's
`redis.db`):

```
redis://<meta-host>:6379/5
```

### 2.3 Format the JuiceFS filesystem

On any operator host with the `juicefs` binary:

```bash
juicefs format \
  --storage minio \
  --bucket http://<minio-host>:9000/blowball-juicefs \
  --access-key <MINIO_KEY> \
  --secret-key <MINIO_SECRET> \
  redis://<meta-host>:6379/5 \
  blowball
```

### 2.4 Enable `user_allow_other` (required for executor tools)

JuiceFS runs as a FUSE mount that, by default, only the mounting uid can access.
blowball's executor sandbox uses `bwrap --unshare-user`, so sandboxed processes
run as a **mapped uid** and need the mount to allow other uids:

```bash
# /etc/fuse.conf — uncomment or add:
user_allow_other
```

### 2.5 systemd mount unit (mount BEFORE blowball)

Create `/etc/systemd/system/blowball-juicefs.mount` (unit name must match the
escaped mount path) **or** a service unit. A service unit is simpler:

```ini
# /etc/systemd/system/blowball-juicefs.service
[Unit]
Description=JuiceFS mount for blowball shared workspace
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Mount JuiceFS onto {data-dir}/data with --allow-other so bwrap's mapped uid
# can read/write /workspace. --cache-dir/-size are recommended defaults.
ExecStart=/usr/local/bin/juicefs mount \
  redis://<meta-host>:6379/5 \
  /var/lib/blowball/data \
  --allow-other \
  --cache-dir /var/cache/juicefs \
  --cache-size 1073741824
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Order it **before** blowball so the mount is ready when blowball boots:

```ini
# in blowball.service [Unit]:
Before=blowball.service
Requires=blowball-juicefs.service
After=blowball-juicefs.service
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now blowball-juicefs.service
mountpoint -q /var/lib/blowball/data && echo "mounted"
```

### 2.6 Point blowball at shared mode and start

```yaml
# config.yaml
storage:
  workspace:
    backend: shared
```

On startup blowball logs `shared workspace backend enabled; running mount health
check` and verifies `/var/lib/blowball/data` is writable and FUSE-family. If the
operator forgot to mount it, blowball **fatals and refuses to start** (so the
node cannot silently write local disk and diverge). If executor tools are
enabled and this is an `agent`/`all` role, an extra bwrap probe catches a
missing `--allow-other`/`user_allow_other`.

Start both roles against the **same** `-d` root and MinIO bucket + metadata
engine so they share the data plane:

```bash
./bin/blowball serve --role api   -d /var/lib/blowball -f /etc/blowball/config.yaml
./bin/blowball serve --role agent -d /var/lib/blowball -f /etc/blowball/config.yaml
```

---

## 3. Migrating an existing local-mode deployment

Goal: move an existing `{data-dir}/data` from local disk onto JuiceFS with no
data loss, then switch `backend` to `shared`.

1. **Stop writes.** Stop blowball (or put it in maintenance) so the data tree is
   quiescent.
2. **Mount JuiceFS to a temporary point** (NOT yet onto `{data-dir}/data):
   ```bash
   sudo mkdir /mnt/juicefs-migrate
   sudo juicefs mount redis://<meta-host>:6379/5 /mnt/juicefs-migrate --allow-other
   ```
3. **Copy** the existing data in, preserving all POSIX attributes (the `-aHAX`
   flags preserve owners, hard links, ACLs, xattrs — important for per-user
   ownership and JuiceFS metadata fidelity):
   ```bash
   sudo rsync -aHAX --info=progress2 /var/lib/blowball/data/ /mnt/juicefs-migrate/
   ```
4. **Verify** the copy: compare file counts and a checksum sample.
   ```bash
   sudo find /var/lib/blowball/data -type f | wc -l
   sudo find /mnt/juicefs-migrate   -type f | wc -l
   ```
5. **Atomic switch.** Rename the old local dir out of the way and mount JuiceFS
   onto `{data-dir}/data`, then bring up the systemd mount unit from §2.5:
   ```bash
   sudo mv /var/lib/blowball/data /var/lib/blowball/data.local-backup
   sudo systemctl start blowball-juicefs.service   # mounts onto .../data
   ```
6. **Set `backend: shared`** and start blowball. Confirm the health check passes.
7. **Keep `data.local-backup`** as a rollback snapshot until you have verified
   the cluster, then delete it.

---

## 4. Backup & restore (DR contract)

> **Contract**: a valid backup is the MinIO data bucket **and** the metadata
> engine snapshotted **together, consistently**. JuiceFS stores data blocks in
> the bucket and the index in the metadata engine; either one alone is
> **unrecoverable**.

### 4.1 Backup

Take the two snapshots at the same point in time (or use JuiceFS's own
mechanisms, which keep them consistent):

- **MinIO bucket** — snapshot/replicate `blowball-juicefs` per your MinIO
  deployment (e.g. `mc mirror`, bucket replication, or a storage-level snapshot).
- **Metadata engine** — snapshot the dedicated Redis (RDB/AOF + snapshot) or
  TiKV at the same instant. If using a separate Redis DB index, snapshot that DB.

Label the pair together so they are never separated.

### 4.2 Restore

Order matters — **metadata first, then point it back at the bucket**:

1. Restore/reconnect the metadata engine from its snapshot (e.g. reload the
   Redis RDB for DB index `5`).
2. Restore the MinIO bucket snapshot so the data blocks the metadata references
   exist.
3. Mount JuiceFS against the restored metadata URL + the same bucket, onto
   `{data-dir}/data`.
4. Start blowball with `backend: shared`; the health check passes once the mount
   is up.

### 4.3 JuiceFS-native integrity tools

```bash
juicefs gc      redis://<meta-host>:6379/5   # reclaim orphaned data chunks
juicefs fsck    redis://<meta-host>:6379/5   # check metadata/data consistency
juicefs snapshot redis://<meta-host>:6379/5 <name>  # metadata snapshot (engine-dependent)
```

---

## 5. Monitoring & alerting

| Metric | Source | Alert threshold |
|---|---|---|
| JuiceFS client healthy / mounted | `mountpoint -q {data-dir}/data` from a sidecar/health timer; JuiceFS `metrics` (Prometheus) | not mounted → **page** (blowball would refuse to start, but an already-running node losing the mount fails at runtime) |
| Metadata engine reachable | Redis/TiKV health check (ping/sentinel quorum) | unreachable > 30s → **page** (FS unavailable = workspace unavailable even if MySQL/Redis-for-cache are up) |
| Metadata engine HA quorum | Sentinel/Cluster node count | lost quorum → **page** |
| MinIO bucket usage / capacity | MinIO Prometheus metrics, `mc du` | > 80% → warn; > 90% → page |
| JuiceFS latency/errors | JuiceFS Prometheus metrics (op latency, FUSE errors) | elevated error rate → warn |
| blowball startup health-check failures | `blowball[-api|-agent].log` `shared workspace backend health check failed` | any occurrence → page (node refused to boot) |

---

## 6. Verification procedures

These verify the shared-mode guarantees. The scripted parts are in
`scripts/verify-shared-storage.sh` (cross-node workspace visibility); the rest
are operator-run checklists for environments with real MinIO + JuiceFS.

### 6.1 Cross-node workspace visibility (2.2 / 6.3.1)

Run `scripts/verify-shared-storage.sh` with the two nodes' base URLs and a valid
JWT/credential. It uploads a file through node A's workspace API and confirms
node B immediately lists and reads it — proving close-to-open consistency on the
shared mount.

### 6.2 Cross-instance delete semantics (2.3 / 6.3.5)

1. On node A, delete a session (or workspace file).
2. On node B, read that session history (or file). Expect **not-found**.
   - blowball re-validates ownership against MySQL on every read, so a stale
     Redis cache entry on node B is treated as not-found until TTL, matching
     single-node delete semantics. (Redis is intentionally not cleared on delete.)

### 6.3 executor on FUSE (3.1–3.4)

Requires a real JuiceFS mount + `--allow-other` + `user_allow_other`. On an
`agent`/`all` role with executor tools enabled:

- **3.1**: confirm the startup bwrap self-check passed (look for the absence of
  the `executor shared-workspace self-check failed` fatal). Then run an agent
  `bash`, `python`, and `pip_install` turn against a user and confirm no EACCES.
- **3.2**: on node A, `pip_install numpy`; on node B, run a `python` turn that
  `import numpy` — it must succeed without reinstalling (shared `/workspace/.pip`
  via `PYTHONPATH`).
- **3.3**: trigger an OnlyOffice save on node A and reopen on node B; confirm
  the latest content is served (the atomic `os.Rename` in `onlyOfficePersist`
  holds on JuiceFS).
- **3.4**: confirm the Landlock policy does not reject reads/writes under
  `{d}/data` when it is a FUSE mount. If xizhi/executor IO fails under Landlock,
  the mount path is already in the RW ruleset (`{d}/data`); file an issue.

### 6.4 Degradation behavior (6.3.5 / 6.3.6)

- **Kill the metadata engine**: workspace reads/writes fail with explicit errors
  (no silent corruption). blowball does not mask this; agent turns and workspace
  API calls surface the FS error. Restore the engine to recover.
- **Kill a node's JuiceFS mount**: that node's next startup fails the health
  check and **fatals** (refuses to boot). A running node whose mount drops later
  fails at the next IO.

### 6.5 Local-mode regression (6.4)

With `backend: local` (the default), run the full single-node flow (login,
session create, message stream, workspace upload/download, OnlyOffice, executor
turns) and confirm zero behavior change versus before the change. The new config
knob defaults to `local` and the health check is skipped entirely.

---

## 7. Rollback to local mode

1. Stop blowball.
2. Set `storage.workspace.backend: local` (or remove the `storage` block).
3. If you migrated FROM local (§3) and still have `data.local-backup`, or rsync
   the shared data back to local disk:
   ```bash
   sudo systemctl stop blowball-juicefs.service
   sudo mv /var/lib/blowball/data /var/lib/blowball/data.shared-saved
   sudo mv /var/lib/blowball/data.local-backup /var/lib/blowball/data
   # (or: sudo rsync -aHAX /var/lib/blowball/data.shared-saved/ /var/lib/blowball/data/)
   ```
4. Start blowball. It now uses local disk per-process again, identical to the
   pre-shared-storage behavior.
