# MoCenter 服务端证书签发工具（signcert）

**按服务端的「实际地址」签发 TLS 证书 —— 默认自动探测本机公网 IP**，一条命令解决
Agent 连不上、报 `x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>` 的问题。

---

## 背景：为什么会连不上

Agent 用 `-ca-cert ca.crt` 校验服务端证书时，会同时校验 **证书 SAN 是否包含它连接的地址**
（IP 或域名）。如果服务端证书的 SAN 只有 `127.0.0.1`、`0.0.0.0`，Agent 用公网 IP 连接
就会直接失败：

```
[agent] 连接断开: TLS 连接失败且本机已持有客户端证书，拒绝降级为明文：
        tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, ::1, not 82.156.7.85
```

服务器往往部署之后才知道公网 IP，手敲 `-ip` 很容易漏。本工具默认**自动探测公网 IP**
（含 IPv6）并写入 SAN，同时补上本机网卡地址与回环地址，避免"签完本地又连不上"。

> **优先考虑更省事的路径**：该项目默认可自动换取证书（Agent 只要 `-agent-key`），
> 校验方式是按**证书链**而不是按地址/SAN，因此服务端换 IP、加域名都不会失败。
> 只有在**手工铺证书**（`-tls-cert` / `-ca-cert` / `-client-cert`，例如要用外部 CA 或
> 预置固定证书）时才需要关心 SAN —— 那时本工具就是修复"换过 IP 连不上"的入口。
> 另一条免维护路径是给 Agent 配 `-server-fingerprint`（按部署身份指纹校验，与 SAN 无关）。

---

## 快速开始

```bash
cd scripts/signcert

# 1) 最常见的用法：按本机公网 IP 重签服务端证书
go run .

# 2) 顺便把证书路径写进服务端配置（重启服务端即生效）
go run . -server-config ../../mocenter-server.json

# 3) 明确指定地址（推荐：把 Agent 实际会用的 IP 和域名都写上）
go run . -ip 82.156.7.85 -dns mocenter.example.com

# 4) 只看看会解析出哪些地址，不签发
go run . -print
```

首次运行时若 `-dir` 下没有 CA，会自动创建一套（`ca.crt` / `ca.key`）。

输出示例：

```
[ip] 探测到公网 IPv4: 82.156.7.85
[ip] 本机网卡地址: 10.0.0.5

== 将写入服务端证书 SAN ==
   IP : 10.0.0.5, 127.0.0.1, 82.156.7.85, ::1
   DNS: mocenter.example.com
[ok] 服务端证书: certs/server.crt
[ok] 服务端私钥: certs/server.key
```

---

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-dir` | `certs` | 证书输出目录（同时是 CA 的读取位置） |
| `-ip` | 空 | 服务端 IP，逗号分隔可多个；**留空=自动探测公网 IP** |
| `-dns` | 空 | 服务端域名，逗号分隔可多个（Agent 若用域名连接，必须加） |
| `-auto` | `true` | 未指定 `-ip` 时是否自动探测公网 IP |
| `-local-ips` | `true` | 是否把本机网卡地址加入 SAN（内网直连调试用） |
| `-loopback` | `true` | 是否把 `127.0.0.1` / `::1` 加入 SAN |
| `-extra-ip` | 空 | 额外附加 IP（如 `0.0.0.0`、其他网卡地址） |
| `-client` | `false` | 同时用同一 CA 签发 Agent 客户端证书 |
| `-server-config` | 空 | 生成后把 `tls_cert` / `tls_key` / `ca_cert` 写入服务端配置 |
| `-agent-config` | 空 | 生成后把 `client_cert` / `client_key` / `ca_cert` 写入 Agent 配置（并置 `tls:true`） |
| `-renew-ca` | `false` | 重新生成 CA（原 CA 会备份为 `*.bak-<时间>`；**已分发的 Agent 客户端证书会全部失效**） |
| `-cert-out` / `-key-out` | `<dir>/server.crt` / `.key` | 自定义服务端证书 / 私钥输出路径 |
| `-print` | `false` | 只预览解析到的地址与 SAN，不签发 |
| `-timeout` | `5s` | 公网 IP 探测超时 |

> 关掉自动探测：`-auto=false -local-ips=false -loopback=false`，此时 SAN 只含 `-ip` / `-dns`。

---

## 与 scripts/gencerts 的关系

| 工具 | 职责 |
|---|---|
| `scripts/gencerts/` | 证书**全套**管理：CA、客户端证书、写回两侧配置；服务端证书同样支持自动探测公网 IP |
| `scripts/signcert/` | 专注**服务端证书的地址解析与签发**（换 IP / 换域名后快速重签） |

两者**同用一套 CA**（默认都在 `certs/`）：
`gencerts` 生成过 CA 后，直接 `signcert` 重签即可，Agent 侧 `client.crt` / `ca.crt` 不用动。

---

## 换地址 / 换域名后的标准操作

```bash
# 服务端：按当前实际地址重签（同一 CA）
cd scripts/signcert && go run . -ip <新公网IP> -dns <域名>

# 重启服务端（证书只在启动时加载）
systemctl restart mocenter-server     # 或前台重启

# Agent 侧：无需改动（同一 CA，只换了服务端的 server.crt/server.key）
```

只有 **CA 也换了**（`-renew-ca`）才需要重新分发 `ca.crt` 与 `client.crt` 给所有 Agent。

---

## 常见问题

**Q1：Agent 报 `certificate is valid for ..., not <地址>`**
SAN 里没有 Agent 用的那个地址。用 `-ip`（IP）或 `-dns`（域名）把它加进去重签，重启服务端即可。
建议 IP 和域名**一次都写上**，避免换个连法又失败。

**Q2：探测不到公网 IP（内网机器 / 出口被拦）**
提示会打印 "未能探测到公网出口 IP"，此时只用网卡与回环地址。请显式指定：
`go run . -ip <公网IP> -auto=false`。

**Q3：私钥会被提交到 git 吗？**
`.gitignore` 已忽略 `certs/`、`*.key`、`*.crt`、`*.pem`。工具写出的文件权限为 `0600`。

**Q4：签发后服务端日志仍报旧 SAN？**
证书在服务端**启动时**加载，替换文件后必须重启服务端进程。
