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
  **首页（`/`）即公开状态页**（顶部悬浮汇总条 + 小巧卡片 / 完整列表双视图，含地区国旗、
  单节点与站点级网络流量、系统负载、历史趋势；卡片与列表的每一项都受后台「展示配置」
  逐项开关控制，列表可按非动态字段排序）；**管理后台整体在 `/admin/*`** ——
  `/admin` 为后台仪表盘，登录页为 `/admin/login`，登录成功后落地到 `/admin`；
  根级旧路径（`/terminal`、`/audit` …）自动跳转到对应 `/admin` 地址。

## 技术选型

| 模块 | 技术 | 说明 |
|------|------|------|
| 后端 | Go 1.25 | 高并发网络通信（`server` 最低 1.22，`agent`/`installer` 的 `go.mod` 要求 1.25） |
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
│   ├── Dockerfile         # 容器镜像（多阶段构建，单进程托管 Web）
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
├── installer/             # Windows 图形安装程序（Inno Setup 工程）
│   ├── setup.iss          # 向导定义（角色 / 参数 / 服务注册）
│   ├── messages-cn.iss    # 向导中文文案覆盖
│   ├── main.go            # mocenter-deploy.exe：命令行安装引擎
│   └── payload/           # 打包输入：三个二进制 + web 前端
├── scripts/               # 构建 / 打包 / 部署脚本
│   ├── build-linux-all.sh          # 统一构建/打包入口（Linux/macOS 构建机）
│   ├── build-all.bat               # 统一构建入口（Windows 构建机）
│   ├── build-windows-installer.bat # Windows 图形安装程序（Inno Setup）
│   ├── lib/common.sh               # 脚本公共库（参数、版本、架构、打包工具）
│   ├── deploy/                     # 部署期脚本 + systemd/配置模板（构建与安装共用）
│   ├── gen-cert.sh                 # 生成 TLS/mTLS 证书（需 openssl）
│   ├── fetch-packages.sh           # 拉取 VNC 安装包到服务端托管目录
│   ├── mcmd/                       # 远程命令调试工具（REST + WebSocket）
│   ├── gencerts/                   # 证书生成的 Go 实现（Windows 无需 openssl）
│   └── signcert/                   # 按实际地址（默认公网 IP）签发服务端证书
└── docs/                  # 部署与运维手册
```

## 快速开始

### 1. 构建

**构建只需要记两个文件**：`scripts/build-linux-all.sh`（Linux/macOS 构建机）与
`scripts/build-all.bat`（Windows 构建机）。参数同名同义，产物统一落到 `build/`。
需 **Go 1.25+**，Web 需 **Node.js 18+**。

```bash
# 默认：全部类型 + 全部平台 + 全部架构 + 全部安装包
bash scripts/build-linux-all.sh

# 常用组合
bash scripts/build-linux-all.sh --type=server --package=deb --arch=amd64,arm64
bash scripts/build-linux-all.sh --type=agent  --package=all  --arch=amd64
bash scripts/build-linux-all.sh --type=agent,server --platform=windows --package=zip
bash scripts/build-linux-all.sh --outdir=/tmp/mocenter-build      # 自定义输出根目录
bash scripts/build-linux-all.sh --list                            # 查看全部可选值
```

```bat
REM Windows 构建机（只构建 Windows 产物，不交叉编译 Linux）
scripts\build-all.bat                                            REM 全部
scripts\build-all.bat --type=server --arch=amd64
scripts\build-all.bat --type=agent --package=zip --arch=amd64,386
scripts\build-all.bat --type=installer --web=1                    REM 图形安装程序
scripts\build-all.bat --outdir=D:\mocenter-build
```

**参数（两个脚本一致）**

| 参数 | 取值 | 默认 | 说明 |
|------|------|------|------|
| `--type=` | `agent`\|`server`\|`web`\|`installer`\|`none`\|`all` | `all` | 构建哪些角色，可逗号组合 |
| `--package=` | `deb`\|`rpm`\|`zip`\|`none`\|`all` | `all` | 生成哪些安装包；按平台自动过滤（`deb`/`rpm`→Linux，`zip`→Windows） |
| `--platform=` | `linux`\|`windows`\|`all` | `all`（Linux 脚本） | 产出哪些平台的产物 |
| `--arch=` | `amd64`\|`386`\|`arm64`\|`arm`\|`all` | `all` | 目标架构，可逗号组合（`arm` 仅 Linux；`windows/arm` 不是 Go 支持的目标，见下） |
| `--binary=` | `1`\|`0` | `1` | 是否输出二进制（`0` 表示只打包，复用已有二进制） |
| `--web=` | `1`\|`0`\|`auto` | `auto` | `auto` = 已有 `web/dist` 就复用，否则构建 |
| `--outdir=` | 目录 | `<项目>/build` | 输出根目录 |
| `--version=` / `--agent-version=` | 版本号 | 读源码 | 服务端 / Agent 版本（两者互相独立） |
| `--clean` | — | 关 | 构建前清空 outdir（默认只清本次涉及的子目录） |
| `--list` / `--help` | — | — | 列出可选值 / 显示帮助 |

**产物布局**（先按平台分类，再按用途分类）

```
build/
├── bin/                                     # Go 二进制（按角色分目录，平台在文件名/下一层）
│   ├── server/{linux,windows}/mocenter-server-<arch>[.exe]
│   ├── agent/{linux,windows}/mocenter-agent-<arch>[.exe]
│   └── deploy/windows/mocenter-deploy.exe   # Windows 安装引擎（图形安装程序的素材）
├── web/                                     # 前端静态资源（两平台共用同一份）
├── packages/
│   ├── deb/                                 # .deb 安装包（内置 systemd）
│   ├── rpm/                                 # .rpm 安装包（内置 systemd）
│   └── zip/mocenter-<role>-<ver>-windows-<arch>.zip   # Windows 免安装包（含服务注册脚本）
└── setup/MoCenter-Setup-<版本>.exe           # 图形安装程序（需 Windows + ISCC）
```

> **只按用途分类一次**：平台信息体现在文件名（`…-windows-amd64.zip`）与 `bin/<角色>/<平台>/`
> 这一层，不再像早期那样 `build/<平台>/binaries/<角色>/<平台>/…` 一层层套 —— 那种布局里
> `linux` 在单个路径中出现过三次，而前端本就两平台相同，却被复制成两份。

> 需要 `--type=web` 或 `--package=deb/rpm`（内嵌前端）时会**强制 `--web=1`**，不会静默产出"没有界面"的包。
> 子目录仍有独立脚本（`server/build.sh`、`agent/build.sh`、`web/build.sh`，及对应 `.bat`），
> 用于单模块调试；**全部脚本的用法、参数与产物位置见 [构建脚本参考](#构建脚本参考)。**

#### 容器化部署

仓库自带 `server/Dockerfile`（多阶段构建，单进程同时提供 :9000 与 :8080）：

```bash
bash web/build.sh                                          # 可选：先出前端，镜像才能托管页面
docker build -f server/Dockerfile -t mocenter-server:latest .   # 上下文为仓库根目录
docker run -d --name mocenter-server -p 9000:9000 -p 8080:8080 \
  -v mocenter-data:/var/lib/mocenter \
  -e MOCENTER_USER=admin -e MOCENTER_PASSWORD='强口令' \
  mocenter-server:latest
