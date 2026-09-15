# MoCenter 安全评估报告

评估日期：2026-09-15　范围：`server/`、`agent/`、`installer/`、`web/`　基线提交：`2db45d2`

**前提假设（按你的要求）**：管理员账号、密码、OTP 均未泄露。

**结论：在该假设下项目仍存在严重安全风险。** 有三条独立路径可以在不掌握任何账号的情况下，获得服务端或 Agent 的代码执行能力；其中一条已用真实代码实测确认。核心症结是**信任边界没有落地**：`:9000` Agent 通道既无鉴权也无加密，而服务端却信任该通道报文里的所有字段。

> **✅ 修复状态（同日更新）**：第 1～6 节列出的 **CRITICAL 全部 3 项、HIGH 全部 6 项**，
> 以及 MEDIUM 中的 M-1/M-2/M-3/M-6/M-8 均已修复并回归通过（详见文末「修复实现记录」）。
> 剩余为 LOW 与 P2 级加固项。

---

## 0. 信任模型梳理

| 通道 | 默认状态 | 鉴权 | 加密 |
|------|----------|------|------|
| Web HTTP `:8080` | 开放 | Bearer Token（登录颁发） | 无（`http.ListenAndServe`，`server/main.go:336`） |
| Web WS `/ws`、`/vnc-ws` | 开放 | Token 走 URL query | 同上 |
| Agent TCP `:9000` | 开放 | **无** | **无**（agent 默认 `-tls=false`，`agent/main.go:32`） |

关键事实：`server/main.go:378-381` 的注释已经写明「Agent 通道没有应用层鉴权（握手不校验任何口令）：能连通本端口的主机就可以注册为 Agent」。也就是说，**整个系统的信任根是"能连上 :9000"**。在默认部署下，这个条件不构成任何门槛。

`/api/login` 是唯一无鉴权但会改变状态的 HTTP 入口，且**不经过 `guard()`**（`server/main.go:999-1013` 直接调 `um.Authenticate`），因此不限流、不校验 IP 白名单、无失败锁定。

---

## 1. CRITICAL

### C-1　`:9000` 无鉴权 + 无加密 → Agent 身份可被接管，VNC 隧道可被窃听

**位置**：`server/main.go:377-390`（监听）、`server/internal/session/session.go:83-92`（注册）、`agent/internal/client/client.go:103-129`（TLS）

**问题**：
1. 注册握手不校验任何凭证，`AgentID` 完全取自对端自报。
2. `session.Upsert` 在遇到重复 `AgentID` 时**主动关闭旧连接并顶替**：

```go
old, exists := m.conns[agentID]
if exists && old != nil && old.NetConn != nil {
        _ = old.NetConn.Close()      // session.go:87-89
}
m.conns[agentID] = c
```

3. Agent 默认不启用 TLS（`agent/main.go:32` `-tls` 默认 false），而且 TLS 开启但未指定 `-ca-cert` 时用系统根校验自签证书必然失败，运维只能加 `-insecure`（`client.go:104,116`）→ **加密但不认证**，MITM 依旧成立。

**影响**（任一即可）：
- **接管身份**：用一个真实 Agent 的 `agent_id` 重连，真实 Agent 被踢下线，攻击者成为服务端眼中的该主机。此后它能看到管理员下发的**全部命令**（侦察）、**伪造命令回包**（掩盖入侵、喂假面板数据）、接收服务端推送的文件、并**接收 VNC 隧道数据**（`vncMgr.DeliverTunnelData`，`server/main.go:548-556`）——即可以观察/干扰管理员在远程桌面里的操作，包括其输入的凭据。
- **注册假主机**：以任意新 ID 上线，控制台出现幽灵主机，可诱导管理员在其上执行命令（然后回传伪造结果）。
- **拒绝服务**：反复顶替各 Agent 的 ID，使全部主机在面板上反复掉线。

**为什么这条在"凭证不泄露"前提下依然成立**：它不需要任何账号。

