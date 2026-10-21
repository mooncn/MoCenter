# MoCenter 整体安全性评估（2026-10-21）

## 1. 范围与方法

- **对象**：当前工作树（含未提交的登录页/退出入口/OTP 改密、Agent 页面合并、系统配置与监听热切换改动）。
- **方法**：
  1. 对鉴权、Agent 注册与证书换取、命令/文件通道、分享链接、备份恢复、SSH 部署、前端 XSS 面逐入口静态走查；
  2. 对新增的会话时长、Agent/后台监听配置做攻击面分析；
  3. 用临时 Server + 新版 Agent 做真实入口验证（监听切换、失败回滚、重启后配置保持、Agent 重连）；
  4. 对照 `security-review-2026-09-22.md` §10–§12，只复核仍开放项，不重复已修复结论。
- **边界**：本次不是第三方渗透测试，未做二进制逆向、依赖供应链审计或公网压测；结论以当前源码与本地实测为准。

## 2. 总体结论

**整体风险：中等偏低。** 默认部署下 Web 控制台走 TLS、Agent 通道走 TLS/mTLS，注册面已从"共享密钥"向"一次性令牌 + 证书 TOFU + 指纹钉扎"收敛；本轮新增的配置面没有引入新的未鉴权入口，高风险操作还加了二次验密。

最需要关注的三件事：

1. **部署级共享注册密钥仍是可选路径**（一把钥匙认全网）。全部主机迁移到一次性令牌后应加 `-disable-shared-agent-key`。
2. **发布物签名体系（H-5）仍未完成**：升级链的验签能力已在 Agent 侧，但官方发布物的签名/校验闭环尚未收口。
3. **控制台会话 Token 存在 localStorage**：CSP 与页脚白名单显著缩小了 XSS 面，但一旦 Token 被浏览器扩展/截图/本机恶意软件拿走，终端等高权限操作仍可直接执行；监听变更已要求再次输入密码，建议把备份恢复、SSH 部署等同样列为二次验密操作。

## 3. 新增功能的安全设计与验证

### 3.1 系统配置接口

