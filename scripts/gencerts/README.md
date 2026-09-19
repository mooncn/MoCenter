# MoCenter 证书生成工具（gencerts）

用 Go 标准库生成/重签 MoCenter 的自签名证书（mTLS），**Windows 无需安装 openssl**。
用于给「服务端（server）↔ Agent」之间的 TLS/mTLS 加密通道签发证书，服务端与客户端证书**分开生成**。

---

## 背景：为什么需要

- Server 与 Agent 之间的长连接默认明文。启用加密后：
  - 服务端加载 **server.crt / server.key**，并配置 **ca.crt**（用于校验客户端证书，开启双向认证）；
  - Agent 加载 **client.crt / client.key / ca.crt**。
- mTLS 会校验「服务端证书的 SAN」与 Agent 连接时使用的 IP/域名是否一致，所以生成服务端证书时必须指定**真实 IP 或域名**。
- 用同一个 CA（ca.crt/ca.key）签发所有证书，Client/Server 相互信任。

---

## 前置条件

- 已安装 Go（1.16+，建议 1.22+）。Windows 无需额外安装 openssl。
- 首次使用先在项目根目录初始化普通执行方式即可，本工具单独一个 Go 模块：

```bash
cd scripts/gencerts
```

---

## 快速开始（推荐）

一条命令完成：生成全新证书 + 自动写入两侧配置文件。

```bash
# -ip 留空时会自动探测本机公网 IP（含 IPv6）
go run . all \
  -server-config ../mocenter-server.json \
  -agent-config ../mocenter-agent.json
```

作用：

1. 生成 `certs/` 下的 `ca.crt`、`ca.key`、`server.crt`、`server.key`、`client.crt`、`client.key`；
2. server 证书 SAN 包含**自动探测到的公网 IP** + 本机网卡地址 + 回环地址
   （也可用 `-ip` / `-dns` 显式指定，例如 `-ip 192.168.123.196 -dns test.mocenter.local`）；
3. 把证书**绝对路径**写进两侧配置（不覆盖已有字段）：
   - 服务端配置：`tls_cert` / `tls_key` / `ca_cert`
   - Agent 配置：`client_cert` / `client_key` / `ca_cert`，并自动置 `tls: true`

生成后直接启动即可使用：

```bash
# 服务端（自动读取 config 中的证书路径）
./mocenter-server.exe

# Agent（同样从配置读取客户端证书）
./mocenter-agent.exe -server 192.168.123.196:9000
```

---

## 子命令

| 子命令 | 说明 |
|---|---|
| `ca` | 生成/覆盖 CA（`ca.crt`、`ca.key`）。仅首次需要 |
| `server` | 用现有 CA 签发**服务端**证书（`server.crt`、`server.key`），SAN 视 `-ip/-dns` |
| `client` | 用现有 CA 签发**客户端**证书（`client.crt`、`client.key`） |
| `all` | 依次执行 `ca` + `server` + `client`（推荐一键） |

> 重复运行 `server`/`client` 会用现有 CA **重签**。此时 Agent 侧的 `client.crt`、`ca.crt` 无需更换，只需服务端替换 `server.crt` / `server.key`。

---

## 参数说明

| 参数 | 默认 | 说明 |
|---|---|---|
| `-dir` | `certs` | 证书输出目录 |
| `-ip` | 空（自动探测公网 IP） | 服务端真实 IP，逗号分隔可填多个（`server`/`all` 使用），写入 SAN |
| `-no-auto` | `false` | 不自动探测公网 IP（仅用 `-ip`/`-dns` 指定的地址） |
| `-dns` | 空 | 服务端域名，逗号分隔可填多个，写入 SAN |
| `-server-config` | 空 | 生成后把 `tls_cert/tls_key/ca_cert` 绝对路径写入**服务端**配置文件 |
| `-agent-config` | 空 | 生成后把 `client_cert/client_key/ca_cert` 绝对路径写入 **Agent** 配置文件，并置 `tls:true` |

子命令位置固定为第一个参数，旗标跟在后面：

```bash
go run . server -ip 1.2.3.4 -dns a.com,b.com -dir certs
go run . client -dir certs
```

---

## 分步使用（手动）

如果不想自动写配置，可以手动分步生成、再自行填写：

```bash
# 1. 生成 CA（只需一次）
go run . ca

# 2. 为服务端签发证书（按服务端真实 IP/域名；省略 -ip 则自动探测公网 IP）
go run . server -ip 192.168.123.196 -dns test.mocenter.local
go run . server          # 等价于自动探测公网 IP（本机网卡与回环地址始终写入）

# 3. 为 Agent 签发客户端证书
go run . client
```

输出参考：

```
[ok] CA:     .../ca.crt .../ca.key
[ok] Server: san ip=[127.0.0.1 0.0.0.0 192.168.123.196] dns=[mocenter-server test.mocenter.local]
[ok] Client: .../client.crt .../client.key
```

---

## 配置文件内证书路径的优先级

工具只负责**写入**配置文件；运行时读取采用以下优先级（见 server / agent 的 `main.go`）：

1. **命令行显式参数**（如 `-tls-cert`、`-client-cert`、`-ca-cert`）优先；
2. 其次读取配置文件（`mocenter-server.json` / `mocenter-agent.json`）；
3. 再次是环境变量默认值。

并且：**任何显式传入的命令行参数都会被自动保存回配置文件**，下次可直接读取，无需重复传参。

---

## 常见问题

**Q1：Agent 报 `tls: failed to verify certificate: x509: certificate is valid for ..., not <IP>`**
服务端证书 SAN 未包含该地址。重签服务端证书并重启服务端即可（同一 CA，Agent 侧无需改动）：

```bash
go run . server                       # 自动按公网 IP 重签
go run . server -ip <IP> -dns <域名>   # 或显式指定，建议 IP 与域名都写
```

只想重签服务端证书时也可用更专注的 `scripts/signcert`（默认同样自动探测公网 IP）。

> 注意：证书只在服务端**启动时**加载，替换 `server.crt`/`server.key` 后必须重启服务端进程。

**Q2：证书私钥会被误提交到 git？**
`.gitignore` 已忽略 `certs/`、`*.key`、`*.crt`、`*.pem`，不会误入版本库。

**Q3：想只加密、不做双向认证（单向 TLS）？**
服务端不配置 `ca_cert`（`-ca-cert`），Agent 使用 `-tls -insecure`（或带 `-ca-cert ca.crt` 做服务端证书校验）即可。gencerts 仍照常生成证书。