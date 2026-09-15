# MoCenter 部署与运维手册

本文档说明 MoCenter 总控服务端、Agent、Web 前端的部署步骤，以及证书、备份恢复、常见故障处理。

## 一、环境要求

| 组件 | 环境 |
|------|------|
| 服务端 | Linux x86_64，2 核 4G（压测 4 核 8G），公网 IP，开放 TCP 端口（默认 9000）与 HTTP 端口（默认 8080） |
| Agent | Linux（CentOS/RHEL 7/8、Ubuntu 18.04~22.04、Debian 10/11）、Windows 10 1809+ / Server 2019+，x86_64 为主 |
| Web | 浏览器 Chrome/Edge/Firefox 最新两个大版本 |

## 二、部署方式

### 方式一：基于 Docker 容器化部署

```bash
# 构建服务端镜像（需 Dockerfile，可参考规划书容器化章节）
docker build -t mocenter-server ./server
docker run -d -p 9000:9000 -p 8080:8080 \
  mocenter-server -listen :9000 -http :8080 -user admin -password your-password
```

### 方式二：物理机直接部署（单进程托管 Web）

```bash
# 1. 构建前端并交由服务端托管
cd web && bash build.sh   # 生成 dist/
cd ../server && bash build.sh
# 2. 启动（同时托管 Web 静态资源，单进程部署）
./mocenter-server -listen :9000 -http :8080 -user admin -password your-password -web ../web/dist
```

## 三、Agent 部署

```bash
# 交叉编译 Linux / Windows Agent
bash agent/build.sh linux
bash agent/build.sh windows   # 生成 .exe

# 分发到各内网主机并启动
./mocenter-agent -server your.server.com:9000 -agentid host-01
```

### Linux 开机自启（systemd）

```ini
# /etc/systemd/system/mocenter-agent.service
[Unit]
Description=MoCenter Agent
After=network.target

[Service]
Type=simple
ExecStart=/opt/mocenter/mocenter-agent -server your.server.com:9000 -agentid %H -daemon
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now mocenter-agent
```

### Windows 开机自启

**推荐：图形安装程序** —— 双击 `MoCenter-Setup-<版本>.exe`，按向导走：

1. **选择角色**：Agent（被控端）/ Server（总控端）/ 两者
2. **连接与认证**：服务端地址（装 Agent 必填）、后台登录用户名与密码（装 Server 必填）
3. **高级设置**：数据目录、监听端口、代理前缀、TLS 参数（均有默认值）

安装后：

