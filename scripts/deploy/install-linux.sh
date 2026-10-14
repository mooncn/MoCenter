#!/usr/bin/env bash
# =============================================================
# MoCenter Linux 安装/服务注册脚本（需 root）
#
# 把二进制装到 /usr/local/bin，注册 systemd 服务并设置开机自启。
# Agent 与 Server 共用本脚本，用 --role 区分：
#   · 服务单元与默认配置直接取自 scripts/deploy/ 下的模板文件，
#     与「构建/打包」用的是同一份 —— 不会再出现「打包脚本改了、安装脚本忘改」。
#
# 用法:
#   sudo bash scripts/deploy/install-linux.sh --role=agent  --bin=./mocenter-agent --server=host:9000
#   sudo bash scripts/deploy/install-linux.sh --role=server --bin=./mocenter-server --user=admin --password-file=/root/pw.txt
#   sudo bash scripts/deploy/install-linux.sh --role=all    --bin-dir=./dist
#   sudo bash scripts/deploy/install-linux.sh --role=agent status
#   sudo bash scripts/deploy/install-linux.sh --role=agent uninstall
#
# 选项:
#   --role=agent|server|all     要安装/操作的组件，默认 agent
#   --force                     清理重装：删除既有配置后重新生成（默认是升级模式，保留配置）
#   --no-keepalive              不注册保活任务（默认注册：systemd timer，无 systemd 时用 cron）
#   --dry-run                   只打印将要执行的动作，不修改系统
#   --bin=<路径>                单个组件时可直接指定二进制路径
#   --bin-dir=<目录>            在该目录下按 mocenter-<role>[-<arch>] 自动查找
#   --server=host:port          Agent 要连接的服务端地址（安装 Agent 时必填）
#   --agentid=名称              Agent 标识（留空回退为设备 UUID/主机名）
#   --proxy-ip=前缀             国内加速代理前缀（可选）
#   --user=名称               Server 后台登录账号（默认 admin）
#   --password-file=路径      Server 初始口令取自该文件（建议 0600，装完可删）
#   --password                从**标准输入**读一行口令：printf '%s' "$pw" | ... --password
#                             ⚠️ 不提供 --password=*** 写法：命令行参数对任意本地
#                             用户可见（ps -ef），还会落进 shell 历史。
#                             两者都不给时自动生成 16 位随机强口令并打印一次。
#                             无论哪种来源，最终都落成 0600 的
#                             /etc/mocenter/initial-password，服务单元只引用该文件。
#   --listen= / --http=         Server 监听地址（默认 :9000 / :8080）
#   --web-dir=路径              Server 托管的 Web 静态资源目录
#   --force-config              覆盖已存在的配置文件（默认保留现场修改）
#   --purge                     卸载时同时删除配置与数据（默认保留）
#
# 动作（位置参数，默认 install）: install | upgrade | uninstall | status
#
# 升级语义（重要）:
#   检测到配置文件已存在即视为「已安装」，按**升级**处理：
#      · 只替换二进制、服务单元与保活任务；
#      · 服务端地址、注册密钥、账号、数据目录**原样保留**；
#      · 因此升级时不必重复提供 --server / --user / --password-file。
#   要推倒重来必须显式加 --force（会删除既有配置后重新生成）。
#
# 保活自启策略（自动选择）:
#   · 有 systemd：注册 mocenter-<role>.service（开机自启 + Restart=always）
#     **并**注册 mocenter-<role>-watchdog.timer（每 5 分钟巡检，被强杀也能回来）；
#   · 无 systemd（容器/精简系统）：写 /etc/cron.d/mocenter-<role>，
#     @reboot 拉起 + 每 5 分钟巡检，共用 /usr/local/bin/mocenter-keepalive。
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TPL_DIR="$SCRIPT_DIR"
ACTION="install"

