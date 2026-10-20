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

---

**2026-10-20 增补**：用户实测的一次性令牌接入失败、离线主机删除、`/admin/enroll` 教程与 Windows 兼容性结论，见 `security-review-2026-09-19.md` 末尾「2026-10-20 用户实测问题修复」段。**同日再增补**：命令行注册成功但 Windows 图形安装的服务未注册、令牌用后仍「待使用」的根因与修复（凭据来源回传、跳过换证日志、证书身份漂移自愈、安装器升级合并 Agent ID/令牌并清理旧证书），见同文件末尾「2026-10-20 增补：命令行注册成功但 GUI 服务未注册 / 令牌一直『待使用』」段。

---

## 10. 2026-10-20 复审：RCE / SQL 注入 / 越权三类结论 + 9 条低危全部收口

### 10.1 三类主问题的结论（逐入口走查当前工作树，含未提交的「邀请远程管理 / 令牌分页」改动）

- **RCE：未发现可被未认证方利用的路径。** 服务端自身不执行任何外部程序（全仓 `os/exec` 只出现在测试里）；
  命令下发 / 批量 / 交互终端 / VNC 安装与口令 / 升级 / 一键部署 / 端口映射全部在 `guard()` 之后
  （`server/main.go:3394`，路由注册 `main.go:743-823`）；分享访客的上行消息在 `handleWebMessage`
  入口处就被收敛为「只允许终端输入」（`main.go:3811-3822`），访客侧每个端点另做「功能作用域 + 主机绑定」
  （`server/share_api.go:169-190`、`applyPortmapOp` `share_api.go:349-401`）。Agent 执行任意命令是产品功能，
  其收口位置在授权与审计，不在字符串匹配（黑名单的边界声明见 `internal/route/router.go:274-314`）。
  升级链无跳过验签开关，URL 只认 https、摘要与签名覆盖「目标版本 + 包内容」且校验/落盘用的是同一份字节
  （`agent/internal/upgrade/upgrade.go:113-126`、`:241-281`）。
- **SQL 注入：不存在。** 全部数据绑定走 `?` 占位符（含审计检索的 `LIKE ?` 与 `LIMIT ?/OFFSET ?`，
  `internal/storage/queries.go:170-225`）；动态标识符只有两处，且取值来自内部常量或
  `quoteIdent` 的正则白名单 `^[A-Za-z_][A-Za-z0-9_]*$`。`grep` 全仓无 `Query/Exec(fmt.Sprintf(...))`。
- **越权 / 冒名：未发现。** 报文里的 `agent_id` 一律不以自报为准：身份取自连接证书 CN 且必须验签发链
  （`main.go:1723-1741`），探针/上传/命令回执/交互输出/隧道数据各自做归属校验
  （`connOwns` `main.go:1027`、`router.CommandOrigin` `main.go:1879-1900`、VNC `Deliver*` 的 `vw.agentID != agentID`
  分支 `internal/vnc/vnc.go:437-481`）；重复 `agent_id` 不允许顶替（`session.Upsert`），同 UUID 的旧会话只在
  新连接被接纳**之后**才被顶掉；一次性令牌优先、在线 ID 拒发、SPKI TOFU 绑定三段闸门见
  `enrollAgentCert`（`main.go:1531-1640`）。分享凭据只存 SHA-256 且常量时间比对（`internal/share/share.go:246-280`）。

### 10.2 本轮修复的 9 条低危（逐条：证据 → 修法 → 验证）