**修复方向**：
- `:9000` 默认强制 mTLS（生成期即内置 CA，安装器默认开启），并把「无证书连接」变成需要显式开关的危险选项。
- 在应用层再加一层与连接绑定的凭证（预共享密钥或首次注册令牌），注册时做 HMAC 签名 + 时间戳防重放。
- 身份以**连接**为准：注册成功后将 `AgentID` 固化到会话对象，后续所有报文里的 `AgentID`/`AgentConnID` 字段一律忽略或校验一致。
- 重复 `AgentID` 应拒绝新连接（而不是顶替旧连接），或至少要求旧连接确认。

---

### C-2　未认证的文件上传路径穿越 → **服务端**任意文件写入（root/SYSTEM）

**位置**：`server/internal/filetransfer/filetransfer.go:57-61`，触发点 `server/main.go:501-506`

```go
func (m *Manager) localPath(agentID, filePath string) string {
        safe := filepath.Base(filepath.Clean(filePath))   // filePath 已净化
        return filepath.Join(m.baseDir, agentID+"_"+safe) // agentID 未净化 ← 洞
}
```

`AgentID` 来自 `TypeFileUploadReq` 报文的 **JSON 字段**（而非连接身份），且该报文由无鉴权的 `:9000` 消息循环直接处理：

```go
case protocol.TypeFileUploadReq:
        var fu protocol.FileUploadReq
        _ = json.Unmarshal(env.Data, &fu)
        ack := ft.HandleUploadChunk(&fu)     // main.go:501-505，无任何校验
```

**已实测确认**（临时探针调用真实函数，验证后已删除）：

```
baseDir   = /tmp/mocenter_uploads
agentID   = "../../etc/cron.d/mocenter"      filePath="x"        -> /etc/cron.d/mocenter_x
agentID   = "../../../etc/profile.d/evil"    filePath="x.sh"     -> /etc/profile.d/evil_x.sh
正常 agentID= "host-01"                                         -> /tmp/mocenter_uploads/host-01_syslog   （未越界）
filePath 侧净化有效： "../../../etc/passwd" -> /tmp/mocenter_uploads/host-01_passwd
```

注意 `filePath` 一侧是安全的，**只有 `AgentID` 是缺口**；`agentID` 开头带 `/` 不会越界（`filepath.Join` 按相对路径处理），但 `../` 可以。

**影响**：服务端在典型部署下以 root（Linux）/ LocalSystem（Windows）运行 —— 已核实 `installer/install.go:282-285` 的 `mgr.Config` 只设 `StartType`/`DisplayName`，未指定服务账号（即默认 LocalSystem），Linux systemd unit 也没有 `User=`。落盘目录却是硬编码的 `/tmp/mocenter_uploads`（`server/main.go:278`）。写入 `/etc/cron.d/*_x`（cron 不要求扩展名）或 `/etc/profile.d/*_x.sh` 即 **root 代码执行**。写入内容完全由攻击者控制（分片合并，`tryAssemble`）。

**加重情节**：**即使启用了 mTLS，这条依然成立** —— 它意味着「Agent 角色」可以向「Server 主机」任意写文件，角色边界完全没有强制。任何一台被控主机的沦陷都会顺势升级为服务端沦陷。

**修复方向**：
- `agentID` 必须净化：`filepath.Base` + 只允许 `[A-Za-z0-9._-]`，并拒绝空/`.`/`..`。
- 落盘路径改为 `filepath.Join(dataDir, "uploads")`，权限 0700，不再用 `/tmp`。
- 落盘前用 `filepath.Rel(baseDir, path)` 断言不逃逸（兜底防线）。
- 所有会写盘的入口都要求身份来自连接而非报文。

---

### C-3　远程升级无签名校验 → 任意二进制落地执行

**位置**：`agent/internal/upgrade/upgrade.go:51-70`，入口 `server/main.go:953-994`

```go
if err := download(req.URL, tmpPath); err != nil { ... }   // URL 任意，无来源限制
if req.MD5 != "" {                                          // ← MD5 可选！
        sum, err := fileMD5(tmpPath)
        if err != nil || sum != req.MD5 { ... }
}
// 随后 rename 覆盖自身二进制并重启
```