ROLE="agent"
BIN=""
BIN_DIR=""
SERVER="${MOCENTER_SERVER:-}"
AGENT_ID="${MOCENTER_AGENT_ID:-}"
PROXY_IP="${MOCENTER_PROXY_IP:-}"
S_USER="${MOCENTER_USER:-admin}"
# 默认**不再是 change-me**：固定默认口令写在脚本里等于公开口令，
# "忘了改"就是一次现成的入侵路径。留空 = 安装时生成随机强口令。
S_PASS="${MOCENTER_PASSWORD:-}"
S_PASS_FILE=""
READ_PASS_STDIN=0
S_LISTEN="${MOCENTER_LISTEN:-:9000}"
S_HTTP="${MOCENTER_HTTP:-:8080}"
S_WEB_DIR="${MOCENTER_WEB_DIR:-/usr/share/mocenter/web}"
FORCE_CONF=0
PURGE=0
FORCE=0
NO_KEEPALIVE=0
DRY_RUN=0

BIN_DIR_DEFAULT="/usr/local/bin"
CONF_DIR="/etc/mocenter"
# 初始口令文件：0600、仅 root 可读。服务启动时读取，创建完账号即可删除。
PASS_FILE="$CONF_DIR/initial-password"
DATA_DIR="/var/lib/mocenter/data"
PKG_DIR="/var/lib/mocenter/packages"

die() { printf '** %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*"; }
ok() { printf '✓ %s\n' "$*"; }

# usage：打印文件头那段注释（两条 `# ====` 分隔线之间的内容）。
# 不写死行号：加一行说明就让 --help 少一截，是这类脚本最容易烂掉的地方。
usage() {
  awk 'NR==1 { next }
       /^# ={5,}[[:space:]]*\r?$/ { seen++; if (seen >= 2) exit; next }
       seen >= 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
  exit 0
}

# ---------------------------------------------------------------- 解析参数
while [ $# -gt 0 ]; do
  case "$1" in
    --role=*)      ROLE="${1#*=}" ;;
    --role)        shift; ROLE="${1:-}" ;;
    --bin=*)       BIN="${1#*=}" ;;
    --bin)         shift; BIN="${1:-}" ;;
    --bin-dir=*)   BIN_DIR="${1#*=}" ;;
    --bin-dir)     shift; BIN_DIR="${1:-}" ;;
    --server=*)    SERVER="${1#*=}" ;;
    --server)      shift; SERVER="${1:-}" ;;
    --agentid=*)   AGENT_ID="${1#*=}" ;;
    --agentid)     shift; AGENT_ID="${1:-}" ;;
    --proxy-ip=*)  PROXY_IP="${1#*=}" ;;
    --proxy-ip)    shift; PROXY_IP="${1:-}" ;;
    --user=*)      S_USER="${1#*=}" ;;
    --user)        shift; S_USER="${1:-}" ;;
    --password-file=*) S_PASS_FILE="${1#*=}" ;;
    --password-file)   shift; S_PASS_FILE="${1:-}" ;;
    # 只接受"从 stdin 读"这一种交互形态；=value 形式一律拒绝（见文件头说明）
    --password)    READ_PASS_STDIN=1 ;;
    --password=*)  die "--password 不接受命令行取值（会出现在 ps 与 shell 历史里）。
     改用：--password-file=<路径>，或 printf '%s' \"\$pw\" | $0 --password" ;;
    --listen=*)    S_LISTEN="${1#*=}" ;;
    --listen)      shift; S_LISTEN="${1:-}" ;;
    --http=*)      S_HTTP="${1#*=}" ;;
    --http)        shift; S_HTTP="${1:-}" ;;
    --web-dir=*)   S_WEB_DIR="${1#*=}" ;;
    --web-dir)     shift; S_WEB_DIR="${1:-}" ;;
    --force-config) FORCE_CONF=1 ;;
    --force)       FORCE=1 ;;
    --no-keepalive) NO_KEEPALIVE=1 ;;
    --dry-run)     DRY_RUN=1 ;;
    --purge)       PURGE=1 ;;
    --help|-h)     usage ;;
    install|upgrade|uninstall|status) ACTION="$1" ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

