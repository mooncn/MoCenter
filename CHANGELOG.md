# 更新记录

版本号口径：**Server 与 Agent 互相独立、不做一致性校验**，各自在
`server/internal/version/version.go` 与 `agent/internal/version/version.go` 维护；
安装包版本由 `installer/setup.iss` 的 `AppVersion` 决定。发布纪律：只在明确发版时修改版本号。

## 未发布（2026-10-20）

### 邀请远程管理（分享链接）

- 仪表盘主机列表最后一列由「查看」改为「操作」，可按主机打开趋势、远程终端、远程桌面、
  端口映射，并创建分享链接。
- 新增「分享控制」与访客页签 `ShareConsole`：能力链接按**主机 + 功能**授权、有有效期
  （5 分钟 ~ 7 天）、可随时撤销；明文 token 只在创建响应出现一次，落盘仅保存 SHA-256 摘要
  （`<data>/shares.json`，0600），列表接口永不回显。
- 访客权限按最小面收口：终端仅允许交互式上行输入、不参与全局广播；VNC 隧道在握手与
  会话层双重校验；端口映射操作强制落在分享绑定的主机上，且不能占用 1024 以下保留端口；
  分享来宾只能关闭自己会话发起的命令。
- **到期与撤销真正生效**：到期定时器会关闭已建立的终端 / 桌面连接并结束在途远端命令
  （向 Agent 下发 `InteractiveClose`），不再出现"链接过期但会话还能一直用"。
- 分享命令 ID 限定 `share-` 命名空间；路由层对同一 `(agent_id, command_id)` 的跨会话
  复用一律拒绝，杜绝访客抢占管理端终端归属。

### 注册令牌管理

- `/admin/enroll` 的令牌列表改为分页（每页 10 条，可切换），避免历史令牌把页面撑长。
- 新增「删除记录」：仅对**已注销主机且曾完成证书换取**的令牌开放，服务端同样强制校验；
  仍在册或从未接入过的令牌只能吊销，避免误删待用凭据。删除写操作审计。
- 删除主机时同步清理其分享链接；仪表盘删除后派发 `mc-agents-refresh`，接入页即时刷新。

### Web 控制台

- 主机标识统一为「备注 → 主机名 → UUID(agent_id)」：仪表盘、远程终端、延迟监测、
  远程桌面、端口映射、运维操作、离线机器与分享弹窗中的主机选择器、列表与提示均走同一
  取值逻辑；有备注时以备注为主显示，副行保留主机名与 agent_id，便于区分同名设备。
- **修复手机 Chrome（Android）打开页面直接黑屏**：noVNC 1.7.0 在模块顶层 `await`
  WebCodecs H.264 自检，部分 Android 设备上 `VideoDecoder.flush()` 永不返回，
  顶层 await 会卡住整个入口模块图。构建时将自检改为非阻塞并加 800ms 上限，
  桌面端在探测成功后仍会正常启用 H.264。

### 登录与账号安全

- **登录页页脚归位**：`.login-wrap` 改为纵向排列，版权/页脚排在登录框下方居中；
  此前横向 flex 会把页脚摆到登录框右侧，窄屏时直接挤出视口。
- **后台增加显眼退出入口**：桌面端内容区右上与移动端顶栏各一个「退出登录」，
  侧边栏原有入口保留，并为菜单区加上滚动，菜单超出视口高度时仍可触达。
- **OTP 重新生成/关闭需验证当前密码**：`/api/otp/setup` 改为 POST，OTP 已启用时
  重新生成必须携带当前密码；`/api/otp/enable` 在已启用状态下再次绑定同样校密；
  `/api/otp/disable` 强制当前密码 + 当前动态验证码。此前仅凭一枚会话 Token 就能
  换绑验证器或关掉二次验证，等于把 OTP 的防护面让给了 Token 窃取者。

### Agent 页面与系统配置

- **「部署 Agent」与「Agent 接入」合并为「Agent」页**：两个页签分别对应
  SSH 部署（Linux）与通用部署（Agent 接入）；旧地址 `/admin/deploy`、
  `/admin/enroll` 自动重定向到对应页签，书签不失效。
