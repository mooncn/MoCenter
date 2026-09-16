# MoCenter 部署与运维手册

本文档说明 MoCenter 总控服务端、Agent、Web 前端的部署步骤，以及证书、备份恢复、常见故障处理。

## 一、环境要求

| 组件 | 环境 |
|------|------|
| 服务端 | Linux x86_64，2 核 4G（压测 4 核 8G），公网 IP，开放 TCP 端口（默认 9000）与 HTTP 端口（默认 8080） |
| Agent | Linux（CentOS/RHEL 7/8、Ubuntu 18.04~22.04、Debian 10/11）、Windows 10 1809+ / Server 2019+，x86_64 为主 |
| Web | 浏览器 Chrome/Edge/Firefox 最新两个大版本 |

## 二、部署方式

### 方式一：基于 Docker 容器化部署

```bash
# 构建服务端镜像（需 Dockerfile，可参考规划书容器化章节）
docker build -t mocenter-server ./server
docker run -d -p 9000:9000 -p 8080:8080 \
  mocenter-server -listen :9000 -http :8080 -user admin -password your-password
```

### 方式二：物理机直接部署（单进程托管 Web）

```bash
# 1. 构建前端并交由服务端托管
cd web && bash build.sh   # 生成 dist/
cd ../server && bash build.sh
# 2. 启动（同时托管 Web 静态资源，单进程部署）
./mocenter-server -listen :9000 -http :8080 -user admin -password your-password -web ../web/dist
```

## 三、Agent 部署

```bash
# 交叉编译 Linux / Windows Agent
bash agent/build.sh linux
bash agent/build.sh windows   # 生成 .exe

# 分发到各内网主机并启动
./mocenter-agent -server your.server.com:9000 -agentid host-01
```

### Linux 开机自启（systemd）

```ini
# /etc/systemd/system/mocenter-agent.service
[Unit]
Description=MoCenter Agent
After=network.target

[Service]
Type=simple
ExecStart=/opt/mocenter/mocenter-agent -server your.server.com:9000 -agentid %H -daemon
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now mocenter-agent
```

### Windows 开机自启

**推荐：图形安装程序** —— 双击 `MoCenter-Setup-<版本>.exe`，按向导走：

1. **选择角色**：Agent（被控端）/ Server（总控端）/ 两者
2. **连接与认证**：服务端地址、Agent 注册密钥（装 Agent 必填）、后台登录用户名与密码（装 Server 必填）
3. **高级设置**：数据目录、监听端口、代理前缀、TLS 参数（均有默认值）

> **Agent 注册密钥**在总控机的数据目录下（`agent.key`，权限 0600；首次启动自动生成，
> 也可用 `-agent-key` 显式指定）。Agent 注册时用 HMAC 证明持有该密钥，
> 没有它连接会被服务端拒绝 —— 这是 :9000 通道唯一的应用层身份凭证。

安装后：