case "$ROLE" in agent|server|all) ;; *) die "--role=$ROLE 不支持，可选：agent|server|all" ;; esac
case "$ACTION" in install|upgrade|uninstall|status) ;; *) die "未知动作：$ACTION（可选 install|upgrade|uninstall|status）" ;; esac
# upgrade 与 install 走同一条路径：具体是"升级"还是"全新安装"由配置文件是否存在决定
INSTALL_ACTION=0
[ "$ACTION" = "install" ] || [ "$ACTION" = "upgrade" ] && INSTALL_ACTION=1

# Agent 必须有明确的服务端地址（L-7）。
# 之前这里默认填 your.server.com:9000：占位符长得像一份合法配置，Agent 会安静地
# 去解析一个不存在的域名并无限重连，运维看到的是"服务在跑但主机一直不在线"。
# 安装阶段就该问清楚的参数，不该推到运行期去猜。
if [ "$INSTALL_ACTION" = 1 ] && [ "$ROLE" != "server" ]; then
  [ -n "$SERVER" ] || die "缺少服务端地址：Agent 需要 --server=host:port（或环境变量 MOCENTER_SERVER）。
     例：sudo bash $0 --role=agent --bin=./mocenter-agent --server=10.0.0.5:9000"
fi

need_root() {
  [ "$DRY_RUN" = 1 ] && return 0
  [ "$(id -u)" = "0" ] || die "请以 root 运行：sudo bash $0 --role=$ROLE $ACTION"
}

# run：dry-run 时只打印，不改系统
run() {
  if [ "$DRY_RUN" = 1 ]; then
    info "  [dry-run] $*"
    return 0
  fi
  "$@"
}

ROLES=()
[ "$ROLE" = "all" ] && ROLES=(agent server) || ROLES=("$ROLE")

unit_name() { echo "mocenter-$1.service"; }

# ---------------------------------------------------------------- 查找二进制
# 依次尝试：显式 --bin、--bin-dir、同目录、同目录多架构子目录、构建产物目录
find_bin() {
  local role="$1" cand
  local here="$SCRIPT_DIR"
  if [ -n "$BIN" ] && [ "${#ROLES[@]}" = 1 ]; then echo "$BIN"; return 0; fi
  local dirs=()
  [ -n "$BIN_DIR" ] && dirs+=("$BIN_DIR")
  dirs+=("$here" "$here/bin" "$here/../.." "$here/../../build/linux/binaries/$role/linux")
  local names=("mocenter-${role}" "mocenter-${role}-amd64" "mocenter-${role}-arm64" "mocenter-${role}-arm" "mocenter-${role}-386")
  local d n
  for d in "${dirs[@]}"; do
    for n in "${names[@]}"; do
      cand="$d/$n"
      [ -f "$cand" ] && { echo "$cand"; return 0; }
    done
  done
  return 1
}

# ---------------------------------------------------------------- 环境探测
# have_systemd：systemctl 存在**且** systemd 真的是 PID 1。
# 容器里常有 systemctl 却没有可用 systemd（PID 1 是应用本身），
# 只看 command -v systemctl 会误判，然后 enable/restart 全部失败。
have_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 1
  [ -d /run/systemd/system ] || return 1
  [ -f /.dockerenv ] && return 1
  grep -qa 'container=' /proc/1/environ 2>/dev/null && return 1
  return 0
}

# installed：配置文件存在即视为已安装（与升级判定同源）。
installed() { [ -f "$CONF_DIR/mocenter-$1.conf" ]; }