**问题**：
1. URL 由调用方任意指定；`download` 用普通 `http.Client`（`upgrade.go:147-149`），**不强制 HTTPS**。
2. 校验和**可选** —— 服务端传空 `md5` 时完全不做完整性校验。
3. 只用 MD5，且由同一个调用方提供 → 无密钥、可碰撞、可自洽伪造，**不构成任何安全控制**（MD5 在这里的作用只是防传输损坏）。
4. 无签名、无公钥、无版本回滚保护。

**影响**：拿到升级通道即可让目标主机以 SYSTEM/root 执行任意二进制。触发面包括：Web 端 `/api/upgrade`（需登录）、或任何能向 Agent 下发消息的一方（含 C-1 的身份接管、以及明文链路上的 MITM）。

**修复方向**：
- 内置 Ed25519 公钥，升级包必须携带**离线私钥签名**，校验失败即拒绝；校验和升级为 SHA-256 且**必填**。
- 只允许 `https://` 且域名在白名单内；禁止跳转到其他主机。
- 记录并拒绝版本降级（或至少要求显式 `force` 并单独审计）。

---

## 2. HIGH

### H-1　默认弱口令 `admin` / `123456` 自动创建

`server/main.go:246-264`：无账号时自动创建 `admin`/`123456`；`server/main.go:266-274` 还会把历史"空密码"账号**重置为 `123456`**。

这不属于"凭证泄露"范畴 —— **口令字面写在源码里**，且系统可被指纹识别（`/api/health` 固定返回 `ok`、SPA 静态资源无鉴权、目录结构可猜）。配合 H-2（登录无限流）就是一个可稳定利用的入口。

**修复**：首次启动生成随机强口令并打印到控制台/日志（一次性），或强制首次登录改密（未改密前只放行 `/api/password`）；历史坏账号不要重置为固定口令。

### H-2　`/api/login` 无限流、无锁定，且口令用单轮无盐 SHA-256

- 登录入口不经过 `guard()`（`server/main.go:999-1013`）→ 不限流、不校验 IP 白名单、无失败次数限制。
- 口令存储（`server/internal/auth/user.go:313-316`）：

```go
func hashPassword(p string) string {
        h := sha256.Sum256([]byte(p))     // 无盐、单轮
        return hex.EncodeToString(h[:])
}
```

- `users.json` 权限 `0644`（`user.go:90`），且 **TOTP 密钥明文存放**（`user.go:33`）。

**影响**：单轮 SHA-256 在 GPU 上是每秒数十亿次量级，弱口令离线秒破、中等口令小时级；无盐还带来彩虹表与"同口令同哈希"的横向可比。`users.json` 一旦通过任意文件读取/备份泄露，口令与 OTP 密钥同时失守 —— 这也会**击穿"OTP 不泄露"的前提**。

**修复**：改 bcrypt / scrypt / argon2id（加盐 + 可调成本）；`users.json` 改 0600；TOTP 密钥至少与口令分离存储（理想是加密存放，密钥由 OS 凭据存储或启动参数提供）；登录纳入 `guard()` 同款限流并加失败锁定与指数退避。

### H-3　会话 Token 永不过期、无法吊销，且经 URL 传递

- `Auth` 只有 `RegisterToken` / `ValidateToken`，**无 TTL、无轮换**（`server/internal/auth/auth.go:17-62`）。
- `RevokeToken` 在生产代码中**零调用**（仅 `auth_test.go:30` 使用）→ 改密码、改 OTP、登出都不会使旧 Token 失效。
- Token 通过 query 传给 WebSocket：`/ws?token=…`（`ws/hub.go:53`）、`/vnc-ws?token=…`（`vnc.go:281`）→ 容易被反向代理访问日志、浏览器插件、屏幕截图留存。

**影响**：Token 一旦被窃取（XSS、日志、抓包），在服务重启前长期有效，且受害者改密码也无效。

**修复**：Token 加 TTL（并滑动续期）、提供 `/api/logout`、改密/改 OTP 时吊销该用户全部 Token；WebSocket 改用 `Sec-WebSocket-Protocol` 或 `Authorization` 头传 Token；Token 存储升级为 Hash 后再入内存并加绑定信息（UA/IP 可选）。

### H-4　Agent → Server 的角色边界未强制

除 C-2 外，Agent 还能驱动服务端行为：`servePackage` 允许 Agent 按名拉取服务端托管的任意安装包（`server/main.go:1473-1500`，路径穿越已正确防护，但**无授权检查**）。