```

> 数据请挂载持久卷：容器重建会重新生成 `agent.key`，已注册的 Agent 需改用新密钥重新注册。

### 2. 启动服务端

```bash
cd server
# 口令走 0600 文件（推荐）：避免出现在 ps / systemctl show 里
printf '%s' 'your-password' > /etc/mocenter/initial-password && chmod 600 /etc/mocenter/initial-password
./mocenter-server -listen :9000 -http :8080 -user admin -password-file /etc/mocenter/initial-password
# 可选: -whitelist 1.2.3.4,10.0.0.0/8   # IP 白名单
# 可选: -web web/dist                  # 同时托管 Web 前端（单进程部署）
# 可选: -require-agent-cert            # 强制 Agent 携带客户端证书（默认允许"先换取"）
# 可选: -admin-tls-days 30             # Web 控制台自动 HTTPS 证书有效期；0 = 关闭（走 TLS 反代时用）
# 可选: -server-name prod-01            # 部署身份名，区分多套总控
```

> **开箱即用的加密**：启动时会自动生成部署身份 CA（`<data>/ca.crt` + `ca.key`，0600）：
> - **Agent 通道**：自动签发服务端证书，"开箱即 TLS"；
> - **Web 控制台**：自动签发当前部署临时证书，**默认 HTTPS/WSS**（浏览器会提示自签名，
>   接受即可；要受信任证书就配 `-web-tls-cert/-web-tls-key`，或 `-admin-tls-days=0` 交给 TLS 反代）。

### 3. 启动 Agent

```bash
# Linux：只需要服务端地址 + 注册密钥（<data>/agent.key 的内容），无需任何证书文件
./mocenter-agent -server your.server.com:9000 -agentid host-01 -agent-key <注册密钥>
# 后台守护 + 崩溃自启
./mocenter-agent -server your.server.com:9000 -agentid host-01 -agent-key <注册密钥> -daemon

# Windows 交叉编译
bash agent/build.sh windows   # 生成 mocenter-agent.exe
```

首次启动会自动用注册密钥向服务端**换取客户端证书**，之后每次连接都携带证书（mTLS），
证书落在 `<配置目录>/creds/`（0600），配置里的 `enrollment_secret` 随即被清空。

> **手工证书方式仍可用**（外部 CA / 正式证书 / 预置审计场景）：
> ```bash
> ./mocenter-server ... -tls-cert server.crt -tls-key server.key -ca-cert ca.crt
> ./mocenter-agent ... -client-cert client.crt -client-key client.key -ca-cert ca.crt
> ```
> 还可以用**公钥指纹**免掉 CA 文件、也不再受服务端换 IP/证书影响：
> `./mocenter-agent ... -server-fingerprint <服务端指纹>`（服务端启动日志与 Web 会显示）。

> **设备身份说明**：系统以**硬件 UUID** 作为一台机器的唯一身份，`agent_id` 只是
> 可变的名字（显示与路由用）。首次启动会自动探测 UUID 并写入配置（`uuid` 字段），
> 因此**修改 `agent_id`、重装或换配置目录都不会让同一台机器变成两台**，不会产生冗余数据。
> 也可显式指定：`-agentid host-01 -uuid <硬件UUID>`。

### 4. 生成 TLS/mTLS 证书（一般不需要）

服务端与 Agent 默认已自动完成证书签发与换取，**不需要手工生成**。
仅在需要外部 CA / 正式证书 / 预置固定证书时使用：

```bash
bash scripts/gen-cert.sh ./certs   # 生成 ca / server / client 三套证书
# 或用 Go 版（Windows 无需 openssl，可按真实 IP/域名写 SAN、并把路径写进两侧配置）
cd scripts/gencerts && go run . all -ip <服务端IP> -dns <域名> \
  -server-config ../../mocenter-server.json -agent-config ../../mocenter-agent.json
# 只重签服务端证书（换过公网 IP / 换域名后）：默认自动按本机公网 IP 写入 SAN
cd scripts/signcert && go run .
```

> **服务端证书的 SAN 必须包含 Agent 连接时使用的地址。** 少一个地址，Agent 用 `-ca-cert`
> 校验时就会报 `x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>`。
> 服务器公网 IP 往往部署后才知道，所以两个工具都支持**自动探测公网 IP 写入 SAN**；
> 服务端自动签发证书时也会把本机地址 + 探测到的公网 IP 一并写入（启动日志会打印 SAN）。
> 用 `-server-fingerprint` 校验部署身份指纹时不受 SAN 影响（推荐路径）。

> 想让服务端复用这套 CA（例如要与既有 Agent 共用同一份信任根）：
> 把 `ca.crt` 与 `ca.key` 放进服务端数据目录，服务端会直接沿用（日志会打印身份名与指纹）。

### 5. 访问 Web

```bash
cd web
npm run dev     # 开发模式 (代理到 :8080)
# 或部署构建产物 web/dist 到 Nginx / 由 Go 托管
```

浏览器打开后，用服务端配置的**用户名 + 密码**登录（首次启动未指定 `-user`/`-password` 时
会自动创建账号 `admin` 并生成**随机强口令**，写入 `<数据目录>/initial-password.txt`，
权限 0600；请查看后立即登录修改并删除该文件）。

## 构建脚本参考

> 所有脚本均以**仓库根目录**为基准定位文件，可在任意当前目录下调用。

### 环境要求

| 工具 | 版本 | 用于 |
|------|------|------|
| Go | **1.25+** | 编译 `server` / `agent` / `installer`（`server` 最低 1.22） |
| Node.js + npm | 18+ | 构建 Web 前端 |
| [Inno Setup 6](https://jrsoftware.org/isdl.php) | 6.x | **仅**编译 Windows 图形安装程序 |
| OpenSSL | — | **仅**生成 TLS/mTLS 证书 |
| `dpkg-deb` 或 `fpm` | — | **仅**生成 Linux `.deb`（Agent 与 Server 均适用） |
| `fpm` 或 `rpmbuild`+`file` | — | **仅**生成 Linux `.rpm`（走 `rpmbuild` 分支时 `file(1)` 为必需项） |

> Go / npm 缺失时，一键脚本会打印 `!! ... 跳过 ...` 并**继续构建其余部分**（不中断）。
> 因此看到跳过提示不等于构建失败，请以最终产物为准。

### 版本号速查

三套版本号**互相独立**，不需要保持一致，也不存在跨端一致性校验：

| 版本号 | 定义位置 | 作用 | 覆盖方式 |
|--------|----------|------|----------|
| Server 版本 | `server/internal/version/version.go` | 服务端自身版本（/api/version、备份清单口径） | 改常量（仅发版时） |
| **Agent 版本** | `agent/internal/version/version.go` | **仅**标记 Agent 功能迭代代际 | 改常量，或构建时 `MOCENTER_AGENT_VERSION` |
| 安装包版本 | `installer\setup.iss` 的 `AppVersion`、`MOCENTER_VERSION` | 安装包文件名与注册表版本 | 环境变量 |

```bat
REM 只影响 Agent 二进制内嵌的版本号，不影响服务端
set MOCENTER_AGENT_VERSION=1.2.0
scripts\build-all.bat
```

### 构建报错 `zip: not a valid zip file`

Go 模块缓存（`go env GOMODCACHE`）里存在**被截断或写坏的模块压缩包**时，
Go 会报 `zip: not a valid zip file`，随后**所有**从该模块解析进来的包都会编译失败
（例如 `golang.org/x/sys`、`winapp_windows.go`）—— 看起来像源码坏了，实际只是本地下载产物损坏。

`scripts\build-all.bat` 已内置自愈：先正常 `go mod download` + `go build`，
失败就 `go clean -modcache` 后切到镜像代理（`https://goproxy.cn`）重下一次再重试。
手工处理等价于：

```bat
go clean -modcache
set GOPROXY=https://goproxy.cn,direct
scripts\build-all.bat
```

> 也可以直接删缓存目录后重跑：`rmdir /s /q "%USERPROFILE%\go\pkg\mod"`。
> 脚本**不会**设置 `GOFLAGS=-mod=mod`：第三方代码必须继续以 `go.sum` 为准。

### 脚本总览

