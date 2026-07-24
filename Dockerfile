

# 一个镜像同时承载 api 与 agent 两个角色，运行时用 `serve --role <api|agent>` 区分。

# ---- Stage 1: build the blowball binary (serve + seed 子命令共用同一个二进制) ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 依赖单独成层，利用缓存。
COPY go.mod go.sum ./
RUN go mod download

# 拷源码并构建静态二进制。
#   CGO_ENABLED=0  纯 Go 静态构建（mysql/redis 驱动都是纯 Go），
#                  在 musl 的 alpine 运行时镜像上跑得干净，也利于 Landlock/bwrap。
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blowball ./cmd/blowball/

# ---- Stage 2: minimal runtime ----
FROM alpine:3.22.4

# 运行期依赖：
#   ca-certificates   OpenAI / MCP / webfetch / luban 的 git clone 走 HTTPS
#   tzdata            日志时间戳正确
#   git               luban_install_skill 运行时 `git clone`
#   python3, py3-pip  python / pip 执行器工具
#   bubblewrap        执行器工具的 bwrap 沙箱（仅 agent 角色使用）
#   nodejs, npm       沙箱内经 bash 调用 node/npm（apk 源里的版本）
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        git \
        python3 \
        py3-pip \
        bubblewrap \
        nodejs \
        npm

COPY --from=builder /out/blowball /usr/local/bin/blowball

# 共享挂载点：宿主 /opt/cowork（含 config.yaml 与 meta-data/ 数据根）。
RUN mkdir -p /opt/cowork
VOLUME ["/opt/cowork"]

ENTRYPOINT ["blowball"]
# 角色与 -f/-d 由 `docker run` 传入，镜像本身不绑定角色。