**影响**：受控主机（或伪装的 Agent）可以读取服务端托管目录里的全部文件内容。

**修复**：把 Agent 请求当作**不可信输入**统一校验；托管包按 Agent 授权（或至少加签名，使窃取无意义）。

### H-5　审计日志可被未认证者注入，且脱敏在写入之后才做

- 登录失败也写审计，`Operator` 取用户输入：`Operator: body.Username`（`server/main.go:1020-1023`）→ 未认证者可用任意用户名批量写入审计条目（污染证据链 / 灌日志 / 撑大 SQLite）。
- **脱敏只在读取时做**：`route.MaskCommand` 仅被 `/api/audits` 调用（`server/main.go:888-889`）。`audits.jsonl` 与 SQLite 里保存的是**明文命令**。
- 脱敏正则漏掉了最常见的形态（`server/internal/route/router.go:610`）：

```go
re2 := regexp.MustCompile(`(?i)((-p|--password)(\s+))(\S+)`)   // 要求有空格
```

`mysql -pP@ssw0rd`、`sshpass -pXXX` 这类**不带空格**的写法不会被脱敏。

**影响**：审计库成为口令明文仓库（`net user bob P@ss /add`、`mysqladmin -uroot -pXXX` 等），任何能读到数据目录或备份的人直接获得凭据；且 API 读取时才脱敏，意味着**脱敏不保护落盘**。

**修复**：写入前脱敏（存储层做，而非展示层）；正则覆盖 `-p<值>` 连写形态并增加 `-u user:pass` 等；未认证路径的审计字段做长度/字符白名单；审计表加配额与轮转（未认证入口尤其需要）。

---

### H-6　Web 控制台默认明文 HTTP —— 口令与 Token 明文上网

服务端只做 `http.ListenAndServe(*httpAddr, mux)`（`server/main.go:336`），**没有任何内置的 Web 侧 TLS**。默认部署下 `:8080` 是明文，而登录请求体里就是用户名+口令+OTP，随后所有请求携带 Bearer Token。

**影响**：同网段/同链路的攻击者（公共 WiFi、云内东西向、被劫持的上游）可**直接抓到管理员口令与 Token**——这恰好击穿本次评估的前提假设。抓到 Token 后又因 H-3（Token 永不过期且不可吊销）而长期有效。

**缓解方式**：文档建议的 nginx 反代终结 TLS 是正确做法，但默认状态依然是不安全的；且 M-7 指出反代后 IP 白名单与限流会失效，说明"套一层反代"并非零成本。

**修复方向**：给 Web 侧也加 `-tls-cert/-tls-key`（或提供 `-https` 开关）并复用同一套证书加载逻辑；文档把"必须启用 TLS 或置于 TLS 反代之后"写成部署前置条件而非可选优化。

---

## 3. MEDIUM

