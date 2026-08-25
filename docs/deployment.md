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
  -e MOCENTER_API_KEY=your-secret-key \
  mocenter-server
```

### 方式二：物理机直接部署

```bash
# 1. 准备公网服务器，编译服务端二进制
cd server && bash build.sh
# 2. 启动（配置端口与防火墙规则）
./mocenter-server -listen :9000 -http :8080 -apikey your-secret-key
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

将 Agent 注册为系统服务（如使用 NSSM）：

```bat
nssm install MoCenterAgent "C:\opt\mocenter-agent.exe" -server your.server.com:9000 -agentid host-01
nssm set MoCenterAgent AppStdout C:\opt\agent.log
nssm start MoCenterAgent
```

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

1. **修改 API Key**：服务端启动时通过 `-apikey` 设置强随机密钥，Web 登录时使用该 Key。
2. **IP 白名单**：通过 `-whitelist 1.2.3.4,10.0.0.0/8` 限制可访问服务端的源 IP。
3. **TLS/mTLS**：生产环境为 Agent<->Server 配置全链路 TLS 1.2+ 与 mTLS 双向认证；Web 端启用 WSS。证书采用自签名 CA 体系，最长有效期 90 天，到期前自动续期。
4. **命令黑名单**：高危命令（`rm -rf /`、`mkfs`、`sudo` 等）由服务端拦截并记录审计。

## 六、备份与恢复

- **审计日志**：默认保留 180 天，建议定期导出归档（`/api/audits`）。
- **探针趋势**：服务端聚合存储默认 90 天。
- **配置备份**：备份服务端启动参数、证书文件、映射规则配置。

## 七、常见故障处理

| 现象 | 排查方法 |
|------|----------|
| Agent 无法上线 | 检查 `-server` 地址端口、防火墙规则、服务端是否启动 |
| Agent 频繁重连 | 查看心跳日志；弱网环境服务端会自动拉长心跳间隔 |
| 命令执行超时 | 调整 `CommandReq.timeout`；检查命令自身是否阻塞 |
| Web 连接断开 | 确认 Token 有效、WebSocket 代理配置正确（Upgrade 头） |
| 端口映射不通 | 确认映射规则已启用、Agent 在线、目标内网服务可达 |

## 八、远程升级 Agent

服务端发布新版本 → Agent 检测到新版本 → 下载升级包 → 校验签名 → 停止旧进程 → 替换二进制 → 重启新进程 → 上报版本信息。支持差分升级、失败自动回滚、灰度升级（规划书 12.4）。
