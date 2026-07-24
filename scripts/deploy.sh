#!/usr/bin/env bash
#
# Jenkins 自由风格任务「Execute shell」里调用：bash scripts/deploy.sh
#
# 一个镜像、本机直接 run（无 ssh、无镜像仓库）：
#   构建 → 停旧容器 → 起 api(普通权限) → 起 agent(--privileged) → 双端口 /healthz 健康检查。
#
# 一次性前提（你已就绪）：
#   - 宿主 /opt/cowork/meta-data/data 是 JuiceFS(MinIO 后端) 挂载，带 --allow-other
#   - /opt/cowork/config.yaml 已写好（明文密钥，按你们规矩；运行时挂载，不进镜像）
#   - Jenkins agent 本机已装 docker
set -euo pipefail

# ---------- 可调参数 ----------
IMAGE="blowball:latest"
CONTAINER_API="blowball-api"
CONTAINER_AGENT="blowball-agent"

COWORK="/opt/cowork"
CONF="${COWORK}/config.yaml"
DATA_ROOT="${COWORK}/meta-data"

PORT_API=8080      # api 角色：server.port
PORT_AGENT=8081    # agent 角色：server.agent_port

HEALTH_TIMEOUT=60  # /healthz 探活最长等待秒数
# --------------------------------

# 构建上下文 = 仓库根。在 Jenkins 内若以 inline 方式粘贴脚本，设 REPO_ROOT=$WORKSPACE。
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

log() { echo "==> $*"; }

# ---- 0. 前置检查 ----
[ -f "${CONF}" ]      || { echo "!! 缺少配置 ${CONF}"; exit 1; }
[ -d "${DATA_ROOT}" ] || { echo "!! 缺少数据根 ${DATA_ROOT}"; exit 1; }

# ---- 1. 构建镜像 ----
log "building ${IMAGE} from ${REPO_ROOT}"
docker build -t "${IMAGE}" "${REPO_ROOT}"

# ---- 2. 停掉并删除旧容器（两个角色）----
log "removing old containers (if any)"
docker rm -f "${CONTAINER_API}" "${CONTAINER_AGENT}" >/dev/null 2>&1 || true

# ---- 3. 启动 api 角色（普通权限，最小权限；不跑执行器，无需特权）----
log "starting ${CONTAINER_API} on :${PORT_API}"
docker run -d \
  --init \
  --name "${CONTAINER_API}" \
  --restart unless-stopped \
  -p "${PORT_API}:8080" \
  -v "${COWORK}:${COWORK}" \
  "${IMAGE}" serve --role api -f "${CONF}" -d "${DATA_ROOT}"

# ---- 4. 启动 agent 角色（--privileged：bwrap 要在容器内再建 user/mount/pid/net 命名空间）----
log "starting ${CONTAINER_AGENT} on :${PORT_AGENT} (privileged)"
docker run -d \
  --init \
  --privileged \
  --name "${CONTAINER_AGENT}" \
  --restart unless-stopped \
  -p "${PORT_AGENT}:8081" \
  -v "${COWORK}:${COWORK}" \
  "${IMAGE}" serve --role agent -f "${CONF}" -d "${DATA_ROOT}"

# ---- 5. 健康检查：两个角色都暴露未鉴权的 /healthz ----
wait_health() {
  local name="$1" port="$2"
  log "waiting for ${name} /healthz on :${port}"
  local i=0
  until docker exec "${name}" wget -q -O- "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; do
    i=$((i + 2)); sleep 2
    if [ "${i}" -ge "${HEALTH_TIMEOUT}" ]; then
      echo "!! ${name} 未在 ${HEALTH_TIMEOUT}s 内就绪，最近日志："
      docker logs --tail 50 "${name}" || true
      exit 1
    fi
  done
  log "${name} healthy ✓"
}

wait_health "${CONTAINER_API}"  "${PORT_API}"
wait_health "${CONTAINER_AGENT}" "${PORT_AGENT}"

log "deploy complete: ${CONTAINER_API}:${PORT_API} (api) + ${CONTAINER_AGENT}:${PORT_AGENT} (agent)"