- **新增「系统配置」页**：账号设置（改密 + OTP）收编为第一个折叠分区，
  另含登录会话、Agent 监听、后台配置；分区以 toggle 展示。
- **登录会话保持时长**：可选 1h / 12h / 24h / 7d / 15d / 30d，白名单校验后
  即时生效并落盘；缩短时同步收紧现有会话，过期由 401 自动退出登录。
- **监听地址可视化配置**：Agent 通道（默认本机外网 IP:9000）与 Web 控制台
  （默认本机外网 IP:8080）的 host/port 可在控制台修改。换端口时先绑新监听再关旧；
  同端口换 Host 时先返回响应再切换，失败自动恢复旧地址并回滚配置。
  两项变更都要求当前密码 + 风险确认并写审计。
- **配置跨重启保持**：安装器把初始地址写进服务命令，而命令行优先级更高；
  新增 `listen_managed` / `http_managed` 标记，控制台接管后配置文件优先，
  避免重启后监听地址被改回旧值。
- **端口变更自动放通防火墙**：Agent/后台监听端口变化时按服务端操作系统尝试放通
  TCP 入站：Windows 用 PowerShell（`:9000` 风格查询任意已放行规则，老系统回退 netsh），
  Linux 识别运行中的 firewalld / ufw；已有规则直接跳过。放通失败只提示，不回滚监听。
- **保存后直达新地址**：修改后台端口成功后自动跳转到「当前访问地址 + 新端口」的系统配置页；
  跳转前申请一次性会话交接码（128 位、60 秒、单次、绑定 IP、原 Token 必须有效），
  避免换 origin 后 localStorage Token 丢失而重新登录。
- **账号与安全大屏双栏**：宽屏下「修改密码」与「OTP 动态验证码」左右排列，
  窄屏自动回落为上下排列。
- **Agent 地址预下发**：新监听绑定成功后、断开旧连接前，向在线 Agent 下发新地址；
  Agent 落盘后回 Ack 并主动重连。只断开已确认的连接，旧版 Agent 保持原连接，
  响应与界面会回显通知/确认台数；全部网卡监听时只下发端口，保留各 Agent 自己的主机。
- **不绑定令牌回填实际身份**：签发时 `agent_id` 留空的令牌，首次成功换取证书/注册后
  会把实际上报的 `agent_id` 写回令牌记录；列表随之显示真实主机，不再一直显示「不绑定」。
  HMAC 校验仍按"签发时是否绑定"判定，回填不会让未绑定令牌获得接管已有主机的权限。
- **已使用令牌可按主机清理**：删除条件从"必须仍有证书绑定"放宽为"已使用且对应主机已不在册"。
  主机被删除、手动清理或绑定记录被清除后，「删除记录」即可用；未使用的令牌仍只能吊销。
- **安全评估**：本轮整体评估与新增配置面分析见 `docs/security-review-2026-10-21.md`。

### 服务端内存管控

- **探针历史 7 天 + 自动归档**：`probe_history` 按 10 秒采样直接保留 30 天会到百万行
  （8 台主机约 7 万行/天）。现在只保留最近 **7 天原始采样**；更早数据先导出为
  `<数据目录>/history-archive/<起始>2<结束>2<8位随机>_probe_history.gz`
  （gzip CSV，含全部 24 列，时间格式 `yyyymmddhhiiss`），确认落盘后才从库里删除；
  归档失败则保留数据、下轮重试。启动后维护一次存量，此后每 6 小时滚动执行；
  首次大清理后执行 VACUUM 并启用 `auto_vacuum=INCREMENTAL`，后续只做增量回收。
  延迟历史维持原有策略：24 小时原始 + 24 小时~30 天按 5 分钟聚合。
- **清理/压缩索引**：`probe_history(timestamp)`、`latency_history(timestamp)` 增加索引，
  避免保留期清理全表扫描；百万行库的清理从数秒级 IO 降到按索引范围删除。
- **SQLite 内存上限**：连接固定 `cache_size=8 MiB`、`temp_store=FILE`、
  `soft_heap_limit=64 MiB`；大范围 GROUP BY/排序落盘，不再把百万行塞进内存，
  这是本轮"内存暴涨"的直接止血项。
