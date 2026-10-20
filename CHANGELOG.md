# 更新记录

版本号口径：**Server 与 Agent 互相独立、不做一致性校验**，各自在
`server/internal/version/version.go` 与 `agent/internal/version/version.go` 维护；
安装包版本由 `installer/setup.iss` 的 `AppVersion` 决定。发布纪律：只在明确发版时修改版本号。

## 1.0.3（2026-10-20）

### 注册与接入

- **一次性注册令牌的 `agent_id` 改为选填（新增「不绑定令牌」）** —— 后台 `/admin/enroll` ②
  与 `POST /api/enroll/tokens` 均允许留空：
  - 填写 `agent_id`：令牌仍只认该主机，名称不一致直接拒绝，且**不消耗**令牌；
  - 留空：令牌不绑定主机，**任何主机可用一次**，身份由该机决定 —— Agent 未指定 `-agentid`
    （Windows 安装向导未填「Agent 名称/ID」）时默认使用机器硬件 UUID，探测不到才用主机名；
  - 一次性语义不变：换取证书成功即作废；列表以「不绑定」标识；不绑定令牌不对应主机，
    只能逐条吊销，不能按 `agent_id` 批量吊销；
  - 控制台在签发前明确提示 bearer 风险：谁先拿到谁就能用一次，且可用于覆盖同名主机的 TOFU 绑定。
  - 服务端 `enroll.json` 允许空 `agent_id` 记录并正常重载；签发审计注明绑定/不绑定。
- **注册失败提示可操作化**：`注册凭证校验失败` 现在会提示核对 `agent_id`、令牌有效期/使用状态
  与共享密钥一致性；仍不区分"令牌不存在/证明不对"，不把未鉴权通道变成探测某个 `agent_id` 的 oracle。
- **Windows 图形安装向导新增「Agent 名称/ID」**（1.0.2 及更早没有该输入项，GUI 安装只能以机器 UUID 注册）。
  该字段保持选填：绑定令牌须与签发时一致；不绑定令牌留空即按机器 UUID 上线。

### Web 控制台

- `/admin/enroll` 接入教程按 **Linux / Windows** 分列；补充 Windows 覆盖安装（保留既有配置、
  按新身份清旧证书并重新换证）与常见问题。
- 令牌命令生成适配不绑定令牌：不绑定时不生成 `-agentid`，Agent 按默认身份上线；
  按 `agent_id` 批量吊销只在填写了名称且存在对应待用令牌时出现。
- 新增**删除离线主机**：仅允许删除离线主机，清理设备记录、探针/延迟历史、端口映射规则与备注，
  并写操作审计；不删除证书 TOFU 绑定与未使用令牌，也不卸载目标机 Agent。

### Windows 安装程序

- 向导按角色分页：只装 Agent 不显示 Server 后台账号与监听端口，只装 Server 不显示 Agent 连接参数。
- 卸载时询问是否删除配置与数据（默认保留）；数据目录记入 `HKLM\Software\MoCenter\DataDir`，
  自定义目录也能清理；静默卸载需显式传 `/PURGE`。
- 升级安装把显式传入的 `-server` / `-agentid` / 注册凭据合并进现有配置；Agent 身份变化时
  清理旧证书（保留 `client.key`）后重新换取；换取成功后清空配置里的一次性令牌。

### Agent

- 首次接入诊断增强：日志打印证书与注册凭据来源、实际使用的 `agent_id`；身份漂移时删除
  `client.crt` / `ca.crt`（保留 `client.key`）并自动重新换取；令牌只在换取成功后从配置清除，
  名称不匹配等失败场景保留令牌以便重试。
- **修复交互会话模式的 winvnc 在 Session 0 直启**：服务形态的 Agent 不再直启 winvnc，
  改走 `schtasks /it` 拉进交互会话；验收加严为"至少一个 winvnc 运行在交互会话"，
  否则明确报错并建议改用服务模式（不再出现"端口通了但画面全黑"的假成功）。
- **修复 headless（Xvfb）虚拟屏被 X 屏保刷黑**：启动 VNC 前执行 `xset s off` + `xset -dpms`。

### 构建与安装

- Server / Agent 版本号升至 **1.0.3**；`installer/setup.iss` 的 `AppVersion`、`agent/build.sh`、
  `agent/build.bat`、`scripts/build-all.bat`、`scripts/lib/common.sh` 的兜底版本同步。
- 修复 `agent/build.sh` 从源码读取版本号时只认 `const`、对 `var Version` 失效的问题
  （此前始终落到兜底值）。
- **Windows 口令入口收口（H-9 残留）**：`install-windows.bat` 移除位置式口令（调用方命令行
  对同机任意进程可见），改走 `--password-file` / `MOCENTER_PASSWORD` / 标准输入 / 交互输入 /
  留空自动生成随机强口令；`mocenter-deploy.exe -password` 退役并明确报错，新增 `-password-stdin`。
  顺带修复旧口令生成逻辑在括号块内判断 `%S_PASS%` 恒为空、"随机口令从未显示"的 bug。

### 测试增量

