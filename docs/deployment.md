# MoCenter 部署与运维手册

本文档说明 MoCenter 总控服务端、Agent、Web 前端的部署步骤，以及证书、备份恢复、常见故障处理。

## 一、环境要求

| 组件 | 环境 |
|------|------|
| 服务端 | Linux x86_64，2 核 4G（压测 4 核 8G），公网 IP，开放 TCP 端口（默认 9000）与 HTTP 端口（默认 8080） |
| Agent | Linux（CentOS/RHEL 7/8、Ubuntu 18.04~22.04、Debian 10/11）、Windows 10 1809+ / Server 2019+，x86_64 为主 |
| Web | 浏览器 Chrome/Edge/Firefox 最新两个大版本 |

## 二、部署方式

> 仓库里保留着 `server/Dockerfile`，但**容器不是本项目的交付形态**：不发镜像、不做兼容性维护，
> 因此下面不写 Docker 步骤。真要自己起容器请自行核对参数（尤其**别把口令放进 `-e`/命令行**，
> 它会出现在 `ps -ef` 与 shell 历史里；服务端支持 `-password-file`）。
> 支持的部署方式只有下面两种：单进程物理机直跑，或一键脚本装成 systemd 服务。

### 方式一：物理机直接部署（单进程托管 Web）

```bash
# 1. 构建前端并交由服务端托管
cd web && bash build.sh   # 生成 dist/
cd ../server && bash build.sh
# 2. 启动（同时托管 Web 静态资源，单进程部署）
#    口令走文件而不是 -password：后者会出现在 ps -ef 与 shell 历史里
install -d -m 700 /etc/mocenter
printf '%s' '强口令' > /etc/mocenter/admin-password && chmod 600 /etc/mocenter/admin-password
./mocenter-server -listen :9000 -http :8080 -user admin -password-file /etc/mocenter/admin-password -web ../web/dist
```

### 方式二：Linux 一键构建 + 安装包（推荐）

构建与打包统一由 **`scripts/build-linux-all.sh`** 完成（`scripts/packaging/` 下的零碎脚本已合并进它），
产物只按用途分类一次落到 `build/`（平台体现在文件名与 `bin/<角色>/<平台>/` 这一层）：

```bash
# 默认：全部类型 + 全部平台 + 全部架构 + 全部安装包
bash scripts/build-linux-all.sh

# 只出服务端多架构二进制（amd64/arm64/arm/386 -> build/bin/server/linux/）
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
| 服务端二进制 | `build/bin/server/linux/mocenter-server-<arch>` |
| Agent 二进制 | `build/bin/agent/<linux\|windows>/mocenter-agent-<arch>[.exe]` |
| Web 前端 | `build/web/` |
| `.deb` / `.rpm` | `build/packages/{deb,rpm}/` |
| Windows zip | `build/packages/zip/` |
| Windows 图形安装程序 | `build/setup/`（需在 Windows 上编译） |

安装包注册服务后的落盘位置：

| 项目 | 位置 |
|------|------|
| 二进制 | `/usr/local/bin/mocenter-server` |
| 启动参数 | `/etc/mocenter/mocenter-server.conf`（**请修改口令与白名单**） |
| 全量配置 | `/etc/mocenter/mocenter-server.json`（首次启动自动生成） |
| 数据目录 | `/var/lib/mocenter/data`（审计日志、`users.json`、`agent.key`、`ca.key`、`node.key`、`ssh_known_hosts`） |
| 托管安装包 | `/var/lib/mocenter/packages` |
| Web 前端 | `/usr/share/mocenter/web` |
| 装卸说明 | `/etc/mocenter/README.server` |
| 日志 | `journalctl -u mocenter-server -f` |

不打安装包、直接用脚本注册（需 root）：

```bash
bash scripts/build-linux-all.sh --type=server --package=none
sudo bash scripts/deploy/install-linux.sh --role=server \
     --bin=build/bin/server/linux/mocenter-server-amd64 \
     --user=admin --password-file=/root/admin-pw.txt   # 口令走文件；也可 printf '%s' "$pw" | ... --password
sudo bash scripts/deploy/install-linux.sh --role=all status
sudo bash scripts/deploy/install-linux.sh --role=server uninstall      # 加 --purge 连配置数据一起删
```

**升级**（配置已存在即按升级处理，只换二进制与服务，不动配置）：

```bash
sudo bash scripts/deploy/install-linux.sh upgrade --role=server \
     --bin=build/bin/server/linux/mocenter-server-amd64
