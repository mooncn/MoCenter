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
#   sudo bash scripts/deploy/install-linux.sh --role=server --bin=./mocenter-server --user=admin --password=***
#   sudo bash scripts/deploy/install-linux.sh --role=all    --bin-dir=./dist
#   sudo bash scripts/deploy/install-linux.sh --role=agent status
#   sudo bash scripts/deploy/install-linux.sh --role=agent uninstall
#
# 选项:
#   --role=agent|server|all     要安装/操作的组件，默认 agent
#   --bin=<路径>                单个组件时可直接指定二进制路径
#   --bin-dir=<目录>            在该目录下按 mocenter-<role>[-<arch>] 自动查找
#   --server=host:port          Agent 要连接的服务端地址（默认 your.server.com:9000）
#   --agentid=名称              Agent 标识（留空回退为设备 UUID/主机名）
#   --proxy-ip=前缀             国内加速代理前缀（可选）
#   --user= / --password=       Server 后台登录账号（密码默认 change-me，务必修改）
#   --listen= / --http=         Server 监听地址（默认 :9000 / :8080）
#   --web-dir=路径              Server 托管的 Web 静态资源目录
#   --force-config              覆盖已存在的配置文件（默认保留现场修改）
#   --purge                     卸载时同时删除配置与数据（默认保留）
#
# 动作（位置参数，默认 install）: install | uninstall | status
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TPL_DIR="$SCRIPT_DIR"
ACTION="install"

ROLE="agent"
BIN=""
BIN_DIR=""
SERVER="${MOCENTER_SERVER:-your.server.com:9000}"
AGENT_ID="${MOCENTER_AGENT_ID:-}"
PROXY_IP="${MOCENTER_PROXY_IP:-}"
S_USER="${MOCENTER_USER:-admin}"
S_PASS="${MOCENTER_PASSWORD:-change-me}"
S_LISTEN="${MOCENTER_LISTEN:-:9000}"
S_HTTP="${MOCENTER_HTTP:-:8080}"
S_WEB_DIR="${MOCENTER_WEB_DIR:-/usr/share/mocenter/web}"
FORCE_CONF=0
PURGE=0

BIN_DIR_DEFAULT="/usr/local/bin"
CONF_DIR="/etc/mocenter"
DATA_DIR="/var/lib/mocenter/data"
PKG_DIR="/var/lib/mocenter/packages"

die() { printf '** %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*"; }
ok() { printf '✓ %s\n' "$*"; }

usage() { sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

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
    --password=*)  S_PASS="${1#*=}" ;;
    --password)    shift; S_PASS="${1:-}" ;;
    --listen=*)    S_LISTEN="${1#*=}" ;;
    --listen)      shift; S_LISTEN="${1:-}" ;;
    --http=*)      S_HTTP="${1#*=}" ;;
    --http)        shift; S_HTTP="${1:-}" ;;
    --web-dir=*)   S_WEB_DIR="${1#*=}" ;;
    --web-dir)     shift; S_WEB_DIR="${1:-}" ;;
    --force-config) FORCE_CONF=1 ;;
    --purge)       PURGE=1 ;;
    --help|-h)     usage ;;
    install|uninstall|status) ACTION="$1" ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

case "$ROLE" in agent|server|all) ;; *) die "--role=$ROLE 不支持，可选：agent|server|all" ;; esac
case "$ACTION" in install|uninstall|status) ;; *) die "未知动作：$ACTION（可选 install|uninstall|status）" ;; esac

need_root() {
  [ "$(id -u)" = "0" ] || die "请以 root 运行：sudo bash $0 --role=$ROLE $ACTION"
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
        "$TPL_DIR/mocenter-agent.conf" > "$dest"
    [ -n "$PROXY_IP" ] && printf 'MOCENTER_PROXY_IP=%s\n' "$PROXY_IP" >> "$dest"
  else
    sed -e "s|^MOCENTER_LISTEN=.*|MOCENTER_LISTEN=${S_LISTEN}|" \
        -e "s|^MOCENTER_HTTP=.*|MOCENTER_HTTP=${S_HTTP}|" \
        -e "s|^MOCENTER_USER=.*|MOCENTER_USER=${S_USER}|" \
        -e "s|^MOCENTER_PASSWORD=.*|MOCENTER_PASSWORD=${S_PASS}|" \
        -e "s|^MOCENTER_WEB_DIR=.*|MOCENTER_WEB_DIR=${S_WEB_DIR}|" \
        "$TPL_DIR/mocenter-server.conf" > "$dest"
  fi
  chmod 0600 "$dest"
  info "  已生成配置：$dest"
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

  info "== 安装 $role =="
  install -m 0755 "$bin" "$BIN_DIR_DEFAULT/mocenter-${role}"
  ok "二进制 -> $BIN_DIR_DEFAULT/mocenter-${role}"
  render_conf "$role" "$CONF_DIR/mocenter-${role}.conf"

  [ -f "$TPL_DIR/$unit" ] || die "缺少服务单元模板：$TPL_DIR/$unit"
  install -m 0644 "$TPL_DIR/$unit" "/etc/systemd/system/$unit"
  ok "服务单元 -> /etc/systemd/system/$unit"

  systemctl daemon-reload
  systemctl enable "$unit" >/dev/null 2>&1 || true
  systemctl restart "$unit" || true
  sleep 1
  systemctl is-active --quiet "$unit" \
    && ok "服务已启动并开机自启：$unit" \
    || info "  !! 服务未处于 active，请查看 journalctl -u $unit -n 50"
}

# ---------------------------------------------------------------- 卸载
do_uninstall_role() {
  local role="$1" unit; unit="$(unit_name "$role")"
  info "== 卸载 $role =="
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
    rm -rf "$CONF_DIR/mocenter-${role}.conf" "$CONF_DIR/certs"
    [ "$role" = "server" ] && rm -rf "$DATA_DIR" "$PKG_DIR"
    ok "已删除配置与数据（--purge）"
  else
    info "  配置与数据已保留（$CONF_DIR / $DATA_DIR）；如需彻底删除请加 --purge"
  fi
}

do_status_role() {
  local role="$1" unit; unit="$(unit_name "$role")"
  info "== $role =="
  systemctl status "$unit" --no-pager 2>/dev/null || info "  服务 $unit 未安装"
  systemctl is-enabled "$unit" 2>/dev/null || info "  未启用开机自启"
}

case "$ACTION" in install) need_root ;; esac

for r in "${ROLES[@]}"; do
  case "$ACTION" in
    install)   do_install_role "$r" ;;
    uninstall) need_root; do_uninstall_role "$r" ;;
    status)    do_status_role "$r" ;;
  esac
done

if [ "$ACTION" = "install" ]; then
  printf '\n'
  ok "完成。"
  printf '   Agent : journalctl -u mocenter-agent -f\n'
  printf '   Server: journalctl -u mocenter-server -f\n'
  [ "$S_PASS" = "change-me" ] && printf '   !! Server 默认口令仍为 change-me，请立即修改 /etc/mocenter/mocenter-server.conf\n'
  exit 0
fi
