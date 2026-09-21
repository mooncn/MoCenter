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
set -eu

ROLE="${1:-agent}"
case "$ROLE" in agent|server) ;; *) echo "用法: $0 <agent|server>" >&2; exit 2 ;; esac

BIN="/usr/local/bin/mocenter-${ROLE}"
CONF="/etc/mocenter/mocenter-${ROLE}.conf"
LOG="/var/log/mocenter-${ROLE}-keepalive.log"

# 二进制或配置缺失：说明未安装（或已卸载），静默退出 —— 卸载后残留的定时任务
# 不应该每隔 5 分钟往日志里刷错误。
[ -x "$BIN" ] || exit 0
[ -f "$CONF" ] || exit 0

# 已有实例在跑就不管。
# 用 -x（按进程名精确匹配）而不是 -f（按整条命令行匹配）：后者会命中任何
# **参数里带了这个路径**的进程 —— 例如运维正在 `tail -f /usr/local/bin/mocenter-agent`
# 或跑着这条巡检脚本自己，于是"看起来有实例"，真正的服务却再也没被拉起来。
if pgrep -x "mocenter-${ROLE}" >/dev/null 2>&1; then
  exit 0
fi

# 读配置文件（systemd EnvironmentFile 同款格式：KEY=VALUE）。
#
# 这里**绝不 source**：这份文件由安装脚本生成、按部署习惯可能被放宽权限，
# 而本脚本以 root 运行 —— `source "$CONF"` 等于把文件内容当 shell 执行，
# 谁改了它谁就拿到了 root。因此只按"取某个键的值"逐行解析，多余内容一律忽略。
conf_value() {
  # conf_value <KEY>：取 EnvironmentFile 里该键的值（无则空串）。
  # 容忍：键前后空格、值两端成对的引号、Windows 换行留下的 \r。
  grep -E "^[[:space:]]*$1[[:space:]]*=" "$CONF" 2>/dev/null | tail -n 1 | tr -d '\r' | cut -d= -f2- \
    | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
          -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}

# conf_or <KEY> <默认值>：键缺失时回退。安装脚本一定会写全这些键，
# 这里的默认值只为手工部署时也能拉起一个参数正确的实例，
# 而不是"保活起来的进程因参数为空而退出，每 5 分钟重试一次"。
conf_or() {
  v=$(conf_value "$1")
  if [ -n "$v" ]; then printf '%s' "$v"; else printf '%s' "$2"; fi
}

# 组装启动参数：与 systemd 单元保持一致，避免"保活拉起的实例参数不同"。
# 默认值也必须与单元/安装脚本一致（-pkg-dir 是写死的路径，单元里同理）。
if [ "$ROLE" = "agent" ]; then
  set -- -config "$(conf_or MOCENTER_CONFIG /etc/mocenter/mocenter-agent.json)" \
         -server "$(conf_or MOCENTER_SERVER '')" \
         -agentid "$(conf_or MOCENTER_AGENT_ID '')" \
         -proxy-ip "$(conf_or MOCENTER_PROXY_IP '')" \
         -daemon
else
  set -- -listen "$(conf_or MOCENTER_LISTEN :9000)" \
         -http "$(conf_or MOCENTER_HTTP :8080)" \
         -user "$(conf_or MOCENTER_USER admin)" \
         -password-file "$(conf_or MOCENTER_PASSWORD_FILE /etc/mocenter/initial-password)" \
         -data "$(conf_or MOCENTER_DATA /var/lib/mocenter/data)" \
         -config "$(conf_or MOCENTER_CONFIG /etc/mocenter/mocenter-server.json)" \
         -pkg-dir /var/lib/mocenter/packages \
         -web "$(conf_or MOCENTER_WEB_DIR /usr/share/mocenter/web)"
fi

echo "$(date '+%F %T') 检测到 mocenter-${ROLE} 未运行，重新拉起" >> "$LOG" 2>/dev/null || true
# setsid：脱离当前会话，避免被 timer/cron 的会话终止时一并带走
setsid "$BIN" "$@" >> "$LOG" 2>&1 || true