# 升级时无需重复提供 --user/--password-file；要清理重装（删除既有配置）加 --force
```

**升级顺序与前置条件**（一次说清，避免踩坑）：

1. **服务端**：替换二进制 + 重启即可（上面那条命令），配置/账号/数据保留。
   注意**证书只在启动时加载** —— 换过证书必须重启进程才生效。
2. **Agent** 两条路：
   - **远程升级**（推荐，不用登录被控机）：`POST /api/upgrade` 或 Web「运维操作 → 远程升级」。
     要求包为 https 且同时通过 SHA-256 与 Ed25519 签名。
     **前提是 Agent 已配 `-upgrade-pubkey`** —— 没配则一律拒绝，这是默认安全态而非故障。
   - **重新部署**：Web「部署 Agent」（SSH 一键）或
     `install-linux.sh upgrade --role=agent --bin=...`。
     目标机已有 `/etc/mocenter/mocenter-agent.json` 时**只替换二进制并重启服务，配置原样保留**，
     因此可以直接当升级用（本机定制与设备身份不会被冲掉）。
3. **建议先升 Agent 再升服务端**：新服务端若下发了旧 Agent 不认识的消息，旧 Agent 会
   **静默忽略**（不会崩），只是对应功能会超时；反过来（新 Agent + 旧服务端）没有这个问题。
4. **给存量 Agent 补 `-upgrade-pubkey`**：重新部署**不会**覆盖已有配置，所以补不进去 ——
   需手工编辑目标机的配置，或先删除该文件再部署一次（后者会按总控侧的值重写配置，本机定制会丢）。
   也不能靠「文件分发」补：`-file-roots` 默认只允许写 `<配置目录>/files`，改不到配置文件本身。

**保活自启**（脚本按环境自动选择）：

| 环境 | 注册内容 |
|------|----------|
| 有 systemd | `mocenter-<角色>.service`（开机自启 + `Restart=always`）+ `mocenter-<角色>-watchdog.timer`（每 5 分钟巡检，进程不在就拉起） |
| 无 systemd（容器/精简系统） | `/etc/cron.d/mocenter-<角色>`：`@reboot` + 每 5 分钟巡检，共用 `/usr/local/bin/mocenter-keepalive` |
| 两者都没有 | 告警，并提示用容器编排的 `--restart=always` / `restartPolicy: Always` |

> 巡检脚本（`scripts/deploy/mocenter-keepalive.sh`）幂等：进程已在运行就直接退出；
> 极端竞态下重复拉起的一方会被程序自身的单实例保护挡下，不会出现两个实例同时服务。
> 用 `--no-keepalive` 可跳过保活任务注册，`--dry-run` 只打印将要执行的动作。

> 安装脚本注册的 systemd 单元与安装包内的是**同一份模板**（`scripts/deploy/mocenter-*.service`），
> 不会再出现"打包脚本改了、安装脚本忘改"。
> 服务只监听 `:9000`（Agent 长连接）与 `:8080`（Web 控制台 / REST API）。
> 生产环境请配置 TLS 证书（`MOCENTER_TLS_CERT` / `MOCENTER_TLS_KEY`，并可用
> `MOCENTER_CA_CERT` 启用 mTLS）、设置 `MOCENTER_WHITELIST`，并用防火墙限制来源网段。
>
> **装完必须自查两件事，否则症状不会出现在服务端身上**（`install-linux.sh` 装服务端角色时
> 会自动检查并打印整改命令，加 `--open-firewall` 可让它直接放行）：
>
> 1. **防火墙要放行这两个端口**。firewalld 默认的 `public` 区只开 `ssh/cockpit/dhcpv6-client`，
>    新机器装完服务端，Agent 侧的表现只是"连接超时"，而服务端日志一切正常，在本机查不出名堂。
>    `firewall-cmd --list-ports` 应当看到 `9000/tcp 8080/tcp`（缺哪个补哪个）：
>    `firewall-cmd --add-port=9000/tcp --add-port=8080/tcp`，要持久化再加 `--permanent` 并 `--reload`。
> 2. **两端时钟要同步**。注册报文带时间戳且只认 ±60 秒（防重放），服务端自己慢几分钟
>    就会导致所有 Agent 换不到客户端证书，拒绝理由写在审计的 `auth` 类里：
>    「注册报文时间戳偏差过大（…，请检查两端时钟）」。`timedatectl` 里
>    `NTPSynchronized=no` 就先 `timedatectl set-ntp true`（离线环境指向内网 NTP 源）。
>    虚拟机从挂起恢复后常出现这种偏移，测试环境尤其容易撞上。
> 证书生成：`bash scripts/build-linux-all.sh --help` 中的 `gen-certs`，或
> `bash scripts/gen-cert.sh ./certs`（需 openssl）、`cd scripts/gencerts && go run . all`
> （默认按本机公网 IP 写入 SAN）。
> 服务端换过 IP / 证书 SAN 不含 Agent 所用地址导致连不上时，用
> `cd scripts/signcert && go run .` 按实际地址重签（详见「TLS 证书 SAN 不匹配」）。

## 四、Web 前端部署

```bash
cd web && npm run build   # 生成 dist/
bash scripts/deploy/precompress-web.sh web/dist   # 预压缩 .br/.gz（推荐）
# 将 dist 静态文件部署到 Nginx，或由 Go 后端一并托管
```

### 加载性能（移动端浏览器尤其重要）

服务端托管前端时（`-web <dist>`）已内置三项优化：

1. **预压缩优先**：存在 `xxx.js.br` / `xxx.js.gz` 时按 `Accept-Encoding` 直接回传，
   不再逐次压缩。前端主包 gzip 后约 280KB，弱网移动端差别是数倍。
2. **缓存头**：`/assets/*`（带内容哈希）返回 `immutable` 长缓存；`index.html` 与任意路由
   回退均为 `no-cache`，发版即时生效。
3. **前端侧分包**：`web/vite.config.js` 做依赖分包，终端（xterm）、远程桌面（noVNC）等
   重依赖页面改为路由级懒加载 —— 首屏只下载 react/router 等基础块。

> 用 Nginx 托管时请自行打开 `gzip`/`brotli` 与静态缓存；`try_files $uri /index.html;`
> 的 SPA 回退保持不变即可。预压缩产物（`.gz`/`.br`）已在 `.gitignore` 中，属可再生成物。

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
        # Host 必须透传：三条 WebSocket 端点都按 Origin 与服务端看到的 Host 判同源，
        # 不透传时 Host 会是 127.0.0.1:8080，浏览器握手一律被判跨站（403）。
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
    # 远程桌面隧道（同样是 WebSocket 升级）
    location /vnc-ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
    # 公开状态页的实时推送（免登录）；同样需要 Host 与 WebSocket 升级头
    location /api/public/ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
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
| `site_title` | **网站标题**（浏览器标签页 `<title>`）。全站生效：公开页、节点详情、登录页与后台均读取它；留空回退默认 `MoCenter · 集中式主机远控管理系统` |
| `site_footer` | **页脚自定义内容**，支持 HTML（版权、备案号、备案链接等）。可用 `{year}` 占位符表示当前年份，留空回退默认 `{year}©MOONCN`（渲染为 `2026©MOONCN` 这样的当前年份）。展示位置：公开页、节点详情页与登录页底部 |

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

### 站点标题与页脚

「展示配置 → 站点标题与页脚」维护两处**站点外观**文案，与公开页总开关**无关**
（后台、登录页同样生效，因此不必为了改标题而开启公开页）：

- **网站标题**：浏览器标签页标题。公开页与后台共用一份来源，避免"登录前一个标题、
  登录后又变回去"；
- **页脚内容**：支持 HTML，用于写版权、备案号与备案链接。`{year}` 会替换为当前年份，
  默认值 `{year}©MOONCN` 因此不需要每年手工改。

安全处理：页脚是**全站唯一**一处把服务端下发的字符串交给 `dangerouslySetInnerHTML`
的地方，而它的读者包括未登录访客（公开页）与其他运维的会话（后台），所以
"能写配置的人"可信、"写进来的那段内容"不可信。服务端按原样存储（保留备案链接这类
正常用法），**净化在渲染入口做**：`web/src/utils/siteBranding.js` 的 `sanitizeFooterHTML`
是**白名单重建** —— 只有展示型标签（`a/b/strong/i/em/p/br/img/ul/li/…`）与
`href/src/target/title/alt` 这几个属性会被重新拼出，其余标签连内容一起丢，
事件属性与 `style` 根本没有出现的通道；链接协议限定 `http/https/mailto/tel/ftp` 与相对路径，
图片只认 `http(s)` 与位图 `data:`（**相对路径的图片会被丢掉**，需要引图的请用绝对地址）。
判定协议前会先解码实体并去掉控制符，`&#106;avascript:`、`java\tscript:` 这类写法穿不过去。
兜底是整站 `Content-Security-Policy`（`script-src 'self'`，无 `unsafe-inline`），
即使净化被绕过也执行不了脚本。白名单的每一条都有回归用例：`npm run check:footer`。
品牌文案通过免鉴权的 `GET /api/branding` 下发（登录页在登录前就要用正确标题），
该端点**只**返回标题与页脚，不返回 `host_ids`/`fields` 等公开策略信息。
"公开页是否启用"是另一个端点 `GET /api/public/enabled` 的事，它跟着开关走：
关掉时直接 404，不留一个可被扫描器反复确认的探针点。

安全设计（部署前请知悉）：

1. **主机标识不公开**：对外只有服务端派生的节点编号 `n-` + 16 位十六进制，
   它用**本机部署私钥** `<data>/node.key`（首次启动随机生成，0600）做 HMAC-SHA256 后再截断，
   外部拿候选主机名离线穷举也命中不了；`agent_id`、硬件 UUID、内网地址在公开响应结构里根本不存在。
   - 换编号的方式：删除 `<data>/node.key` 并重启，服务端会重新生成一把，所有公开编号整体变更。
     历史数据不受影响（库内按 `agent_id` 存），但已分享出去的 `/public/node/…` 链接会全部失效。
   - 该密钥**刻意不进入自动备份**：备份一旦外发，攻击者就同时拿到"主机名候选 + 派生规则"，
     公开编号的不可枚举性随之失效。需要跨机迁移时请单独手工保管。
2. **敏感项默认关闭**：主机名、系统版本、内核版本、公网 IP 默认不可见，可显式开启；
   公网 IP 即使开启也会做部分掩码（`203.0.*.*`），精确经纬度永不公开。
3. **独立限流**：公开端点为匿名入口，另有独立于登录的限流 —— 按「来源 IP × 路由」
   计数，每条路由 30 次/分钟（详情页 15s 轮询 × 三个标签页 ≈ 12 次/分钟，留一倍余量），
   被拒时返回 `429 + Retry-After`。**它与登录额度完全分开**：访客刷公开页不会把
   管理员挡在自己的后台外面。**注意**：计数只看 TCP 对端地址（不信任
   `X-Forwarded-For`），因此反向代理之后全站访客共用一个源 IP、共享这 30 次/分钟；
   对外访问量级大时请把公开页的轮询频率调低，或为 `/api/public/*` 单独加一层缓存。
   单次历史趋势最多返回 300 点、回溯窗口上限 30 天。
4. **匿名请求不出网**：公开页读的是地理位置**缓存**（`AllCached`），不会替访客去
   请求外部 GeoIP 接口 —— 额度只由后台仪表盘的推送路径消耗，访客再多也打不空它。
   未命中缓存的主机在公开页上就是没有地区，直到后台那次正常推送把它暖起来。
5. **开关是"真关"**：`Enabled:false` 时 `/api/public/*`（含 `/api/public/enabled`）
   一律 404，对外**不存在**；已经连上的匿名 WebSocket 会在配置落库的瞬间自己结束，
   不需要等访客关掉标签页。品牌文案（标题/页脚）走常开的 `GET /api/branding`，
   因为它服务的是登录页，与公开页开关无关。
6. **审计留痕**：每次修改展示配置都会写入操作审计（分类 `op`）。

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

   Agent 只需要一份注册凭据，其余全自动：

   ```bash
   # 服务端（不给任何证书参数）
   ./mocenter-server -listen :9000 -http :8080 -user admin -password <密码> -web ../web/dist

   # Agent（只给注册凭据；首次连接自动换取客户端证书，之后走 mTLS）
   # 推荐用 Web「Agent 接入」页签发的**一次性注册令牌**（一台一份、用后即废），见下节
   ./mocenter-agent -server mc.example.com:9000 -agentid host-01 -agent-key <一次性注册令牌>
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
   | `-allow-unauthenticated-agents`（服务端） | **危险**，迁移期临时关闭注册鉴权。同时允许"无凭据注册"和"无凭据签发证书" |
   | `-disable-shared-agent-key`（服务端） | 关闭部署级共享注册密钥，**只接受一次性注册令牌**。现网全部迁移到令牌后再开 |
   | `-server-name`（服务端） | 部署身份名，写入 CA，用于区分多套总控 |
   | `-server-fingerprint`（Agent） | 公钥指纹固定，免 CA 文件、不受服务端换 IP/证书影响 |
   | `-enroll-secret`（Agent） | 换取证书用的注册凭据（默认取 `agent_key`，换取成功后自动清空） |
   | `-creds-dir`（Agent） | 客户端证书落盘目录（默认 `<配置目录>/creds`） |
   | `-upgrade-pubkey`（服务端 / Agent） | 升级包验签公钥（Ed25519 hex）。服务端配了就会写进**新部署**的 Agent 配置（已有配置不会被覆盖，见「一键部署」的升级语义）；**Agent 没配它就不接受任何升级指令**，没有跳过验签的开关 |
   | `-file-roots`（Agent） | 文件分发/取回允许的根目录（逗号分隔），默认 `<配置目录>/files`；只认本机配置与命令行，服务端指令改不了 |

   **注册凭据：优先用一次性令牌**

   Web「Agent 接入」页（或 `POST /api/enroll/tokens`）可为指定 `agent_id` 签发**一次性注册令牌**：
   绑定主机名、默认 24 小时过期、用后即废，泄露只影响那一台那一次。相关的存储与行为：

   - `<data>/enroll.json`（0600）：待用令牌 + 每台主机的 `agent_id → 客户端公钥指纹(SPKI)` 绑定。
     令牌以明文存储是必要的 —— 注册凭证要拿去做 HMAC，服务端必须能还原原文；
     安全收益来自"按台绑定 + 短有效期 + 单次消费"，而不是存储形态。
   - **TOFU 绑定**：首次注册记下公钥指纹；同一 `agent_id` 换不同密钥的证书再来注册会被拒，
     除非带上一份新签发的**一次性令牌**（`enroll.json` 记录 `rebinds` 次数，控制台可清除绑定）。
   - **在线 `agent_id` 拒发**：目标主机已有活动会话时不签发证书，避免"趁在线冒名顶替"。
     凭据校验在这一步**之前**完成（否则这里就成了"某主机是否在线"的探测口），
     所以被拒时那枚令牌也已经消耗掉，需要重新签发。
   - 共享注册密钥（`<data>/agent.key`）仍可用**一个过渡版本**：它是一把钥匙认全网，
     泄露任一台即可冒名注册任意主机，且**必须轮换 `<data>/ca.key` 并让所有 Agent 重新换取证书**才能彻底清除。
     迁移期内每次使用它注册都会在服务端日志里告警；迁完后用 `-disable-shared-agent-key` 关掉。
   - **`agent.key` 的明文不出现在控制台**：「Agent 接入」页只给指纹 + 文件路径。要拿明文只能
     「轮换共享密钥」（`POST /api/enroll/shared-key/rotate`，body `{"confirm":true}`），
     它写入新密钥（0600）、进操作审计（只记新旧指纹）、**新明文只回显这一次**，旧密钥对新注册立刻失效。
     已凭证书在线的主机不受影响。注意：若密钥来自 `-agent-key`/配置文件 `agent_key`/`MOCENTER_AGENT_KEY`，
     下次启动那个值会盖掉 `agent.key`，轮换响应会告警 —— 要长期生效须先删掉那个配置项。
   - Agent 换取到证书后，后续连接以证书身份（CA 校验 + CN＝自报 `agent_id`）为准，
     **不再需要任何注册凭据**，重启/重连都不会要求重发令牌。

   **信任模型（三类角色各管一件事）**

   | 凭据 | 位置 | 作用 | 生命周期 |
   |------|------|------|----------|
   | 注册凭据（一次性令牌 / `agent.key`） | 服务端 `<data>/enroll.json`、`agent.key`，Agent 命令行或配置 | 让 Agent 被接纳 | 一次性：换取证书后从 Agent 配置清空；令牌用后即废 |
   | CA 证书/私钥 `ca.crt`/`ca.key` | 服务端 `<data>/`（0600） | 让双方确认属于同一套总控 | 长期；**变更即需重新换取**，建议备份 |
   | 客户端证书 `client.crt`/`client.key` | Agent `<配置目录>/creds/`（0600） | 让每次连接可验证 | 自动签发（默认 3650 天），私钥仅存本机 |

   升级/迁移注意：

   - Agent 换取过证书后，配置里 `enrollment_secret` 会被清空，`agent_key` 可保留（便于
     服务端 CA 重建后重新换取）。要重新换取：删除 `creds/client.crt` 与 `creds/ca.crt`
     （保留 `client.key`，见下条 TOFU 说明）并把凭据填回 `enroll-secret`（或 `agent_key`）。
   - 服务端重装/更换 CA 后，Agent 会自动识别并重新换取证书（见下条），**不再需要人肉登录每台被控机**。
     **服务端重装前请备份 `<data>/ca.key`。**
   - **服务端换过部署身份 CA 时的行为**（`server_fingerprint` 决定是否自动收敛）：

     | 场景 | Agent 行为 |
     |------|-----------|
     | 本机没有 CA 记录（首次接入） | 直接换取证书（信任根是注册凭据） |
     | CA 与本机记录的一致 | 正常连接，无需干预 |
     | CA 变化且**已配置** `server_fingerprint`＝新指纹 | **自动**丢弃旧证书、重新换取、恢复连接 |
     | CA 变化且**未配置** `server_fingerprint` | **拒绝**自动重换并报错，给出新旧指纹 |

     最后一行为什么默认拒绝：换取请求里带着注册凭据的 HMAC 证明。若"换个地址就自动重换"，
     等于任何能让 Agent 连上自己的对端都能拿到它的注册凭据 —— 证书锁定会被彻底架空。
     因此自动恢复必须由运维显式授权（填一次 `server_fingerprint` 即可，之后远程下发配置文件就行）。

     TOFU 绑定为什么不会挡住这条自动恢复：Agent 重新换取时**复用本机私钥**（`creds/client.key`），
     公钥指纹不变，服务端按同一台机器放行。因此手工清理时只删 `client.crt` 与 `ca.crt`，
     **保留 `client.key`**；把整个 `creds/` 删掉等于自造一次"换机器身份"，
     此时必须为该 `agent_id` 重新签发一次性注册令牌（或在控制台「清除绑定」）才能重签。

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

5. **响应安全头与 WebSocket 同源（挂反向代理前必读）**：

   服务端对每个响应下发下列头（`server/main.go` 的 `securityHeaders`）：

   | 头 | 取值 | 为什么 |
   |----|------|--------|
   | `Content-Security-Policy` | `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self' ws: wss:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'` | XSS 的最后一道：注入进来的 HTML 拿不到执行脚本的能力 |
   | `X-Frame-Options: DENY` + `frame-ancestors 'none'` | | 后台与公开页都不允许被嵌入，防点击劫持 |
   | `X-Content-Type-Options: nosniff` | | 不把上传的文件当脚本嗅探执行 |
   | `Referrer-Policy: no-referrer` | | Token 不会经 `Referer` 漏给第三方 |
   | `Cache-Control: no-store` | 仅 `/api/*` | 登录响应带 Token、审计响应带命令结果、备份响应带文件，中间代理存一份就多一处泄露点；`/assets/*` 仍是内容哈希 + 长缓存 |
   | `Strict-Transport-Security` | 仅 TLS 请求 | 明文态下发会把同主机的明文站点一起锁死 |

   CSP 的 `script-src 'self'` **刻意不含** `'unsafe-inline'` / `'unsafe-eval'`：构建产物全是外链
   脚本，且产物里没有 `eval`/`new Function`/`new Worker`（`npm run build` 后 grep 三平台产物确认）。
   `style-src` 留 `'unsafe-inline'` 是因为 `web/index.html` 的首屏内联底色是有意为之
   （样式表被企业代理拦掉时不会白屏）；`img-src` 留 `data:` 是因为国旗是运行时画到 canvas
   再取 `toDataURL` 的。`connect-src` 写死 `ws: wss:` 而不只靠 `'self'`：`'self'` 对
   `ws://` 同源是否算匹配各家浏览器口径不一，写死才不会把终端与远程桌面拍死。

   三条 WebSocket 端点（`/ws`、`/vnc-ws`、`/api/public/ws`）现在都：

   - **校验 `Origin` 同源**（`server/internal/ws/origin.go`）：浏览器发起的跨站 WebSocket
     握手不受 CORS 约束，唯一能挡的地方是服务端认来源。**不带 `Origin` 的请求放行**
     （那只能是 curl/探活脚本等非浏览器客户端，而跨站劫持必然带 Origin）。端口只在
     两侧都显式写出时才参与比对，协议不参与比对，所以 TLS 终止型代理**不需要**透传
     `X-Forwarded-Proto`；
   - **只从 `Sec-WebSocket-Protocol` 子协议头取 Token**：`?token=` 回退已删除，因为 URL 会进
     反向代理访问日志、浏览器历史与截图。

   由此对反向代理只有一条硬要求：**透传原始 `Host`**（nginx `proxy_set_header Host $host;`，
   见「四、Web 前端部署」的示例）。不透传时服务端看到的 `Host` 是 `127.0.0.1:8080`，
   浏览器写的 `Origin` 是对外域名，握手会被判跨站返回 403，表现为"终端 / 远程桌面 /
   公开页推送全断"。部署后可用一条请求自查：

   ```bash
   # 期望 101；若为 403 就是 Host 没透传，若为 401 是 Token 没走子协议头
   curl -s -o /dev/null -w '%{http_code}\n' \
     -H "Origin: https://mocenter.example.com" -H "Host: mocenter.example.com" \
     -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
     -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
     -H "Sec-WebSocket-Protocol: mocenter,<会话Token>" https://mocenter.example.com/ws
   ```

## 六、高级运维能力

### 1. 交互式 PTY 终端
Web「远程终端」页面选择在线主机并执行交互式命令（如 `bash`、`top`、`vim`），支持实时双向 IO、终端尺寸自适应、Ctrl-C 中断。

### 2. 批量命令
Web「运维操作」页面勾选多台在线主机，批量下发同一命令；服务端逐一审计留痕。REST 接口：`POST /api/command/batch`。

### 3. 远程升级 Agent

升级包**必须有 Ed25519 签名**，Agent 侧没有"跳过验签"的开关：配了公钥才可能升级，
没配公钥则一律拒绝（这是默认安全态，不是故障）。

一次升级的完整链路：

```bash
# ① 发布机上生成一次密钥对（私钥 0600，只留在发布机，永远不分发）
cd scripts/signpkg && go run . -gen -key-out /root/mocenter-upgrade.key -pub-out /root/mocenter-upgrade.pub
#   -> 打印公钥 hex，填进服务端/Agent 的 -upgrade-pubkey

# ② 每次发版：对"目标版本号 + 升级包内容"签名，并拿到摘要
go run . -sign -key /root/mocenter-upgrade.key -version 1.1.0 <升级包路径>
#   -> 打印 sha256 / signature，以及一段可直接粘进 POST /api/upgrade 的 JSON
#   发布前自检：go run . -verify -pubkey /root/mocenter-upgrade.pub \
#                    -version 1.1.0 -sha256 <摘要> -signature <签名> <同一个包>

# ③ 下发：Web「运维操作 → 远程升级」填 目标版本 / https 下载地址 / SHA-256 / 签名
#    或直接调接口
curl -X POST https://<总控>:8080/api/upgrade -H "Authorization: Bearer <token>" -d '{
  "agent_id":"host-a","version":"1.1.0","url":"https://dl.example.com/mocenter-agent-1.1.0",
  "sha256":"<64位hex>","signature":"<256位hex>","force":false,"allow_downgrade":false}'
```

Agent 侧的执行顺序（`agent/internal/upgrade`）：先确认本机配了公钥 → 判定该不该装
（同版本需 `force`，低于当前版本属降级需 `allow_downgrade`）→ 只允许 **https** 取包 →
**一次读入内存**（上限 256 MiB）→ 用同一份字节同时做 SHA-256 比对与 Ed25519 验签
（签名消息 = `"MoCenter-Upgrade-v1\n" + 目标版本 + "\n" + 包内容`）→ 落盘为 0600 临时文件
并**复核盘上字节**→ 备份旧二进制 → 换入新二进制 → 重新拉起 → 失败回滚。
"读入即校验、校验即落盘"是为了不给"两次读文件之间被动过"留窗口；签名把版本号钉进去，
则保证同一份包不能被冒充成别的版本。

服务端配置公钥（同时会写进「一键部署」生成的 Agent 配置）：

```bash
./mocenter-server ... -upgrade-pubkey <64字节公钥的hex>
```

Agent 单独配置时是 `-upgrade-pubkey`（或环境变量 `MOCENTER_UPGRADE_PUBKEY`）。
REST 接口：`POST /api/upgrade`。

### 4. 端口映射
Web「端口映射」页面创建规则（公网端口 → 内网服务端口），服务端反向代理实时转发到目标 Agent，支持源 IP 白名单、连接跟踪与流量统计。REST 接口：`GET/POST /api/portmap/rules`、`POST /api/portmap/op`。

### 5. 文件传输
支持 Agent 文件上传/下载，分片传输 + MD5 校验 + ACK/NACK + 断点续传，接收目录默认为服务端 `/tmp/mocenter_uploads`。

### 6. 文件分发到主机
Web「运维操作」页面选择目标在线主机与 Agent 端目标路径，上传本地文件分片经服务端中转、MD5 校验后写入 Agent 指定路径。适用于批量下发部署包、配置文件、脚本等场景。

**路径被限制在文件根目录内**（`agent/internal/client/fileroots.go`）：Agent 只允许读写
`-file-roots`（或配置 `file_roots`、环境变量 `MOCENTER_FILE_ROOTS`）列出的目录及其子目录，
默认是**配置目录下的 `files/`**（systemd 部署即 `/etc/mocenter/files`）。
这条闸门只认本机配置与命令行参数，服务端指令改不了它 —— 否则"总控被拿下"就等于
"所有被控机的任意路径可写"。越界的路径（含相对路径、`..`、指向根外的软链）会被拒绝，
在「操作审计」与分发提示里能看到 `rejected`。要放开某个目录：

```bash
./mocenter-agent ... -file-roots=/etc/mocenter/files,/srv/deploy
```

### 7. VNC 安装包由服务端分发

Agent 一键安装的 Windows 包最终以 **SYSTEM 静默执行**（UltraVNC 安装器）或**内核驱动**
（无头虚拟机需要的 VDD 虚拟显示驱动）方式落地，因此**服务端托管是默认且唯一免配置的取包方式**：

```bash
bash scripts/fetch-packages.sh ./packages                 # 下载并逐个核对内置摘要
./mocenter-server ... -data ./data -pkg-dir ./packages    # 指向该目录（不指定则默认 <data>/packages）
```

启动日志会打印已托管的包清单（`[pkg] 安装包目录 ... 已托管: ...`）。
Agent 通过它与服务端之间**既有的长连接**拉取（该通道已 mutual-TLS），
落盘后先比对 Agent 内置的 SHA-256 钉扎摘要，**执行/解压前**校验不过就拒装并把实际摘要写进错误消息。

服务端未托管某个包时**不会**悄悄改用公网 —— 安装失败并回传原因。确需公网取包
（winget / choco / 厂商直连 / 加速代理）时，在**目标机本机**的 Agent 配置里设
`allow_public_package_download: true`（或启动参数 `-allow-public-package-download`）；
该开关服务端无法下发，因为它一开就把「装哪个版本的什么内容」交给了第三方源。

托管文件名须与 Agent 期望的一致（`scripts/fetch-packages.sh` 会按此命名下载并校验）：

| 文件名 | 平台 | 内置摘要 |
|--------|------|----------|
| `UltraVNC_1824_x64_Setup.exe` / `UltraVNC_1824_x86_Setup.exe` | Windows | 有 |
| `VirtualDisplayDriver-x86.Driver.Only.zip` | Windows（仅无头虚拟机需要） | 有 |
| `x11vnc-linux-amd64` / `-arm64` / `-arm` / `-386` | Linux（需自备） | 无 |

> **Linux 说明**：x11vnc 官方只发布源码、无任何预编译产物，因此没有可内置的摘要，脚本也不代为下载。
> Linux 侧优先由 Agent 用发行版包管理器安装（RHEL/Rocky/Alma 走 EPEL，Debian/Ubuntu 直接有）；
> 仅当目标机无法访问软件源时，才需自行构建/自备二进制并按上表命名放入托管目录 —— 这条包的来源由你保证，
> Agent 只校验 `ELF` 魔数。

若给安装包下载配了加速代理，**只有 `https://` 前缀会被使用**（明文前缀忽略并留日志）。
服务端会校验包名以防目录穿越；Agent 另有魔数校验（`MZ`/`ELF`）挡掉"下回一个 HTML 错误页"；
安装包拉取属于内部行为，不会出现在「文件分发」列表中。

### 8. VNC 隧道端口闸门与口令交付

远程桌面隧道连的是**被管主机的回环地址**：如果不限制端口，每个登录会话就等价于拿到一台
"打到该机任意端口"的内网代理（Redis、SSH、调试端口都会顺带可连）。因此服务端默认只放行
RFB 约定区间：

| 参数 | 默认 | 作用 |
|------|------|------|
| `-vnc-ports`（服务端）/ 配置 `vnc_ports` | `5900-5999` | 隧道与探测可指向的目标端口，逗号分隔并支持区间（如 `5900-5999,6900`） |

- 跑在非常规端口的部署**用该参数显式放行需要的那几个**，而不是整体放开：传空串表示不限制，
  且这个"不限制"状态**不会写进配置文件**（必须每次启动显式给出），避免一次误操作被永久固化。
- 清单解析失败会在启动阶段直接退出，不会静默退化成"全部放行"。
- 越界端口的表现：`/api/vnc/probe`、`/api/vnc/install` 返回带区间和放行办法的失败原因，
  `/vnc-ws` 在握手前直接拒绝（不落 101），并记一条 `[vnc] 拒绝隧道请求` 日志。

VNC 口令（一键安装在目标机上生成的随机口令）只走**按需 + 留痕**两条路径：

| 接口 | 是否带出口令 | 审计 |
|------|--------------|------|
| `POST /api/vnc/install` | 是（谁下的指令谁取一次，用于自动回填） | 记「VNC 安装/启动」+ 端口/模式/结果 |
| `GET /api/vnc/status` | **否**，只回 `has_password` | 无（前端每 3s 轮询） |
| `POST /api/vnc/password` | 是 | 记「获取 VNC 口令」，结果只写"取到了没有" |

状态接口是轮询的，口令一旦进它的响应体就等于长期驻留在浏览器里；审计里存"取过"而不是口令本身，
否则审计库会变成第二份明文凭据存储。

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
| `agent.key` | Agent 注册预共享密钥。**默认不打包**，勾「包含凭据文件」/`{"include_secrets":true}` 才带上 | 是 |

**刻意不备份**：`packages/`（安装包，可从上游重下，动辄上百 MB）、`uploads/`（临时文件）、
日志、SQLite 的 `-wal`/`-shm`，以及 `<data>/node.key`（公开节点编号的派生私钥，
见「公开状态页 → 安全设计」）和 `<data>/ca.key`（注册 CA，丢失需所有 Agent 重新换取证书）。

> 备份包**含凭据**（口令哈希与 OTP 密钥）。请按"等同口令文件"对待，不要随意外发；
> 备份接口能下载到的包默认不再带全网身份根 —— `agent.key` 只在显式勾选时进包，
> 且这一次选择会写进操作审计。恢复后如果需要它，直接在新服务端「Agent 接入」页
> 轮换一把新的更稳妥（旧的很可能已随外流的包泄露）。

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
4. 写入 Agent 配置（服务端地址、Agent 名称、注册凭据、代理、延迟目标，权限 0600）
5. 安装为 systemd 服务并启动（配置与二进制分离：`/etc/mocenter/mocenter-agent.json` + `/usr/local/bin/mocenter-agent`）
6. 回读状态确认（`is-active` / `is-enabled` / `-version`）

### 安全约束

- **凭证不落盘、不入日志**。密码/私钥只在单次请求内存中存在；审计日志只记"谁在何时部署了哪个地址"与该机的**主机密钥指纹**（指纹是公开信息，用于事后核对）。
- **主机密钥必须校验**（known_hosts TOFU，见下一小节）。
- **命令注入防护（纵深防御）**：AgentID 过白名单（`[A-Za-z0-9._-]{1,64}`）、安装目录与服务名过格式校验、
  所有拼进 shell 的变量经 POSIX 单引号转义。即使上游校验有疏漏，也不会因一个引号变成命令注入。
- **上传体积上限** 200 MB；安装包文件名拒绝目录成分。
- 接口需登录（与其它后台接口同一套 Token + IP 白名单 + 限流）。

### 主机密钥信任（known_hosts TOFU）

一键部署是**服务端主动去连别人的机器**，并且连的时候手里握着注册密钥和操作员的口令/私钥。
如果握手时不校验对方是谁，路径上的中间人就能同时收下这三样东西。因此这条通道没有
"忽略主机密钥"的开关（也没有对应的命令行参数）：

| 情况 | 行为 |
|------|------|
| 第一次连接某个 `主机[:端口]` | 记录密钥并放行，结果里回显指纹 + 前端标黄提示"请核对" |
| 之后密钥与记录一致 | 正常放行 |
| 之后密钥变了 | **中止部署**，错误信息里同时给出记录指纹与实际指纹（可能是中间人，也可能该机真重装过） |
| 表单填了 `主机密钥指纹` | 强制钉扎：与该值不一致就中止，不走首次信任（省略 `SHA256:` 前缀也可以） |
| 记账文件读不了/数据目录缺失 | 中止部署（fail-closed），不会退回不校验 |

- 记账文件：`<数据目录>/ssh_known_hosts`，权限 0600，OpenSSH known_hosts 格式，只追加不重写。
  与运维个人的 `~/.ssh/known_hosts` 分开，避免互相污染。
- 撤销信任：直接删掉该文件里对应的那一行，再重新部署并核对指纹。
- 指纹来源：目标机上 `ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub`。
- 新机器建议先点「测试连接与探测环境」（仅探测、不改动）拿到指纹，核对后再填入执行部署。

> **Windows 未实现**：OpenSSH for Windows 虽然存在，但凭证、服务注册与权限模型与 Linux 差异大，
> 按需求"没有类似方案则略过"。Windows 侧请继续用图形安装程序（`MoCenter-Setup-<版本>.exe`）或
> `scripts\deploy\install-windows.bat`。

## 十一、保活自启（服务之外的第二道保险）

**目标**：进程被结束、服务被停掉、或环境根本不允许注册系统服务时，Agent/Server 仍能自动回来。

### Windows：两个计划任务

图形安装程序与 `mocenter-deploy.exe install` 都会额外注册（以 SYSTEM 主体运行，无需保存口令）：

| 任务名 | 触发 | 作用 |
|--------|------|------|
| `<角色>Boot`（`MoCenterAgentBoot` / `MoCenterServerBoot`） | 开机（`ONSTART`，不可用时回退登录 `ONLOGON`） | 开机自启 |
| `<角色>KeepAlive`（`MoCenterAgentKeepAlive` / `MoCenterServerKeepAlive`） | 每 5 分钟 | 巡检并拉起（进程被结束/崩溃后最多 5 分钟回来） |

> - 服务能正常运行的环境里，它们是空转的安全网；容器/组策略禁用服务的环境里，
>   它们就是唯一的自启与防杀通道。
> - 计划任务通过 `mocenter-deploy.exe run -role <角色> -exe … -config …` 拉起进程，
>   目的是让子进程带 `CREATE_NO_WINDOW`，避免每次拉起都闪一个黑窗口。
> - `mocenter-deploy.exe uninstall` 会一并删除这两个任务；`status` 会打印它们是否存在。
> - **注册前会检查路径可写性，检查不过就不注册**：这两个任务以 `SYSTEM` + 最高权限运行，
>   跑的是安装目录里的 `mocenter-deploy.exe` 与被守护的二进制，读的是那份配置文件。
>   如果这三者中任何一个所在的目录允许非管理员写入（把 `-dir` 指到公共可写位置就会这样），
>   本机任意用户改掉那个文件即等于拿到 SYSTEM —— 安装器此时**拒绝注册**，打印受影响路径与
>   一条可直接执行的 `icacls` 整改命令，然后继续完成安装（服务本身不受影响，只是少了这道保险）。
>   整改后重跑一次安装即可启用。装到默认的 `C:\Program Files\MoCenter` 不会被拦。
> - **数据目录（`%ProgramData%\MoCenter\<角色>`）与配置文件写入后会显式收紧 ACL**，
>   只留 SYSTEM / Administrators / 安装时的登录用户。Go 在 Windows 上的 `0600` 不产生任何
>   ACL 语义（对象只继承父目录授权），所以这一步不能省：配置里是注册密钥与客户端私钥。
>   升级安装会顺带把该目录下**已存在**的旧文件逐个补紧。
> - 一键安装的 VNC 同理：`ultravnc.ini` 里那份 VNC 口令用的是厂商公开的固定 DES 密钥混淆，
>   **读到文件就等于读到口令**，因此写完也会立即收紧为仅特权身份可读。

### Linux：systemd timer 或 cron

见「方式二」中的**保活自启**表：有 systemd 时注册 `watchdog.timer`，
无 systemd 时写 `/etc/cron.d/mocenter-<角色>`，两者共用
`scripts/deploy/mocenter-keepalive.sh`（幂等，进程在跑就直接退出）。

### 与单实例保护的关系

保活手段（计划任务 / cron / timer）都只按"进程名是否存在"判断，天然有竞态：
极端情况下可能同时拉起两份。这里**不依赖脚本自身去重**，而是由程序内的单实例保护兜底
（见下节）：后启动的一方拿不到锁，会打印持有者信息后**主动退出**，不会出现两个实例
同时连服务端互相顶号。

## 十二、版本号管理

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
可以高于、等于或低于服务端版本，接口不会因此拒绝。它**必填** —— 版本号参与升级包签名，
服务端替操作者补一个默认值只会让 Agent 报"签名校验失败"，看起来像包被动过。

Agent 侧的判定（`decideUpgrade`）：
- 目标版本 == 自身版本 → 跳过，除非勾了「同版本强制重装」（`force`）；
- 目标版本 < 自身版本 → 按**降级**拒绝，除非显式带 `allow_downgrade`；
  `force` 不再顺带放开降级，它只管"同版本重装"；
- 版本号无法解析（如 `latest`）→ 直接报错，不做模糊比较。

因此这个字段只需要在**同一台机器的历史序列**里有意义，不需要跨端对齐。

## 十三、单实例保护

Agent 与 Server **同一份配置只允许一个实例**。跨平台实现不同，但对使用者是同一件事：

| 平台 | 机制 | 说明 |
|------|------|------|
| Windows | 命名互斥体（`CreateMutexW`，优先 `Global\`） | 内核对象，进程退出即释放 |
| Linux / macOS / *BSD | `flock(2)` 独占锁 | 文件锁，进程退出（含 `SIGKILL`、断电重启）由内核释放 |

两个平台都**不需要清理陈旧状态**：不会出现"进程已经死了、锁还在"把新实例永久挡在门外的问题。

### 为什么 Linux 上必须做

`systemd` 只保证"同一 unit 不会跑两份"。但下面这些情况它管不到，而它们恰好都很常见：

- **定时任务保活**：`*/5 * * * * pgrep -f mocenter-agent || /usr/local/bin/mocenter-agent ...`
  —— 脚本按进程名判断，服务重启、手工换路径、`-daemon` 派生都会让它判断失误；
- **手工排查再启一份**：排障时最常见的动作；
- **服务报失败但进程还活着**（`nohup` / supervisor / `-daemon` 混用）。

重复实例的危害不是"多占点内存"：它们共用同一配置（同一 `agent_id`、同一套客户端证书），
会一起连服务端**互相顶号**；隧道、VNC、端口映射的会话状态错乱；
若 Server 也重复启动，还会共用同一个 SQLite 文件互相踩踏。定时任务每 5 分钟拉一个，越滚越多。

### 锁文件在哪

**与 `-config` 指定的配置文件同目录**，命名为 `.mocenter-<role>-<哈希>.lock`：

| 部署形态 | 配置文件 | 锁文件 |
|----------|----------|--------|
| systemd 安装（推荐） | `/etc/mocenter/mocenter-agent.json` | `/etc/mocenter/.mocenter-Agent-xxxxxxxx.lock` |
| 便携 / 手工启动 | 二进制同目录的 `mocenter-agent.json` | 同目录 |

> 安装包的 systemd 单元**必须**显式传 `-config /etc/mocenter/mocenter-agent.json`。
> 不传的话配置（与锁文件）会落到 `/usr/local/bin`，而"以 root 跑服务 + 以普通用户跑 cron 保活"
> 这种最常见的组合就**拿不到同一把锁**（目录不可写 → 降级放行），保护等于没做。
> 本仓库的 `scripts/deploy/mocenter-agent.service` 已按此写好。

### 拿到锁失败时的行为（刻意设计）

1. **退出，绝不杀进程**。日志会打印持有者 PID、可执行文件、配置路径与启动时间：

   ```
   [单实例] 已有实例正在运行: PID 12345（该 PID 来自上一次开机，属异常退出遗留，不可按编号判断进程）
   （可执行文件 /usr/local/bin/mocenter-agent，配置 /etc/mocenter/mocenter-agent.json，启动于 2026-09-18T10:00:00+08:00）
   ```

2. **不自动"清理"锁文件**。判断可能出错（容器 PID 命名空间、其它用户），
   而误删 / 误杀一个正在跑远控会话的 Agent 后果严重。确认系统里确实没有实例在运行，
   再手工删锁文件重启。

### 锁文件打开失败时

只读文件系统、目录不可写、SELinux 等情况下**不阻塞启动**（宁可多跑一个，也要让 Agent 起得来），
但会打印明确的告警，不会静默失效：

```
[单实例] 警告: 无法打开锁文件 /etc/mocenter/.mocenter-Agent-xxxxxxxx.lock: permission denied
—— 本次启动**未启用**单实例保护
```

看到这行说明**保护没生效**，请把 `-config` 指到可写目录（或修正目录权限）。

### 同一台机器跑多份 Agent

用**不同的 `-config`** 即可。锁按"配置路径哈希 + 角色"区分，
不同配置的实例互不干扰 —— 这也是调试时同机跑两个 Agent 的正规做法。

## 十四、常见故障处理

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
| 一键部署报服务启动成功但 Agent 不上线 | 检查写入的注册凭据是否有效（`/etc/mocenter/mocenter-agent.json` 的 `agent_key`：一次性令牌要用对应该 `agent_id` 且未过期未使用的）、服务端地址是否从目标机可达；`systemctl status mocenter-agent` 看日志 |
| 恢复备份后数据没变 | 恢复后需**重启服务端**；被覆盖的旧文件在 `<文件名>.before-restore` |
| 日志出现「已有一个使用同一配置的 Agent 在运行，本次启动终止」 | **这是保护生效，不是故障**：已有实例持有单实例锁。用 `systemctl status mocenter-agent` / `ps -ef \| grep mocenter-agent` 确认。若确实是上次断电遗留（日志会提示锁文件多久没被刷新、PID 是否来自上一次开机），删掉锁文件后重启即可，**不要**去杀那个 PID（可能已被复用）。详见「单实例保护」 |
| 日志出现「无法打开锁文件 … 本次启动**未启用**单实例保护」 | 锁文件所在目录对运行用户不可写。锁文件与 `-config` 指定的配置文件同目录，请确保该目录可写（systemd 单元里是 `/etc/mocenter`，见「单实例保护」） |
| Agent 报 `x509: certificate is valid for 127.0.0.1, ::1, not <IP>` | 服务端证书 SAN 不含 Agent 连接用的地址。见下方「TLS 证书 SAN 不匹配」 |
| Agent 报 `服务端地址仍是安装占位符 "your.server.com:9000"` 后退出 | 地址来自历次安装介质预置的占位值（旧 `.deb`/`.rpm` 里的 `mocenter-agent.conf`、旧 `install-windows.bat` 示例）。它连不上任何主机但形似合法配置，过去的表现是"服务在跑、这台永远不在线"，现在 Agent 启动即拒绝并把问题写在一行日志里。改法：把 `MOCENTER_SERVER`（或 `-server`、配置文件里的 `server`）改成实际地址。当前版本的 conf 模板已改为留空、`install-linux.sh` 装 Agent 时缺 `--server` 直接报错退出 |
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

## 十五、远程升级 Agent

规划书 12.4 的原始表述，落地后的实际形态如下（细节与操作见「六、高级运维能力 → 3. 远程升级 Agent」）：

- **触发**：由操作者在 Web「运维操作」或 `POST /api/upgrade` 下发，**不是** Agent 自己探测新版本；
  灰度就是"只对选中的一批主机下发"，天然支持。
- **校验**：升级包必须是 https 来源，且同时通过 SHA-256 摘要与 Ed25519 签名
  （签名覆盖"目标版本号 + 包内容"）；Agent 侧没有跳过验签的开关。
- **落地**：一次性读入内存 → 校验 → 0600 临时文件落盘并复核摘要 → 备份旧二进制 →
  替换 → 以分离进程重新拉起 → 旧进程退出；替换失败会回滚备份。
- **不支持**：差分升级（未实现，走整包替换）。