| 脚本 | 平台 | 作用 | 产物 |
|------|------|------|------|
| `scripts/build-linux-all.sh` | Linux/macOS | **统一入口**：Server/Agent/Web/安装包，全参数可定制 | `build/` |
| `scripts/build-all.bat` | Windows | **统一入口**：Server/Agent/Web/图形安装程序 | `build\` |
| `scripts/build-windows-installer.bat` | Windows | 编译图形安装程序（父级为 `build-all.bat --type=installer`） | `build\setup\MoCenter-Setup-<版本>.exe` |
| `scripts/lib/common.sh` | — | 脚本公共库（被两个入口加载） | — |
| `scripts/deploy/mocenter-*.service` | — | systemd 单元模板（打包与安装共用） | — |
| `scripts/deploy/install-linux.sh` | Linux | 安装/升级/卸载 Agent·Server（systemd 服务 + 保活定时器；无 systemd 时回退 cron） | — |
| `scripts/deploy/install-windows.bat` | Windows | 注册/卸载 Windows 服务（免安装包内自带） | — |
| `scripts/deploy/mocenter-keepalive.sh` | Linux | 保活巡检（systemd timer / cron 共用；进程不在则拉起） | — |
| `scripts/deploy/precompress-web.sh` | Linux/macOS | 前端产物预压缩 `.br`/`.gz`（服务端优先回传，弱网/移动端首屏更快） | `web/dist/` |
| `agent/build.sh` · `agent/build.bat` | 跨平台 | 单独构建 Agent（模块内调试用） | `agent/mocenter-agent[.exe]` |
| `server/build.sh` · `server/build.bat` | 跨平台 | 单独构建 Server（模块内调试用） | `server/mocenter-server[.exe]` |
| `web/build.sh` · `web/build.bat` | 跨平台 | 单独构建前端 | `web/dist/` |
| `scripts/gen-cert.sh` | Linux/macOS | 生成 CA / 服务端 / 客户端证书（需 openssl） | 指定目录（默认 `certs/`） |
| `scripts/gencerts/` | 跨平台 | 证书生成的 Go 实现（**Windows 无需 openssl**，服务端证书默认按公网 IP 签发） | 指定目录 |
| `scripts/signcert/` | 跨平台 | **按实际地址重签服务端证书**（默认自动探测公网 IP 写入 SAN） | 指定目录（默认 `certs/`） |
| `scripts/fetch-packages.sh` | Linux/macOS | 拉取 VNC 安装包到服务端托管目录 | 指定目录 |
| `scripts/mcmd/` | 跨平台 | 远程命令调试（REST + WebSocket） | — |
| `server/Dockerfile` | Docker | 构建 Server 容器镜像 | `mocenter-server:latest` |

### 一键构建

```bat
REM Windows 构建机
scripts\build-all.bat                                 REM 全部：Server + Agent + Web + 图形安装程序
scripts\build-all.bat --type=agent --arch=amd64,386   REM 只要 Agent 两个架构
scripts\build-all.bat --type=server --package=none    REM 只要服务端二进制
scripts\build-all.bat --type=installer --web=1        REM 只要图形安装程序（并强制构建前端）
```

```bash
# Linux/macOS 构建机
bash scripts/build-linux-all.sh                        # 全部
bash scripts/build-linux-all.sh --type=server --package=deb --arch=amd64
bash scripts/build-linux-all.sh --type=agent,server --platform=windows --package=zip
bash scripts/build-linux-all.sh --package=none         # 只要二进制
```

参数、默认值与产物布局见上文[「1. 构建」](#1-构建)的表格。
`--type=none` / `--package=none` / `--binary=0` 可组合出"只打包"或"只构建"等场景。
`--clean` 会先清空 outdir；默认只清理本次涉及的子目录，所以 `--type=agent` 不会误删上一次的 server 产物。

> **Windows 脚本不交叉编译 Linux 产物**（.deb/.rpm/Linux 二进制）——这是刻意的：
> .deb 需要 `dpkg-deb`、.rpm 需要 `fpm`/`rpmbuild`，且 Linux 侧才是发布目标环境。
> 在 Windows 上要到 Linux 产物，请在 Linux/macOS 构建机（或 CI）上执行 `build-linux-all.sh`。

### 单独构建某个模块

```bash
bash agent/build.sh [linux|windows|darwin] [amd64|386|arm64|arm]   # 默认当前平台 / amd64
bash server/build.sh [linux|darwin|windows] [amd64|arm64|arm|386]  # 默认当前平台 / 本机架构
bash web/build.sh
```

```bat
REM Windows：参数为架构，默认 amd64
agent\build.bat [386|amd64|arm|arm64]
server\build.bat [386|amd64|arm|arm64]
web\build.bat
```

Server 依赖 `modernc.org/sqlite`（纯 Go），因此**无需 CGO 即可交叉编译**到 arm64 / arm 等架构。

输出分别是 `agent/mocenter-agent[.exe]`、`server/mocenter-server[.exe]`、`web/dist/`（注意：**落在模块目录内**，
与统一脚本输出到 `build/` 不同）。

### 多架构二进制与安装包

```bash
# Agent 多架构：amd64 / arm64 / arm / 386
bash scripts/build-linux-all.sh --type=agent --package=none
# -> build/bin/agent/linux/mocenter-agent-<arch>

# Server 多架构
bash scripts/build-linux-all.sh --type=server --package=none
# -> build/bin/server/linux/mocenter-server-<arch>

# .deb / .rpm（内置 systemd 单元，安装即注册服务、开机自启）
bash scripts/build-linux-all.sh --type=agent,server --package=all --arch=amd64,arm64
# -> build/packages/{deb,rpm}/

# Windows 多架构 exe + 免安装 zip
bash scripts/build-linux-all.sh --type=agent,server --platform=windows --package=zip
# -> build/packages/zip/mocenter-<role>-<版本>-windows-<arch>.zip
```

> **Windows 架构只支持 amd64 / 386 / arm64。** 32 位 ARM Windows（`windows/arm`）不是
> Go 支持的目标 —— 它在 Go 的 broken ports 表里（[go.dev/issue/68552](https://go.dev/issue/68552)），
> 也不出现在 `go tool dist list` 中。部分 Go 工具链会直接报
> `go: unsupported GOOS/GOARCH pair windows/arm` 并中断整批构建，因此已从构建矩阵中移除。
> ARM Windows 设备请使用 `arm64`。

版本号来源优先级：`--version=` / `--agent-version=` > 环境变量
`MOCENTER_SERVER_VERSION` / `MOCENTER_AGENT_VERSION`（旧名 `MOCENTER_VERSION` 仍兼容）
> 从 `server/internal/version`、`agent/internal/version` 自动读取 > 兜底值。
自动读取是为了避免"二进制里是 1.0.1、包名却是 1.0.0"这类版本漂移。
两端版本**允许不一致**（例如「安装包 1.2.0 里装的 Agent 仍标记 1.0.1」是合法组合）。

**Linux 安装包内置的 systemd 单元**（Agent 与 Server 各一份）：

| 项目 | Agent | Server |
|------|-------|--------|
| 服务名 | `mocenter-agent.service` | `mocenter-server.service` |
| 二进制 | `/usr/local/bin/mocenter-agent` | `/usr/local/bin/mocenter-server` |
| 启动参数 | `/etc/mocenter/mocenter-agent.conf` | `/etc/mocenter/mocenter-server.conf`（**请改口令/白名单**） |
| 数据目录 | — | `/var/lib/mocenter/data`（审计、`users.json`、`agent.key`） |
| 托管安装包 | — | `/var/lib/mocenter/packages` |
| Web 前端 | — | `/usr/share/mocenter/web`（有 `web/dist` 时随包装入） |

不打包也可用 `scripts/deploy/install-linux.sh` 直接注册（需 root）：

```bash
sudo bash scripts/deploy/install-linux.sh --role=server \
     --bin=build/bin/server/linux/mocenter-server-amd64 \
     --user=admin --password='强口令'
