#!/bin/bash
# =============================================================================
# 构建并部署 office-vers 镜像 —— 直接粘贴到 Jenkins「执行 shell」框即可。
# 前提：Jenkins agent 与部署目标为同一台机器，已安装 docker。
# 配置通过命令行 flag 注入程序（程序不支持 .env 文件，flag > env > 默认值）。
# =============================================================================
set -euo pipefail

# ===================== 配置区（按需修改）=====================
APP=office-vers                       # 容器名
IMAGE=office-vers                     # 镜像名（不含 tag）
REGISTRY=                             # 例: harbor.example.com/library；留空=不推送，仅本地
PORT=8088:8080                       # 宿主端口:容器端口（容器端口需与 --listen-addr 一致）

MINIO_ENDPOINT=127.0.0.1

MINIO_ACCESS_KEY=minioadmin
MINIO_SECRET_KEY=minioadmin
MINIO_BUCKET=office-vers

# 程序运行参数（直接透传给 vers-control 的 flag）
ARGS=(
  --minio-endpoint   "${MINIO_ENDPOINT}:9000"
  --minio-access-key ${MINIO_ACCESS_KEY}
  --minio-secret-key ${MINIO_SECRET_KEY}
  --minio-bucket     ${MINIO_BUCKET}
  --listen-addr 8080
)
# 可选：--minio-use-ssl / --minio-region

HEALTH_URL=http://127.0.0.1:8088/healthz
HEALTH_TIMEOUT=60
# ============================================================

cd "${WORKSPACE:-.}"

# 1) 版本号：Jenkins BUILD_NUMBER + git 短 SHA；本地手跑则用时间戳
SHA=$(git rev-parse --short=8 HEAD 2>/dev/null || echo nogit)
TAG="${BUILD_NUMBER:-$(date +%Y%m%d%H%M%S)}-$SHA"
[ -n "$REGISTRY" ] && IMAGE="$REGISTRY/$IMAGE"
IMG="$IMAGE:$TAG"
echo "==> 目标镜像: $IMG"

# 2) 构建镜像（同时打版本号 tag 和 latest）
docker build \
  --build-arg VERSION="$TAG" \
  --build-arg COMMIT="$SHA" \
  --build-arg BUILD_TIME="$(date -u +%FT%TZ)" \
  -t "$IMG" -t "$IMAGE:latest" .

# 3) 推送（配置了仓库才执行）
if [ -n "$REGISTRY" ]; then
  echo "==> 推送镜像"
  docker push "$IMG"
  docker push "$IMAGE:latest"
fi

# 4) 重启容器（本地滚动替换；ARGS 作为 flag 透传给程序）
echo "==> 重启容器 $APP"
docker rm -f "$APP" >/dev/null 2>&1 || true
docker run -d --name "$APP" --restart unless-stopped \
  -p "$PORT" "$IMG" "${ARGS[@]}"

# 5) 健康检查
echo "==> 健康检查 $HEALTH_URL"
tries=$((HEALTH_TIMEOUT / 3))
for i in $(seq 1 "$tries"); do
  if curl -fsS --max-time 5 -o /dev/null "$HEALTH_URL"; then
    echo "    服务已就绪 ✔"; break
  fi
  [ "$i" -eq "$tries" ] && { echo "    健康检查失败 ✖"; exit 1; }
  sleep 3
done

# 6) 清理悬空镜像
echo "==> 清理悬空镜像"
docker image prune -f >/dev/null

echo "==> 完成: $IMG -> 容器 $APP ($PORT)"