| # | 位置（修复前） | 问题 | 修法 | 验证 |
|---|---|---|---|---|
| L-1 | `internal/filetransfer/push.go:75-170` | 分发任务无条目/字节/TTL 闸门（上传侧 M-8 有，分发侧漏），泄漏一枚 Token 即可吃满内存 | 与上传侧同口径补四道闸（64 条 / 256 MiB / 512 MiB / 30 min），`CleanupStale`（`push.go:171`）接进 `cleanupSweeper`（`main.go:2060`），超限原因如实广播给 Web | 新增 `push_limits_test.go` 5 例；`go test ./internal/filetransfer` 通过 |
| L-2 | `main.go:3250-3258` | `randHex` 吞掉 `crypto/rand` 错误 → 极端情况下发出全零会话 Token | 返回 error；登录路径失败即拒发并留日志 | 真实入口登录成功（见 10.4），`enroll_api_test.go` 全绿 |
| L-3 | `web/src/main.jsx:38-47` | 崩溃面板把 `error.message/stack/失败资源 URL`（部分源自被管主机回传）未转义拼进 `innerHTML`；同段内联 `onclick` 被本站 CSP 拦死 → 两个按钮点了没反应 | 改 DOM API + `textContent` + `addEventListener` | 浏览器注入含 `<img onerror>`/`<script>` 的错误报文：面板出现、`img=0 script=0`、未执行；点「回到首页」确实导航到 `/` |
| L-4 | `internal/vnc/vnc.go:476-488` | 隧道失败通知用字符串拼 JSON，`msg` 是 Agent 自报文本 → 被拿下的主机可越出字段边界给运维的桌面通道注入任意帧 | 改 `json.Marshal`（现 `vnc.go:481`） | `go test ./internal/vnc` 通过 |
| L-5 | `agent/internal/client/client.go:1182-1196`（旧 openTunnel） | 隧道的 `LocalHost` 完全由服务端决定 → 总控失守时每台被管机都是通往其所在网络的代理 | 默认只认回环（`tunnelTargetAllowed` `client.go:1165`）；非回环需**本机** `-tunnel-hosts`（`MOCENTER_TUNNEL_HOSTS`）放行，服务端指令打不开；启动日志打印生效边界 | 新增 `tunnel_hosts_test.go`；真机回归：控制台建端口映射 18123→127.0.0.1:18080，经隧道 `curl` 得 `ok`（回环路径未被误伤） |
| L-6 | `agent/internal/client/enroll.go:264-300`（旧 dialForEnroll） | 换取阶段「TLS 校验失败就重拨一条不验签的」—— 本机明明配了 `server_fingerprint` 时也照样降级，等于填了指纹却不生效 | 有锚（指纹 / `ca_cert` / 已落盘 `creds/ca.crt`，判定见 `tlsutil.go:113`）时**拒绝降级并把凭据留在本机**；无锚的首次接入保留 TOFU 引导但大声告警 | 新增 `enroll_pin_test.go`：错指纹换取失败且假服务端收到 **0 次**换取请求；无锚首次换取成功并落下 `ca.crt` 成为下一次锚 |
| L-7 | `internal/auth`（判定一律按 TCP 对端） | 反代之后 `-whitelist` 只对代理 IP 生效、120 次/分的额度被全站访客共享（匿名公开页能把管理员挡在后台外） | 新增 `-trusted-proxies`（配置键 `trusted_proxies`）：仅当 TCP 对端在可信网段内才按 XFF「从右往左第一个非可信跳」还原客户端地址，最外层一次性归一 `RemoteAddr`，白名单/限速/登录退避/审计来源/公开页并发共用同一口径；默认空 = 行为不变 | 新增 `proxy_test.go` 4 组断言；真机：`-trusted-proxies 127.0.0.1 -whitelist 203.0.113.0/24`，带 `X-Forwarded-For: 203.0.113.9` 登录 200 且审计记「来自 203.0.113.9」，不带 XFF 直接 403 |
| L-8 | 启动日志（一次性、没人看） | 匿名注册、Agent 通道退回明文、部署级共享密钥未收口这三项长期生效却在界面零呈现 | `/api/server-info` 增回 `allow_anon_agents` / `agent_tls_on` / `shared_key_disabled`（TLS 结论用原子量从 `start()` 带回），后台顶部常驻红色横幅 | 单元测试断言字段随参数翻转；浏览器登录后仪表盘实际出现「⚠️ 接入身份保障已降级 · 仍在使用部署级共享注册密钥」 |
| L-9 | `main.go:1490-1510`、`agent/internal/command/vncinstall_unix.go:444-490` | ① `validAgentID` 允许 `< > " ' \` & ; /` 等元字符（当前每条出口都各自转义，属纵深缺口）；② 本机安装 x11vnc 用 `bash -c "...'路径'..."` 拼单引号、`command -v <name>` 把名字拼进 shell | ① 身份入口显式拒元字符集（保留字母数字与 `._-` 及非 ASCII，兼容中文主机名/UUID）；② 改 `exec.LookPath` + `os.MkdirAll`/`io.Copy`，不再经 shell | 新增 `agentid_test.go`（合法/危险两组）；`GOOS=linux go vet` 覆盖 unix 专属文件；真机：`e2e-agent-drift2` 重新注册上线并成功执行 `echo L9-OK`（exit 0、输出回填审计） |