sudo bash scripts/deploy/install-linux.sh --role=agent  \
     --bin=build/bin/agent/linux/mocenter-agent-amd64 \
     --server=your.server.com:9000
sudo bash scripts/deploy/install-linux.sh --role=all status
sudo bash scripts/deploy/install-linux.sh --role=server uninstall --purge   # --purge 连配置数据一起删
```

> 安装脚本与安装包引用的是**同一份 systemd 单元模板**（`scripts/deploy/mocenter-*.service`），
> 不会再出现"打包脚本改了、安装脚本忘改"。

### Windows 图形安装程序

```bat
scripts\build-all.bat --type=installer --web=1
REM 等价于：scripts\build-windows-installer.bat
```

脚本分三步：

1. 编译 `agent` / `server` / 安装引擎 → `installer\payload\`
2. 复制 `web\dist` → `installer\payload\web\`（前端尚未构建时会告警，但不影响打包，只是不含 Web UI）
3. 调用 ISCC 编译 `installer\setup.iss` → `build\setup\MoCenter-Setup-<版本>.exe`

**ISCC 自动探测顺序**（Inno Setup **不必装在 C 盘**，D 盘等非系统盘同样会被找到）：

1. 环境变量 `INNO_SETUP_ISCC`
2. `PATH`
3. `%ProgramFiles(x86)%\Inno Setup 6\` 与 `%ProgramFiles%\Inno Setup 6\`
4. `C:` ~ `J:` 各盘符下的 `Program Files [x86]\Inno Setup 6\`

装在其它盘符或自定义目录时，显式指定即可：

```bat
set INNO_SETUP_ISCC=D:\Tools\Inno Setup 6\ISCC.exe
scripts\build-all.bat --type=installer
```

> 改版本号：**Server 版本与 Agent 版本互相独立**，各自修改自己的常量 ——
> Server 改 `server/internal/version/version.go`，Agent 改 `agent/internal/version/version.go`，
> 两者**不需要保持一致**，也不存在跨端一致性校验。
> 打包/安装包版本另由 `installer\setup.iss` 的 `#define AppVersion` 与
> 构建脚本的 `--version=` / `--agent-version=`（或对应环境变量）决定。
> **版本号只在明确发版时修改**，两端都不提供运行时改版本的开关（避免线上跑着一个"版本号被临时改过"的二进制导致升级判定错乱）。
> 安装包**无需维护文件清单**：向导只负责收集参数与摆放文件，服务注册、配置落盘、单实例保护
> 都交给随包安装的 `mocenter-deploy.exe`（其失败原因会写入安装目录下的 `mocenter-deploy.log`）。

### 部署 / 安装脚本

```bat
REM Windows：注册为系统服务（需管理员）。免安装 zip 解压后自带该脚本
scripts\deploy\install-windows.bat install your.server.com:9000 [agentid] [proxy]
scripts\deploy\install-windows.bat install-server [user] [password] [listen] [http]
scripts\deploy\install-windows.bat status
scripts\deploy\install-windows.bat uninstall
```

```bash
# Linux：安装/升级（需 root），Agent 与 Server 共用同一脚本
sudo bash scripts/deploy/install-linux.sh --role=agent  --bin=./mocenter-agent --server=your.server.com:9000
sudo bash scripts/deploy/install-linux.sh --role=server --bin=./mocenter-server --user=admin --password='强口令'

# 升级：配置已存在即按升级处理，只换二进制与服务，**不动配置**
sudo bash scripts/deploy/install-linux.sh upgrade --role=agent --bin=./mocenter-agent
# 清理重装：删除既有配置后重新生成（配置损坏时用）
sudo bash scripts/deploy/install-linux.sh install --role=agent --bin=./mocenter-agent --force

sudo bash scripts/deploy/install-linux.sh --role=all status
sudo bash scripts/deploy/install-linux.sh --role=all uninstall [--purge]
```

`--bin=` 可直接指定二进制路径，`--bin-dir=` 指定目录后按 `mocenter-<role>[-<arch>]` 自动查找
（默认也会找 `build/bin/<role>/linux/`）。`--purge` 连配置与数据一起删，
`--no-keepalive` 跳过保活任务，`--dry-run` 只打印将要执行的动作。

**保活自启按环境自动选择**：

| 环境 | 注册内容 |
|------|----------|
| 有 systemd | `mocenter-<角色>.service`（开机自启 + `Restart=always`）**并**额外注册 `mocenter-<角色>-watchdog.timer`（每 5 分钟巡检，进程不在就拉起） |
| 无 systemd（容器等） | `/etc/cron.d/mocenter-<角色>`：`@reboot` 拉起 + 每 5 分钟巡检，共用 `/usr/local/bin/mocenter-keepalive` |
| 两者都没有 | 明确告警，并提示改用容器编排的重启策略（`--restart=always` / `restartPolicy: Always`） |

> 巡检脚本幂等（进程在跑就直接退出），并由程序自身的单实例保护兜底，
> 极端竞态下重复拉起的一方会自行退出，不会出现两个实例同时服务。
> **升级语义**：检测到配置文件已存在即视为「已安装」——只替换二进制、服务单元与保活任务，
> 服务端地址/注册密钥/账号/数据目录原样保留；要推倒重来必须显式加 `--force`。

> Windows 侧**推荐使用图形安装程序或 `mocenter-deploy.exe install`**（见下方「Windows 静默 / 脚本化安装」）：
> 它们走 SCM 握手，服务状态可被 `sc query` / 服务管理器正确观察。
> `install-windows.bat` 是直接用 `sc create` 的手动备选路径，
> 仅适合不便运行安装器的场景 —— 正在用它的主机建议改用上述两种方式注册。

### 辅助工具

```bash
# 生成 CA / 服务端 / 客户端三套证书（Agent<->Server 的 TLS 或 mTLS）
bash scripts/gen-cert.sh ./certs

# 把 VNC 安装包放进服务端托管目录（供 Agent 经长连接拉取，避免公网慢/被劫持）
bash scripts/fetch-packages.sh ./packages win

# 远程命令调试：经 REST 下发 + WebSocket 收结果，等效一次「远程终端」命令
cd scripts/mcmd
go run . -agent <agent_id> -cmd "ipconfig" -server http://127.0.0.1:8088 -shell cmd -timeout 60
```

- `fetch-packages.sh` 用法为 `[目标目录] [win|linux|all]`；目标目录需与服务端 `-pkg-dir` 一致
  （未指定时为 `<数据目录>/packages`）。启动日志会打印 `[pkg] 安装包目录 ... 已托管: ...` 以便确认。
- `scripts/gencerts/` 是证书生成的 Go 实现，用法见其 `README.md`。
- `scripts/signcert/` 专门解决「服务端换过 IP / 证书 SAN 不含 Agent 用的地址」导致连不上的问题：
  默认自动探测本机公网 IP 写入 SAN，重签后只需重启服务端（同一 CA，Agent 侧无需改动）。
  用法见 `scripts/signcert/README.md`。

## 登录认证

后台**只有一种登录方式：用户名 + 密码**（可再叠加 OTP 二次校验）。
登录成功后服务端颁发一次性会话 Token，前端存在 `localStorage` 并在后续请求中以
`Authorization: Bearer <token>` 携带。

> 历史版本曾有一把「全局 API Key」（`-apikey`）作为替代登录凭证，**现已移除**：
> 它是一把对所有接口都有效的万能口令，会绕过账号体系，审计里也只能记成 `api-key`。
> 现在任何未由 `/api/login` 颁发的 Token 都会被拒绝。

### 首次启动与账号

未指定 `-user` 时，服务端会自动创建账号 `admin` 并生成**随机强口令**
（写入 `<数据目录>/initial-password.txt`，权限 0600，请查看后删除）。
也可在启动时直接指定：

