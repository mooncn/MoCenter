#!/bin/sh
# =============================================================
# MoCenter 保活巡检脚本（systemd timer / cron 共用）
#
# 作用：**服务之外的第二道保险**。
# systemd 的 Restart=always 能覆盖"进程自己崩了"，但覆盖不了：
#   · 进程被 SIGKILL / OOM killer 以外的强制方式干掉后 systemd 未及时反应；
#   · 有人手工 systemctl stop 掉自己以为"临时停一下"；
#   · 容器里 systemd 与 PID 1 语义不一致，服务状态与实际进程脱节。
# 本脚本每 5 分钟巡检一次：目标进程不在就按同一份配置重新拉起。
#
# 幂等：进程已在运行则立即退出（Agent/Server 自身也有单实例保护兜底，
# 极端竞态下重复拉起的一方会自行退出，不会出现两个实例同时服务）。
#
# 用法（由 systemd timer / cron 调用，不建议手工执行）：
#   mocenter-keepalive.sh <agent|server>
# =============================================================
set -u

ROLE="${1:-agent}"
case "$ROLE" in agent|server) ;; *) echo "用法: $0 <agent|server>" >&2; exit 2 ;; esac

BIN="/usr/local/bin/mocenter-${ROLE}"
CONF="/etc/mocenter/mocenter-${ROLE}.conf"
LOG="/var/log/mocenter-${ROLE}-keepalive.log"

# 二进制或配置缺失：说明未安装（或已卸载），静默退出 —— 卸载后残留的定时任务
# 不应该每隔 5 分钟往日志里刷错误。
[ -x "$BIN" ] || exit 0
[ -f "$CONF" ] || exit 0

# 已有实例在跑就不管（服务或手工启动的都算）
if pgrep -f "$BIN" >/dev/null 2>&1; then
  exit 0
fi

# 载入配置文件（systemd EnvironmentFile 同款格式：KEY=VALUE）
# shellcheck disable=SC1090
. "$CONF" 2>/dev/null || true

# 组装启动参数：与 systemd 单元保持一致，避免"保活拉起的实例参数不同"
if [ "$ROLE" = "agent" ]; then
  set -- -config "${MOCENTER_CONFIG:-/etc/mocenter/mocenter-agent.json}" \
         -server "${MOCENTER_SERVER:-}" \
         -agentid "${MOCENTER_AGENT_ID:-}" \
         -proxy-ip "${MOCENTER_PROXY_IP:-}" \
         -daemon
else
  set -- -listen "${MOCENTER_LISTEN:-:9000}" \
         -http "${MOCENTER_HTTP:-:8080}" \
         -user "${MOCENTER_USER:-admin}" \
         -password-file "${MOCENTER_PASSWORD_FILE:-/etc/mocenter/initial-password}" \
         -data "${MOCENTER_DATA:-/var/lib/mocenter/data}" \
         -config "${MOCENTER_CONFIG:-/etc/mocenter/mocenter-server.json}" \
         -pkg-dir "${MOCENTER_PKG_DIR:-/var/lib/mocenter/packages}" \
         -web "${MOCENTER_WEB_DIR:-/usr/share/mocenter/web}"
fi

echo "$(date '+%F %T') 检测到 mocenter-${ROLE} 未运行，重新拉起" >> "$LOG" 2>/dev/null
# setsid：脱离当前会话，避免被 timer/cron 的会话终止时一并带走
setsid "$BIN" "$@" >> "$LOG" 2>&1 &
