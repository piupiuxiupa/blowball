# =============================================================================
# officevers(office-version-service)镜像,自独立仓库迁入后以仓库根为 context:
#   docker build -f deploy/officevers/Dockerfile -t officevers .
#   构建：   golang:1.26-alpine（版本跟随 go.mod，可用 --build-arg GO_BASE_IMAGE 覆盖）
#   运行：   alpine:3.20，静态二进制、含 ca-certificates/tzdata
# =============================================================================
ARG GO_BASE_IMAGE=golang:1.26-alpine

# ----------------------------- 构建阶段 --------------------------------------
FROM ${GO_BASE_IMAGE} AS builder

WORKDIR /src

# 先拷贝依赖，利用 Docker 层缓存（依赖不变则该层命中缓存）
COPY go.mod go.sum ./
RUN go mod download

# 服务自包含（仅依赖 gin/minio 与标准库），拷贝自身目录即可。
COPY services/officevers/ ./services/officevers/

# ARG 由构建脚本通过 --build-arg 注入；main 包未声明的变量链接器自动忽略。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildTime=${BUILD_TIME}" \
        -o /out/vers-control ./services/officevers/cmd/server

# ----------------------------- 运行阶段 --------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/vers-control /usr/local/bin/vers-control

ENV TZ=Asia/Shanghai \
    LISTEN_ADDR=:8088

# 端口实际由 --listen-addr flag 决定；默认 :8088
EXPOSE 8088
# 服务自身处理 SIGTERM 实现优雅关断，无需 tini 等垫片
ENTRYPOINT ["/usr/local/bin/vers-control"]
