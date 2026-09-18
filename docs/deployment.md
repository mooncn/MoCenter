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

仓库自带 `server/Dockerfile`，多阶段构建为单进程镜像（同时提供 :9000 与 :8080）。

```bash
# 1) 先构建 Web 前端（可选；不构建则容器不含 Web 界面）
bash web/build.sh

# 2) 构建镜像 —— 构建上下文是仓库根目录（镜像需 web/dist），故用 -f 指定 Dockerfile
docker build -f server/Dockerfile -t mocenter-server:latest .

# 3) 运行（数据挂载到持久卷，否则重建容器会重新生成 agent.key，
#    已注册的 Agent 需要改用新密钥重新注册）
docker run -d --name mocenter-server \
  -p 9000:9000 -p 8080:8080 \
  -v mocenter-data:/var/lib/mocenter \
  -e MOCENTER_USER=admin -e MOCENTER_PASSWORD='强口令' \
  mocenter-server:latest

docker logs -f mocenter-server      # 查看启动日志（含托管安装包清单）
```

启动参数通过环境变量注入（镜像内已全部展开）：`MOCENTER_USER` / `MOCENTER_PASSWORD` /
`MOCENTER_LISTEN` / `MOCENTER_HTTP` / `MOCENTER_WHITELIST` / `MOCENTER_TLS_CERT` /
`MOCENTER_TLS_KEY` / `MOCENTER_CA_CERT`；其它参数可用 `MOCENTER_EXTRA_ARGS` 透传。
未提供用户名/口令时，容器内会自动创建 `admin` 并生成随机强口令写入
`/var/lib/mocenter/initial-password.txt`（权限 0600），可用
`docker exec mocenter-server cat /var/lib/mocenter/initial-password.txt` 查看后删除。

### 方式二：物理机直接部署（单进程托管 Web）

```bash
# 1. 构建前端并交由服务端托管
cd web && bash build.sh   # 生成 dist/
cd ../server && bash build.sh
# 2. 启动（同时托管 Web 静态资源，单进程部署）
./mocenter-server -listen :9000 -http :8080 -user admin -password your-password -web ../web/dist
```

### 方式三：Linux 一键构建 + 安装包（推荐）

构建与打包统一由 **`scripts/build-linux-all.sh`** 完成（`scripts/packaging/` 下的零碎脚本已合并进它），
产物按平台分类落到 `build/`：

```bash
# 默认：全部类型 + 全部平台 + 全部架构 + 全部安装包
bash scripts/build-linux-all.sh

# 只出服务端多架构二进制（amd64/arm64/arm/386 -> build/linux/binaries/server/linux/）
bash scripts/build-linux-all.sh --type=server --package=none

# 只出 .deb（amd64）：会自动先构建需要的 linux 二进制
bash scripts/build-linux-all.sh --type=server --package=deb --arch=amd64

# Server + Agent + Web + 两类安装包，一步到位
bash scripts/build-linux-all.sh --type=server,agent,web --package=all --arch=amd64

# Windows 免安装 zip（在 Linux 上用交叉编译产出）
bash scripts/build-linux-all.sh --type=agent,server --platform=windows --package=zip
```

产物布局：

| 产物 | 路径 |
|------|------|
| 服务端二进制 | `build/linux/binaries/server/linux/mocenter-server-<arch>` |
| Agent 二进制 | `build/linux/binaries/agent/<linux\|windows>/mocenter-agent-<arch>[.exe]` |
| Web 前端 | `build/linux/web/` |
| `.deb` / `.rpm` | `build/linux/packages/{deb,rpm}/` |
| Windows zip | `build/windows/packages/zip/` |
| Windows 图形安装程序 | `build/windows/installer/`（需在 Windows 上编译） |

安装包注册服务后的落盘位置：

| 项目 | 位置 |
|------|------|
| 二进制 | `/usr/local/bin/mocenter-server` |
| 启动参数 | `/etc/mocenter/mocenter-server.conf`（**请修改口令与白名单**） |
| 全量配置 | `/etc/mocenter/mocenter-server.json`（首次启动自动生成） |
| 数据目录 | `/var/lib/mocenter/data`（审计日志、`users.json`、`agent.key`） |
| 托管安装包 | `/var/lib/mocenter/packages` |
| Web 前端 | `/usr/share/mocenter/web` |
| 装卸说明 | `/etc/mocenter/README.server` |
| 日志 | `journalctl -u mocenter-server -f` |

不打安装包、直接用脚本注册（需 root）：