# ---------------------------------------------------------------- 保活自启
# 注册「开机自启 + 每 5 分钟巡检」两层保险。
# 服务能跑时巡检是空转的安全网；服务不可用（容器/策略禁用）时它就是唯一通道。
install_keepalive() {
  local role="$1"
  [ "$NO_KEEPALIVE" = 1 ] && { info "  按 --no-keepalive 跳过保活任务注册"; return 0; }

  run install -m 0755 "$TPL_DIR/mocenter-keepalive.sh" /usr/local/bin/mocenter-keepalive

  if have_systemd; then
    local wd="mocenter-${role}-watchdog"
    # 模板必须按角色取：巡检脚本看护哪个进程写死在单元的 ExecStart 里，
    # 早先这里恒用 agent 模板，结果是"--role=server 装出来的 watchdog 其实在看护
    # agent，而 server 自己根本没有保活"。缺模板就直接报错，不要静默装个错的。
    [ -f "$TPL_DIR/${wd}.service" ] && [ -f "$TPL_DIR/${wd}.timer" ] \
      || die "缺少保活单元模板：$TPL_DIR/${wd}.{service,timer}"
    run install -m 0644 "$TPL_DIR/${wd}.service" "/etc/systemd/system/${wd}.service"
    run install -m 0644 "$TPL_DIR/${wd}.timer" "/etc/systemd/system/${wd}.timer"
    if [ "$DRY_RUN" = 0 ]; then
      systemctl daemon-reload
      systemctl enable --now "${wd}.timer" >/dev/null 2>&1 \
        && ok "保活定时器 -> ${wd}.timer（每 5 分钟巡检，开机自启）" \
        || info "  !! 保活定时器启用失败，请查看 systemctl status ${wd}.timer"
    else
      info "  [dry-run] 启用 ${wd}.timer"
    fi
    return 0
  fi

  # 无 systemd：用计划任务兜底
  if ! command -v crond >/dev/null 2>&1 && ! command -v cron >/dev/null 2>&1; then
    info "  !! 既无可用 systemd 也无 cron —— 保活任务无法注册。"
    info "     请用容器编排的重启策略兜底（docker --restart=always / k8s restartPolicy: Always）"
    return 0
  fi
  local cronfile="/etc/cron.d/mocenter-${role}"
  if [ "$DRY_RUN" = 1 ]; then
    info "  [dry-run] 写入 $cronfile（@reboot + 每 5 分钟巡检）"
    return 0
  fi
  mkdir -p "$(dirname "$cronfile")"
  cat > "$cronfile" <<EOF
# MoCenter ${role} 保活任务（由 scripts/deploy/install-linux.sh 生成）
# 卸载请用同一个脚本：--role=${role} uninstall
@reboot root /usr/local/bin/mocenter-keepalive ${role}
*/5 * * * * root /usr/local/bin/mocenter-keepalive ${role}
EOF
  chmod 0644 "$cronfile"
  ok "保活任务 -> $cronfile（@reboot + 每 5 分钟巡检）"
}

uninstall_keepalive() {
  local role="$1"
  local wd="mocenter-${role}-watchdog"
  if have_systemd; then
    run systemctl disable --now "${wd}.timer" >/dev/null 2>&1 || true
    [ "$DRY_RUN" = 0 ] && rm -f "/etc/systemd/system/${wd}.timer" "/etc/systemd/system/${wd}.service"
  fi
  run rm -f "/etc/cron.d/mocenter-${role}"
  # 巡检脚本仅在所有角色都卸载后才移除（--role=all 时两个角色先后调用）
  if [ ! -f "$CONF_DIR/mocenter-agent.conf" ] && [ ! -f "$CONF_DIR/mocenter-server.conf" ]; then
    run rm -f /usr/local/bin/mocenter-keepalive
  fi
}

# ---------------------------------------------------------------- 配置模板渲染
render_conf() {
  # render_conf <角色> <目标文件>
  local role="$1" dest="$2"
  if [ -f "$dest" ] && [ "$FORCE_CONF" = 0 ]; then
    info "  保留现有配置：$dest（如需覆盖请加 --force-config）"
    return 0
  fi
  if [ "$role" = "agent" ]; then
    sed -e "s|^MOCENTER_SERVER=.*|MOCENTER_SERVER=${SERVER}|" \
        -e "s|^MOCENTER_AGENT_ID=.*|MOCENTER_AGENT_ID=${AGENT_ID}|" \
        -e "s|^MOCENTER_CONFIG=.*|MOCENTER_CONFIG=$CONF_DIR/mocenter-agent.json|" \
        "$TPL_DIR/mocenter-agent.conf" > "$dest"
    [ -n "$PROXY_IP" ] && printf 'MOCENTER_PROXY_IP=%s\n' "$PROXY_IP" >> "$dest"
  else
    sed -e "s|^MOCENTER_LISTEN=.*|MOCENTER_LISTEN=${S_LISTEN}|" \
        -e "s|^MOCENTER_HTTP=.*|MOCENTER_HTTP=${S_HTTP}|" \
        -e "s|^MOCENTER_USER=.*|MOCENTER_USER=${S_USER}|" \
        -e "s|^MOCENTER_PASSWORD_FILE=.*|MOCENTER_PASSWORD_FILE=${PASS_FILE}|" \
        -e "s|^MOCENTER_WEB_DIR=.*|MOCENTER_WEB_DIR=${S_WEB_DIR}|" \
        "$TPL_DIR/mocenter-server.conf" > "$dest"
  fi
  chmod 0600 "$dest"
  info "  已生成配置：$dest"
}

