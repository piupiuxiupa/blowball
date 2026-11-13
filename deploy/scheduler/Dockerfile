# blowball-scheduler 独立镜像。
# 构建命令(仓库根为 context):
#   docker build -f deploy/scheduler/Dockerfile -t blowball-scheduler .

# ---- Stage 1: build the blowball-scheduler binary ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 依赖单独成层，利用缓存。
COPY go.mod go.sum ./
RUN go mod download

# 拷源码并构建静态二进制（Scheduler 只依赖任务/调度组件与传输契约，纯 Go）。
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/blowball-scheduler ./services/scheduler/cmd/scheduler/

# ---- Stage 2: minimal runtime ----
FROM alpine:3.22.4

RUN echo "http://mirrors.aliyun.com/alpine/v3.22/main" > /etc/apk/repositories && \
    echo "http://mirrors.aliyun.com/alpine/v3.22/community" >> /etc/apk/repositories

# 运行期依赖：gRPC/TLS 证书链与时区。
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
        && echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai

COPY --from=builder /out/blowball-scheduler /usr/local/bin/blowball-scheduler

ENTRYPOINT ["blowball-scheduler"]
# -f/--config 由 `docker run` 传入；镜像本身不绑定配置。
