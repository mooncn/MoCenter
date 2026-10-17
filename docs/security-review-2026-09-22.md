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

### 真机端到端复测（2026-10-01，Rocky 8 测试机）—— 24/25 通过，唯一失败项撞出一个真缺陷
`.testtools/hn1-e2e.sh`：起**真实服务端**（三个开关逐一置为 false/true 组合）+ `openssl` 现出现场私钥与 CSR，走 `TypeEnrollReq` 真 TLS 握手，A–G 七组：

| 组 | 场景 | 结果 |
|---|---|---|
| A | 空凭据 + 自选 `agent_id`（就是原 POC 的输入） | 换不到证书；给出明确拒绝原因；绑定表里不存在该主机 |
| B | 错令牌 | 换不到证书；Agent 侧看得到服务端原因；拒签在服务端/回执链路留痕 |
| C | 正确一次性令牌 | **正向路径可用**：拿到证书 + CA + 服务端指纹；证书公钥与本机私钥配对（私钥不出机器）；绑定表按 SPKI 记账（TOFU 落库，`spki=20e581…bae4da` 与证书算出的完全一致）；换取后以 mTLS 正常上线；配置里的 `enrollment_secret` 被清空（一次使用） |
| D | 已消耗令牌二次使用 | 换不到第二张证书；**在册绑定指纹未被改动**；留下拒绝原因；令牌列表接口不回显明文 |
| E | 拿别的主机的令牌换自己的 `agent_id` | 拒；绑定表里没有该主机 |
| F | 对**已有活动会话**的 `agent_id`（在册主机 `rocky8-01`）抢注 | 拒，理由写明"当前有活动会话"；`rocky8-01` 的绑定指纹未被改动；**未打断在册主机的既有会话** |
| G | 注册面审计 | 见下 |

G 组断言"签发与拒绝都要进审计且带 `auth` 分类"失败：22 条拒签记录全部挂着 `category='command'`。这不是判据写错，而是一个**真缺陷**：`Router.Audit` 在把内存审计抄成 SQLite 行时漏了 `Category` 字段（内存回退路径抄了，所以单测全绿），`storage.InsertAuditLog` 又把空值补成 `'command'` —— 后果是注册面拒签、运维操作在库里永久显示成"命令执行"，`/api/audits?category=auth` 一条也查不到，即"拒发必留审计"这条防线在**检索面**上是失效的。已修（`server/internal/route/router.go:549`）并加回归 `TestAuditCategorySurvivesSQLite`（删掉那一行即判红，验过红/绿双向）；真机复核脚本 `.testtools/audit-category-check.sh` 装上修复后的构建重跑一次拒签 + 一次签发，`RESULT pass=3 fail=0`：`?category=auth` 命中且含本轮 `ac-check` 的 4 条拒签、`?category=op` 命中签发与吊销、`?category=command` 里本轮记录 0 条。**修复前那 22 条历史行不回改**（无回填脚本，且它们的时间戳与内容都在，只是类别标签旧）。

**两条过程教训（都写进了判据，避免下次自我欺骗）**：
1. 第一轮 `RESULT pass=12 fail=12`，Agent 退出码 **126** —— `vm.py put` 上去的 Agent 二进制是 644，**没一个用例真的跑起来**，而"没拿到证书"类断言全部白给 PASS。修法是 `chmod +x` 之外给脚本加 `CASE_ALIVE` 闸门：本轮 Agent 没启动则任何 PASS 一律记 FAIL。
2. G 组的反向断言第一版按"command 桶里有几条拒签字样"计数，把修复前的历史行当成缺陷，误报一次 FAIL；改为只按本轮 `agent_id` 挑记录。

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

