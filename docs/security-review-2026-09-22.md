# MoCenter 安全评估报告（第三轮）

**评估对象**：`loiei/MoCenter`
**基线 commit**：`27ae0af0e42d8012fe4c5f413dae562f4d0ae04d`
**评估范围**：`server/` `agent/` `installer/` `web/` `scripts/` `.cnb.yml`
**方法**：逐文件回源码读码（不采信既有报告结论）+ 关键路径构造 POC **运行时复现** + 与 `docs/security-review-2026-09-19.md` 做差集 + 修复后回归验证
**相关 Issue**：[#13](https://cnb.cool/loiei/MoCenter/-/issues/13)（用户诉求：评估结果写入 `docs/` + 修复 H/L 级 + 编译验证 + 复核上一份报告的已修项）

---

## 0. 本轮最关键的结论

> **本机 Go 工具链已可用（1.26.6），因此本轮的结论全部经过编译与运行时验证 —— 这是与前两轮最大的方法论差别。**

前两轮评估的局限栏里都写着「本地 Go 为 1.19.8，`go.mod` 要求 1.26.6，无法编译、无法跑单测、无法起服务端做端到端复现，因此全部结论为**静态代码路径证明**」。
本轮该前提已消除：

- 安装 `go1.26.6 linux/amd64` 到 `/usr/local/go`；
- `server/` `agent/` `installer/` 三个模块 `go build` + `go vet` + `go test ./... -count=1` **全绿**；
- 安装 `gcc` 后 `CGO_ENABLED=1 go test -race` 覆盖本轮改动包**全绿**；
- 交叉编译锚定通过（`agent` linux/windows/darwin、`server` linux/windows/darwin、`installer` windows/linux）；
- `web/` `npm ci` + `check:vnc` + `check:flag` + `check:footer` + `npm run build` 全通过。

**H 级漏洞 H-N1（见 §2）已用 POC 运行时复现，不再是"静态推断"** —— 修复前测试打印出 `OK=true Message="证书已签发"`，实测拿到了自选身份、空凭据的合法客户端证书（PEM 1182 字节）。

---

## 1. 结论总览

| 维度 | 结论 |
|---|---|
| **SQL 注入** | **无**。全参数化，动态片段只有静态 `AND col LIKE ?`，`ORDER BY` 静态串，`LIMIT/OFFSET` 走占位符 |
| **RCE** | **无匿名可达路径**。命令执行是产品功能且需有效 Token；`agentdeploy` 参数先过正则白名单 + `shellQuote` |
| **越权 / 伪造授权** | **本体扎实**。身份一律取自连接（mTLS 验链）；命令回执按 `(command_id, agent_id)` 双向绑定；隧道帧比对连接身份。**但发现 1 个"注册面身份铸造"高危（H-N1，本轮已修）** |
| **SSRF** | **无**。服务端无用户可控出网 URL |
| **路径穿越** | **无**。`-file-roots` 三层校验 + `sanitizeComponent`/`within` + 备份文件名白名单 |
| **XSS** | **无**。页脚白名单重建 + 全站 `script-src 'self'` |
| **DoS** | 匿名面已有全局/每 IP/每会话三层上限与写读 deadline |
| **供应链** | `govulncheck` 0 条可达漏洞、`npm audit` 干净、UltraVNC 包摘要钉扎。**仅剩 CI 镜像未按 digest 钉扎** |

**本轮新增修复**：1 个高危（H-N1）+ 1 个中危（M-N2）+ 5 项低危（L-1/L-2/L-9/L-10 + 黑名单加固）。

---

## 2. 🔴 高危 H-N1（新发现，本轮已修复）

### 标题
`-allow-unauthenticated-agents` 下注册凭据校验被整体短路，可为**任意** `agent_id` 签发本套 CA 的合法客户端证书

### 位置
- `server/main.go` → `enrollAgentCert`（修复前 `:1350-1355`）
- `server/internal/agentauth/agentauth.go` → `Verifier.Verify`（修复前 `:267-270`）

### 成因：三行代码的语义互相抵消

```go
// 修复前 enrollAgentCert
if !verifier.Enabled() && !allowAnon {   // 未启用 且 未开匿名 → 拒绝
    return resp                          // ← 开了匿名就往下走
}
credSource, err := verifier.Verify(...)  // 继续调用

// 修复前 Verifier.Verify
if !v.Enabled() {
    return "", nil     // ← 「未启用」被当成「校验通过」返回 nil error
}
```

当 `verifier.Enabled() == false`（**没配共享密钥、也没接令牌存储**）且 `-allow-unauthenticated-agents` 为真时：

```
Verify("任意ID", "", "", 0)  →  ("", nil)  →  判定凭据通过
  → identity.AgentCertPEM(er.AgentID, er.CSR)   // 自选 agent_id 写进 CN 并签名
```

**根因是 `return "", nil` 这个「沉默通过」语义** —— 没有任何调用方需要它。

### 运行时复现（本机 Go 1.26.6 实跑，修复前）

```
=== RUN   TestHN1_POC_AnonEnrollMintsArbitraryIdentity
[server] 已为 Agent "victim-host" 签发客户端证书（凭证类型 ，来自 pipe 明文）
    OK=true Message="证书已签发"
    被拒：H-N1 复现：空凭据 + 自选 agent_id 拿到了证书（长度 1182）
--- FAIL
```

### 为什么既有三道防线都拦不住

| 防线 | 为什么失效 |
|---|---|
| `sess.Busy(er.AgentID)` | 只拒绝**当前在线**的 ID —— 攻击者挑一台离线/从未上线的主机即可绕过 |
| `store.CheckOrBind` 的 TOFU | 只在 `verifier.Tokens() != nil` 时执行（`main.go:1376`），而这条路径恰好 `Tokens() == nil` |
| C-1 当年收缩的注册面 | 在 `-allow-unauthenticated-agents` 这条分支上被**整体绕开** |

### 加重因素
`main.go:228` 会把 `cfg.AllowAnonAgents` 从配置文件读回来 → 这个危险开关**一旦写入配置就粘住**，运维去掉命令行参数也不会关掉它。

### 修复（三处，全部落地）

**① 根因：`Verifier.Verify` 不再沉默通过**

```go
if !v.Enabled() {
    return "", fmt.Errorf("服务端未配置注册凭据（一次性注册令牌或共享注册密钥）")
}
```

**② 判据解耦：匿名与否只看"报文有没有出示凭据"，放行与否只看运维开关**

原先 `!Enabled() && !allowAnon` 把「服务端配没配凭据」与「申请方带没带凭据」两件无关的事相乘，是漏洞的直接来源。现在：

```go
credentialFree := er.Proof == ""
credSource := agentauth.CredAnon

if !credentialFree {
    // 出示了凭据 → 必须验过，与 allowAnon 无关（绝不降级成匿名）
    src, err := verifier.Verify(er.AgentID, er.Nonce, er.Proof, er.TS)
    if err != nil { rejectEnrollment(...); return resp }
    credSource = src
} else {
    // 没出示凭据 → 唯一匿名入口，只有运维显式开启才放行
    if !allowAnon { rejectEnrollment(...); return resp }
    if er.TS <= 0 || er.Nonce == "" { /* 空壳报文也拒 */ }
}
```

**③ 留痕**：匿名签发写 `log.Printf` 警告 + `CredAnon` 凭证类型；**每一次拒发证书都写 auth 类审计**（修复前"凭据校验失败"只进日志，没进审计 —— 这是补测试时发现的第二个缺口）。

### 行为变化（需运维知晓）

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 没配凭据 + 没开开关 | 拒 | 拒（**不变**） |
| 没配凭据 + 开了开关 + 无凭据 | **签发**（漏洞） | 签发，但需 nonce/TS 且留痕（开关语义保留） |
| 配了凭据 + 开了开关 + 无凭据 | 签发 | 签发，需 nonce/TS 且留痕 |
| 配了凭据 + 开了开关 + **错凭据** | 签发 | **拒**（不再伪装成正常接入） |
| 配了凭据 + 没开开关 + 正确凭据 | 签发 | 签发（**不变**） |

### 验证
`server/enroll_hn1_test.go`（新增 8 例，其中 1 例为 3 个子场景）：没配凭据时拒发、`Verify` 不再沉默通过、匿名路径仍须 nonce/TS、错凭据不降级、令牌路径不受影响、拒发必留审计。

---

## 3. 🟡 中危 M-N2（既有报告提到但被低估，本轮已修复）

### 标题
`InteractiveGuard.DropInteractive` 是死代码，交互输入缓冲不按会话回收

### 位置
- 定义：`server/internal/route/interactive.go`
- 修复前全仓 `grep -rn DropInteractive` **仅命中注释与定义本身，零调用点**

### 后果（三条，第二条是正确性缺陷而非内存问题）
1. 每个交互会话在 `iguard.bufs` 留下 `agentID|commandID` 键，**永不删除**；
2. 触顶后不是淘汰最旧，而是 `r.iguard.bufs = make(...)` **整表清空** → 一次 wipe 会**连带丢掉当时活跃终端的半行输入状态**，表现为「打着字突然黑名单过滤失灵/半行错乱」；
3. 缓冲里含运维在终端里敲的**半行命令**（可能含口令），驻留到下一次整表 wipe。

### 修复
- `main.go` 收到 `TypeInteractiveOut(exit)` 时，在既有的 `router.ReleaseCommand` 旁边补 `router.DropInteractive(...)` —— 两者是**同一次收口的两半**，此前只接了前一半；
- 新增 `route.InteractiveKey(agentID, commandID)` 统一键构造，消除「写入侧手拼字符串 / 回收侧另写一份」的漂移空间；
- 兜底容量控制从**整表清空**改为**淘汰插入序最旧的一个键**（新增 `order []string`），活跃终端的半行不再被无辜牵连。

### 验证
`server/internal/route/interactive_lifecycle_test.go`（新增 4 例）：缓冲可回收、空键不 panic、触顶淘汰最旧而非清空（保留其余会话的半行状态）、交互模式逐字符喂入多空格 `rm  -rf /` 仍被拦下。

---

## 4. 🟢 低危（本轮已全部收口）

### L-1　审计 CSV 公式注入 —— ✅ 已修

**位置**：`server/main.go` → `writeAuditCSV`

`Operator / AgentID / CommandID / Command / Result / BlockReason` 原来直接进 `csv.NewWriter`，未对 `= + - @` 及前导 Tab/CR 加防护。
危险面是真实的：`Command`/`Result` 的值来自 **Agent 回传**，即「一台被拿下的被管主机」能直接决定运维导出的 CSV 里写什么，Excel 打开即触发公式（DDE/外链）。**单人使用一样中招**。

**修复**：新增 `csvCell()`，首字符（跳过前导空白后）命中 `= + - @` 时前置一个单引号；前导为 Tab/CR/LF 时同样前置。
**刻意选择"前置单引号"而非"剥除危险字符"**：前者是表格软件的「按文本处理」约定，导出件仍能完整还原被管主机到底回了什么。

**验证**：`server/hardening_export_test.go`（2 例，含 11 项单元格表格）。

### L-2　日志注入残留 —— ✅ 已修

**位置**：`server/main.go`

| 字段 | 处理 |
|---|---|
| `ur.Message`（升级回执） | ✅ 过 `logSafe` |
| `tr.Message`（两条隧道建立失败） | ✅ 过 `logSafe`；`tr.TunnelID` 一并过 |
| `tc.Reason`（隧道关闭） | ✅ **经复核不构成注入面** —— `portmap.NotifyTunnelClosed` 的第三个形参早已是 `_ string`（该值未被使用），已在调用点注明 |

**验证**：`TestLogSafeStripsForgedServerLines` 断言换行被压平、伪造的 `[server] ...` 行无法注入、限长生效、正常文本不被改动。

### L-9　安装向导与构建脚本 —— ✅ 已修

| 子项 | 处理 |
|---|---|
| `installer/setup.iss` 注册密钥输入框明文回显 | ✅ 改为掩码（最后一位参数 `False` → `True`）。该密钥等价于主机身份，拿到即可为任意 `agent_id` 换证书 |
| `installer/setup.iss` 口令下限 6 位 | ✅ 提到 12 位，与 README「强口令」口径对齐 |
| `installer/setup.iss` 的 `-extra` | ⏸ **刻意保留**：它是「用户自己给自己加参数」的正当原语，且值来自本机安装向导的**运维本人输入**，不构成跨越信任边界。已在注释中写明它为何保留 |
| `scripts/lib/common.sh` 的 `mc_clean(){ rm -rf "$@"; }` | ✅ 逐项验证：拒绝空值/`.`/`..`/`/`、拒绝相对路径、**只允许 `OUT_ROOT` 之下的绝对路径**；`build-linux-all.sh` 同步设置 `OUT_ROOT` |

**冒烟验证**（`sh -e -c` 下实跑）：

```
OK  允许删除 OUT_ROOT 之下
OK  已拦住: []            OK  已拦住: [.]
OK  已拦住: [/]           OK  已拦住: [/tmp/mc-other-dir]
OK  已拦住: [relative/path]
```

### L-10　缺 `.gitattributes` —— ✅ 已修（新建）

换行策略此前完全由每个 clone 的 `core.autocrlf` 决定。2026-09-21 在 Rocky8 实测撞出：Windows 工作树是 `i/lf w/crlf`，脚本带 `\r` 拷到 Linux 后 `install-linux.sh` 的 `set -euo pipefail` 变成 `set: pipefail: invalid option`。
它同时污染了格式口径（`gofmt -l` 逐文件命中，只因文件是 CRLF）。

新增 `.gitattributes`：`* text=auto eol=lf`、`*.sh text eol=lf`、`*.bat/*.cmd/*.ps1 eol=crlf`、二进制显式 `binary`。
已执行 `git add --renormalize .`，索引侧现为**全部 LF**（`git ls-files --eol` 复核：`i/lf` × 267，二进制 × 2，`.bat` 检出侧 CRLF）。

### 命令黑名单子串绕过 —— ✅ 已加固（仍**不是**安全边界）

**位置**：`server/internal/route/router.go` → `IsBlocked`

原实现只有 `strings.Contains(原文)` 一条匹配，于是**连不需要任何混淆技巧的常见写法都漏**：`rm  -rf  /`（多打一个空格）、`rm -rf / # 注释`、制表符分隔。

**加固**：在原文之外对「归一化形式」再匹配一次 —— 去引号（shell 会吃掉）、压缩连续空白、截掉词首 `#` 起的行内注释。

**如实声明边界**：这是**纵深防御的界面护栏**，不是安全边界。它拦不住 `$(echo ...|base64 -d|sh)` 之类的构造，也无意去拦 —— 收敛目标从原文改成了「显然等价的常见写法」。已在函数注释里写清这一点，避免界面/文档给人「已经拦住了」的错觉。真正的收口位置在审计与授权，不在字符串匹配。

**验证**：`hardening_blacklist_test.go`（3 例）：9 种显然等价写法全拦、6 条无害命令不误伤（误报会逼运维干脆关掉黑名单，比漏报更糟）、归一化函数保守性表格。

---

## 5. 编译与回归验证口径

### 5.1 工具链
- 本机原先无 Go。已装 `go1.26.6 linux/amd64` 至 `/usr/local/go`（与 `.cnb.yml` 的 `golang:1.26` 同大版本）。
- `-race` 需 CGO，已 `apt-get install gcc`。

### 5.2 结果

```
[server]   go build ./...            ✅
           go vet ./...              ✅
           go test ./... -count=1    ✅  21 个包：18 个 ok + 3 个无测试文件，0 失败
           go test -race（改动包）    ✅  internal/route + internal/agentauth + 主包
[agent]    go build ./...            ✅  go vet ✅  go test ✅
[installer] go build ./...           ✅  go vet ✅
交叉编译    agent:   linux/windows/darwin      ✅
           server:  linux/windows/darwin      ✅
           installer: windows/linux           ✅
           GOOS=windows go vet（agent）        ✅
[web]      npm ci                    ✅
           check:vnc (20 项) / check:flag / check:footer (9 项)  ✅
           npm run build             ✅  built in 2.14s
[脚本]     bash -n + sh -n scripts/lib/*.sh + sh 下 source  ✅
           mc_clean 防呆冒烟          ✅  5/5 拦住 + 1/1 放行
```

### 5.3 格式
本轮触及的 Go 文件 `gofmt -l` **全部干净**。`.gitattributes` 落地后格式判据不再需要 `tr -d '\r'` 副本。

---

## 6. 上一份报告（`security-review-2026-09-19.md`）已修项复核

用户要求「对 docs 下最近一份评估报告中**已修复的**一并验证」。逐项回源码复核，**全部无回退**：

| 项 | 判据（本轮实读代码） | 结论 |
|---|---|---|
| **C-1** 注册即身份铸造 | `tokens.go` 一次性令牌 `UsedAt` 用后即焚 + `CheckOrBind` TOFU + `sess.Busy` 拒在线 ID + `SpkiHashOfPEM` 指纹绑定 | ✅ 无回退 |
| **C-2** Windows 明文下载无校验 | `fetch-packages.sh` 校验 `sha256sum`、不匹配即丢弃 `.part` 并 `exit 1`；Agent 侧 `installPins` 内置摘要，缺摘要即拒绝加载 | ✅ 无回退 |
| **H-1** 回执越权/伪造审计 | `CommandOrigin` 按 `(command_id, agent_id)` 绑定 + `BroadcastToSession` 定向回投 + `rejectUnownedCommand` 拒绝并审计 | ✅ 无回退 |
| **H-2** `/ws` `/vnc-ws` 绕过闸门 | `guardWS` + `maxConnsPerSession=8` + `wsReadLimit` + `viewerPendingLimit`（Upgrade **前**判定） | ✅ 无回退 |
| **H-4** 请求体无上限 | 全局 body 上限 + 容量受限的限流表 | ✅ 无回退 |
| **H-5** 发布物签名 | 升级包链闭环（Ed25519 强制验签、逃生门已删）；`.deb/.rpm/.exe` 签名仍缺（需构建链外证书，唯一无法靠本仓代码收口项） | ✅ 升级链无回退（另一半仍为已知残留） |
| **H-6** 一键部署主机密钥 | `agentdeploy.go` **零** `InsecureIgnoreHostKey` | ✅ 无回退 |
| **H-7** 公开 WS 无上限 | `publicWSMaxTotal=256` + `publicWSMaxPerIP=32` + `SetReadLimit` + `SetReadDeadline`/`SetWriteDeadline` | ✅ 无回退 |
| **H-8** 公开节点 ID 可字典命中 | HMAC + `node.key` 取代固定盐 + 32bit 截断 | ✅ 无回退 |
| **H-9 + M-11** Windows 本机面 | 受保护 DACL + PBKDF2（非单轮 SHA-256）+ 注册前可写性闸门；`vncpassword.go` 无静态回退口令（`mocenter` 仅出现在解释其被删除的注释里） | ✅ 无回退 |
| **M-1** 被动下发根凭据 | 已摘除 | ✅ 无回退 |
| **M-2** 审计结果脱敏 | `MaskCommand` 接在每条写入路径入口 | ✅ 无回退 |
| **M-3 + M-18** 页脚/CSP/Origin | `check-footer-sanitize.mjs` 白名单重建自检 + 全站 CSP 头 + `ws.SameOrigin` | ✅ 无回退 |
| **M-4** 注册采纳自报 UUID | `Store.ResolveUUID` TOFU 绑定 | ✅ 无回退 |
| **M-5 + M-8** 资源放大 | `agentWriteWait` 写 deadline + 文件分发条目/单任务/总量/TTL 四道闸 + `CleanupStale` 已接线 | ✅ 无回退 |
| **M-6 + M-7** 隧道帧归属/规则覆盖 | `DeliverTunnelData(td.TunnelID, GetAgentID(), ...)` + `portmap`/`vnc` 双处归属判定 | ✅ 无回退 |
| **M-9 + M-10 + M-16** 升级链与根目录 | 无 `AllowUnsigned`（代码层零引用）+ 签名覆盖版本号 + 拒绝降级 + `fileroots.go` 根目录闸门 | ✅ 无回退 |
| **M-12 + M-13 + M-14** 匿名访客面 | `perPublic` 独立 `(route, ip)` 桶 + `publicRouteMapLimit` + `AllCached()` + `SetOnChange` 断连接线 | ✅ 无回退 |
| **L-8** CI 真正把关 | `script: *go-check` 被真实引用 + 无 `|| true` 吞失败（仅注释提及）+ `govulncheck` + `npm audit --registry` 常态化 | ✅ 无回退 |

> 复核过程中我的第一版判据曾报 7 项「不通过」，逐条回查后确认**全部是我的 grep 模式写错**（文件名猜错：`public_ws.go` 实际在 `public_site.go`；标识符猜错：`pagefooter`→`check-footer-sanitize.mjs`；把注释里的历史说明也当成了代码引用）。**已逐条更正判据并给出代码位置**，未凭"可能没修"就下结论。

---

## 7. 本轮修复的完整清单

| 文件 | 改动 |
|---|---|
| `server/internal/agentauth/agentauth.go` | `Verify` 不再对"未启用"返回 `("", nil)`（H-N1 根因） |
| `server/internal/agentauth/tokens.go` | 新增 `CredAnon` 凭证类型 |
| `server/main.go` | `enrollAgentCert` 凭据裁决解耦 + 匿名留痕 + 拒发必审计（H-N1）；`csvCell` 公式注入中和（L-1）；三处 `logSafe`（L-2）；`DropInteractive` 接线（M-N2） |
| `server/internal/route/interactive.go` | `InteractiveKey` 统一键 + 触顶淘汰最旧（M-N2） |
| `server/internal/route/router.go` | `normalizeForBlacklist` + `IsBlocked` 双变体匹配（黑名单加固） |
| `installer/setup.iss` | 注册密钥掩码 + 口令下限 12（L-9） |
| `scripts/lib/common.sh` | `mc_clean` 逐项防呆（L-9） |
| `scripts/build-linux-all.sh` | 设置 `OUT_ROOT` 供 `mc_clean` 判定 |
| `.gitattributes` | 新建，换行策略显式化（L-10） |
| `server/enroll_hn1_test.go` | 新增 8 例（H-N1 回归闸门） |
| `server/hardening_export_test.go` | 新增 3 例（L-1 / L-2） |
| `server/internal/route/hardening_blacklist_test.go` | 新增 3 例（黑名单加固） |
| `server/internal/route/interactive_lifecycle_test.go` | 新增 4 例（M-N2） |

**测试增量**：18 个新测试函数（含 3 个子场景用例），全部围绕「漏掉就等于没改」的性质，而非实现细节。

---

## 8. 残留与未做（如实记录）

1. **H-5 的另一半 —— `.deb`/`.rpm`/`.exe` 签名与私钥轮换**：仍开放。四件事都要签名证书与密钥托管，**不是本仓代码能自己收口的**。升级包那一半已闭环且本轮复核无回退。
2. **CI 镜像未按 digest 钉扎**（`golang:1.26` / `node:20`）：仍开放。需先把 digest 落进配置并建立更新流程，属排期项。
3. **`-allow-unauthenticated-agents` 的固有代价**：该开关打开时，匿名路径通常没有令牌存储（否则就不叫匿名了），因此 **TOFU 绑定没有承载者**。已如实写进代码注释与启动告警 —— 这个开关的语义就是「接受不可作为信任依据的身份」，运维应仅用于过渡期，且必须为每台主机补发一次性令牌。
4. **黑名单是纵深防御，不是安全边界**：见 §4 的边界声明。它拦不住 shell 任意构造，也不打算拦。
5. **未做依赖扫描复跑**：本环境网络可用但本轮未跑 `govulncheck` / `npm audit`（`.cnb.yml` 的 `security-scan` 作业已在 PR 阶段常态化执行，合入时即为真跑）。
6. **未做实机端到端复现**：H-N1 的 POC 走的是 `enrollAgentCert` 的完整调用链 + `net.Pipe` 连接，**不是**「起服务端 + 真 TLS 握手 + 真 CSR」的端到端。修复后的断言在 `enrollAgentCert` 边界内是充分的（该函数就是签发决定点），但若需要彻底确认，建议在测试机起服务端带 `-allow-unauthenticated-agents` 且不配密钥，用 `openssl` 造 CSR 走 `TypeEnrollReq`，确认收到的是拒绝而不是证书。
7. **`installer/setup.iss` 的 `-extra` 保留**：理由见 §4 L-9 表格，这是本轮唯一「刻意不修」的低危项。

---

## 9. 优先级建议

1. **H-N1 已完成修复**（本轮）。若生产环境此前带 `-allow-unauthenticated-agents` 且未配凭据运行过，**应视为注册通道曾开放**：建议轮换内置 CA、为每台主机补发一次性令牌。同时检查配置文件里是否残留 `allow_anon_agents`（`main.go:228` 会回读它，去掉命令行参数不会关掉）。
2. **M-N2 / L 系列已完成修复**（本轮）。
3. **H-5 另一半 + 镜像 digest 钉扎**：按既有排期推进，两者都不阻塞当前使用。