- `agentauth`：不绑定令牌接受任意上报身份且仅一次、证明与上报身份绑定、失配不消耗令牌、
  空 `agent_id` 跨重启持久化、列表标记 `unbound`。
- 签发 API：`agent_id` 留空签发成功并返回 `unbound`；以机器 UUID 换取成功；列表不回显明文。
- 真实入口回归（工作树构建）：浏览器登录 `/admin/enroll` 签发不绑定令牌 → Agent 不带
  `-agentid` 以机器 UUID 换取证书并上线；绑定令牌名称不匹配仍被拒且令牌保持待使用；
  删除离线主机全链路。
- `go test ./...`（server / agent / installer）与 `npm run build` 全绿。

## 1.0.2（2026-10-14）

### Agent

- **新增：读取当前 VNC 口令（只读）** —— 新增 `VncPasswordReq/Resp` 协议消息与
  `VncReadPassword()`（Windows / Linux / 其他三平台实现）。Windows 从 `ultravnc.ini`
  取 `passwd`（先核对 `WritePrivateProfileStruct` 校验和再解 HEX，校验和不符即报损坏、
  绝不返回必然连不上的值）；Linux 读 x11vnc 的 `-rfbauth` 口令文件。多处配置不一致时
  明确报错而非任取一个。服务端一重启、换人操作或 VNC 为手工安装时不再只能"重装重置"，
  也不会打断正在使用的远程会话。
- **修复：UltraVNC 握手间歇性失败（两个真缺陷）**
  1. 顺序错误：原流程"先写配置、后杀旧实例"，而 winvnc.exe 退出时会用内存中的**旧口令**
     回写 ini，覆盖刚写入的新口令 → 前端拿到新口令、服务端用旧口令应答 → 认证失败。
     已改为「清环境 → 写配置 → 读回校验」收进同一函数（`prepareUltraVNCConfig`）。
  2. 环境清理是"请求式"的：`taskkill /F` 只发请求不验证，靠 sleep 猜等待。
     已改为轮询确认（`waitNoWinvnc`）+ `confirmPortIsOurs`（端口开着却无 winvnc 进程
     = 启动失败），并新增 ini 读回校验 `verifyUltraVNCIni`。
- 一键安装按钮语义明确为「一键安装/重置口令」：未安装则安装，已安装则清实例、
  换口令再启动。

### Server

- **修复：审计 `Category` 落 SQLite 时丢失（真机复测撞出）** —— `Router.Audit` 抄
  SQLite 行时漏了 `Category` 字段，注册面拒签与运维操作在库里永久显示成"命令执行"，
  `?category=auth` 检索失效。已修（`route/router.go`）并加回归
  `TestAuditCategorySurvivesSQLite`。
- **`/api/vnc/password` 改为实时向 Agent 读取** —— 不再回落到服务端内存缓存
  （离线 Agent 的缓存口令只会给出过期值），读取针对请求中的 `agent_id`，超时 20s，
  审计只记"取到了没有"。
- **公开状态页收敛（L-4 收口）** —— 匿名 `/api/public/state` 不再下发
  `publicFieldCatalog()`（含 sensitive 标记的字段目录），只下发 `config.fields`。
- **「部署 Agent」升级语义与 `install-linux.sh` 对齐** —— 目标机已有
  `/etc/mocenter/mocenter-agent.json` 时只替换二进制并重启服务，**配置原样保留**
  （本机定制与设备身份不被冲掉）；文档同步更正"公钥随一键部署写进**新部署**的 Agent 配置"。
- **接入页总控地址补全公网 IP（修复两个真 bug）** —— `-listen` 写成 `:9000` 时生成的
  命令曾是 `-server 9000`（必然连不上）；新增带缓存的公网 IP 探测访问器，
  `/api/server-info` 新增 `public_ip` 字段，前端按 `-listen` 拆主机/端口并用其补全。
- 文档：`docs/deployment.md` 新增「升级顺序与前置条件」；示例域名全仓库统一为
  `mc.example.com`。

### 构建与安装

- Server / Agent 版本号升至 **1.0.2**，`installer/setup.iss` 的 `AppVersion` 同步；
  `agent/build.sh` / `agent/build.bat` / `scripts/build-all.bat` / `scripts/lib/common.sh`
  的兜底版本号同步更新。
- 新增本更新记录（`CHANGELOG.md`）。

### 测试增量

- Agent：VNC 口令反混淆权威向量与往返、`decodeUltraVNCPassword` 校验和（篡改必报错）、
  ini 回写覆盖检测、tasklist CSV 解析等。
- Server：审计类别 SQLite 回归、VNC 口令交付+留痕（注入桩）、读取必须针对请求
  `agent_id`、部署脚本 `sh -n` 语法校验与"保留既有配置"回归、公开页 guard 等。

## 1.0.1

早期基线版本：核心远控链路（上线注册 / 心跳 / 探针 / 命令执行 / 交互终端 / 文件传输 /
端口映射 / 远程升级）、公开状态页、三轮安全评估修复（H-N1 等）、统一构建脚本
（`build-linux-all.sh` / `build-all.bat`）、Windows 图形安装程序。详见 git log 与
`docs/security-review-*.md`。
