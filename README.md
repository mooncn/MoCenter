# MoCenter · 集中式主机远控管理系统

基于 [项目规划书](docs/项目规划书.md) 实现的一套集中式主机远控管理系统。统一管理跨网络环境的 **Linux** 与 **Windows** 主机，通过 **Web 界面**实现远程命令执行与运行状态监控。

## 系统架构

采用 **C/S 架构**，由三个独立部分构成（对应仓库 `web` / `server` / `agent` 三个目录）：

```
                    ┌─────────────────────────┐
   Web 前端 ──WSS──▶│      总控服务端          │
   (React/xterm)    │   Server (Go)          │◀── TCP 长连接(全链路TLS/mTLS)
                    │   · 连接管理            │
                    │   · 命令路由/交互        │      ┌──────────────┐
                    │   · 探针聚合            │──────▶│  Agent (Go)  │ Linux
                    │   · 端口映射(反向代理)   │      │  · 心跳保活   │ Windows
                    │   · 鉴权/审计/黑名单     │      │  · 探针采集   │
                    └─────────────────────────┘      │  · 命令执行   │ Docker
                    WebSocket 实时推送               │  · 断线重连   │
                                                      └──────────────┘
```

- **Agent 客户端**（`agent/`）：部署在各内网主机，主动连接公网服务端维持 TCP 长连接；心跳保活、探针采集、命令执行、断线指数退避重连、进程守护崩溃自启。
- **总控服务端**（`server/`）：维护在线会话池，命令路由、交互转发、端口映射反向代理、探针聚合、鉴权/审计/命令黑名单、REST API + WebSocket。
- **Web 前端**（`web/`）：主机探针面板、远程终端（xterm.js）、端口映射管理、离线机器、操作审计。

## 技术选型

| 模块 | 技术 | 说明 |
|------|------|------|
| 后端 | Go 1.22 | 高并发网络通信 |
| 前端 | React 18 + Vite 5 + xterm.js 5 | 组件化 + 标准终端模拟 |
| 通信 | TCP 长连接 + WebSocket | Agent<->Server 用 TCP，Web<->Server 用 WebSocket |
| 协议 | JSON（可迁移 Protobuf） | 协议定义见 `proto/` |
| 鉴权 | Token + IP 白名单 + 速率限制 | `server/internal/auth` |

## 目录结构

```
MoCenter/
├── agent/                 # Agent 客户端 (Go)
│   ├── main.go
│   └── internal/
│       ├── client/        # 连接管理、心跳、断线重连
│       ├── probe/         # 探针采集
│       ├── command/       # 命令执行
│       ├── daemon/        # 进程守护/崩溃自启
│       └── protocol/      # 消息协议
├── server/                # 总控服务端 (Go)
│   ├── main.go
│   └── internal/
│       ├── session/       # 会话池
│       ├── route/         # 命令路由/审计/黑名单
│       ├── probe/         # 探针聚合
│       ├── portmap/       # 端口映射反向代理
│       ├── ws/            # WebSocket Hub
│       ├── auth/          # 鉴权/白名单/限流
│       └── protocol/      # 消息协议
├── web/                   # Web 前端 (React)
│   └── src/
│       ├── api/           # REST + WebSocket 客户端
│       ├── pages/         # 各页面
│       └── App.jsx
├── proto/                 # Protobuf 协议定义
├── scripts/               # 一键构建脚本
│   ├── build-all.sh
│   └── build-all.bat
└── docs/                  # 部署与运维手册
```

## 快速开始

### 1. 构建

一键构建三大部分（需安装 Go 1.22+ 与 Node.js 18+）：

```bash
# Linux/macOS
bash scripts/build-all.sh

# Windows
scripts\build-all.bat
```

各子目录也提供独立构建脚本（`server/build.sh`、`agent/build.sh`、`web/build.sh`，及对应 `.bat`）。

### 2. 启动服务端

