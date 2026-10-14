# 更新记录

版本号口径：**Server 与 Agent 互相独立、不做一致性校验**，各自在
`server/internal/version/version.go` 与 `agent/internal/version/version.go` 维护；
安装包版本由 `installer/setup.iss` 的 `AppVersion` 决定。发布纪律：只在明确发版时修改版本号。

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
