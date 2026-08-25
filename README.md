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
```

### 3. 启动 Agent

```bash
# Linux
./mocenter-agent -server your.server.com:9000 -agentid host-01
# 后台守护 + 崩溃自启
./mocenter-agent -server your.server.com:9000 -agentid host-01 -daemon

# Windows 交叉编译
bash agent/build.sh windows   # 生成 mocenter-agent.exe
```

### 4. 访问 Web

```bash
cd web
npm run dev     # 开发模式 (代理到 :8080)
# 或部署构建产物 web/dist 到 Nginx / 由 Go 托管
```

浏览器打开后，使用服务端启动时的 `-apikey` 作为登录 Token。

## 功能清单

- ✅ Agent 上线注册、心跳保活、离线判定（3 个心跳周期）
- ✅ 探针采集（CPU/内存/磁盘/网络流量/系统信息）与 Web 实时展示
- ✅ 远程命令执行（bash/sh/cmd/powershell）、超时控制、审计日志、命令黑名单
- ✅ WebSocket 实时推送主机列表与命令输出
- ✅ 端口映射规则管理、连接跟踪、超时清理（`server/internal/portmap`）
- ✅ 离线机器列表、操作审计界面
- ✅ 鉴权（Token）+ IP 白名单 + 速率限制
- ✅ Agent 断线指数退避重连、进程守护崩溃自启

> 说明：本项目当前为可运行的 MVP 骨架，核心链路（Agent 上线→探针→命令执行→结果回传→Web 展示）已端到端打通。生产级能力（mTLS 证书体系、文件传输断点续传、远程升级、批量命令、MFA 等）已按规划书完成模块化设计，可在此基础上继续增强。

## 许可证

内部项目。