### 10.3 复测中顺带撞出的一个非安全类缺陷（已修）

`-admin-tls-days 0` 被配置文件里的旧值顶回去：回填判据是「值等于默认就当没传」，而该参数默认值恰是 30，
于是"显式关掉自动 HTTPS"这个意图静默失效。修法是提前算出 `givenFlags`（`flag.Visit` 并剔除被归一化的空值参数），
配置回填必须让位给它 —— 与 Agent 侧 `flagWasSet("allow-public-package-download")` 是同一条纪律。
真机复测：`-admin-tls-days 0` 后日志即为「REST API / Web 控制台监听 …（**明文 HTTP**）」。

### 10.4 本轮验证口径

- 单元测试：`cd server && go test ./...` 全绿；`cd agent && go test ./...` 全绿；
  `GOOS=linux go build ./... && GOOS=linux go vet ./...` 通过（Linux 专属安装路径）；`cd web && npm run build` 通过。
- 真实入口回归（本地一次性实例，非用户生产环境）：浏览器登录 `/admin/login` → 仪表盘横幅可见 →
  命令下发经产品链路执行并回填审计 → 端口映射隧道（回环）连通 → XFF 还原与白名单联动 → 崩溃面板安全且按钮可用。
- 未在用户生产实例（192.168.117.1:8080）上做任何写操作；改动全部留在工作区，未提交、未推送。

### 10.5 仍开放项（本轮未动，理由不变）

H-5 发布物签名（`.deb`/`.rpm`/`.exe`）与 CI 镜像 digest 钉扎；Win10「交互会话模式」VNC 缺陷；
无头 VDD 场景复测。见本文件 §8/§9。

---

## 11. 2026-10-20 二次复评：1 高危 + 3 中危（已全部修复）

9 条低危收口后重新按「RCE / SQL 注入 / 越权」走查当前工作树，主结论不变（无未授权 RCE、无 SQL 注入、
无直接鉴权绕过），但新功能的边界上发现 4 条问题，已按高危→中危顺序修复。

### 11.1 高危：Agent 的 CA 指纹钉扎可被“附带公开 CA 证书”绕过

- **证据（修复前）**：`agent/internal/client/tlsutil.go:51` 在 `InsecureSkipVerify=true` 下只遍历对端
  发来的证书、任一 SPKI 与 pin 相同即放行，**从未验证叶子是否由该 CA 签发**；而服务端下发的链是
  `[叶子, CA]`（`server/internal/agentauth/pki.go:478-482`），且 Agent 端口允许无客户端证书握手
  （`CATLSConfig` 用 `RequestClientCert`），CA 公钥证书对任何能连到该端口的人都是公开可取的。
- **可利用性**：主动中间人用自己的自签叶子 + 公开 CA 证书组成 `[attackerLeaf, victimCA]`，pin 命中第二张，
  握手成功。已用临时测试复现（自签叶子与 CA 无任何签名关系，`tls.Dial` 仍成功；临时文件已删除）。
  后果是中间人可冒充服务端向 Agent 下发 `CommandReq`（以 Agent 账户执行任意命令），或在换取阶段截获
  一次性令牌的 HMAC 证明后抢先换取证书、接管主机身份。
- **修法**：`tlsutil.go:53-100` 重写 pin 回调——叶子命中 pin 时校验有效期；CA 命中 pin 时以它为 `Roots`、
  其余证书为 `Intermediates`，走一次标准 `Verify(KeyUsages: ServerAuth)`（签名、有效期、用途），
  不校验 SAN。仍保留 `InsecureSkipVerify`，避免破坏自签部署换 IP 的可用性。
- **回归**：新增 `TestPinRejectsInjectedCA`（合法链通过、注入链必须失败）与 `TestPinMatchesLeaf`；
  原有 `TestPinAcceptsCAInChain` 保持通过。