| 项目 | 位置 |
|------|------|
| 二进制 | `%ProgramFiles%\MoCenter\` |
| 配置与数据 | `%ProgramData%\MoCenter\<角色>\`（与二进制分离，升级不覆盖） |
| 服务 | `MoCenterAgent` / `MoCenterServer`（开机自启 + 失败自动重启） |
| 日志 | `%ProgramData%\MoCenter\<角色>\logs\`（服务模式 stdout 无去处，故落文件） |

卸载走控制面板「应用和功能」，服务会被停止并移除，**配置与数据默认保留**。

**批量/静默部署**用随包安装的命令行引擎（参数与向导一致）：

```bat
mocenter-deploy.exe install -role agent -server your.server.com:9000 -agent-key <注册密钥>
mocenter-deploy.exe install -role server -user admin -password <登录密码>
mocenter-deploy.exe status -role all      REM 无需管理员
mocenter-deploy.exe uninstall -role all   REM 默认保留配置与数据
```

**手动注册**（服务名必须与程序内置的一致，并用 `-config` 指向固定位置的配置）：

```bat
sc create MoCenterAgent binPath= "\"C:\Program Files\MoCenter\mocenter-agent.exe\" -config C:\ProgramData\MoCenter\agent\mocenter-agent.json" start= auto
sc start MoCenterAgent
```

> 程序通过 `svc.IsWindowsService()` 自动识别"被 SCM 拉起"，**不需要 NSSM 之类的包装器**，
> 也不需要 `-daemon` 开关。早前用 `-daemon` 注册的方式没有 SCM 握手，SCM 会报 1053
> 且 `sc stop` 管不住它，现已修正。
>
> 便携形态仍然可用：直接运行 `mocenter-agent.exe` 即可，配置落在 **exe 同目录**。
>
> 打包安装程序：`scripts\packaging\build-setup.bat`（需先装 Inno Setup 6）。

## 四、Web 前端部署

```bash
cd web && npm run build   # 生成 dist/
# 将 dist 静态文件部署到 Nginx，或由 Go 后端一并托管
```

Nginx 反向代理示例（将 /api 与 /ws 代理到 Go 服务端）：

```nginx
server {
    listen 80;
    server_name mocenter.example.com;

    location / {
        root /var/www/mocenter;
        try_files $uri /index.html;
    }
    location /api {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
    location /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
    # 公开状态页的实时推送（免登录）；同样需要 WebSocket 升级头
    location /api/public/ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

### 公开状态页（未登录访客可见的非敏感信息）

**首页即公开状态页**：打开站点默认看到的是节点状态（不是登录表单），未登录访客
可查看主机**非敏感**运行指标，点击卡片直接进入 `/public/node/<节点编号>`
查看该节点信息与历史趋势；`/public` 与 `/` 指向同一页面（旧书签可用）。

页面结构：

- **顶部悬浮汇总条**（sticky，滚动时始终可见）：节点总数 / 在线节点 / 离线节点 /
  上行总网速 / 下行总网速 / 累计流量（含上下行拆分）。站点统计集中在这里，
  卡片区不再重复。
- **卡片视图**（小巧紧凑，一屏可见更多机器）：名称（带国旗与状态点）/ 系统类型 /
  CPU / RAM / 磁盘 / 负载。
- **列表视图**：节点名称 / 状态 / 系统类型 / 系统版本 / 地区 / CPU / 内存 / 磁盘 /
  网速 / 流量 / 负载。
  表头可按**非动态字段**（名称、状态、系统类型、系统版本、地区、磁盘容量）排序：
  第一次点击升序、再点降序、第三次恢复默认（在线优先 + 名称）。CPU / 网速 / 负载
  每 5 秒变化，不提供排序 —— 否则行序会不停跳动。

卡片与列表展示的**每一项都受后台「展示配置 → 展示信息项」门控**：未勾选的字段
在服务端结构体层面即零值，JSON 里根本不存在，前端自然也不会渲染对应列/项
（双重保证：既能按需精简，也不会因前端疏漏而泄露）。

**登录后台的落地页是 `/admin`（后台仪表盘），不是公开页**：管理后台整体挂在
`/admin/*` 下 —— `/admin` 仪表盘、`/admin/terminal`、`/admin/audit`、
`/admin/site-config` …；登录页为 `/admin/login`。根级旧路径
（`/terminal`、`/audit` …）会自动跳转到对应的 `/admin` 地址，未登录时先带
`redirect` 去登录页并在登录后回到原地址。

> 注意：**不要**把 `/admin/*` 反代成公开页或登录页，也不要让前端路由丢给后端
> 做 301 —— 前端用 SPA 路由自己处理。Nginx 只需把未命中的路径回退到 `index.html`。

**默认关闭**，需在后台「展示配置」中开启。

### 节点地区 / 国旗不显示怎么排查

公开页的「地区」与国家国旗来自**服务端**按主机公网 IP 解析的地理位置。链路是：

```
Agent 探测公网出口 IP → 上报 → 服务端多源解析 → 公开页渲染 国家/省/市 + 国旗
```

**服务端解析采用多源降级**（任一源成功即返回，全部失败才放弃）：

**仅 IPv4 走主源**：`api.iping.cc` 对 IPv6 恒返回 `code=500`（"IPv6 数据正在升级优化中"），
排在前面只会多一次无效往返和一条误导性的错误日志。因此 IPv6 直接从备源开始。

| 顺序 | 数据源 | 说明 |
|------|--------|------|
| 1 | `api.iping.cc`（主，仅 IPv4） | `https://api.iping.cc/v1/query?ip=<ip>&language=zh`，中文国家/省/市名；偶发限流（`code=500` + `msg` 含 `frequent`）；**不返回 IP 所属国家代码**，但返回 `as_country` / `company_country`（归属国 ISO）可作为国家代码 |
| 2 | `ip-api.com`（备） | `http://ip-api.com/json/<ip>?lang=zh-CN`，带 `countryCode`；免费额度约 45 次/分钟 |
| 3 | `ipwho.is`（兜底） | `https://ipwho.is/<ip>`，HTTPS，带 `country_code` |

国家代码按优先级确定，**绝不猜测**：

1. 主源的 `as_country` / `company_country`；
2. 备源 / 兜底源返回的 `countryCode` / `country_code`；
3. 都没拿到时，用**中英文国名 → ISO 代码对照表**
   （`internal/geo/countries.go`，覆盖 249 个 ISO 代码）反查。

中文国名统一为英文名（避免同一批节点因两个源口径不同而看起来像两个国家）。
三步都查不到时保留上游原值 —— 宁可不显示国旗，也不要把国家写错。

Agent 侧公网 IP 回显服务按国内可达性排序：
`tool.mooncn.win/ip/?format=json` → `api-ipv4.ip.sb` → `ipv4.icanhazip.com` →
`ipv4.ident.me` → `api.ipify.org`（IPv6 走对应 v6 服务）。回显结果会**按地址族过滤**：
无 IPv6 出口的机器请求 IPv6 回显会拿到 IPv4，直接过滤掉而不是写进 `public_ipv6`。
探测失败会 **60 秒后重试**（而不是缓存半小时）。

任何一环断了都表现为"地区与国旗是空的"，按下面顺序排查：

1. **主机有没有公网 IP**：`curl -s localhost:8080/api/agents -H "Authorization: Bearer <token>"`
   看 `public_ip`。为空说明 Agent 探测失败（回显服务不可达 / 未配 `-proxy-ip`）。
2. **服务端能否访问上游接口**：看服务端日志里的 `[geo]` 行，会逐源打印失败原因
   （`HTTP 429` / 连接超时 / `code=500`）。主源限流会自动降级到备源；三源全挂时
   失败结果只缓存 10 分钟，恢复后自动补上，无需重启。
   直连不稳定时可给**服务端**配加速代理：
   `mocenter-server -proxy-ip http://fa.mooncn.win/?url=`（或配置文件
   `proxy_ip` / 环境变量 `MOCENTER_PROXY_IP`）。
3. **是不是内网 IP**：内网 / 保留地址（`10.*`、`192.168.*`、CGNAT `100.64.*`、
   IPv6 ULA 等）在公网上本就没有地理位置，服务端直接跳过解析（不消耗额度），
   公开页自然不显示地区 —— 这是正常的，不是故障。
4. **展示配置里「地区」是否勾选**：`fields.geo` 未开启时，地区字段在服务端
   结构体层面就是零值，JSON 里根本不存在。

### 国旗的渲染方式（为什么不用 emoji）

前端拿到 `country_code` 后**不使用国旗 emoji**，而是渲染内置字形。原因是
用区域指示符拼出的 🏳 emoji 能不能显示**取决于访客的操作系统字体**：

| 环境 | 区域指示符 emoji 的表现 |
|------|------------------------|
| Windows 10 及更早 | Segoe UI Emoji 没有连字，显示成两个方框 |
| 精简版 Linux 容器 / 无桌面字体 | 完全没有 emoji 字体，显示豆腐块 |
| 禁用彩色字体回退的浏览器 | 同上 |

症状是"接口里 `country_code` 明明有值，页面上国旗却是空白/方框"，而且
**只在部分访客的机器上复现**（运维本机正常），极易被误判成后端数据问题。

因此字形被固化进前端资源（`web/src/utils/flagSprite.js`）：

- 覆盖 249 个 ISO 3166-1 代码 + 9 个常用特殊代码（XK / EU / UN 等）；
- 栅格化为 36×36（覆盖 2× DPI 下 18px 显示尺寸）+ 160 色全局调色板 + deflate，
  产物约 45 KB（gzip 后约 35 KB），**不请求任何外部 CDN**，内网/离线同样可用；
- 渲染与宿主字体完全无关；未收录的代码退回 emoji 兜底（此时至少还能看到代码文本）。

相关脚本（都在 `web/`）：

```bash
npm run gen:flag     # 重新生成字形资源：需联网 + pip3 install cairosvg pillow
npm run check:flag   # 校验字形数据与解码逻辑是否自洽（无需网络，可在 CI 跑）
```

生产部署前建议跑一次 `check:flag` —— 字形数据是**生成**的、解码是**手写**的，
两边脱节的表现就是"国旗又不见了"。

配置项（页面可视化配置，落盘于数据目录 `public-site.json`）：

| 配置 | 说明 |
|------|------|
| `public_enabled` | 总开关。关闭时公开页与 `/api/public/*` 对外**不存在**（404） |
| `hosts_enabled` | 主页是否展示主机条目；关闭后只展示站点级汇总指标 |
| `catalog_enabled` | 是否展示全部主机；关闭则只公开 `host_ids` 白名单内的主机 |
| `host_ids` | 公开主机白名单（agent_id） |
| `fields` | 逐字段开关（备注、CPU/内存/磁盘/负载、地区、网络流量、系统、公网 IP…），未勾选的字段**数据不下发**，卡片与列表对应的项/列同步隐藏 |
| `traffic_mode` | 流量统计口径：`boot`=自设备启动以来累计（网卡计数器）；`window`=历史留存窗口内累计（采样增量求和）。默认 `boot` |

### 网络流量统计口径

「网络流量」开启后，公开页展示**单节点**与**站点合计**的上下行数据：

- 卡片/列表：实时下行、上行速率 + 累计流量；
- 站点汇总条：下行速率、上行速率、累计流量（含 ↓/↑ 拆分）；
- 节点详情：明细字段 + 趋势图（速率序列走独立右轴，与 CPU/内存/磁盘的 0–100% 左轴分开）。

安全上的两点处理（直连连接类节点尤其重要）：

1. **只下发速率与按口径裁剪的累计量**，网卡累计原始计数器（`net_in_bytes`）不出现在
   公开响应中 —— 后者等于公开「这台机器总共跑了多少流量」，即业务承载量。
2. **趋势图的流量由服务端按相邻采样差值计算**，前端拿不到累计序列；计数器回绕、
   时间戳重复/倒退的点不输出（前端不连线），而不是画成 0 或反向尖刺。
   首次升级后历史记录里的增量列（`net_in_delta`/`net_out_delta`）为 0，趋势图会
   跳过这些旧点，属正常现象。

安全设计（部署前请知悉）：

1. **主机标识不公开**：对外只有服务端派生的节点编号 `n-xxxxxxxx`（SHA-256 前 4 字节），
   无法反推 `agent_id`、硬件 UUID 或内网地址 —— 这些字段在公开响应结构里根本不存在。
2. **敏感项默认关闭**：主机名、系统版本、内核版本、公网 IP 默认不可见，可显式开启；
   公网 IP 即使开启也会做部分掩码（`203.0.*.*`），精确经纬度永不公开。
3. **独立限流**：公开端点为匿名入口，另有独立于登录的限流（每路由独立计数），
   避免被扫描器滥用；单次历史趋势最多返回 300 点、回溯窗口上限 30 天。
4. **审计留痕**：每次修改展示配置都会写入操作审计（分类 `op`）。

对外发布前建议自查：

```bash
# 应输出 404（未启用）
curl -si http://your-host/api/public/state | head -1
# 启用后确认响应中不含 agent_id / uuid / 内网 IP / 经纬度
curl -s http://your-host/api/public/state | grep -Ei 'agent_id|uuid|remote_addr|"lat"|"lon"'
```

## 五、安全配置

1. **设置登录账号**：服务端启动时通过 `-user` / `-password` 指定后台账号，并在 Web「账号设置」里
   绑定 OTP 二次校验。凭证只有一种：用户名 + 密码(+OTP)，**没有全局 API Key**。
2. **IP 白名单**：通过 `-whitelist 1.2.3.4,10.0.0.0/8` 限制可访问服务端的源 IP。
3. **TLS/mTLS**：Agent<->Server 支持全链路 TLS 加密，可升级为 mTLS 双向认证。生成自签名证书：

   ```bash
   bash scripts/gen-cert.sh certs   # 生成 ca.crt / server.crt / client.crt
   ```

   **单向 TLS**（加密传输）：
   ```bash
   # 服务端
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -tls-cert certs/server.crt -tls-key certs/server.key
   # Agent（自签名跳过校验）
   ./mocenter-agent -server host:9000 -agentid host-01 -tls -insecure
   ```

   **mTLS 双向认证**（服务端校验 Agent 客户端证书）：
   ```bash
   # 服务端（额外提供 -ca-cert，要求 Agent 携带有效客户端证书）
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -tls-cert certs/server.crt -tls-key certs/server.key -ca-cert certs/ca.crt
   # Agent（携带客户端证书，并用 CA 校验服务端证书）
   ./mocenter-agent -server host:9000 -agentid host-01 -tls -client-cert certs/client.crt -client-key certs/client.key -ca-cert certs/ca.crt
   ```

   生产环境建议使用受信任 CA 签发的证书，实现 mTLS 双向认证。Web 端启用 HTTPS/WSS。

4. **命令黑名单**：高危命令（`rm -rf /`、`mkfs`、`sudo` 等）由服务端拦截并记录审计。

## 六、高级运维能力

### 1. 交互式 PTY 终端
Web「远程终端」页面选择在线主机并执行交互式命令（如 `bash`、`top`、`vim`），支持实时双向 IO、终端尺寸自适应、Ctrl-C 中断。

### 2. 批量命令
Web「运维操作」页面勾选多台在线主机，批量下发同一命令；服务端逐一审计留痕。REST 接口：`POST /api/command/batch`。

### 3. 远程升级 Agent
Web「运维操作」页面填写升级包 URL 与 MD5 校验和，下发升级指令。Agent 依次执行：下载升级包 → MD5 校验 → 备份旧版本 → 替换二进制 → 自动重启；失败自动回滚。REST 接口：`POST /api/upgrade`。

### 4. 端口映射
Web「端口映射」页面创建规则（公网端口 → 内网服务端口），服务端反向代理实时转发到目标 Agent，支持源 IP 白名单、连接跟踪与流量统计。REST 接口：`GET/POST /api/portmap/rules`、`POST /api/portmap/op`。

### 5. 文件传输
支持 Agent 文件上传/下载，分片传输 + MD5 校验 + ACK/NACK + 断点续传，接收目录默认为服务端 `/tmp/mocenter_uploads`。

### 6. 文件分发到主机
Web「运维操作」页面选择目标在线主机与 Agent 端目标路径，上传本地文件分片经服务端中转、MD5 校验后写入 Agent 指定路径。适用于批量下发部署包、配置文件、脚本等场景。

### 7. VNC 安装包由服务端分发

Agent 一键安装 VNC 时，默认从公网下载安装包（UltraVNC，约 4–5MB）。为规避公网限速与
下载被劫持/篡改的风险，可把安装包放到服务端托管目录：

```bash
bash scripts/fetch-packages.sh ./packages                 # 下载安装包到托管目录
./mocenter-server ... -data ./data -pkg-dir ./packages    # 指向该目录（不指定则默认 <data>/packages）
```

启动日志会打印已托管的包清单（`[pkg] 安装包目录 ... 已托管: ...`）。
Agent 会**优先通过它与服务端之间既有的长连接**拉取（该通道可启用 TLS/mTLS），
服务端未托管该包时自动回退公网下载，因此托管是可选增强、不影响可用性。

托管文件名须与 Agent 期望的一致（`scripts/fetch-packages.sh` 会按此命名下载 Windows 包，
也可加参数只下某平台：`bash scripts/fetch-packages.sh ./packages win`）：

| 文件名 | 平台 |
|--------|------|
| `UltraVNC_1824_x64_Setup.exe` / `UltraVNC_1824_x86_Setup.exe` | Windows |
| `x11vnc-linux-amd64` / `-arm64` / `-arm` / `-386` | Linux（需自备） |

> **Linux 说明**：x11vnc 官方只发布源码、无任何预编译产物，因此脚本不代为下载。
> Linux 侧优先由 Agent 用发行版包管理器安装（RHEL/Rocky/Alma 走 EPEL，Debian/Ubuntu 直接有）；
> 仅当目标机无法访问软件源时，才需自行构建/自备二进制并按上表命名放入托管目录。

服务端会校验包名以防目录穿越；Agent 会校验可执行文件魔数（`MZ`/`ELF`），
拒绝执行错误页或被替换的内容；安装包拉取属于内部行为，不会出现在「文件分发」列表中。

## 七、延迟监测（ICMP / TCP / HTTP）

Agent 按**按主机下发**的目标列表探测往返延迟，服务端存历史并在后台「延迟监测」页出曲线。

### 目标格式

| 写法 | 探测方式 | 实现 |
|------|----------|------|
| `icmp:1.1.1.1` | ICMP | 调用系统 `ping` 并解析往返时间（Linux `-c 1 -W <秒>` / Windows `-n 1 -w <毫秒>` / macOS `-W <毫秒>`） |
| `tcp:1.1.1.1:443` | TCP | 建立 TCP 连接并测量握手耗时；地址缺端口时默认 443 |
| `http:https://example.com` | HTTP | 发起 HEAD 请求并测量首字节往返；缺 scheme 时按 https 处理 |

省略 `kind:` 前缀时按 ICMP 处理（例如 `8.8.8.8`）。单机最多 **8 个**目标。

> **为什么 ICMP 用系统 ping 而不是原生 socket**：原生 ICMP 需要 `CAP_NET_RAW`/root，
> 还要自己拼 ICMP 包、处理校验和与 IPv4/IPv6 双栈。系统 ping 在三大平台都内置且输出格式稳定，
> 引入的复杂度远大于收益。代价是目标机若没有 ping 命令（极简容器），ICMP 探测会失败 ——
> 表现为该目标**恒定超时**（而不是 0ms），属于可观测的正常降级。

### 两个必须知道的口径

1. **超时点画成断点，不是 0**。探测失败的目标不出现在上报数据里，接口返回的每个点带 `ok` 标记，
   前端的折线会**断开**。若把失败写成 0，"完全不通"的机器会看起来比谁都快 —— 这是监控系统里
   最危险的一类显示错误，因此这条约束贯穿 agent（不上报）、服务端（`ok=false` 落库）、
   前端（分段绘制）三层。
2. **HTTP 的 4xx/5xx 仍算"可达"**。这条链路测的是"网络是否可达、往返多久"，不是"业务是否正常"。
   把 403 当成探测失败，会让"有防火墙但链路正常"的机器显示成超时。

### 可用率与丢包率

每次探测（含失败）都会落 `latency_history` 表（一行一个"时间+目标"），因此后台能算出
**丢包率**。配置目标但当前无数据 = 该目标本轮超时，仪表盘会显示"N 个超时"。

### 配置方式

- **后台**：`延迟监测` 页选择主机 → 下方「探测目标」填目标（每行一个）→ 保存，下一个采集周期（默认 10 秒）生效。
- **Agent 侧**：`-latency-targets 'icmp:1.1.1.1,tcp:1.1.1.1:443'`，或配置文件字段 `latency_targets`。

留存：延迟历史与探针历史同口径，均保留 **30 天**（每 6 小时清理一次）。

## 八、离线 GeoIP（可选）

节点地区默认由三个外部接口降级解析（api.iping.cc → ip-api.com → ipwho.is）。配置离线库后
**本地优先解析**，未命中才走外部接口。

```bash
# 把 GeoLite2-Country.mmdb 放到数据目录，并配置路径
./mocenter-server -mmdb /var/lib/mocenter/GeoLite2-Country.mmdb
# 或写入配置文件字段 mmdb_path，或环境变量 MOCENTER_MMDB
```

### 为什么建议配

| 问题 | 未配离线库 | 配了离线库 |
|------|-----------|-----------|
| 服务端无外网出口（纯内网 / 白名单出网） | **全站节点地区都是空的** | 正常解析 |
| 一批 Agent 同时上线 | 免费接口按分钟限流，互相挤掉额度，表现为"某时间点之后新上线的机器都没地区" | 不走网络，无额度概念 |
| IPv6 节点 | 主源 api.iping.cc 对 IPv6 恒返回 `code=500` | 本地库直接支持 |
| 解析耗时 | 每台首次解析一次 HTTP 往返（数十~数百 ms） | 本地文件查询，微秒级 |

### 支持的库与获取

兼容 **MaxMind DB 格式**：`GeoLite2-Country.mmdb`、`GeoLite2-City.mmdb`、
DB-IP / IPinfo 的 Country / City 库均可（字段名兼容）。GeoLite2 可从 MaxMind 免费账号下载。

库文件更新后**无需重启服务端**：`Resolver.ReloadMMDB(path)` 即可热重载
（当前 REST 层未开放热重载端点，可用配置变更后重启，或按需扩展）。

### 校验是否生效

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/version | jq '{mmdb_loaded, mmdb_path}'
```

启动日志也会明确打印：加载成功打印库路径与数据库类型；未配置则提示"地区解析走外部接口"
并给出配置指引；配置了但加载失败会告警并**自动降级**（不会让服务端起不来）。

## 九、备份与恢复

后台「备份恢复」页可直接操作，也可用 REST API。

### 备份内容

| 文件 | 说明 | 是否含凭据 |
|------|------|-----------|
| `mocenter.db` | SQLite：主机、探针/延迟历史、端口映射、审计日志、黑名单 | 是 |
| `users.json` | 账号与口令哈希、OTP 密钥 | 是 |
| `remarks.json` | 主机备注（用户输入的唯一副本） | — |
| `public-site.json` | 公开展示配置 | — |
| `agent.key` | Agent 注册预共享密钥（丢了所有 Agent 都连不上） | 是 |

**刻意不备份**：`packages/`（安装包，可从上游重下，动辄上百 MB）、`uploads/`（临时文件）、
日志、SQLite 的 `-wal`/`-shm`。

> 备份包**含凭据**。请按"等同口令文件"对待，不要随意外发。

### 关键实现细节

- **SQLite 用 `VACUUM INTO` 而不是直接拷文件**。默认 WAL 模式下未 checkpoint 的事务在 `-wal` 里，
  直接拷主库会丢这部分数据 —— 恢复出来表现为"最近一段时间的审计/探针记录莫名消失"。
  `VACUUM INTO` 产出一份事务一致、已 compact 的完整副本。
- **包内带 manifest.json**：格式版本、生成时间、服务端版本、来源主机、每个文件的大小与 SHA-256。
  恢复前可先 `inspect` 查看，恢复时逐条校验摘要（不一致直接失败，而不是"先恢复了再说"）。
- **恢复的多重防护**：条目名必须在白名单内、必须是纯文件名（拒绝 `/`、`\`、`..`）、
  落盘前再校验绝对路径仍在数据目录内（防 zip-slip）、解压总体积有上限（防 zip bomb）。
- **恢复前自动留存**：被覆盖的文件另存为 `<文件名>.before-restore`，恢复失败时提示可手工回退。
- **恢复后需重启服务端**才能让配置与账号生效。

### REST API

```bash
# 创建备份
curl -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/backup
# 列表 / 查看清单 / 恢复 / 删除
curl -H  "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/backup
curl -X POST -H "Authorization: Bearer $TOKEN"   "http://127.0.0.1:8080/api/backup?action=inspect&file=mocenter-backup-20260101-120000.zip"
curl -X POST -H "Authorization: Bearer $TOKEN"   "http://127.0.0.1:8080/api/backup?action=restore&file=mocenter-backup-20260101-120000.zip"
curl -X DELETE -H "Authorization: Bearer $TOKEN"   "http://127.0.0.1:8080/api/backup?file=mocenter-backup-20260101-120000.zip"
```

备份文件存放在数据目录的 `backups/` 下。`?download=` / `?file=` 只接受**纯文件名**，
不做任意路径读取（否则这里就是个任意文件下载漏洞）。

## 十、后台一键部署 Agent（Linux）

后台「部署 Agent」页：填目标机地址 + 账号/密码（或私钥），服务端通过 SSH 登录后
上传 Agent、写入配置、注册为 systemd 服务。

### 前置条件

- 目标机可 SSH 登录，且具备 **root 权限或免密 sudo**（`sudo -n true` 可用）；
- 服务端数据目录的 `packages/` 下有**匹配目标架构**的 Agent 二进制
  （`mocenter-agent-amd64` / `-arm64` / `-arm` / `-386`；也可用 `?binary=` 指定，支持 `.exe` 后缀）；
- 没有 systemd 的环境（容器）会退化为"后台进程 + 进程守护"，**不提供开机自启**，步骤里会明确告知。

### 支持"先探测再安装"

「测试连接与探测环境」按钮走 `probe_only=true`：只做 SSH 连通性、认证、架构、权限与
systemd 可用性探测，**不做任何改动**。建议先探测确认，再点部署。

### 部署流程与回传

每一步都会回传状态与原始输出（前端按顺序展示，失败时能直接看到卡在哪一步）：

1. SSH 连接与认证（密码 / 键盘交互 / 私钥 / 私钥+passphrase）
2. 探测目标机环境（`uname -m` → GOARCH、`/etc/os-release`、`id -u`、`sudo -n`、是否 systemd）
3. 上传 Agent 二进制（走 stdin 的 `cat > file`，不依赖目标机是否启用 sftp 子系统）
4. 写入 Agent 配置（服务端地址、Agent 名称、注册密钥、代理、延迟目标，权限 0600）
5. 安装为 systemd 服务并启动（配置与二进制分离：`/etc/mocenter/mocenter-agent.json` + `/usr/local/bin/mocenter-agent`）
6. 回读状态确认（`is-active` / `is-enabled` / `-version`）

### 安全约束

- **凭证不落盘、不入日志**。密码/私钥只在单次请求内存中存在；审计日志只记"谁在何时部署了哪个地址"。
- **命令注入防护（纵深防御）**：AgentID 过白名单（`[A-Za-z0-9._-]{1,64}`）、安装目录与服务名过格式校验、
  所有拼进 shell 的变量经 POSIX 单引号转义。即使上游校验有疏漏，也不会因一个引号变成命令注入。
- **上传体积上限** 200 MB；安装包文件名拒绝目录成分。
- 接口需登录（与其它后台接口同一套 Token + IP 白名单 + 限流）。

> **Windows 未实现**：OpenSSH for Windows 虽然存在，但凭证、服务注册与权限模型与 Linux 差异大，
> 按需求"没有类似方案则略过"。Windows 侧请继续用图形安装程序（`MoCenter-Setup-<版本>.exe`）或
> `scripts\packaging\install-windows.bat`。

## 十一、版本号管理

**server 与 agent 共用同一版本号**，集中定义在：

- `server/internal/version/version.go` → `Version`
- `agent/internal/version/version.go` → `Version`

### 发布纪律

- **只有在明确发版时才修改版本号**。服务端**不提供**运行时改版本的命令行参数或环境变量 ——
  避免线上跑着一个"版本号被临时改过"的二进制，导致升级判定错乱。
- 改版本号时**必须同时修改两处**，且同步 `installer/setup.iss` 的 `#define AppVersion`。
  两处 Go 常量的一致性由 `server/internal/version` 的单元测试守护（会读取 agent 侧源码比对），漏改会直接测试失败。
- `scripts/packaging/build-linux-pkg.sh` 的版本号自动从 agent 源码读取，
  只有读不到时才用兜底值；仍可用 `MOCENTER_VERSION` 显式覆盖。

### 运行时可观测

```bash
# 打印版本号后退出（不读配置、不连服务端）
./mocenter-agent -version
./mocenter-server -version

# 服务端版本 + 各 Agent 版本与"是否需要升级"判定 + 离线 GeoIP 状态
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/version
```

Agent 在注册时把版本号写入 `AgentInfo.AgentVersion` 并落库，仪表盘「版本」列展示；
**空版本**表示旧版 Agent 未上报，前端显示「未知」，升级判定按"需升级"处理（它一定不是当前版本）。

远程升级时，升级包声明的 `version` **必须等于当前服务端版本**（不填则自动取服务端版本），
否则接口直接拒绝 —— 防止出现"server 1.0.1 却把 agent 升到 1.0.2"的版本漂移。

## 十二、常见故障处理

| 现象 | 排查方法 |
|------|----------|
| Agent 无法上线 | 检查 `-server` 地址端口、防火墙规则、服务端是否启动 |
| Agent 频繁重连 | 查看心跳日志；弱网环境服务端会自动拉长心跳间隔 |
| 命令执行超时 | 调整 `CommandReq.timeout`；检查命令自身是否阻塞 |
| Web 连接断开 | 确认 Token 有效、WebSocket 代理配置正确（Upgrade 头） |
| 端口映射不通 | 确认映射规则已启用、Agent 在线、目标内网服务可达 |
| 节点地区/国旗不显示 | ① 是否有外网出口；无外网请配 `-mmdb` 离线库。② 看日志里的 `[geo]` 行（会区分"限流 / 网络不通 / 不支持该地址族 / 该 IP 无数据"）。③ 后台「展示配置」总开关是否开、地区字段是否勾选。④ 直连 ip-api.com 不稳时配 `-proxy-ip` |
| 延迟目标一直显示"超时" | ICMP 依赖目标机的 `ping` 命令（极简容器可能没有），换 `tcp:` 目标验证链路；`http:` 目标缺 scheme 时按 https 处理，自签证书不受影响（探测忽略证书校验） |
| 一键部署报 `sudo: not found` | 已修复：当前用户是 root 时不再使用 sudo。若版本较旧请升级服务端 |
| 一键部署报服务启动成功但 Agent 不上线 | 检查写入的注册密钥是否与服务端一致（`/etc/mocenter/mocenter-agent.json` 的 `agent_key`）、服务端地址是否从目标机可达；`systemctl status mocenter-agent` 看日志 |
| 恢复备份后数据没变 | 恢复后需**重启服务端**；被覆盖的旧文件在 `<文件名>.before-restore` |

## 十三、远程升级 Agent

服务端发布新版本 → Agent 检测到新版本 → 下载升级包 → 校验签名 → 停止旧进程 → 替换二进制 → 重启新进程 → 上报版本信息。支持差分升级、失败自动回滚、灰度升级（规划书 12.4）。