# ---------------------------------------------------------------- 初始口令文件
# 把口令落成 0600 的**文件**，而不是让服务单元通过 -password 传参。
#
# 原因：systemd 的 ExecStart 参数对任意本地用户可读
#   · ps -ef | grep mocenter-server
#   · systemctl show mocenter-server -p ExecStart
# 于是"口令只告诉了我"这件事并不成立。文件方式下只有 root 能读。
#
# 生成策略：
#   · 用户显式给了 --password-file / --password(stdin)：用它；
#   · 没给：生成 16 位随机强口令（避免出现"两边都是默认口令"的部署），
#     并在安装结束打印一次，提示登录后立即修改。
#
# 已存在**任何账号**时服务端会忽略该文件内容，所以重复安装不会把口令重置回去；
# 但本脚本也不覆盖已存在的口令文件，避免把运维刚改好的口令顶掉。
write_password_file() {
  local mode
  if [ -f "$PASS_FILE" ]; then
    info "  保留现有口令文件：$PASS_FILE（如已登录过，该文件可安全删除）"
    return 0
  fi
  # 口令在这里汇合成一个值，来源按 --password-file > --password(stdin) > 环境变量 排序，
  # 都不给时才随机生成。刻意不接受 --password=xxx：那会把口令暴露在 ps 与 shell 历史里。
  if [ -n "$S_PASS_FILE" ]; then
    [ -r "$S_PASS_FILE" ] || die "无法读取 --password-file=$S_PASS_FILE"
    # 只提示不阻断：这份文件是操作者自己准备的，读完就不再需要它
    mode="$(stat -c '%a' "$S_PASS_FILE" 2>/dev/null || true)"
    if [ -n "$mode" ] && [ "${mode: -1}" != "0" ]; then
      info "  !! 口令文件 mode=$mode，其他用户可读；建议 chmod 600 后再继续"
    fi
    S_PASS="$(tr -d '\r\n' < "$S_PASS_FILE")"
    [ -n "$S_PASS" ] || die "--password-file=$S_PASS_FILE 内容为空"
  elif [ "$READ_PASS_STDIN" = 1 ]; then
    printf '请输入 Server 初始口令（输入不回显，可用管道传入）: '
    IFS= read -r -s S_PASS || true
    printf '\n'
    [ -n "$S_PASS" ] || die "标准输入未读到口令"
  fi
  local generated=0
  if [ -z "$S_PASS" ]; then
    # tr 只取字母数字，避免生成带 $/`/\ 的口令在后续脚本里被二次解析
    S_PASS="$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 16 || true)"
    [ -n "$S_PASS" ] || die "随机口令生成失败，请用 --password-file= 显式指定"
    generated=1
  fi
  # umask 兜底：即使 install 的 -m 在某些文件系统上被忽略，也不会短暂变成可读文件
  ( umask 077; printf '%s' "$S_PASS" > "$PASS_FILE" )
  chmod 0600 "$PASS_FILE"
  chown root:root "$PASS_FILE" 2>/dev/null || true
  if [ "$generated" = 1 ]; then
    printf '\n  >>> 已生成初始口令（仅本次显示）：%s\n' "$S_PASS"
    printf '      写入：%s（0600）\n' "$PASS_FILE"
  else
    info "  已写入初始口令文件：$PASS_FILE（0600）"
  fi
  printf '      提示：登录后请立即在「账号设置」修改密码，随后删除该文件。\n'
}