```bash
cd server
./mocenter-server -listen :9000 -http :8080 -apikey your-secret-key
# 可选: -whitelist 1.2.3.4,10.0.0.0/8   # IP 白名单
# 可选: -web web/dist                  # 同时托管 Web 前端（单进程部署）
# 可选: -tls-cert server.crt -tls-key server.key   # Agent 连接启用 TLS 加密
# 可选: -ca-cert ca.crt                            # 启用 mTLS 双向认证（Agent 须携带客户端证书）
```

### 3. 启动 Agent

```bash
# Linux
./mocenter-agent -server your.server.com:9000 -agentid host-01
# 后台守护 + 崩溃自启
./mocenter-agent -server your.server.com:9000 -agentid host-01 -daemon
# 启用 TLS 加密连接（自签名证书用 -insecure 跳过校验）
./mocenter-agent -server your.server.com:9000 -agentid host-01 -tls -insecure
# 启用 mTLS 双向认证：携带客户端证书，并用 CA 校验服务端证书
./mocenter-agent -server your.server.com:9000 -agentid host-01 -tls \
  -client-cert client.crt -client-key client.key -ca-cert ca.crt

# Windows 交叉编译
bash agent/build.sh windows   # 生成 mocenter-agent.exe
```

### 4. 生成 TLS/mTLS 证书（可选）

```bash
bash scripts/gen-cert.sh ./certs   # 生成 ca / server / client 三套证书
```

### 5. 访问 Web

```bash
cd web
npm run dev     # 开发模式 (代理到 :8080)
# 或部署构建产物 web/dist 到 Nginx / 由 Go 托管
```

浏览器打开后，使用服务端启动时的 `-apikey` 作为登录 Token。

## 登录认证

系统**默认没有内置的账号密码**。登录后台有两种方式：

### 方式一：使用 API Key（默认，推荐）
- 在登录页顶部的 **「访问 Token / API Key」** 输入框中，填写服务端启动时的 `-apikey` 值。
- 服务端未通过 `-apikey` 指定时，默认值为 `change-me-api-key`。
- 在后端程序目录的 `mocenter-server.json` 配置文件中，字段 `apikey` 也即该值。

> 例如：启动服务端执行了 `mocenter-server -apikey MySecretKey`，登录时在输入框填 `MySecretKey` 即可。

### 方式二：用户名 + 密码（可选，需服务端启用）
服务端可通过启动参数（或写入配置）启用账号登录，并可选启用 OTP 二次校验：

```bash
# 仅账号（用户名+密码）
mocenter-server -user admin -password your-password

# 账号 + OTP（需提供 base32 密钥，可用任意 TOTP 生成器生成，兼容 Google Authenticator）
mocenter-server -user admin -password your-password -totp-secret JBSWY3DPEHPK3PXP
```

启用后，登录页填写 **用户名、密码**（及 OTP 动态验证码，若启用了）即可登录；此时“访问 Token”可留空。
对应配置字段：`user` / `password` / `totp_secret`（`mocenter-server.json`）。

> ⚠️ 安全说明：**密码不会明文落盘**。首次通过 `-user/-password` 创建账号时，密码会以 SHA-256 哈希持久化到服务端数据目录的 **`users.json`**；`mocenter-server.json` 配置文件**不保存**明文密码与 TOTP 密钥。
> 创建完成后，即可在 Web 端侧边栏 **「账号设置」** 页面修改密码（无需再在命令行用明文参数），修改同样只写哈希到 `users.json`。

### 修改密码
1. 登录后点击左侧 **「账号设置」**。
2. 填写 用户名、当前密码、两次新密码（至少 6 位；若该账号启用了 OTP，需一并填写动态验证码）。
3. 提交成功后，下次登录使用新密码。

### 说明
- 未配置 `-user` 时后端 `/api/login` 返回 501，登录页仅需填写 API Key（Token）。
- Token 为空时才走账号登录；两种方式互不冲突。

## 功能清单