```bash
# 【推荐】口令放 0600 文件，只传文件路径
printf '%s' 'your-password' > /etc/mocenter/initial-password
chmod 600 /etc/mocenter/initial-password
mocenter-server -user admin -password-file /etc/mocenter/initial-password

# 也可直接在命令行传口令（**不推荐**，见下方说明）
mocenter-server -user admin -password your-password

# 再叠加 OTP：需提供 base32 密钥（兼容 Google Authenticator）
mocenter-server -user admin -password-file /etc/mocenter/initial-password -totp-secret JBSWY3DPEHPK3PXP
```

> **为什么推荐 `-password-file` 而不是 `-password`**
>
> 命令行参数对**同机任意用户可见**：
>
> ```bash
> ps -ef | grep mocenter-server        # 进程列表直接显示 -password <明文>
> systemctl show mocenter-server -p ExecStart   # 服务单元里的参数一模一样（无需 root）
> ```
>
> 也就是说，一旦口令走命令行，它就等同于写在 `/etc/systemd/system/mocenter-server.service`
> 里公开摆着 —— 安装脚本也会把它带进 `ExecStart`，升级/排障时随手 `systemctl show` 就露了。
> 走 `-password-file`（0600）后只有 root 能读到；账号建好后这个文件就可以删掉。
>
> 服务端对 `-password-file` 是 **fail-closed** 的：文件不存在、是目录、内容为空、
> 或权限宽松到同组/其他用户可读，都会**拒绝启动并说明原因**，而不是静默回退到随机口令 ——
> 后者会让运维以为在用文件里的口令，登录不上时才发现且极难定位。
>
> 已有账号时该文件被**完全忽略**（不读、不校验）：即使文件被删或权限变化也不会影响启动，
> 更不会把用户在后台改过的口令重置回文件里的值。

登录页只填 **用户名、密码**（若该账号绑定了 OTP，再填 6 位动态验证码）。

> ⚠️ **密码不落盘明文**：密码以 **PBKDF2-HMAC-SHA256**（加盐 + 12 万次迭代）摘要写入
> 服务端数据目录的 **`users.json`**（权限 0600）；`mocenter-server.json` **不保存**
> 明文密码与 TOTP 密钥。旧版单轮 SHA-256 哈希仍可登录，登录成功后会**自动升级**为新格式。
> 之后在 Web 端 **「账号设置」** 修改密码，同样只写摘要。
>
> **会话 Token 是内存态**（默认 12 小时有效，活跃自动续期）：服务端重启后全部失效；
> **登出**会立即吊销当前 Token；**修改密码或开关 OTP** 会吊销该账号的其它会话。
> WebSocket 的 Token 走 `Sec-WebSocket-Protocol` 子协议头，不再出现在 URL 里。

### Agent 注册密钥（应用层鉴权）

Agent TCP 通道要求每台 Agent 持有与服务端一致的**预共享密钥**，注册时以
`HMAC-SHA256(key, agent_id|nonce|ts)` 证明身份（密钥不上线传输，且带时间窗与重放保护）。

- 服务端：`-agent-key` 显式指定；否则自动生成并写入 `<数据目录>/agent.key`（0600）。
- Agent：`-agent-key` 或配置文件字段 `agent_key` 填同一个值。
- 图形安装器在「连接与认证」页填写该密钥。
- 迁移期如需临时回退旧行为（不校验），可在服务端加
  `-allow-unauthenticated-agents`（**危险**，会打印醒目警告）。

### 证书与信任关系（为什么现在几乎不用配）

需要运维分发的**只有一个内容物：注册密钥（`agent.key`）**。除此之外全自动：

| 环节 | 做法 | 运维要做什么 |
|------|------|--------------|
| 服务端身份 | 首次启动自动生成部署身份 CA（`<data>/ca.crt` + `ca.key`，0600） | 无 |
| Agent 通道加密 | 用内置 CA 现场签发服务端证书 | 无 |
| Agent 身份 | 首次连接用注册密钥换取客户端证书（换到的证书 CN = `agent_id`） | 只在 `-agent-key` 填密钥 |
| 服务端校验 Agent | 校验证书链；后续连接必须携带证书 | 无 |
| Web 控制台 | 自动签发当前部署临时证书，默认 HTTPS/WSS | 无（可用正式证书替换） |

三类角色各管一件事，避免"一个口令管所有事"：

- **注册密钥**（`agent.key`，0600）：**让 Agent 被接纳**，一次性（换取证书后即从配置里清空）；
- **CA 身份**（`ca.crt`/`ca.key`，0600）：**让双方确认彼此属于同一套总控**，CA 一变指纹就变；
- **客户端证书**（自动签发、自动轮换，`<配置目录>/creds/`，0600）：**让每次连接可被验证**，
  私钥由 Agent 本地生成，服务端从不下发、也不保管。

可选增强：

- 想免掉 CA 文件、也不受服务端换 IP/证书影响 → Agent 配 `-server-fingerprint <指纹>`
  （等价于"记住这台服务器的公钥"，服务端日志与 Web「Agent 接入」页会显示）；
- 想完全禁掉"先换取"这一跳 → 服务端加 `-require-agent-cert`（此后只接受已持证 Agent）。
  **注意先换取、再开启**：客户端证书只在握手阶段生效，若一开始就强制，新 Agent 会在发出
  换取请求前被拒（客户端报 `tls: certificate required`）。先让全部 Agent 完成一次换取，再打开该开关；
- 想用外部 CA / 正式证书 → 照旧配 `-tls-cert/-tls-key/-ca-cert` 与
  `-client-cert/-client-key/-ca-cert`，或把自备的 `ca.crt`+`ca.key` 放进服务端数据目录。
- 服务端 CA 变化（重装、手工替换）后：Agent 会自动识别并重新换取证书（**无需登录被控机**）——
  前提是配置里填了新指纹到 `server_fingerprint`（填了就自动收敛；没填则明确拒绝并给出新旧指纹，
  以免把注册密钥交给陌生对端）。也仍可手工恢复：删除 `<配置目录>/creds/` 后重启。
  **服务端重装前建议先备份 `<data>/ca.key`**（`agent.key` 同理）。

  > 排查提示：这类故障最迷惑的一点是**客户端可能完全看不到错误** ——
  > Agent 通道用"请求但不强制"客户端证书，而 Go 的客户端在服务端广告的 CA 里找不到本机证书时
  > 会静默不发证书，于是握手成功、Agent 只看到"连上又断开"，服务端只看到"未完成注册"。
  > 现在服务端会显式校验证书链并区分日志原因（"尚未持有证书" / "证书不是本套 CA 签发"），
  > Agent 也会收到 `EnrollRequired` 并自动重换。

### 修改密码

1. 登录后点击左侧 **「账号设置」**。
2. 填写 当前密码、两次新密码（至少 6 位；若该账号启用了 OTP，需一并填写动态验证码）。
   身份以登录会话为准，无需填写用户名。
3. 提交成功后，**其它已登录会话会被吊销**（当前会话保留），请用新密码重新登录其它设备。
3. 提交成功后，下次登录使用新密码。

### 账号与 OTP 的其它入口

- **绑定/解绑 OTP**：同样在「账号设置」页，用手机扫描二维码（`otpauth://`）即可绑定。
- **静默部署时创建账号**：`mocenter-deploy.exe install -role server -user <名> -password <密码>`
  会把账号直接写进 `users.json`（只存摘要）。重复安装**不会覆盖**已有账号，避免把用户
  后来改过的密码重置回安装时的值。

## 功能清单