# ---------------------------------------------------------------- 安装
do_install_role() {
  local role="$1" bin unit
  bin="$(find_bin "$role")" || die "未找到 mocenter-${role} 二进制。
  请指定 --bin=<路径>，或用 --bin-dir 指向构建产物目录：
    bash scripts/build-linux-all.sh --type=$role --package=none   # 先构建"
  unit="$(unit_name "$role")"
  mkdir -p "$BIN_DIR_DEFAULT" "$CONF_DIR"
  [ "$role" = "server" ] && mkdir -p "$DATA_DIR" "$PKG_DIR" "$S_WEB_DIR"
  chmod 0700 "$CONF_DIR" 2>/dev/null || true

  # 升级识别：配置存在且未指定 --force 时按升级处理，只换二进制与服务，不动配置。
  # 这样"重新跑一次安装脚本"就是安全的升级动作，不会把现场配置冲掉。
  local upgrading=0
  if [ "$FORCE" = 0 ] && installed "$role"; then
    upgrading=1
    info "== 升级 $role（检测到 $CONF_DIR/mocenter-${role}.conf，将保留配置）=="
  elif [ "$FORCE" = 1 ]; then
    info "== 清理重装 $role（--force：既有配置将被删除）=="
  else
    info "== 安装 $role =="
  fi

  # 升级时二进制可能没随脚本带过来（就地升级场景）—— 显式报错，避免"假升级"
  if [ ! -f "$bin" ] && [ "$upgrading" = 1 ]; then
    die "未找到待替换的二进制（$bin）。升级请显式指定 --bin=<新版本路径>"
  fi

  # 停下旧实例再覆盖二进制：否则运行中的进程会让 install 报"文本文件忙"
  stop_running "$role"

  install -m 0755 "$bin" "$BIN_DIR_DEFAULT/mocenter-${role}"
  ok "二进制 -> $BIN_DIR_DEFAULT/mocenter-${role}"

  if [ "$upgrading" = 1 ]; then
    info "  保留既有配置：$CONF_DIR/mocenter-${role}.conf（升级不动配置；如需重置加 --force）"
    if [ "$role" = "server" ] && { [ -n "$S_PASS" ] || [ -n "$S_PASS_FILE" ] || [ "$READ_PASS_STDIN" = 1 ]; }; then
      info "  提示：升级模式下口令不会写入 —— 已存在的账号口令以此前设置为准"
    fi
  else
    if [ "$FORCE" = 1 ]; then
      # --force：清掉旧配置（以及 server 的口令文件），确保是全新现场
      run rm -f "$CONF_DIR/mocenter-${role}.conf"
      [ "$role" = "server" ] && run rm -f "$PASS_FILE"
    fi
    [ "$role" = "server" ] && write_password_file
    render_conf "$role" "$CONF_DIR/mocenter-${role}.conf"
  fi

  if have_systemd; then
    [ -f "$TPL_DIR/$unit" ] || die "缺少服务单元模板：$TPL_DIR/$unit"
    run install -m 0644 "$TPL_DIR/$unit" "/etc/systemd/system/$unit"
    ok "服务单元 -> /etc/systemd/system/$unit"
    if [ "$DRY_RUN" = 0 ]; then
      systemctl daemon-reload
      systemctl enable "$unit" >/dev/null 2>&1 || true
      systemctl restart "$unit" || true
      sleep 1
      systemctl is-active --quiet "$unit" \
        && ok "服务已启动并开机自启：$unit" \
        || info "  !! 服务未处于 active，请查看 journalctl -u $unit -n 50"
    fi
  else
    info "  !! 未检测到可用的 systemd —— 跳过服务注册，改由保活任务拉起"
  fi

  install_keepalive "$role"
  # 无 systemd 时服务启动交给保活脚本（它就是"立即拉起一次"）
  if ! have_systemd; then
    run /usr/local/bin/mocenter-keepalive "$role" || info "  !! 首次拉起失败，请查看 /var/log/mocenter-${role}-keepalive.log"
  fi
}