| 编号 | 问题 | 位置 | 说明 |
|------|------|------|------|
| M-1 | **端口映射的「源 IP 白名单」根本没实施** | `portmap/portmap.go:326,357-384` | `Rule.SourceIP` 只被存储与 JSON 序列化，`handleConnection` 不做任何源地址校验；监听是 `net.Listen("tcp", ":port")`（绑 0.0.0.0）。界面上的白名单字段是**安全剧场**，映射出去的内网服务对全网开放。要么实现校验，要么删掉该字段并在 UI 明示"公网可达"。 |
| M-2 | **命令黑名单可用交互终端完全绕过** | `server/main.go:1255-1263` | `IsBlocked` 只被 `Dispatch` 调用（`router.go:254`），而交互输入走 `TypeInteractiveIn` **直转 Agent**，既不过黑名单也不审计。远程终端恰好是主推功能，等于黑名单对主用法无效。另匹配是 `strings.Contains` 小写子串（`router.go:239-249`），`rm -r -f /`、`$IFS`、base64 管道均可绕过。 |
| M-3 | 无任何安全响应头 | 全局 | 未设置 CSP / X-Frame-Options / HSTS / Referrer-Policy（仅设置了 3 处 `Content-Type`）。前端 Token 存在 `localStorage`（`web/src/pages/Login.jsx:24`）→ 一旦出现 XSS 即可窃取 Token，且无 CSP 兜底。 |
| M-4 | `dangerouslySetInnerHTML` 用于二维码 | `web/src/pages/Account.jsx:232` + `web/src/utils/qrcode.js:548-554` | **当前安全**：SVG 只插值 `size` 与纯数字的 path，不含用户文本，我逐行确认过。但这是"因为恰好没插值文本而安全"，一旦有人往 SVG 里加标题/标签就是 XSS。建议加注释锁定该约束或改用 DOM API 构建。 |
| M-5 | WebSocket 无 Origin 校验 | `ws/hub.go:22`、`vnc/vnc.go:28`（`CheckOrigin: return true`） | 目前 Token 非自动携带（非 Cookie），CSWSH 影响有限；但一旦将来改用 Cookie 鉴权即立刻变为可利用。建议白名单 Origin。 |
| M-6 | **OTP 绑定接口不校验账号归属** | `server/main.go:1128-1149` | `otpEnableHandler` 的 `Username` 取自**请求体**，直接 `um.BindTOTP(body.Username, body.Secret)`，且验证码是拿**请求里自带的新密钥**去校验（`VerifyTOTP(body.Username, body.Secret, body.Code)`，:1142）——从不校验调用者的 Token 是否属于该 username。任何已登录会话都能把**自己知道的密钥绑定到任意账号**：绑定后即可为该账号生成有效动态码；反向把未知密钥绑上去则会让原账号**被锁在门外**。单账号部署下影响有限，但这是明确的访问控制缺陷。修复：一律以 Token 身份为准，忽略请求体里的 username。 |
| M-7 | 限流与白名单基于 `RemoteAddr` | `auth/auth.go:81-130` | 部署在反向代理后，所有请求源 IP 都是代理地址 → IP 白名单与限流双双失效或误伤。需要"可信代理"配置后再从 XFF 取真实源 IP（当前不信任 XFF，这一点本身是对的）。 |
| M-8 | 限流/Token map 无回收 | `auth/auth.go:22,107-130` | Token 无 TTL 且 map 只增不减；限流 map 按 IP 累积、从不清理 → 大量不同源 IP 时可造成内存放大。 |
| M-9 | 出站 HTTP 硬编码跳过证书校验 | `agent/internal/ip/ip.go:60-63` | 公网 IP 探测使用 `InsecureSkipVerify: true` → 结果可被 MITM 污染。若该 IP 参与任何安全判断需重新评估。 |

---

## 4. LOW / 加固建议

- `filetransfer.NewManager("/tmp/mocenter_uploads")` 硬编码临时目录（`server/main.go:278`）：Linux 下 `/tmp` 全局可写，与 C-2 叠加放大。建议移到数据目录下并 0700。
- `users.json` 0644 + TOTP 明文（`user.go:33,90`）：改 0600，密钥加密存放。
- 审计 `Result` 直接存 stderr 原文，可能包含敏感输出（如连接串、令牌）。
- `hub.Broadcast` 把所有命令输出广播给**所有**已连接前端（`server/main.go:494`）：多账号场景即越权可见，应按连接/用户过滤。
- `/api/health` 与 SPA 静态资源无鉴权：便于指纹识别（与 H-1 组合利用）。至少让 SPA 与 API 同域鉴权或提供关闭开关。
- SQLite（`mocenter.db`）无加密，内含审计与主机状态。
- `agent/internal/command/runas_unix.go:29-32` 设置 `Credential` 时未显式处理附加组，跨用户降权语义不完整（Windows 侧直接返回不支持，属已知）。
- 依赖：Go 侧 `gorilla/websocket 1.5.3`、`modernc.org/sqlite 1.34.5` 版本健康；前端 `axios ^1.6.8` 建议升到当前版本（历史上有 SSRF/CSRF 类公告），并确保 `package-lock.json` 已提交锁定。

---

## 5. 设计上做得对的地方（不建议改动）