- ✅ Agent 上线注册、心跳保活、离线判定（3 个心跳周期）
- ✅ 探针采集（CPU/内存/磁盘/网络流量/系统信息）与 Web 实时展示 + 历史趋势图
- ✅ **主机备注**：仪表盘可为主机设置自定义备注，有备注时优先显示备注、小字显示主机名
- ✅ **公网 IP 展示**：仪表盘展示主机公网出口 IPv4/IPv6，并按 IP 解析地理位置与国家图标（**国旗用内置字形渲染**，不依赖访客的系统 emoji 字体 —— Windows 10 / 无字体容器下也不会显示成方框）
- ✅ 远程命令执行（bash/sh/cmd/powershell）、超时控制、审计日志、命令黑名单
- ✅ **交互式 PTY 终端**（基于 creack/pty，实时双向 IO、尺寸自适应、Ctrl-C）
- ✅ WebSocket 实时推送主机列表、命令输出、交互输出、文件传输状态、升级结果
- ✅ **端口映射反向代理**：规则持久化、连接跟踪、流量统计、超时清理，公网端口实时转发
- ✅ **文件传输断点续传**：分片收发、MD5 校验、ACK/NACK、断点续传、合并落盘
- ✅ **文件分发到主机**：Web 上传文件 → 服务端中转 → Agent 指定路径落盘（分片 + MD5 校验）
- ✅ **批量命令**：一次下发到多台在线主机，审计留痕
- ✅ **远程升级 Agent**：下载升级包（仅 https）→ SHA-256 + Ed25519 签名校验（未配置公钥时默认拒绝）→ 备份 → 替换 → 自动重启，失败回滚
- ✅ 离线机器列表、操作审计界面
- ✅ **操作审计分页与分类**：命令执行 / 运维操作 / 登录认证三类留痕，服务端分页检索（类别、关键字、操作人、时间范围筛选）+ CSV 导出；审计日志按留存策略自动清理
- ✅ **数据增长治理**：审计/探针等持续增长的数据统一采用服务端分页 + 降采样 + 留存清理，长时间跨度趋势图自动按桶聚合，避免全量返回拖垮浏览器与内存
- ✅ **移动端自适应 UI**：响应式布局（侧边栏抽屉、表格卡片化、栅格表单），支持手机/平板操作；仪表盘主机列表在窄屏保持**紧凑短卡片**（CPU/内存/磁盘并排，低频字段省略），趋势图**就地展开在该条记录下方**
- ✅ **前端加载优化**：路由级代码分割（终端/远程桌面/延迟监测等按需加载）+ 依赖分包 + 服务端预压缩（`.br`/`.gz`）与哈希资源长缓存 —— 移动端弱网首屏体积约降至 1/3
- ✅ **首页状态标记与浮动统计栏**：离线节点在卡片右上角标红（与在线绿点同位置）；顶部统计栏滚动吸附，最右侧为后台登录入口
- ✅ 鉴权（Token）+ IP 白名单 + 速率限制
- ✅ Agent 断线指数退避重连、进程守护崩溃自启
- ✅ **保活自启（服务不可用环境的兜底）**：Windows 注册「开机自启 + 每 5 分钟巡检」计划任务；Linux 有 systemd 时注册服务 + `watchdog.timer`，无 systemd（容器等）时回退 `/etc/cron.d` 计划任务 —— 进程被结束也能自动回来
- ✅ **一键升级语义**：安装脚本/安装引擎先探测配置文件是否存在，存在即按**升级**处理（只换二进制与服务、不动配置），要清理重装必须显式加 `--force` / `-force`
- ✅ **零配置 TLS/mTLS**：服务端自动生成部署身份 CA 与证书，Agent 只需注册密钥即可换取客户端证书（mTLS）；Web 控制台默认 HTTPS/WSS + HSTS；也支持外部 CA 与公钥指纹校验
- ✅ **Agent"简单模式"**：一个 `agent.key` 完成加密 + 双向认证，无需分发 ca.crt / client.crt / client.key
- ✅ **服务端托管 Web 静态资源**：`-web web/dist` 单进程部署
- ✅ **Windows 图形安装程序**：向导式安装（选择角色 → 连接与认证 → 高级设置），一键注册为**真正的 Windows 服务**（SCM 握手、失败自动重启），配置与数据固定于 `%ProgramData%\MoCenter\<角色>\`；另附命令行引擎用于静默批量部署；单实例保护（Windows 命名互斥体 / Linux·macOS `flock`），同时保留便携单文件形态
- ✅ **远程桌面 (VNC)**：服务端隧道 + noVNC 浏览器渲染，一键安装/启动（Windows **UltraVNC** / Linux **x11vnc**，支持国内加速代理）；**安装包可由服务端托管并经长连接分发**（避免公网慢/被劫持）；Windows 与 Linux 均支持**交互会话模式**与**服务模式**（后者可查看锁屏/登录界面）；连接后右下角悬浮工具栏支持**全屏**、自适应缩放、Ctrl+Alt+Del、**光标大小调节**与**移动端虚拟鼠标**
- ✅ **延迟监测（ICMP / TCP / HTTP + 历史曲线）**：Agent 侧按目标探测往返延迟（ICMP 走系统 ping、TCP 测握手、HTTP 测首字节），服务端存历史并按时间窗出曲线；**超时点画成断点而不是 0**（否则完全不通的机器会看起来"最快"），逐目标统计平均/最小/最大与丢包率；目标可按主机下发（每行一个，最多 8 个）
- ✅ **离线本地 GeoIP**：支持 MaxMind DB 格式离线库（GeoLite2-Country / City、DB-IP、IPinfo），配置 `-mmdb` / `mmdb_path` 后**本地优先解析**，未命中再降级到外部接口；解决无外网部署、免费接口按分钟限流（批量上线互相挤额度）、以及主源不支持 IPv6 三类问题
- ✅ **备份 / 恢复**：一键导出数据目录为单个 zip（含 manifest 清单与每个文件的 SHA-256），恢复前可查看包内明细；SQLite 走 `VACUUM INTO` 在线一致快照（WAL 下直接拷文件会丢未 checkpoint 的事务）；恢复端做白名单 + 路径穿越校验 + 体积上限 + 摘要校验，覆盖前自动留存 `*.before-restore`
- ✅ **后台一键部署 Agent（Linux）**：在 Web 后台填目标机地址与账号/密码（或私钥），SSH 登录后自动上传 Agent、写入配置（服务端地址 / Agent 名称 / 注册密钥 / 延迟目标）并注册为 systemd 服务；支持"先探测再安装"（仅探测连通性、架构、权限，不做任何改动）；凭证不落盘、不入日志，所有拼进 shell 的变量都经过引号转义与白名单校验
- ✅ **版本号独立管理**：Server（`server/internal/version`）与 Agent（`agent/internal/version`）各自维护版本号，**互相独立、不做一致性校验** —— Agent 只按自身功能迭代节奏发版，服务端发版不会牵动全网 Agent，反之亦然；`AgentInfo.AgentVersion` 随注册上报并由 `/api/version` 展示；升级包的 version 只表示"本次要升到哪个版本"（不填则默认取服务端版本），与当前服务端版本不同也能下发；打包脚本与 Inno Setup 版本号同步
- ✅ **多架构打包**：Windows x86/x86_64/ARM64、Linux amd64/arm64/arm/386 二进制，及 .deb/.rpm 安装包
- ✅ **系统服务注册**：Agent 一键注册为 Windows 服务 / Linux systemd 服务，开机自启

> 说明：核心链路（Agent 上线→探针→命令执行→结果回传→Web 展示）已端到端打通，并在此基础上实现了交互式终端、文件断点续传、端口映射、批量命令、远程升级、TLS 加密等生产级能力。

## VNC 远程桌面的两种模式

Web「远程桌面」页在「一键安装VNC」时可选择安装模式：

| 模式 | Windows 实现 | Linux 实现 | 可见范围 | 适用场景 |
|------|--------------|-----------|----------|----------|
| **交互会话模式**（默认） | UltraVNC **应用模式**（不注册服务，最小权限） | x11vnc 在当前用户会话中拉起 | 仅「已登录且未锁屏」时的用户桌面 | 常规远程协助；Agent 无管理员权限或不允许挂常驻系统服务的环境 |
| **服务模式** | UltraVNC（`uvnc_service`，LocalSystem） | x11vnc 注册为 systemd 系统服务（root，挂接 `:0`） | 用户桌面 **+ 锁屏 / 登录界面** | 需查看锁屏、登录界面，或主机长期无人登录 |

两种模式共用同一套口令编码，切换模式时旧实例会被自动停用，不会争抢端口。

**为什么 Linux 侧实现更简单**：Windows 的登录界面位于独立的「安全桌面」，必须靠 SYSTEM 服务
切换桌面才能抓到；而 Linux/X11 下登录界面与用户会话**共用同一个 X 显示 `:0`**（不存在安全桌面隔离），
因此以 root 挂接 `:0` 即可同时覆盖登录界面、锁屏与用户桌面。两个关键参数：`-auth guess`
（登录界面的 Xauthority 与用户会话不是同一个文件）与 `-loop`（注销回到登录界面时 X 会重启，需自动重连）。

> ⚠️ **Wayland 限制**：x11vnc 只能挂接 X11 显示。若目标机使用 Wayland 会话
> （Ubuntu 22.04+、Fedora、RHEL 9+ 的默认 GNOME 会话等），x11vnc 无法捕获桌面，会黑屏。
> 需改用 Xorg 会话（如 GDM 在 `/etc/gdm3/custom.conf` 设 `WaylandEnable=false` 后重启）。
> Agent 检测到 Wayland 时会在结果消息里明确告警。
> 参考：Rocky/RHEL 8 的 GNOME 默认即 Xorg，可直接使用。

**为什么锁屏必须用服务模式**：Windows 锁屏时会把活动桌面切到 `WinSta0\Winlogon`（安全桌面），
DXGI Desktop Duplication 随即返回 `DXGI_ERROR_ACCESS_LOST`，普通抓屏只能拿到锁屏前的黑帧。
只有以 SYSTEM 身份运行、并能切换到安全桌面的进程才能抓到画面——UltraVNC 的服务模式
（工作进程以 `lpDesktop = "Winsta0\Winlogon"` 启动）正是这种实现。
Windows 侧因此统一采用 UltraVNC（TightVNC 的服务模式在锁屏/登录界面并不可靠，
有大量黑屏反馈），只维护一个 VNC 产品、一套配置与一套口令编码。

服务模式安装失败时，Agent 会自动降级为交互会话模式，并在结果消息中明确说明（锁屏不可见）。

> ⚠️ **安全提示**：服务模式会以 LocalSystem 权限抓取**登录界面**，相当于把主机的登录入口暴露给
> 能访问 Web 控制台的人。请务必配合强口令、IP 白名单与 HTTPS 使用，并仅在确有需要的主机上启用。
> 另外，Agent 需以**管理员身份**运行才能注册服务与安装驱动。

> 说明：无头（无物理显示器）主机无论哪种模式，都需要虚拟显示器提供活动显示输出；
> 一键安装时会自动检测并在必要时尝试安装 VirtualDrivers/Virtual-Display-Driver。

### 安装包由服务端分发（推荐）

Windows 的 VNC 安装包（UltraVNC，约 4–5MB）默认从公网下载。若目标机访问境外站点慢，
或担心下载被劫持/篡改，可把安装包放进**服务端托管目录**——Agent 会优先通过它与服务端之间
**既有的长连接**拉取（该通道可启用 TLS/mTLS），服务端未托管时才回退公网下载。

```bash
# 1) 把安装包放进服务端托管目录（默认 <数据目录>/packages）
bash scripts/fetch-packages.sh ./packages