> **2026-10-01 续**：本节"全部收口"的范围是**本轮新发现或改写过的 5 项**（L-1/L-2/L-9/L-10 + 黑名单加固）。上一份报告 §4 里仍未动的 **L-3（`pickEncoding` 忽略 q 值）、L-4（匿名 `/api/public/state` 回全量字段目录）、L-6（协议死码与"发了没人收"的回执）、L-7（占位服务端地址）已于 2026-10-01 全部收口**，落地、残留与"两个默认值刻意不动"的理由见 `security-review-2026-09-19.md` §4 对应行与其末尾的 2026-10-01 验证基线段。**其中 L-6 修掉的是一条真实缺陷而非死码清理**：服务端每个上传分片回写一条 Agent 永远不会读的 `FileAck`，而 Agent 是在自己的读循环里同步吐分片的 —— 大文件会把服务端这一侧写到 deadline 超时并掐断控制连接。至此 LOW 系列只剩 **L-5（无头 Windows VDD，需真机）** 与 **CI 镜像 digest 钉扎（需注册表摘要）**。

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
| `server/internal/route/router.go` | `normalizeForBlacklist` + `IsBlocked` 双变体匹配（黑名单加固）；**2026-10-01 真机复测追加**：`Audit` 抄 SQLite 行时补上漏掉的 `Category`（`:549`），否则注册面拒签与运维操作在库里全是 `command`，`?category=auth` 查不到 |
| `installer/setup.iss` | 注册密钥掩码 + 口令下限 12（L-9） |
| `scripts/lib/common.sh` | `mc_clean` 逐项防呆（L-9） |
| `scripts/build-linux-all.sh` | 设置 `OUT_ROOT` 供 `mc_clean` 判定 |
| `.gitattributes` | 新建，换行策略显式化（L-10） |
| `server/enroll_hn1_test.go` | 新增 8 例（H-N1 回归闸门） |
| `server/hardening_export_test.go` | 新增 3 例（L-1 / L-2） |
| `server/internal/route/hardening_blacklist_test.go` | 新增 3 例（黑名单加固） |
| `server/internal/route/interactive_lifecycle_test.go` | 新增 4 例（M-N2） |
| `server/internal/route/audit_category_test.go` | **新增（2026-10-01）**：`TestAuditCategorySurvivesSQLite` —— 三类审计各写一条，从 SQLite 路径按 `?category=` 查回并断言类别未丢、auth 行不得出现在 command 桶（真机复测撞出的缺陷的回归闸门；删掉 `router.go` 那行 `Category:` 即判红） |
| `.testtools/hn1-e2e.sh` / `.testtools/audit-category-check.sh` | **新增（2026-10-01）**：H-N1 真机端到端 A–G 组 + 审计类别真机复核，均带"用例未真跑即不作数"的 `CASE_ALIVE` 闸门 |

**测试增量**：18 个新测试函数（含 3 个子场景用例），全部围绕「漏掉就等于没改」的性质，而非实现细节。**2026-10-01 追加 1 例**（`TestAuditCategorySurvivesSQLite`）。

---

## 8. 残留与未做（如实记录）

1. **H-5 的另一半 —— `.deb`/`.rpm`/`.exe` 签名与私钥轮换**：仍开放。四件事都要签名证书与密钥托管，**不是本仓代码能自己收口的**。升级包那一半已闭环且本轮复核无回退。
2. **CI 镜像未按 digest 钉扎**（`golang:1.26` / `node:20`）：仍开放。**2026-10-01 复核过它为什么做不了**：开发机无 `docker` 命令，`auth.docker.io` 与 CNB 注册表根路径 `curl -m 25` 双双超时（exit 28），取不到当前真实摘要；把凭空写出的摘要钉进 CI 比不钉更糟（拉不到镜像，且"钉扎"看起来已生效）。需要一台能出网的构建机先取摘要、再建立更新流程，属排期项而非代码项。
3. **`-allow-unauthenticated-agents` 的固有代价**：该开关打开时，匿名路径通常没有令牌存储（否则就不叫匿名了），因此 **TOFU 绑定没有承载者**。已如实写进代码注释与启动告警 —— 这个开关的语义就是「接受不可作为信任依据的身份」，运维应仅用于过渡期，且必须为每台主机补发一次性令牌。
4. **黑名单是纵深防御，不是安全边界**：见 §4 的边界声明。它拦不住 shell 任意构造，也不打算拦。
5. **未做依赖扫描复跑**：本环境网络可用但本轮未跑 `govulncheck` / `npm audit`（`.cnb.yml` 的 `security-scan` 作业已在 PR 阶段常态化执行，合入时即为真跑）。
6. ~~**未做实机端到端复现**~~　✅ **已于 2026-10-01 在 Rocky 8 测试机补做，24/25 通过**：真实服务端 + 真 TLS 握手 + `openssl` 现出现场 CSR，A–G 七组覆盖"空凭据 / 错令牌 / 正确令牌正向签发（含 SPKI 绑定与私钥配对的密码学核对）/ 令牌二次使用 / 拿别人的令牌 / 抢注有活动会话的 `agent_id` / 审计留痕"，逐组结果与两条过程教训见 §2「真机端到端复测」。**它顺带撞出一个原判据覆盖不到的真缺陷**（审计 `Category` 在 SQLite 落库时被丢弃，导致注册面审计在检索面上失效），已修 + 加回归 + 真机复核 `pass=3 fail=0`。**剩余残留**：修复前那 22 条历史审计行的类别标签仍是 `command`（不做回填）；测试机系统时钟比实际日期慢约 9 天（`date` 显示 9 月 21 日），本轮所有"时间窗"判据都按机器时钟自洽校验，不影响签发/过期逻辑本身。
7. **`installer/setup.iss` 的 `-extra` 保留**：理由见 §4 L-9 表格，这是本轮唯一「刻意不修」的低危项。