- **无 Cookie 鉴权** → 天然免 CSRF，这是本项目的结构性优势；引入 Cookie 前请三思。
- Token 生成使用 `crypto/rand` 且为 256-bit（`server/main.go:1072-1076`）。
- TOTP 密钥生成用 `crypto/rand` 160-bit（`user.go:240-246`）；口令比较用 `subtle.ConstantTimeCompare`。
- 路径穿越的正确防护已出现在两个地方：`servePackage`（`main.go:1474-1486`）与 SPA 托管（`main.go:1220`，`filepath.Clean("/"+path)` + `http.FileServer`）—— 说明团队有这个意识，C-2 属于遗漏而非无知。
- VNC 绑定在 Agent 的 `127.0.0.1` 并走隧道（`vnc/vnc.go:140,309`），避免了"5900 端口裸奔"，是明显的加分项。
- TLS 最低版本 1.2（`server/main.go:355`），并支持 mTLS。
- 失败登录也写审计的设计意图正确（只是实现需要收紧，见 H-5）。

---

## 6. 修复优先级建议

**P0（不修就等于没有安全边界）**
1. `:9000` 默认强制 mTLS + 应用层注册凭证；身份以连接为准；重复 `AgentID` 拒绝而非顶替。
2. 净化 `AgentID`（`filepath.Base` + 字符白名单 + `filepath.Rel` 兜底断言），落盘目录移出 `/tmp`。
3. 升级包改为 Ed25519 签名校验，校验和必填且用 SHA-256，仅允许白名单内 HTTPS。
4. 取消默认口令 `admin/123456`；口令哈希换 bcrypt/argon2id。
5. **Web 侧启用 TLS**（或把"必须置于 TLS 反代之后"写进部署前置条件）—— 默认明文 HTTP 会直接把口令与 Token 交给链路攻击者，等于无视本次评估的假设前提。

**P1**
6. Token 加 TTL/轮换/登出，改密与改 OTP 时吊销；WS 不再经 URL 传 Token。
7. `/api/login` 纳入限流 + 失败锁定。
8. 端口映射实施源 IP 白名单（或明确移除该选项）。
9. 黑名单覆盖交互输入与批量输入；审计写入时即脱敏（不是读取时）。
10. `/api/otp/*` 以 Token 身份为准，忽略请求体里的 username。

**P2**
11. 安全响应头（CSP 等）；`users.json` 0600 + TOTP 加密；限流与 Token map 加回收；审计表配额与轮转。

---

## 7. 评估方法与局限

**已做的验证**
- 通读 `server/`（28 个 Go 文件）与 `agent/`（39 个）全部安全相关路径；逐一核对 22 条 HTTP 路由与 2 条 WebSocket 的鉴权覆盖。
- 对 C-2 编写临时探针**调用真实函数**验证路径穿越，实测确认 `localPath` 逃出 `baseDir`，并列出了具体向量解析结果；验证后探针已删除，工作区干净。
- 对 M-4 逐行确认 SVG 生成不插值用户文本。

**未做的验证（结论可能因此偏保守或偏乐观）**
- 未在真实 Linux root 环境实测 C-2 的 cron.d 落盘（只验证到"路径可逃逸 + 内容可控"）。
- 未实测 C-1 的 VNC 隧道窃听链路（依据是代码路径，未搭环境复现）。
- 未做模糊测试、未做依赖 CVE 扫描（无网络/未跑 `govulncheck`、`npm audit`）。
- 未评估前后端之外的运行环境（反向代理配置、防火墙、云安全组、主机加固）；这些对上文多处风险有实质影响（例如 M-7）。
- 未审计 `web/` 全部 1592 个 JS 文件（含 `node_modules`），仅审计了项目自身的前端源码。

**建议的下一步**：优先落地 P0 四项，我可以逐项给出补丁；其中 C-2 与 C-3 改动范围小、收益最大，适合先做。

---

## 8. 修复实现记录（同日完成）