# 2) 启动服务端时指向该目录（不指定则默认 <data>/packages）
./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -data ./data -pkg-dir ./packages
# 启动日志会确认: [pkg] 安装包目录 ./packages 已托管: UltraVNC_1824_x64_Setup.exe(4.6MB)
```

托管文件名必须与 Agent 期望的一致：

| 文件名 | 平台 |
|--------|------|
| `UltraVNC_1824_x64_Setup.exe` / `UltraVNC_1824_x86_Setup.exe` | Windows |
| `x11vnc-linux-amd64` / `-arm64` / `-arm` / `-386` | Linux（**需自备**，见下） |

> **Linux 说明**：x11vnc 官方（LibVNC/x11vnc）只发布源码、**没有任何预编译产物**，
> 不存在可直下的官方成品。Linux 侧优先由 Agent 用**发行版包管理器**安装
> （RHEL/Rocky/Alma 走 EPEL，实测提供 0.9.17；Debian/Ubuntu 官方源直接有）。
> 仅当目标机无法访问软件源时，才需自行构建或自备 x11vnc 二进制，按上表命名放入托管目录。

> Windows 安装包缺失时自动回退公网下载；Linux 缺失时回退到发行版包管理器，均不影响可用性。
> 无论从服务端还是公网取得，Agent 都会**校验可执行文件魔数**（Windows 为 `MZ`、Linux 为 `ELF`），
> 拒绝执行错误页或被替换的内容。服务端亦会校验包名（防目录穿越），
> 且安装包拉取不会出现在「文件分发」列表中。

## Agent 安装包与系统服务

> 各构建 / 打包脚本的参数与产物位置统一见 **[构建脚本参考](#构建脚本参考)**。
> 本节只讲「装到目标机之后会发生什么」。

### 多架构二进制

| 平台 | 命令 | 产物目录 |
|------|------|----------|
| Linux（amd64 / arm64 / arm / 386） | `bash scripts/build-linux-all.sh --type=agent --package=none` | `build/bin/agent/linux/` |
| Windows（x86_64 / x86 / arm64） | `scripts\build-all.bat --type=agent --package=none` | `build\bin\agent\windows\` |

Windows 产物会同时打包成免安装 zip（`--package=zip`，默认包含在内），
zip 内自带 `install-windows.bat` / `uninstall-windows.bat` 与配置模板，解压即用。

### Linux .deb / .rpm 安装包（含 systemd 服务）

```bash
bash scripts/build-linux-all.sh --type=agent --package=all --arch=amd64
bash scripts/build-linux-all.sh --type=agent,server --package=deb --arch=amd64,arm64
# -> build/packages/{deb,rpm}/
```

安装包内置 systemd 服务单元，安装后自动：
- 注册 `mocenter-agent.service` / `mocenter-server.service`
- 设置开机自启并启动
- Agent 二进制位于 `/usr/local/bin/mocenter-agent`，配置位于 `/etc/mocenter/mocenter-agent.conf`（**请修改服务端地址**）

不想自行打包时，也可用安装脚本直接注册（需 root）：

```bash
sudo bash scripts/deploy/install-linux.sh --role=agent \
     --bin=build/bin/agent/linux/mocenter-agent-amd64 \
     --server=your.server.com:9000
# 也可用 --bin-dir 指定目录，按 mocenter-<role>[-<arch>] 自动匹配
```

> 打包依赖：`.deb` 需 `dpkg-deb`（Debian/Ubuntu）或 `fpm`；
> `.rpm` 优先用 `fpm`，没有 `fpm` 时回退到 `rpmbuild`（两者都没有才跳过，并给出提示）。
> 缺少工具时只跳过对应格式，**不影响其它产物**。
>
> ⚠️ 走 `rpmbuild` 分支时必须装 `file(1)`：`rpmbuild` 的 debuginfo 阶段用 `file`
> 识别 ELF，缺了它 `.rpm` 会以一句**与源码无关**的
> `error: Empty %files file .../debugfiles.list` 失败（详见下节）。

#### `.rpm` 报 `Empty %files file .../debugfiles.list`

这是**环境/工具链问题，不是代码问题**。发行版的 `rpm` 默认已定义 `debug_package`
宏（rpm ≥ 4.14 中自带 `%ifnarch noarch`，Fedora/CentOS/openSUSE 均默认生效），
它让 `rpmbuild` 额外生成一个 `-debuginfo` 子包，文件清单由 `find-debuginfo` 写入
`debugfiles.list`。而该脚本只收录 `file` 判定为「ELF 且 **not stripped**」的文件：

```sh
find "$RPM_BUILD_ROOT" -perm -0111 -type f | file -N -f - |
  sed -n 's/^\(.*\):.*ELF.*, not stripped.*/\1/p'