| 项目 | 位置 |
|------|------|
| 二进制 | `%ProgramFiles%\MoCenter\` |
| 配置与数据 | `%ProgramData%\MoCenter\<角色>\`（与二进制分离，升级不覆盖） |
| 服务 | `MoCenterAgent` / `MoCenterServer`（开机自启 + 失败自动重启） |
| 日志 | `%ProgramData%\MoCenter\<角色>\logs\`（服务模式 stdout 无去处，故落文件） |

卸载走控制面板「应用和功能」，服务会被停止并移除，**配置与数据默认保留**。

**批量/静默部署**用随包安装的命令行引擎（参数与向导一致）：

```bat
mocenter-deploy.exe install -role agent -server your.server.com:9000
mocenter-deploy.exe install -role server -user admin -password <登录密码>
mocenter-deploy.exe status -role all      REM 无需管理员
mocenter-deploy.exe uninstall -role all   REM 默认保留配置与数据
```

**手动注册**（服务名必须与程序内置的一致，并用 `-config` 指向固定位置的配置）：

```bat
sc create MoCenterAgent binPath= "\"C:\Program Files\MoCenter\mocenter-agent.exe\" -config C:\ProgramData\MoCenter\agent\mocenter-agent.json" start= auto
sc start MoCenterAgent
```

> 程序通过 `svc.IsWindowsService()` 自动识别"被 SCM 拉起"，**不需要 NSSM 之类的包装器**，
> 也不需要 `-daemon` 开关。早前用 `-daemon` 注册的方式没有 SCM 握手，SCM 会报 1053
> 且 `sc stop` 管不住它，现已修正。
>
> 便携形态仍然可用：直接运行 `mocenter-agent.exe` 即可，配置落在 **exe 同目录**。
>
> 打包安装程序：`scripts\packaging\build-setup.bat`（需先装 Inno Setup 6）。

## 四、Web 前端部署

```bash
cd web && npm run build   # 生成 dist/
# 将 dist 静态文件部署到 Nginx，或由 Go 后端一并托管
```

Nginx 反向代理示例（将 /api 与 /ws 代理到 Go 服务端）：

```nginx
server {
    listen 80;
    server_name mocenter.example.com;

    location / {
        root /var/www/mocenter;
        try_files $uri /index.html;
    }
    location /api {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
    location /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

## 五、安全配置

1. **设置登录账号**：服务端启动时通过 `-user` / `-password` 指定后台账号，并在 Web「账号设置」里
   绑定 OTP 二次校验。凭证只有一种：用户名 + 密码(+OTP)，**没有全局 API Key**。
2. **IP 白名单**：通过 `-whitelist 1.2.3.4,10.0.0.0/8` 限制可访问服务端的源 IP。
3. **TLS/mTLS**：Agent<->Server 支持全链路 TLS 加密，可升级为 mTLS 双向认证。生成自签名证书：

   ```bash
   bash scripts/gen-cert.sh certs   # 生成 ca.crt / server.crt / client.crt
   ```

   **单向 TLS**（加密传输）：
   ```bash
   # 服务端
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -tls-cert certs/server.crt -tls-key certs/server.key
   # Agent（自签名跳过校验）
   ./mocenter-agent -server host:9000 -agentid host-01 -tls -insecure
   ```

   **mTLS 双向认证**（服务端校验 Agent 客户端证书）：
   ```bash
   # 服务端（额外提供 -ca-cert，要求 Agent 携带有效客户端证书）
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -tls-cert certs/server.crt -tls-key certs/server.key -ca-cert certs/ca.crt
   # Agent（携带客户端证书，并用 CA 校验服务端证书）
   ./mocenter-agent -server host:9000 -agentid host-01 -tls -client-cert certs/client.crt -client-key certs/client.key -ca-cert certs/ca.crt
   ```

   生产环境建议使用受信任 CA 签发的证书，实现 mTLS 双向认证。Web 端启用 HTTPS/WSS。

4. **命令黑名单**：高危命令（`rm -rf /`、`mkfs`、`sudo` 等）由服务端拦截并记录审计。

## 六、高级运维能力

### 1. 交互式 PTY 终端
Web「远程终端」页面选择在线主机并执行交互式命令（如 `bash`、`top`、`vim`），支持实时双向 IO、终端尺寸自适应、Ctrl-C 中断。

### 2. 批量命令
Web「运维操作」页面勾选多台在线主机，批量下发同一命令；服务端逐一审计留痕。REST 接口：`POST /api/command/batch`。

### 3. 远程升级 Agent
Web「运维操作」页面填写升级包 URL 与 MD5 校验和，下发升级指令。Agent 依次执行：下载升级包 → MD5 校验 → 备份旧版本 → 替换二进制 → 自动重启；失败自动回滚。REST 接口：`POST /api/upgrade`。

### 4. 端口映射
Web「端口映射」页面创建规则（公网端口 → 内网服务端口），服务端反向代理实时转发到目标 Agent，支持源 IP 白名单、连接跟踪与流量统计。REST 接口：`GET/POST /api/portmap/rules`、`POST /api/portmap/op`。

### 5. 文件传输
支持 Agent 文件上传/下载，分片传输 + MD5 校验 + ACK/NACK + 断点续传，接收目录默认为服务端 `/tmp/mocenter_uploads`。

### 6. 文件分发到主机
Web「运维操作」页面选择目标在线主机与 Agent 端目标路径，上传本地文件分片经服务端中转、MD5 校验后写入 Agent 指定路径。适用于批量下发部署包、配置文件、脚本等场景。

### 7. VNC 安装包由服务端分发

Agent 一键安装 VNC 时，默认从公网下载安装包（UltraVNC，约 4–5MB）。为规避公网限速与
下载被劫持/篡改的风险，可把安装包放到服务端托管目录：

```bash
bash scripts/fetch-packages.sh ./packages                 # 下载安装包到托管目录
./mocenter-server ... -data ./data -pkg-dir ./packages    # 指向该目录（不指定则默认 <data>/packages）
```

启动日志会打印已托管的包清单（`[pkg] 安装包目录 ... 已托管: ...`）。
Agent 会**优先通过它与服务端之间既有的长连接**拉取（该通道可启用 TLS/mTLS），
服务端未托管该包时自动回退公网下载，因此托管是可选增强、不影响可用性。

托管文件名须与 Agent 期望的一致（`scripts/fetch-packages.sh` 会按此命名下载 Windows 包，
也可加参数只下某平台：`bash scripts/fetch-packages.sh ./packages win`）：

| 文件名 | 平台 |
|--------|------|
| `UltraVNC_1824_x64_Setup.exe` / `UltraVNC_1824_x86_Setup.exe` | Windows |
| `x11vnc-linux-amd64` / `-arm64` / `-arm` / `-386` | Linux（需自备） |

> **Linux 说明**：x11vnc 官方只发布源码、无任何预编译产物，因此脚本不代为下载。
> Linux 侧优先由 Agent 用发行版包管理器安装（RHEL/Rocky/Alma 走 EPEL，Debian/Ubuntu 直接有）；
> 仅当目标机无法访问软件源时，才需自行构建/自备二进制并按上表命名放入托管目录。

服务端会校验包名以防目录穿越；Agent 会校验可执行文件魔数（`MZ`/`ELF`），
拒绝执行错误页或被替换的内容；安装包拉取属于内部行为，不会出现在「文件分发」列表中。

## 七、备份与恢复

- **审计日志**：默认保留 180 天，建议定期导出归档（`/api/audits`）。
- **探针趋势**：服务端聚合存储默认 90 天。
- **配置备份**：备份服务端启动参数、证书文件、映射规则配置。

## 八、常见故障处理

| 现象 | 排查方法 |
|------|----------|
| Agent 无法上线 | 检查 `-server` 地址端口、防火墙规则、服务端是否启动 |
| Agent 频繁重连 | 查看心跳日志；弱网环境服务端会自动拉长心跳间隔 |
| 命令执行超时 | 调整 `CommandReq.timeout`；检查命令自身是否阻塞 |
| Web 连接断开 | 确认 Token 有效、WebSocket 代理配置正确（Upgrade 头） |
| 端口映射不通 | 确认映射规则已启用、Agent 在线、目标内网服务可达 |

## 九、远程升级 Agent

服务端发布新版本 → Agent 检测到新版本 → 下载升级包 → 校验签名 → 停止旧进程 → 替换二进制 → 重启新进程 → 上报版本信息。支持差分升级、失败自动回滚、灰度升级（规划书 12.4）。