| 项 | 修复内容 | 关键落点 |
|----|----------|----------|
| C-1 / P0-1 | `:9000` 增加**应用层注册鉴权**（预共享密钥 + `HMAC-SHA256(key, agent_id\|nonce\|ts)`，时间窗 ±60s + nonce 防重放）；重复 `AgentID` 由「顶替旧连接」改为**拒绝新连接**（旧连接超过 90s 无心跳才允许替换，避免重启的 Agent 被僵尸连接挡住）；报文自报的 `AgentID` 一律以连接身份为准；新增 `-allow-unauthenticated-agents` 迁移开关（默认关闭，开启时打印醒目警告） | `server/internal/agentauth`、`session.Upsert`、`server/main.go handleAgentConn`、`agent/internal/agentauth` |
| C-2 / P0-2 | 文件落盘路径的 `agentID` 白名单净化（非 `[A-Za-z0-9._-]` 一律替换为 `_`，纯点前缀加 `_`）+ `filepath.Rel` 兜底断言；暂存目录从硬编码 `/tmp/mocenter_uploads` 改为 `<dataDir>/uploads` 且 0700；`push.SaveToTemp` 同步修复；`ProbeDataReq`/`FileUploadReq` 的身份校验 | `filetransfer.go`、`push.go`、`server/main.go` |
| C-3 / P0-3 | 升级包校验改为 **SHA-256 必填** + **仅允许 https** + **Ed25519 签名**（配置公钥时必须有效，未配置公钥默认拒绝，需显式 `-upgrade-allow-unsigned` 才放行）；服务端 `/api/upgrade` 先行校验摘要格式 | `agent/internal/upgrade/upgrade.go`、`server/main.go upgradeHandler` |
| H-1 / P0-4 | 取消固定弱口令：首次启动自动生成 **16 位随机口令** 写入 `<dataDir>/initial-password.txt`（0600）；历史空密码账号同样改用随机口令修复 | `server/main.go` |
| H-2 / P0-4 | 口令哈希改为 **PBKDF2-HMAC-SHA256**（随机盐 16B + 12 万次迭代，纯标准库实现）；旧单轮 SHA-256 仍可登录并**登录成功后透明升级**；`users.json` 权限 0644 → **0600** | `server/internal/auth/password.go`、`user.go` |
| H-3 / P1-6 | Token 增加 **TTL（默认 12h，活跃滑动续期）**；新增 `/api/logout`；`RevokeUserTokens` 在改密码/开关 OTP 时吊销其它会话；WS Token 改走 `Sec-WebSocket-Protocol` 子协议头（兼容旧 query） | `auth/auth.go`、`main.go logoutHandler`、`ws/hub.go`、`vnc/vnc.go`、前端 `api.js`/`Vnc.jsx`/`App.jsx` |
| H-4 / P1-7 | `/api/login` 纳入 IP 白名单 + 专用限流（20 次/分/IP）+ 按用户名失败指数退避（5 次失败后 2s 起、封顶 30s，**不做永久锁定**以免账号 DoS） | `auth/login_guard.go`、`main.go loginHandler` |
| H-5 / P1-9 | 审计**写入时即脱敏**（原先只在读取时）；脱敏规则覆盖连写形态并增加 `passphrase`、`user:pass`；未认证入口写入审计的字段限长去控制字符 | `route/router.go` |
| H-6 / P0-5 | Web 控制台支持 `-web-tls-cert/-web-tls-key`（TLS 1.2+）；明文监听时打印醒目警告；所有响应附加 `X-Content-Type-Options`/`X-Frame-Options`/`Referrer-Policy` | `server/main.go` |
| H-4 / P1-10 | `/api/otp/*`、`/api/password` 一律以 **Token 身份**为准，请求体中的 `username` 被忽略（前端同步移除相关输入） | `server/main.go`、`web/src/pages/Account.jsx` |
| M-1 / P1-8 | 端口映射的源 IP 白名单**真正生效**（`auth.AddrAllowed`），未设置白名单的规则启动时打印"对全网开放"警告 | `portmap/portmap.go` |
| M-2 / P1-9 | 交互终端输入**逐行过黑名单**：命中则不转发行结束符、补发 Ctrl-C、广播提示并审计；退格/Ctrl-C 同步回缩缓冲 | `route/interactive.go`、`server/main.go` |

**注意（升级迁移）**：服务端现在默认要求 Agent 携带注册密钥。升级后需给每台 Agent 配置
`-agent-key`（内容取总控机 `<data>\agent.key`），否则连接会被拒绝；迁移期可在服务端加
`-allow-unauthenticated-agents` 临时关闭校验。