### 11.2 中危：分享链接到期后，已建立的会话仍永久有效

- **证据（修复前）**：`/ws` 只在收到上行消息时复查 `shareStore.Active`（`server/main.go:3814`），
  VNC 只在握手时校验一次（`internal/vnc/vnc.go:642`）；访客保持连接受输出/操作时，链接过期不生效
  （撤销有独立入口会主动断开，但到期没有）。撤销也不会结束已在目标机运行的远端 shell。
- **修法**：
  - `share_api.go:54-68` 新增 `scheduleShareExpiry`：按 `ExpiresAt` 注册定时关闭；
    `/ws` 分享连接（`main.go:708-711`）与 VNC 查看器（`main.go:741`）都登记；
    HTTP-only 的分享命令在派发成功时也登记（`share_api.go:302`）。
  - `router.ReleaseSession`（`router.go:486`）+ `closeShareCommands`（`share_api.go:514`）：
    撤销、到期、上行复查发现失效时，结束该分享名下的在途命令并向 Agent 下发 `InteractiveClose`。
- **回归**：`TestReleaseSession`、`TestScheduleShareExpiryPast/Future`；
  真实入口：创建分享 → 开一个 `share-it-2` 终端 → 撤销，响应 `closed_commands=1`。

### 11.3 中危：分享访客可指定任意 `command_id`，抢占管理端命令归属

- **证据（修复前）**：`share_api.go:233` 直接采用访客传入的 `command_id`；`router.go:378` 对同一
  `(agent_id, command_id)` 无条件覆盖归属。管理端终端 ID 是 `it-<Date.now()>`，可猜；一旦命中，
  管理端输入会被判定为“不属于该会话”，访客输入反而通过，且命令输出按新归属投给访客
  （`main.go:1885/1898`），访客还能 `command/close` 掉管理端终端。
- **修法**：
  - 分享入口只接受 `share-` 命名空间且限字符/长度（`validShareCommandID`，`share_api.go:34-53`），
    管理端的 `it-` / `batch-` / `push-` 名字进不来；
  - `Dispatch` 在写审计前与写绑定前各检查一次“该键已属于其他会话”，是则拒绝（`router.go:368-371`、
    `:381-393`）；同会话复用仍允许；
  - `command_id` 全局限长 128 字节（`router.go:66-72`），不再能拿超大字符串驻留 map/审计。
- **回归**：`TestDispatchRejectsCrossSessionCommandID`、`TestValidShareCommandID`；
  真实入口：`command_id="it-..."` 被 400 级拒绝（`ok:false`），`share-it-1` 可正常开/关。

### 11.4 中危：未绑定的一次性令牌可重绑离线主机的证书

- **证据（修复前）**：未绑定令牌对任意 `agent_id` 参与证明匹配（`tokens.go:374`），而 `CheckOrBind`
  只要 `source == CredToken` 就允许换公钥（旧 `tokens.go:420`）。一枚未绑定令牌泄露后，攻击者可选一台
  **离线**的已有主机完成换取并重绑（在线主机被 `sess.Busy` 挡住），接管其身份。
- **修法**：新增 `Verifier.VerifyEx` 返回“命中的令牌是否绑定到该 `agent_id`”（`agentauth.go:272-330`），
  `CheckOrBind` 增加 `tokenBound` 参数：已有绑定且 SPKI 变化时，只有绑定令牌才允许重绑；
  未绑定令牌仅可用于首次接入（`tokens.go:400-440`）。`Verify` 保持原签名兼容旧调用。
- **回归**：`TestUnboundTokenCannotRebindExistingHost`；`TestTokenIsSingleUse` /
  `TestUnboundTokenAcceptsAnyAgentOnce` 断言 `tokenBound` 语义；原有 TOFU/持久化测试更新签名后全绿。

### 11.5 验证口径

- `cd server && go test ./...`、`cd agent && go test ./...` 全绿；两模块 `go build ./... && go vet ./...` 通过。
- 真实入口回归（本地一次性实例，`.testtools/e2e-20261020`，非用户生产环境）：登录 → 分享创建 →
  非法 `it-` 命令 ID 被拒 → `share-it-1` 开/关成功 → 撤销结束在途命令（`closed_commands=1`）→
  管理端 `it-1000` 命令仍可正常下发。