```

本项目二进制在构建期就被 strip（`--ldflags "-s -w"`），`file` 输出为 `... stripped`，
清单恒为空 → 报错。同一条 `find` 还依赖 `file`，所以**缺 `file` 时也会以同一个错收尾**。

构建脚本已默认显式关闭 debuginfo 子包：

```sh
rpmbuild ... --define 'debug_package %{nil}' -bb <spec>
```

`Empty %files` 报错因此不再出现。需要随包产出 `-debuginfo` 子包时用
`MOCENTER_RPM_DEBUGINFO=1` 打开（此时 `file` 是硬依赖）。

```sh
# 缺工具时的修复
sudo apt-get install -y rpm file        # Debian / Ubuntu
sudo dnf install -y rpm-build file      # Fedora / RHEL / openSUSE
```

> 历史背景：该报错曾长期存在，日志里 `Processing files: mocenter-server-debuginfo-...`
> 与主体包 `Wrote:` 并存，容易被误读成「源码编译失败」。实际主包已构建成功，
> 停在 `-debuginfo` 子包上。

### Windows 图形安装程序（推荐）

双击 `MoCenter-Setup-<版本>.exe` 即进入安装向导：

1. **选择角色** —— Agent（被控端）/ Server（总控端）/ 两者都装
2. **连接与认证** —— 服务端地址（装 Agent 必填）、后台登录用户名与密码（装 Server 必填）
3. **高级设置** —— 数据目录、监听端口、代理前缀、TLS 参数（均有默认值，可一路下一步）

安装后：

| 项目 | 位置 / 行为 |
|------|-------------|
| 二进制 | `%ProgramFiles%\MoCenter\` |
| 配置与数据 | `%ProgramData%\MoCenter\<角色>\`（与二进制分离，升级/重装不覆盖） |
| 服务 | `MoCenterAgent` / `MoCenterServer`，开机自启 + 失败自动重启 |
| 保活计划任务 | `<角色>Boot`（开机/登录自启）+ `<角色>KeepAlive`（每 5 分钟巡检，进程不在就拉起）；卸载时一并删除 |
| 日志 | `%ProgramData%\MoCenter\<角色>\logs\`（服务模式下 stdout 无去处，故落文件） |
| 卸载 | 控制面板「应用和功能」；服务会停止并移除，**配置与数据默认保留** |

> **升级语义**：向导不区分"全新安装/升级"，由引擎按"配置是否存在"自动判断 ——
> 已装过的机器上重跑向导只会替换二进制与服务，配置、账号与数据原样保留；
> 需要清理重装时用命令行引擎加 `-force`。
>
> 向导本身只负责**收集参数 + 摆放文件**；服务注册、配置落盘、单实例保护、日志重定向
> 都交给随包安装的 `mocenter-deploy.exe`，两处不重复实现，也就不会出现"界面说的和实际做的"不一致。
> 安装引擎的失败原因会写入安装目录下的 **`mocenter-deploy.log`**，安装失败时先看这个文件。
>
> 打包脚本的用法、以及 Inno Setup 装在非系统盘时如何指定，见
> **[构建脚本参考 → Windows 图形安装程序](#windows-图形安装程序)**。

### Windows 静默 / 脚本化安装（mocenter-deploy.exe）

批量部署时用命令行引擎，参数与向导完全一致：

```bat
REM 被控机
mocenter-deploy.exe install -role agent -server your.server.com:9000 -agent-key <注册密钥>

REM 总控机
mocenter-deploy.exe install -role server -user admin -password <登录密码>

REM 升级：配置已存在即按升级处理，只换二进制与服务，不动配置
mocenter-deploy.exe upgrade -role agent
REM 升级时无需重复提供 -server / -agent-key / -user / -password

REM 清理重装：删除既有配置与数据后重来
mocenter-deploy.exe install -role agent -server your.server.com:9000 -agent-key <注册密钥> -force

REM 查看状态（无需管理员）/ 卸载（默认保留配置与数据）
mocenter-deploy.exe status -role all
mocenter-deploy.exe uninstall -role all
```

> - 需以**管理员**身份运行（注册服务、写 Program Files 都需要）。
> - 监听地址用 `-listen` / `-http`；TLS 等参数用 `-extra "-tls -ca-cert C:\ca.crt"` 透传。
> - **升级不动配置**：检测到 `<数据目录>\<角色>\mocenter-<角色>.json` 已存在即按升级处理，
>   账号/连接参数/数据库全部保留；要清理重装需显式加 `-force`。
> - **保活兜底**：安装时额外注册两个计划任务（以 SYSTEM 运行）—— `<角色>Boot`（开机自启）
>   与 `<角色>KeepAlive`（每 5 分钟巡检，进程不在就拉起）。服务能跑时它们是空转的安全网；
>   服务不可用（容器、组策略禁服务）时就是唯一的自启与防杀通道。卸载会一并摘掉。
> - **便携形态不受影响**：不安装时直接运行 `mocenter-agent.exe` 即可，配置落在 **exe 同目录**
>   （基准是 exe 位置而非当前工作目录，所以服务、计划任务、双击运行都落在同一处）。
> - **单实例保护**：同一份配置只允许一个实例（Windows 命名互斥体 / Linux·macOS `flock`），
>   避免"服务 + 手工启动 + 定时任务保活"并存。锁文件与配置文件同目录，详见
>   [部署文档 → 单实例保护](docs/deployment.md#十二单实例保护)。

### Windows 手动注册服务（备选）

不方便用安装器时也可手动注册。两点必须注意：服务名要与程序内置的一致
（`MoCenterAgent` / `MoCenterServer`），并用 `-config` 指向固定位置的配置：

```bat
sc create MoCenterAgent binPath= "\"C:\Program Files\MoCenter\mocenter-agent.exe\" -config C:\ProgramData\MoCenter\agent\mocenter-agent.json" start= auto
sc start MoCenterAgent
```

> 程序通过 `svc.IsWindowsService()` 自动识别"被 SCM 拉起"，**不需要**额外开关 ——
> 早前用 `-daemon` 注册服务的做法没有 SCM 握手，SCM 会报 1053、`sc stop` 也管不住，现已修正。

### 国内加速代理

Agent 支持 `-proxy-ip http://fa.mooncn.win/?url=` 参数（或配置文件 `MOCENTER_PROXY_IP`），
用于国内网络环境下加速：公网 IP 探测、VNC 安装包下载等境外服务。

Server 侧解析节点地理位置同样支持该参数（配置文件 `proxy_ip` /
环境变量 `MOCENTER_PROXY_IP`），直连不稳定时可一并配上：
`mocenter-server -proxy-ip http://fa.mooncn.win/?url=`。

Server 的地理位置解析为**多源降级**：`api.iping.cc`（主，中文口径）→
`ip-api.com`（备，带 ISO 国家代码）→ `ipwho.is`（兜底）。主源不返回国家代码，
服务端内置中英文国名 → ISO 代码对照表补齐（国旗依赖它），并统一国家名为英文。

公网 IP 探测的回显服务按**国内可达性**排序（`tool.mooncn.win` →
`api-ipv4.ip.sb` → `ipv4.icanhazip.com` → `ipv4.ident.me` → `api.ipify.org`），
结果按地址族过滤、失败 60 秒后自动重试。
排查"节点地区 / 国旗不显示"见 `docs/deployment.md`。

## 许可证

内部项目。
