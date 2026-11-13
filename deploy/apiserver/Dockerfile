# blowball-api 独立薄镜像:api 角色固定,静态二进制 + 最小运行时。
# 不含 agent 执行依赖(bubblewrap/python/node/npm/git 等)——那些只属于
# deploy/agent/Dockerfile 的厚镜像。
# 构建命令(仓库根为 context):
#   docker build -f deploy/apiserver/Dockerfile -t blowball-api .

ARG GO_BASE_IMAGE=golang:1.26-alpine

# ---- Stage 1: build the blowball-api binary ----
FROM ${GO_BASE_IMAGE} AS builder

WORKDIR /src

# 依赖单独成层，利用缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 版本注入(未传时安全默认值;链接器忽略未声明变量)。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/lush/blowball/pkg/buildinfo.Version=${VERSION} \
        -X github.com/lush/blowball/pkg/buildinfo.Commit=${COMMIT} \
        -X github.com/lush/blowball/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/blowball-api ./services/apiserver/cmd/blowball-api/

# ---- Stage 2: minimal runtime ----
FROM alpine:3.22.4

RUN echo "http://mirrors.aliyun.com/alpine/v3.22/main" > /etc/apk/repositories && \
    echo "http://mirrors.aliyun.com/alpine/v3.22/community" >> /etc/apk/repositories

# 运行期依赖:TLS 证书链(office-vers/artifact 通道)、时区、curl(健康探针)。
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        curl \
        && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
        && echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai

COPY --from=builder /out/blowball-api /usr/local/bin/blowball-api

# 共享挂载点:宿主数据根(workspace 文件与 agent 服务共享同一卷)。
RUN mkdir -p /opt/cowork
VOLUME ["/opt/cowork"]

ENTRYPOINT ["blowball-api"]
# serve 子命令与 -f/-d 由 `docker run` 传入。