- 改动与后续低危收尾一并提交（见 git log 与 CHANGELOG「未发布」）。

### 11.6 复评中记录但本轮未改的低危/加固项

端口映射无特权端口下限与规则数上限、默认全网开放；登录时序可枚举用户名；TOTP 比较非恒定时间且无重放表；
`UpdateTraffic` 锁外写；指纹仍只保留 64 bit；畸形 `server_fingerprint` 会静默退化为“无锚”。
这些不构成本轮的高/中危边界，留待后续加固。

---

## 12. 2026-10-20 低危清零（已全部修复）

§11.6 记录的 5 类低危/加固项已全部落地。主结论不变：无未授权 RCE、无 SQL 注入、无直接鉴权绕过。

### 12.1 端口映射：资源上限、保留端口与并发安全

- **规则数上限**：`internal/portmap/portmap.go` 新增 `maxRules = 256`。每条规则都会占用一个真实
  监听端口，不设限时任何能创建规则的一方都能耗尽 fd/端口资源。上限只在**新建**时生效，
  覆盖同端口规则不受影响；提交阶段在持锁下二次检查，并发占满时停掉刚起的监听并还原旧规则。
- **分享访客保留端口**：`share_api.go` 的 `applyPortmapOp` 要求 `PublicPort >= 1024`。
  否则访客可用一条分享链接在总控上顶掉 SSH/HTTP 等基础服务的监听。
- **数据竞争**：`UpdateTraffic` 原先 `RLock` 取指针后锁外写字段，而 `Tracks()/GetTrack()` 把同一
  指针交给 API 层序列化。现在更新在写锁内完成，两个读取接口返回**快照副本**。

### 12.2 登录与 TOTP

- **用户名枚举时延**：`Authenticate` 对不存在的用户执行一次同成本 PBKDF2
  （`dummyPasswordHash`，120000 次迭代），使"用户不存在"与"口令错误"耗时一致。
- **恒定时间比较**：`validateTOTPStep` 用 `subtle.ConstantTimeCompare` 比较 6 位验证码。
- **重放保护**：每个用户记录最近一次通过校验的时间步（`UserManager.otpLast`）；
  同一窗口（含 ±1 步容差）内同一验证码只能用一次，换绑/解绑 TOTP 时清除水位。
- 回归：`TestUserManagerTOTPReplayRejected`；原有 TOTP 绑定/解绑/登录用例保持通过。

### 12.3 服务端身份指纹

- `agentauth.certFingerprint` 与 Agent 侧 `spkiFingerprint`/`certPublicFingerprint` 改为输出
  **完整 SHA-256（64 位 hex）**；此前截断到 16 位（64 bit）作为长期 pin 熵不足。
- 比较统一走 `fingerprintMatches`：完整值必须全等；历史 16 位短指纹按前缀兼容；更短的值不认。
- Agent 启动时用 `client.ValidateServerFingerprint` 校验配置：只接受 64 位或 16 位十六进制
  （允许冒号/空格/短横线分隔），8 位、非 hex 等畸形值直接启动失败，不再静默退化为"无锚"。
- 回归：`TestFingerprintMatching`（server）、`TestFingerprintMatchesFullAndLegacy` /
  `TestValidateServerFingerprint`（agent）；原有 pin/换取/CA 连续性用例保持通过。

### 12.4 Linux 本地符号链接

- `command/vncinstall_unix.go` 的 `copyExecutable` 以 `O_NOFOLLOW` 打开目标：
  `~/.local/bin/x11vnc` 是可预测路径，若被预置为指向其它文件的符号链接，原实现会以 Agent
  权限覆盖目标（本机提权链）。现在遇到链接直接失败。

### 12.5 验证口径

- `cd server && go test ./...`、`cd agent && go test ./...` 全绿；
  两模块 `go build ./... && go vet ./...` 通过；Agent `GOOS=linux GOARCH=amd64 go build ./...` 通过
  （覆盖 `O_NOFOLLOW` 分支）。
- 真实入口回归沿用 §11.5 的分享链路（登录 → 创建分享 → 非法命令 ID 被拒 → 终端开/关 →
  撤销结束在途命令 → 管理端命令正常），本轮改动后可复现通过。