- **命令输出设上限**：Agent 侧单个输出流最多保留 stdout 1 MiB / stderr 256 KiB，
  超出部分丢弃并在末尾提示；超限仍向 `os/exec` 报告写成功，命令不会被 EPIPE 打断。
  这样 `cat` 大日志一类命令不会同时把 Agent 与服务端抬到数百 MB，
  也不会撑爆服务端 16 MiB 的连接扫描上限。
- **审计内存副本收紧**：命令字段入库/常驻上限 8 KiB，结果入库上限 64 KiB、
  内存副本 8 KiB；内存保留条数 10000 → 2000。完整趋势仍以 SQLite 为准，
  内存这份只用于数据库暂不可用时的兜底查询。
- **WebSocket 发送队列按字节限额**：在原有 256 帧上限外增加每连接 8 MiB 字节预算，
  慢客户端积压大帧时丢新帧（空队列仍允许单帧通过），避免"256 × 大消息"的放大。
- **探针历史降副本**：有 SQLite 时内存历史由每设备 1000 条降至 200 条
  （趋势与流量查询本来就走库）；无库模式仍保留 1000 条。
- **文件中转预算下调**：上传/分发单任务 256 → 64 MiB、总在途 512 → 128 MiB
  （分片收齐前全量驻内存，这两个值就是峰值口径）；SSH 一键部署改为**流式上传**
  （不再先把整包读入内存），单文件上限 200 → 64 MiB。
- **交互输入单行封顶 64 KiB**：黑名单要等整行到齐，半行缓冲原来可被一段无换行
  文本无限撑大；超限行直接拒绝并清空缓冲。
- **内存留痕**：堆占用超过 64 MiB 时每 5 分钟输出一次
  `[mem] heap/heap_sys/sys/goroutines`，便于定位后续增长。

### 构建脚本

- 修复 `scripts/build-linux-all.sh` 在 PATH 中存在同名 `env` 垫片（如只设置 PATH 的
  `~/.local/bin/env`）时"go build 返回成功但产物不存在、随后在 `du` 处报错"的问题：
  交叉编译环境改用 POSIX `export` 赋值，并在构建后校验产物确实落盘。
- zip 打包改用经过"真实可执行"筛查的 Python 解释器：Windows 商店的 `python3.exe`
  占位程序能被 `command -v` 找到，但执行时直接退出 49 且无输出，会让整个构建在 zip
  步骤静默中止；现在自动跳过占位程序、回退到可用的 `python` 或 `zip`。

### 安全加固（详见 `docs/security-review-2026-09-22.md` §11、§12）

- **高危**：修复 Agent 的 CA 指纹钉扎可被"附带公开 CA 证书"绕过 —— pin 命中链上 CA 时
  现在会真正验签叶子（签名、有效期、ServerAuth 用途），不再仅凭指纹出现在链里放行。
- **中危**：分享到期不生效、分享访客可抢占 `command_id`、未绑定令牌可重绑离线主机证书，
  三项均已修复并有单元与真实入口回归。
- **低危清零**：端口映射规则数上限（256）与分享来宾保留端口下限（1024）、
  `UpdateTraffic`/`Tracks` 快照消除数据竞争；未知用户名登录执行同成本 PBKDF2（抹平时延）、
  TOTP 改为常量时间比较并按用户拒绝同窗口重放；服务端指纹改为完整 64 位 hex
  （兼容历史 16 位短指纹，Agent 启动拒绝 8 位等畸形值）；Linux 安装 x11vnc 时使用
  `O_NOFOLLOW`，拒绝写入被预置的符号链接目标。

### 测试增量

- 新增：注入 CA 的 pin 绕过回归、分享命令 ID 抢占/命名空间、分享到期定时器、
  未绑定令牌重绑拒绝、端口映射规则上限与快照、TOTP 重放、指纹完整/前缀匹配与格式校验。
- `go test ./...`（server / agent）全绿；两模块 `go build ./... && go vet ./...` 通过；
  Agent 通过 `GOOS=linux` 交叉编译验证 `O_NOFOLLOW` 分支。
- 真实入口回归：登录控制台创建分享 → 非法 `it-` 命令 ID 被拒 → `share-it-1` 开/关终端 →
  撤销时 `closed_commands=1` → 管理端命令不受影响。

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