```bash
bash scripts/build-linux-all.sh --type=server --package=none
sudo bash scripts/deploy/install-linux.sh --role=server \
     --bin=build/linux/binaries/server/linux/mocenter-server-amd64 \
     --user=admin --password='强口令'
sudo bash scripts/deploy/install-linux.sh --role=all status
sudo bash scripts/deploy/install-linux.sh --role=server uninstall      # 加 --purge 连配置数据一起删
```

> 安装脚本注册的 systemd 单元与安装包内的是**同一份模板**（`scripts/deploy/mocenter-*.service`），
> 不会再出现"打包脚本改了、安装脚本忘改"。
> 服务只监听 `:9000`（Agent 长连接）与 `:8080`（Web 控制台 / REST API）。
> 生产环境请配置 TLS 证书（`MOCENTER_TLS_CERT` / `MOCENTER_TLS_KEY`，并可用
> `MOCENTER_CA_CERT` 启用 mTLS）、设置 `MOCENTER_WHITELIST`，并用防火墙限制来源网段。
> 证书生成：`bash scripts/build-linux-all.sh --help` 中的 `gen-certs`，或
> `bash scripts/gen-cert.sh ./certs`（需 openssl）、`cd scripts/gencerts && go run . all`
> （默认按本机公网 IP 写入 SAN）。
> 服务端换过 IP / 证书 SAN 不含 Agent 所用地址导致连不上时，用
> `cd scripts/signcert && go run .` 按实际地址重签（详见「TLS 证书 SAN 不匹配」）。

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

0. **口令不要写在命令行上**：`-password` 会出现在 `ps -ef` 与
   `systemctl show -p ExecStart` 里（普通用户即可读），等于把口令贴在服务单元中。
   改用 `-password-file <0600 文件>`（见「三、口令不会出现在服务单元里」）。
   服务端内置的两条安装链路（`install-linux.sh` / `install-windows.bat`）已默认走文件方式，
   图形安装程序也改为经临时文件传递。
1. **设置登录账号**：服务端启动时通过 `-user` / `-password-file` 指定后台账号，并在 Web「账号设置」里
   绑定 OTP 二次校验。凭证只有一种：用户名 + 密码(+OTP)，**没有全局 API Key**。