---

## 9. 优先级建议

1. **H-N1 已完成修复**（本轮）。若生产环境此前带 `-allow-unauthenticated-agents` 且未配凭据运行过，**应视为注册通道曾开放**：建议轮换内置 CA、为每台主机补发一次性令牌。同时检查配置文件里是否残留 `allow_anon_agents`（`main.go:228` 会回读它，去掉命令行参数不会关掉）。
2. **M-N2 / L 系列已完成修复**（本轮 L-1/L-2/L-9/L-10；**余下的 L-3/L-4/L-6/L-7 已于 2026-10-01 收口**，见 §4 顶部续记）。
3. **H-5 另一半 + 镜像 digest 钉扎**：按既有排期推进，两者都不阻塞当前使用。**2026-10-01 补一句口径**：这两项如今是报告里唯一还开着的"待办"，且**都不是代码问题** —— 前者要签名证书与密钥托管，后者要能取到注册表摘要（本机 `docker` 不存在、注册表 `curl` 超时）。第三类剩余是**真机复测**（H-N1 端到端、H-9 的 Windows 安装、Win10↔Rocky8 的 VNC 链路、L-5 的无头 VDD），需要用户侧开机与提供测试机口令。**2026-10-01 更新**：用户已提供两台测试机口令，**Rocky 8 侧已全部做完**（H-N1 端到端 24/25 + 由其撞出的审计类别缺陷已修并真机复核；L-3/L-4/L-6/L-7 的真机复测见 `security-review-2026-09-19.md` §4 各行的 🖥 段）；**只剩 Win10 侧三项**（H-9 真机安装、Win10↔Rocky8 VNC 链路、L-5 无头 VDD）—— 2026-10-01 复测时该 VM 的 445/3389 可达但**用户给的 FTP(21) 连接被拒**（服务未监听），文件通道待用户确认。**2026-10-14 更新**：FTP(21) 文件通道已开放并经用户实测可用，阻塞解除 —— Win10 侧三项真机复测（H-9 安装、Win10↔Rocky8 VNC 链路、L-5 无头 VDD）可随时推进（尚未执行）。**2026-10-16 更新（三项已全部落地）**：① **H-9 真机安装复测完成**（Win10，`pass=36 fail=2`，两条 FAIL 均在检查脚本侧、产品断言全绿，`win10-01` 真机注册端到端走通，逐条见 `security-review-2026-09-19.md` §10「2026-10-16 真机第三轮」）；② **L-5 判为"不验"**（该 VM 不是无头机：`VMware SVGA 3D status=Started` 有活动输出，VDD 无头场景在此机上证不了）；③ **Win10↔Rocky8 VNC 链路判定完成**（浏览器 noVNC 走产品真实入口：Rocky8 出图 ✅、Win10「服务模式」出图 ✅（人眼确认完整桌面）；**Win10「交互会话模式」失败 ❌ 定为产品缺陷候选**——Session-0 直启 winvnc 赢端口竞争导致 ClientInit 后即断，未修，详见 `security-review-2026-09-19.md` §10「2026-10-16 VNC 出图判定」段）。据此，**§8/§9 里"真机复测"这一类只剩无头 VDD 一项本身（判不验）**；真机侧新开的口子只有 Rocky8 xset 修复的 >600s 复核（等下次部署新构建后顺手做）。