- ✅ Agent 上线注册、心跳保活、离线判定（3 个心跳周期）
- ✅ 探针采集（CPU/内存/磁盘/网络流量/系统信息）与 Web 实时展示 + 历史趋势图
- ✅ **主机备注**：仪表盘可为主机设置自定义备注，有备注时优先显示备注、小字显示主机名
- ✅ **公网 IP 展示**：仪表盘展示主机公网出口 IPv4/IPv6，并按 IP 解析地理位置与国家图标
- ✅ 远程命令执行（bash/sh/cmd/powershell）、超时控制、审计日志、命令黑名单
- ✅ **交互式 PTY 终端**（基于 creack/pty，实时双向 IO、尺寸自适应、Ctrl-C）
- ✅ WebSocket 实时推送主机列表、命令输出、交互输出、文件传输状态、升级结果
- ✅ **端口映射反向代理**：规则持久化、连接跟踪、流量统计、超时清理，公网端口实时转发
- ✅ **文件传输断点续传**：分片收发、MD5 校验、ACK/NACK、断点续传、合并落盘
- ✅ **文件分发到主机**：Web 上传文件 → 服务端中转 → Agent 指定路径落盘（分片 + MD5 校验）
- ✅ **批量命令**：一次下发到多台在线主机，审计留痕
- ✅ **远程升级 Agent**：下载升级包 → MD5 校验 → 备份 → 替换 → 自动重启，失败回滚
- ✅ 离线机器列表、操作审计界面
- ✅ 鉴权（Token）+ IP 白名单 + 速率限制
- ✅ Agent 断线指数退避重连、进程守护崩溃自启
- ✅ **TLS/mTLS 加密**：Agent<->Server 可选全链路 TLS 单向加密，或 mTLS 双向认证（客户端证书 + CA 校验）
- ✅ **服务端托管 Web 静态资源**：`-web web/dist` 单进程部署
- ✅ **远程桌面 (VNC)**：服务端隧道 + noVNC 浏览器渲染，一键安装/启动（Windows TightVNC/TigerVNC、Linux x11vnc，支持国内加速代理）
- ✅ **多架构打包**：Windows x86/x86_64/ARM/ARM64、Linux amd64/arm64/arm/386 二进制，及 .deb/.rpm 安装包
- ✅ **系统服务注册**：Agent 一键注册为 Windows 服务 / Linux systemd 服务，开机自启

> 说明：核心链路（Agent 上线→探针→命令执行→结果回传→Web 展示）已端到端打通，并在此基础上实现了交互式终端、文件断点续传、端口映射、批量命令、远程升级、TLS 加密等生产级能力。

## Agent 安装包与系统服务

### 多架构二进制构建

```bash
# Linux 多架构（amd64/arm64/arm/386）
bash scripts/packaging/build-linux-all.sh

# Windows 多架构（x86_64/x86/arm64/arm）
# 在 Windows 上执行:
scripts\packaging\build-win-all.bat
```

### Linux .deb/.rpm 安装包（含 systemd 服务）

```bash
# 一键生成 .deb 与 .rpm（默认 amd64）
bash scripts/packaging/build-linux-pkg.sh all amd64

# 单独生成 .deb / .rpm
bash scripts/packaging/build-linux-pkg.sh deb arm64
bash scripts/packaging/build-linux-pkg.sh rpm amd64
```

安装包内置 systemd 服务单元，安装后自动：
- 注册 `mocenter-agent.service` 服务
- 设置开机自启并启动
- 配置文件位于 `/etc/mocenter/mocenter-agent.conf`（请修改服务端地址）

> 依赖 `dpkg-deb`（Debian/Ubuntu）或 `fpm`；也可直接使用一键脚本：
> `sudo bash scripts/packaging/install-linux.sh install <server>`

### Windows 系统服务（开机自启）

将对应架构的 `mocenter-agent-*.exe` 命名为 `mocenter-agent.exe` 与 `install-windows.bat` 放在同目录，然后以**管理员**运行：

```bat
install-windows.bat install your.server.com:9000 [agentid] [proxy]
```

脚本会：
- 复制二进制到 `%ProgramFiles%\MoCenter`
- 注册 Windows 服务 `MoCenterAgent`（`sc create`）
- 设置开机自启并启动

### 国内加速代理

Agent 支持 `-proxy-ip http://fa.mooncn.win/?url=` 参数（或配置文件 `MOCENTER_PROXY_IP`），
用于国内网络环境下加速：公网 IP 探测、VNC 安装包下载等境外服务。

## 许可证

内部项目。
