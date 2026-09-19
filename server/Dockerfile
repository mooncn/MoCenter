# MoCenter Server 容器镜像
#
# 用法（在仓库根目录执行，需同时构建前端才有 Web 控制台）：
#   bash web/build.sh
#   docker build -f server/Dockerfile -t mocenter-server:latest .
#   docker run -d --name mocenter-server \
#     -p 9000:9000 -p 8080:8080 \
#     -v mocenter-data:/var/lib/mocenter \
#     -e MOCENTER_PASSWORD='强口令' \
#     mocenter-server:latest
#
# 说明：
#   - 构建上下文是**仓库根目录**（需要 web/dist），因此用 -f 指定本文件。
#   - 运行镜像同时提供 :9000（Agent 长连接）与 :8080（Web 控制台 / REST API）。
#   - 数据（审计日志、users.json、agent.key）落在 /var/lib/mocenter，请挂载持久卷，
#     否则容器重建会重新生成 agent.key，已注册的 Agent 将全部需要重新注册。

# ---------- 构建阶段 ----------
FROM golang:1.25 AS build
WORKDIR /src
# 先只拷 go.mod/go.sum，让依赖层可被缓存复用
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /out/mocenter-server .

# ---------- 运行阶段 ----------
FROM debian:bookworm-slim
# ca-certificates：公网 IP 探测 / 代理下载等出站 HTTPS 需要
# tzdata：日志与审计时间按本地时区显示
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /var/lib/mocenter
COPY --from=build /out/mocenter-server /usr/local/bin/mocenter-server

# Web 前端由服务端托管（构建上下文为仓库根目录；未构建时为空目录，不影响启动）
COPY web/dist/ /usr/share/mocenter/web/

# 数据卷：审计日志、users.json、agent.key、托管安装包
VOLUME ["/var/lib/mocenter"]

# Agent 长连接 / Web 控制台与 REST API
EXPOSE 9000 8080

# 启动参数可用环境变量覆盖：
#   MOCENTER_USER / MOCENTER_PASSWORD / MOCENTER_LISTEN / MOCENTER_HTTP / MOCENTER_WHITELIST
#   MOCENTER_TLS_CERT / MOCENTER_TLS_KEY / MOCENTER_CA_CERT
#   其它参数用 MOCENTER_EXTRA_ARGS 透传
ENV MOCENTER_LISTEN=":9000" \
    MOCENTER_HTTP=":8080" \
    MOCENTER_DATA="/var/lib/mocenter" \
    MOCENTER_CONFIG="/var/lib/mocenter/mocenter-server.json" \
    MOCENTER_WEB_DIR="/usr/share/mocenter/web" \
    MOCENTER_PKG_DIR="/var/lib/mocenter/packages"

# 首启动未提供 MOCENTER_USER / MOCENTER_PASSWORD 时，服务端会自动创建 admin
# 并生成随机强口令写入 /var/lib/mocenter/initial-password.txt（权限 0600）。
#
# 注意：ENTRYPOINT 必须是**单行** JSON 数组 —— Dockerfile 的 "\" 续行只对 shell
# 形式生效，JSON 形式里换行与续行符都属于非法 JSON，docker build 会直接报错。
# 因此这里把启动逻辑写成一个精简的单行 sh -c。
ENTRYPOINT ["/bin/sh", "-c", "exec /usr/local/bin/mocenter-server -listen \"$MOCENTER_LISTEN\" -http \"$MOCENTER_HTTP\" -data \"$MOCENTER_DATA\" -config \"$MOCENTER_CONFIG\" -pkg-dir \"$MOCENTER_PKG_DIR\" -web \"$MOCENTER_WEB_DIR\" -user \"$MOCENTER_USER\" -password \"$MOCENTER_PASSWORD\" -whitelist \"$MOCENTER_WHITELIST\" -tls-cert \"$MOCENTER_TLS_CERT\" -tls-key \"$MOCENTER_TLS_KEY\" -ca-cert \"$MOCENTER_CA_CERT\" $MOCENTER_EXTRA_ARGS"]