2. **IP 白名单**：通过 `-whitelist 1.2.3.4,10.0.0.0/8` 限制可访问服务端的源 IP。
3. **TLS/mTLS —— 默认已开启，通常无需配置**：

   服务端首次启动会自动生成**部署身份 CA**（`<data>/ca.crt` + `ca.key`，0600），并据此：

   - 自动签发 Agent 通道的服务端证书 → 该通道**开箱即 TLS**；
   - 自动签发 Web 控制台"当前部署临时证书" → 控制台**默认 HTTPS/WSS**（并附带 HSTS）。

   Agent 只需要服务端的注册密钥，其余全自动：

   ```bash
   # 服务端（不给任何证书参数）
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -web ../web/dist

   # Agent（只给注册密钥；首次连接自动换取客户端证书，之后走 mTLS）
   ./mocenter-agent -server your.server.com:9000 -agentid host-01 -agent-key <服务端 <data>/agent.key 的内容>
   ```

   浏览器访问 `https://<总控>:8080` 会提示自签名证书，**接受即可**；若要受信任证书，
   配 `-web-tls-cert/-web-tls-key`（或前面挂 TLS 反代并加 `-admin-tls-days=0` 关闭自动 HTTPS）。

   需要手工控制证书时（外部 CA / 正式证书 / 预置固定证书）：

   ```bash
   bash scripts/gen-cert.sh certs   # 生成 ca.crt / server.crt / client.crt
   # 或：cd scripts/gencerts && go run . all -ip <服务端IP> -dns <域名> -server-config ... -agent-config ...

   # 服务端
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> \
     -tls-cert certs/server.crt -tls-key certs/server.key -ca-cert certs/ca.crt
   # Agent（携带客户端证书，并用 CA 校验服务端）
   ./mocenter-agent -server host:9000 -agentid host-01 \
     -client-cert certs/client.crt -client-key certs/client.key -ca-cert certs/ca.crt
   ```

   想避开"自签证书必须匹配 SAN"的老问题（换 IP、加域名、走内网地址都会让校验失败，
   最终往往被迫改成 `-insecure` 等于关掉校验），用**公钥指纹**代替 CA 文件：

   ```bash
   ./mocenter-agent -server host:9000 -agentid host-01 -server-fingerprint <指纹>
   ```

   指纹在服务端启动日志、Web「Agent 接入」页可见（形如 `1f8d 86c9 94fb 5a97`），
   也可直接取 `<data>/ca.crt` 的公钥指纹。它绑定的是**部署身份（CA）**，与服务端地址无关，
   也不受服务端重启（每次启动会重新签发监听证书）影响：校验时按整条证书链比对，
   叶子证书或其签发 CA 命中即通过。

   | 参数 | 作用 |
   |------|------|
   | `-require-agent-cert`（服务端） | 强制 Agent 必须携带客户端证书（不再接受"先换取"）。**建议在全量 Agent 换取完成后再开启** |
   | `-allow-unauthenticated-agents`（服务端） | **危险**，迁移期临时关闭注册鉴权 |
   | `-server-name`（服务端） | 部署身份名，写入 CA，用于区分多套总控 |
   | `-server-fingerprint`（Agent） | 公钥指纹固定，免 CA 文件、不受服务端换 IP/证书影响 |
   | `-enroll-secret`（Agent） | 换取证书用的注册密钥（默认取 `agent_key`，换取成功后自动清空） |
   | `-creds-dir`（Agent） | 客户端证书落盘目录（默认 `<配置目录>/creds`） |

   **信任模型（三类角色各管一件事）**

   | 凭据 | 位置 | 作用 | 生命周期 |
   |------|------|------|----------|
   | 注册密钥 `agent.key` | 服务端 `<data>/`，Agent 配置文件 | 让 Agent 被接纳 | 一次性：换取证书后从 Agent 配置清空 |
   | CA 证书/私钥 `ca.crt`/`ca.key` | 服务端 `<data>/`（0600） | 让双方确认属于同一套总控 | 长期；**变更即需重新换取**，建议备份 |
   | 客户端证书 `client.crt`/`client.key` | Agent `<配置目录>/creds/`（0600） | 让每次连接可验证 | 自动签发（默认 3650 天），私钥仅存本机 |

   升级/迁移注意：

   - Agent 换取过证书后，配置里 `enrollment_secret` 会被清空，`agent_key` 可保留（便于
     服务端 CA 重建后重新换取）。要重新换取：删除 `creds/` 并把密钥填回
     `enroll-secret`（或 `agent_key`）。
   - 服务端重装/更换 CA 后，Agent 会自动识别并重新换取证书（见下条），**不再需要人肉登录每台被控机**。
     **服务端重装前请备份 `<data>/ca.key`。**
   - **服务端换过部署身份 CA 时的行为**（`server_fingerprint` 决定是否自动收敛）：

     | 场景 | Agent 行为 |
     |------|-----------|
     | 本机没有 CA 记录（首次接入） | 直接换取证书（信任根是注册密钥） |
     | CA 与本机记录的一致 | 正常连接，无需干预 |
     | CA 变化且**已配置** `server_fingerprint`＝新指纹 | **自动**丢弃旧证书、重新换取、恢复连接 |
     | CA 变化且**未配置** `server_fingerprint` | **拒绝**自动重换并报错，给出新旧指纹 |

     最后一行为什么默认拒绝：换取请求里带着注册密钥的 HMAC 证明。若"换个地址就自动重换"，
     等于任何能让 Agent 连上自己的对端都能拿到它的注册凭据 —— 证书锁定会被彻底架空。
     因此自动恢复必须由运维显式授权（填一次 `server_fingerprint` 即可，之后远程下发配置文件就行）。

     服务端侧的识别与诊断：应用层会**显式校验客户端证书是否由本套 CA 签发**（不是只看有没有证书），
     不认可时记录明确日志并主动下发 `EnrollRequired` 告知 Agent。这条通路很关键 ——
     Agent 通道是"请求但不强制"客户端证书，而 Go 的客户端在服务端广告的 CA 里找不到本机证书时
     会**静默不发**（握手照常成功、客户端无任何错误），因而不主动告知的话，双方日志都对不上。
   - Agent 侧的身份比对按**公钥**进行（比对服务端证书链里的 CA），不按 CA 名字：
     默认部署下两套 CA 同名（都是 `<server-name>-CA`），比名字无法区分密钥。
   - 旧版 Agent 无需改动：服务端默认允许"无证书连接先换取"，换取成功后自动收敛为 mTLS。
   - **`-require-agent-cert` 的正确用法**：它是"新 Agent 的部署前开关"。
     客户端证书只在 TLS 握手阶段生效；若一上来就强制，新装的 Agent 会在发出换取请求之前
     被拒绝（表现为 `tls: certificate required`，而服务端没有任何日志，运维很难判断原因）。
     因此：**先不带该开关完成全量 Agent 的首次换取，再开启**；
     或对确需提前管控的环境，先给每台 Agent 预置 `client.crt`/`client.key` 再开启。
     服务端日志会对被拒的连接给出两种区分：
     "该 Agent 尚未持有证书" / "对端可能出示了非本套 CA 签发的证书"。

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
> `scripts\deploy\install-windows.bat`。

## 十一、版本号管理

