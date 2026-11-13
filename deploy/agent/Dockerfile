

# blowball-agent 独立厚镜像:agent 角色固定,承载全部 agent 执行依赖
# (bubblewrap 沙箱、python/pip、node/npm、git、rg/fd、bash)。镜像同时构建
# 开发单体 `blowball`(serve --role all|api|agent)作为回滚/过渡路径——
# 厚镜像本就具备单体所需的一切运行时依赖,生产可继续以本镜像跑单体,
# 切换到独立二进制只改入口。
# 构建命令(仓库根为 context):
#   docker build -f deploy/agent/Dockerfile -t blowball-agent .

ARG GO_BASE_IMAGE=golang:1.26-alpine

# ---- Stage 1: build the binaries ----
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
        -X github.com/lush/blowball/pkg/boot.Version=${VERSION} \
        -X github.com/lush/blowball/pkg/boot.Commit=${COMMIT} \
        -X github.com/lush/blowball/pkg/boot.BuildTime=${BUILD_TIME}" \
      -o /out/blowball-agent ./services/agent/cmd/blowball-agent/ && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/blowball ./cmd/blowball/

# ---- Stage 2: full agent runtime ----
FROM alpine:3.22.4

RUN echo "http://mirrors.aliyun.com/alpine/v3.22/main" > /etc/apk/repositories && \
    echo "http://mirrors.aliyun.com/alpine/v3.22/community" >> /etc/apk/repositories

# 运行期依赖：
#   ca-certificates   OpenAI / MCP / webfetch / luban 的 git clone 走 HTTPS
#   tzdata            日志时间戳正确
#   curl              健康探针
#   git               luban_install_skill 运行时 `git clone`
#   bash              bash 执行器工具与沙箱默认 shell（Alpine 出厂仅 busybox ash）
#   python3, py3-pip  python / pip 执行器工具
#   bubblewrap        执行器工具的 bwrap 沙箱（仅 agent 角色使用）
#   nodejs, npm       沙箱内经 bash 调用 node/npm（apk 源里的版本）
RUN apk add --no-cache \
        curl \
        ca-certificates \
        tzdata \
        git \
        bash \
        python3 \
        py3-pip \
        bubblewrap \
        nodejs \
        npm \
        fd \
        ripgrep \
        && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
        && echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai

COPY --from=builder /out/blowball-agent /usr/local/bin/blowball-agent
# 开发单体(回滚路径):docker run --entrypoint blowball ... serve --role all
COPY --from=builder /out/blowball /usr/local/bin/blowball

# 共享挂载点：宿主 /opt/cowork（含 config.yaml 与 meta-data/ 数据根,
# 与 apiserver 服务共享同一卷——workspace 文件双向可见）。
RUN mkdir -p /opt/cowork
VOLUME ["/opt/cowork"]

ENTRYPOINT ["blowball-agent"]
# serve 子命令与 -f/-d 由 `docker run` 传入。