- 所有 `/api/system/*` 都走 `guard()`：IP 白名单 + 每 IP 限流 + Bearer Token 校验。
- 监听地址变更额外要求：`confirm=true`、当前登录密码（`UserManager.VerifyPassword`，[system_config.go:262](file:///D:/Documents/MoCenter/server/system_config.go#L262)、[system_config.go:377](file:///D:/Documents/MoCenter/server/system_config.go#L377)）、并写入审计。
- 地址只接受 IPv4/IPv6 + 1–65535 端口（[listeners.go:257](file:///D:/Documents/MoCenter/server/listeners.go#L257)），不拼接 shell，不存在命令注入/SSRF 面。
- 两个监听变更用 `systemListenApplyMu` 串行化，避免"Ack 登记表 + 配置文件"并发交错（[system_config.go:222](file:///D:/Documents/MoCenter/server/system_config.go#L222)）。

### 3.2 监听热切换的失效保护

- **换端口**：先绑定新监听、成功后才关闭旧监听；绑定失败不影响现有入口（[listeners.go:108](file:///D:/Documents/MoCenter/server/listeners.go#L108)、[listeners.go:159](file:///D:/Documents/MoCenter/server/listeners.go#L159)）。
- **同端口换 Host**：旧监听占着端口，必须先释放。实现为"先返回响应、后台切换，失败自动恢复旧地址并回写配置"，HTTP 监听亦同（[listeners.go:199](file:///D:/Documents/MoCenter/server/listeners.go#L199)）。
- 本地实测：
  - `18080→18081→18080`、`127.0.0.1:18080↔0.0.0.0:18080` 均成功；
  - 绑定不存在地址 `1.2.3.4:18082` 同步失败、旧入口保持可用；
  - 同端口绑定 `1.2.3.4:18080` 后台失败后自动恢复 `127.0.0.1:18080`，配置回滚。

### 3.3 Agent 地址下发与重连

- 新协议 `ServerAddrUpdate/Ack`。服务端**先确保新监听绑定成功**，再向在线 Agent 下发；Agent 落盘成功才回 `OK`，服务端只断开**已确认**的连接，旧版 Agent 不会被无差别踢下线（[system_config.go:186](file:///D:/Documents/MoCenter/server/system_config.go#L186)、[main.go:2060](file:///D:/Documents/MoCenter/server/main.go#L2060)）。先绑后发避免了"Agent 已写入新地址、服务端却绑定失败"的死局。
- Agent 侧写配置采用"保留其它键 + 临时文件 rename"，权限 0600；Ack 的身份以连接为准（`connOwns`），报文自报 agent_id 不可信。
- 监听全部网卡时只下发端口，主机部分沿用各 Agent 自己的配置，避免把内网/域名接入的主机强行改到服务端猜测的公网 IP。
- 本地实测：端口 `19000→19001→19000`、`19000→19005` 均完成"下发→确认→切换→重连"，Agent 配置同步更新；故意绑定失败时服务端回滚地址，Agent 未失联。

### 3.4 会话保持时长

- TTL 只接受 1h/12h/24h/7d/15d/30d 白名单，禁止前端传任意 duration（[system_config.go:109](file:///D:/Documents/MoCenter/server/system_config.go#L109)）。
- 缩短 TTL 时同步收紧现有会话的到期时间，避免"改成 1 小时后旧 Token 还能用 30 天"（[auth.go:64](file:///D:/Documents/MoCenter/server/internal/auth/auth.go#L64)）。
- 过期后服务端返回 401，前端拦截器清 Token 并跳登录页（既有逻辑）。

### 3.5 跨端口会话交接

- 修改 Web 监听端口会进入新 origin，localStorage 中的 Token 不会带过去。为避免重新登录，
  旧地址先申请一次性交接码，新地址凭码换新 Token（[session_handoff.go](file:///D:/Documents/MoCenter/server/session_handoff.go)）。
- 约束：128 位随机、60 秒过期、单次使用、绑定申请时的客户端 IP、原 Token 必须仍然有效；
  兑换接口免登录但走同一套 IP 白名单与限流，响应 `no-store`。
- URL 中的交接码在用后即失效；即使被代理日志记录，也无法再次兑换。

### 3.6 配置跨重启一致性

- 安装器会把初始监听地址写进服务命令（binPath / systemd ExecStart），命令行原本优先级更高。若不处理，控制台改完地址、服务一重启就被改回旧值。
- 新增 `listen_managed` / `http_managed` 标记：控制台接管后配置文件优先（[config.go:22](file:///D:/Documents/MoCenter/server/internal/config/config.go#L22)、[main.go:177](file:///D:/Documents/MoCenter/server/main.go#L177)）。实测用旧参数重启后仍监听新地址，Agent 自动重连。

## 4. 本轮发现并已处置

| 级别 | 问题 | 处置 |
| --- | --- | --- |
| 中 | 监听地址变更原先只需一枚会话 Token 即可发起，可把 Agent 引向攻击者控制的主机或让控制台失去可达性 | 增加当前密码验证 + 显式风险确认 + 审计 |
| 中 | 监听配置保存后会被服务命令行的旧参数在重启时覆盖，表现为"改了等于没改" | 增加 managed 标记，配置文件优先 |
| 中（可用性） | 监听器重构后 `main` 立即返回，普通进程启动后直接退出 | `start()` 后阻塞，服务模式不受影响 |
| 低 | Agent 的延迟重连定时器按"当前连接"取值，可能把刚建立的新连接再关一次 | 定时器只关收到消息时的那条连接 |
| 低 | 旧版 Agent 不识别地址更新，切换后可能失联 | 只踢已 Ack 的连接，响应中回显通知/确认数量并在界面预警 |

## 5. 仍存在的风险与未做项

1. **控制台 Token 存 localStorage（中）**：CSP、页脚 HTML 白名单、审计降低了被利用概率，但不能消除"Token 被窃取即获得控制台能力"的事实。若未来引入多用户/更高暴露面，建议改为 HttpOnly Cookie + CSRF Token，或对全部高危操作要求二次验密。
2. **共享注册密钥（中）**：未关闭时，泄露任意一台主机即可冒名注册。迁移完成后应开启 `-disable-shared-agent-key`。
3. **发布物签名（中，H-5）**：升级链的签名校验已在 Agent 侧生效，但官方发布包的签名与公钥分发闭环仍未完成。
4. **Web 控制台可能被配置为明文/全网卡监听（低）**：`-admin-tls-days=0` 或选择 `0.0.0.0` 会扩大暴露面。启动日志已有告警，系统配置页也提示放通防火墙；仍依赖运维自身网络边界。
5. **旧版 Agent 兼容（低）**：不识别地址更新，切换后若连接断开会继续连旧地址。界面与响应会明确显示"确认台数"，不会伪装成全部成功。
6. **同端口换 Host 的短暂中断（低）**：约 1 秒，且失败会自动恢复；无法做到零中断。

## 6. 验证口径

- `go test -count=1 ./...`（server、agent）全部通过；`go vet ./...` 无输出。
- 前端 `npm run build` 通过。
- 真实入口：登录 → Agent 双页签 → 系统配置四个折叠分区 → 会话时长保存 → Agent/后台监听切换与回滚 → 重启后配置保持 → Agent 自动重连。
- 测试使用本地临时 Server/Agent 与临时数据目录；未读取或输出任何真实口令、users.json、agent.key。
