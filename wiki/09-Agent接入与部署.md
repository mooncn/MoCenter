# 09 Agent 接入与部署

侧边栏 → **Agent**，两个页签：

- **SSH 部署（Linux）**：控制台通过 SSH 把 Agent 装到 Linux 主机（需要 SSH 可达）。
- **通用部署（Agent 接入）**：签发一次性注册令牌，按页面给出的命令手工安装（Linux / Windows 都适用）。

## 1. 一次性注册令牌（推荐）

1. 在「通用部署」里选择 Linux / Windows；
2. 可选填写 **Agent 名称/ID**（如 `web-01`）：
   - 留空 = 不绑定身份，任何主机用这枚令牌注册后都会获得自己的身份；
   - 填写 = 只允许该 ID 注册；
3. 点「签发令牌」，复制页面生成的完整命令到目标机执行。

Linux 示例：

```bash
curl -fsSL https://<控制台>/api/packages/agent-linux-amd64 -o /tmp/mocenter-agent
chmod +x /tmp/mocenter-agent
sudo /tmp/mocenter-agent -server <服务器IP>:9000 -agentid web-01 \
     -enroll-secret <令牌> -daemon
```

Windows（管理员 PowerShell）：

```powershell
.\mocenter-agent.exe -server <服务器IP>:9000 -agentid web-01 -enroll-secret <令牌> -daemon
```

要点：

- 令牌**用后即废**：只在首次换取客户端证书时使用；换取成功后 Agent 会自动清空 `enrollment_secret`。
- 换取失败（如时钟偏差、ID 不一致）**不会消耗**令牌，修正后可继续用。
- 页面下方的令牌列表显示每枚令牌的主机、创建时间、到期时间与状态，可刷新查看。

📷 **截图占位**：`Agent-通用部署`

## 2. 共享注册密钥（过渡方案）

服务端首次启动会生成 `<data>/agent.key`，Agent 也可用 `-agent-key <内容>` 注册。
它是一把钥匙认全网，**泄露任一台即可冒名注册任意主机**。现网全部迁到一次性令牌后，
给 Server 加 `-disable-shared-agent-key` 关闭这条路径（控制台首页的黄色告警会提示）。

## 3. SSH 一键部署（Linux）

「SSH 部署」表单：

| 字段 | 说明 |
|---|---|
| 主机地址 | IP 或域名 |
| SSH 端口 | 默认 22 |
| 登录用户 | 默认 `root`；非 root 需提供 sudo 密码 |
| 认证方式 | 登录密码 或 私钥内容（PEM / OpenSSH，可带口令） |
| 服务端地址 | Agent 回连地址；留空用服务端当前配置 |
| 主机密钥指纹 | 可选；**填写即强制钉扎**，与目标机不符则中止部署 |
| Agent 名称 | 可选，便于识别 |

建议流程：先点「测试连接」→ 核对回显的主机指纹 → 填入指纹 → 部署。
部署过程会校验上传文件的字节数，并在完成后确认 Agent 上线。

📷 **截图占位**：`Agent-SSH部署`

## 4. Agent 配置文件

默认位置：

| 平台 | 配置 | 数据/证书 |
|---|---|---|
| Linux | `/etc/mocenter/mocenter-agent.json` | `/etc/mocenter/creds/` |
| Windows | 安装目录下 `mocenter-agent.json` | 同目录 `creds\` |

常用字段：

```json
{
  "server": "1.2.3.4:9000",
  "agent_id": "web-01",
  "heartbeat": 30,
  "probe_every": 10,
  "tls": true,
  "log_level": "error",
  "file_roots": ["/etc/mocenter/files"],
  "tunnel_hosts": [],
  "latency_targets": ["tcp:1.1.1.1:443"]
}
```

- `heartbeat`：心跳秒数（过大容易被服务端判离线，建议 30）。
- `file_roots`：**文件分发**允许写的目录（文件管理不受此限制）。
- `tunnel_hosts`：端口映射/隧道允许访问的非回环地址（默认只允许 127.0.0.1）。
- 证书 `creds/client.crt|key|ca.crt` 由首次换取自动落盘，**不要手工编辑**。

## 5. 日志级别

- 级别：`off` / `error`（默认）/ `warn` / `info`。
- 本地：配置文件 `log_level`，或启动参数 `-log-level`。
- 远程：控制台「Agent 接入」页可对单台/全部主机下发，**不需要重启**；界面会显示每台的当前级别与回执。
- 日志文件按大小轮转（8MB × 2 份）。

## 6. 升级 Agent

见 [07 运维操作 → 远程升级](07-运维操作.md)。本地 pkg 免签名；在线链接必须配置
`-upgrade-pubkey` 并用对应私钥签名，否则一律拒绝。

## 7. 常见问题

| 报错/现象 | 处理 |
|---|---|
| `注册报文时间戳偏差过大` | 两端时钟差太大：校时（`chronyc makestep` / `w32tm /resync`）后重连，令牌仍可用 |
| `注册凭证校验失败` | 令牌过期/已用/agent_id 不匹配，或共享密钥与服务端不一致；重新签发令牌 |
| 换取证书后立刻掉线 | 服务端换过部署身份（CA）：把新指纹填进 Agent 的 `server_fingerprint`，或删除 `creds/` 重新换取 |
| 提示“已有实例在运行” | 同一配置只能跑一个 Agent（单实例锁）；先停旧进程/服务 |
| Windows 服务起不来 | 用 `sc query MoCenterAgent` 与事件查看器看错误；确认端口/证书路径有效 |
| 主机一直显示离线 | 先确认 `:9000` 可达、Server 与 Agent 时钟接近，再看 Agent 日志 |