**Server 版本与 Agent 版本互相独立**，各自定义、各自发版，集中定义在：

- Server：`server/internal/version/version.go` → `Version`
- Agent：`agent/internal/version/version.go` → `Version`

### 为什么独立

Agent 版本表达的是「**Agent 自身功能迭代到哪一代**」，由 Agent 的发布节奏决定：

- 只改服务端时可以只发服务端，不必牵动全网 Agent 的版本号；
- 只改 Agent 时也不需要动服务端版本，避免一次「没有任何实际改动」的服务端发布；
- 运维可以灰度「先把一部分 Agent 升到 x.y.z」，此时服务端版本本来就不该变。

因此**服务端不做任何版本一致性校验**：Agent 版本高于或低于服务端都属于正常状态。

### 发布纪律

- **只有在明确发版时才修改版本号**。两边都**不提供**运行时改版本的命令行参数或
  环境变量 —— 避免线上跑着一个"版本号被临时改过"的二进制，导致升级判定错乱。
- 服务端发版时同步 `installer/setup.iss` 的 `#define AppVersion`（决定安装包文件名与注册表版本）。
- `scripts/build-linux-all.sh` / `scripts/build-all.bat` 的**服务端 / Agent 版本**自动从
  `server/internal/version`、`agent/internal/version` 读取（各自唯一的事实来源），
  读不到才用兜底值；可用 `--version=` / `--agent-version=`（或环境变量
  `MOCENTER_SERVER_VERSION` / `MOCENTER_AGENT_VERSION`，旧名 `MOCENTER_VERSION` 仍兼容）覆盖。

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

远程升级时，升级包声明的 `version` 表示「本次要升到哪个版本」，**与当前服务端版本无关**：
可以高于、等于或低于服务端版本，接口不会因此拒绝。
不填时自动取服务端版本，只是为"服务端与 Agent 同步发版"这一常见场景顺手给个默认值。

Agent 侧的判定逻辑很简单：目标版本 == 自身版本则跳过（除非勾选强制），否则执行升级。
因此这个字段只需要在**同一台机器的历史序列**里有意义，不需要跨端对齐。

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
| Agent 报 `x509: certificate is valid for 127.0.0.1, ::1, not <IP>` | 服务端证书 SAN 不含 Agent 连接用的地址。见下方「TLS 证书 SAN 不匹配」 |
| Agent 报 `remote error: tls: certificate required` | 服务端启用了 mTLS（`-ca-cert`）但 Agent 未带客户端证书；用 `-agent-key` 走自动换取，或补 `-client-cert`/`-client-key` |
| 换了新证书仍报旧 SAN | 证书只在服务端**启动时**加载，替换文件后必须重启服务端进程 |

### TLS 证书 SAN 不匹配

Agent 报：

```
[agent] 连接断开: TLS 连接失败且本机已持有客户端证书，拒绝降级为明文：
        tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, ::1, not 82.156.7.85
```

说明服务端证书的 SAN 里没有 Agent 正在连接的那个地址。**只在手工铺证书时才会遇到**
（默认的自动换取证书走"证书链"校验，不看地址），修复步骤：

```bash
# 1) 在服务端所在机器上按「Agent 实际使用的地址」重签（默认自动探测公网 IP）
cd scripts/signcert
go run . -server-config ../../mocenter-server.json   # 自动按公网 IP + 本机地址写入 SAN
# 也可以显式指定，IP 与域名一次都写上最稳
go run . -ip 82.156.7.85 -dns mocenter.example.com -server-config ../../mocenter-server.json

# 2) 重启服务端（证书只在启动时加载）
systemctl restart mocenter-server
```

也可用 `scripts/gencerts`（`cd scripts/gencerts && go run . server`，同样默认探测公网 IP）。

要点与替代方案：

- 同一 CA 重签时 Agent 侧 `client.crt`/`ca.crt` **无需更换**，只换服务端 `server.crt`/`server.key`。
- 服务端启动时会把证书 SAN 与「本机实际地址」比对，不一致会打印告警并给出重签命令；
  手工证书的 SAN 也会直接打印在日志里，便于核对。
- **更省事的做法**：改用 `-agent-key` 自动换取（默认路径，不看 SAN），
  或给 Agent 配 `-server-fingerprint <部署身份指纹>`（按指纹校验，服务端换 IP/换证书都不受影响）。
- Agent 侧日志在识别到这类错误时会直接附上上述排查指引。

## 十三、远程升级 Agent

服务端发布新版本 → Agent 检测到新版本 → 下载升级包 → 校验签名 → 停止旧进程 → 替换二进制 → 重启新进程 → 上报版本信息。支持差分升级、失败自动回滚、灰度升级（规划书 12.4）。