# stop_running：停止已运行实例（systemd 优先，其次按进程名）。
# 升级时若不停，覆盖二进制会失败（ETXTBSY），且新旧实例会短暂并存。
stop_running() {
  local role="$1" unit; unit="$(unit_name "$role")"
  if have_systemd && systemctl list-unit-files 2>/dev/null | grep -q "^${unit}"; then
    run systemctl stop "$unit" >/dev/null 2>&1 || true
  fi
  run pkill -f "$BIN_DIR_DEFAULT/mocenter-${role}" >/dev/null 2>&1 || true
  sleep 1
}

# ---------------------------------------------------------------- 卸载
do_uninstall_role() {
  local role="$1" unit; unit="$(unit_name "$role")"
  info "== 卸载 $role =="
  # 先摘保活：否则看门狗会在卸载后把进程又拉起来
  uninstall_keepalive "$role"
  if systemctl list-unit-files 2>/dev/null | grep -q "^${unit}"; then
    systemctl disable --now "$unit" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/$unit"
    systemctl daemon-reload
    ok "已停止并移除服务 $unit"
  else
    info "  服务 $unit 未注册，跳过"
  fi
  rm -f "$BIN_DIR_DEFAULT/mocenter-${role}"
  ok "已移除二进制 $BIN_DIR_DEFAULT/mocenter-${role}"
  if [ "$PURGE" = 1 ]; then
    rm -rf "$CONF_DIR/mocenter-${role}.conf" "$CONF_DIR/certs" "$PASS_FILE"
    [ "$role" = "server" ] && rm -rf "$DATA_DIR" "$PKG_DIR"
    ok "已删除配置与数据（--purge）"
  else
    info "  配置与数据已保留（$CONF_DIR / $DATA_DIR）；如需彻底删除请加 --purge"
  fi
}

do_status_role() {
  local role="$1" unit; unit="$(unit_name "$role")"
  info "== $role =="
  if have_systemd; then
    systemctl status "$unit" --no-pager 2>/dev/null || info "  服务 $unit 未安装"
    info "  开机自启: $(systemctl is-enabled "$unit" 2>/dev/null || echo 未启用)"
    info "  保活定时: $(systemctl is-active "mocenter-${role}-watchdog.timer" 2>/dev/null || echo 未启用)"
  else
    info "  未检测到可用的 systemd"
    [ -f "/etc/cron.d/mocenter-${role}" ] \
      && info "  保活任务: /etc/cron.d/mocenter-${role}（cron）" \
      || info "  保活任务: 未注册"
  fi
  if pgrep -f "$BIN_DIR_DEFAULT/mocenter-${role}" >/dev/null 2>&1; then
    info "  进程: 运行中 (PID $(pgrep -f "$BIN_DIR_DEFAULT/mocenter-${role}" | tr '\n' ' '))"
  else
    info "  进程: 未运行"
  fi
}

case "$ACTION" in install|upgrade) need_root ;; esac

for r in "${ROLES[@]}"; do
  case "$ACTION" in
    install|upgrade) do_install_role "$r" ;;
    uninstall) need_root; do_uninstall_role "$r" ;;
    status)    do_status_role "$r" ;;
  esac
done

if [ "$INSTALL_ACTION" = 1 ]; then
  printf '\n'
  ok "完成。"
  printf '   Agent : journalctl -u mocenter-agent -f\n'
  printf '   Server: journalctl -u mocenter-server -f\n'
  printf '   口令文件: %s（0600，仅 root 可读；创建完账号后建议删除）\n' "$PASS_FILE"
  printf '   !! 口令不会出现在服务单元里：如需查看执行参数请用 systemctl show -p ExecStart\n'
  if have_systemd; then
    printf '   保活: systemd 服务（开机自启 + Restart=always）+ watchdog.timer（每 5 分钟巡检）\n'
  else
    printf '   保活: 无 systemd —— 已改用 cron 计划任务（@reboot + 每 5 分钟巡检）\n'
  fi
  exit 0
fi
