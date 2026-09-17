#!/usr/bin/env bash
# =============================================================
# MoCenter Server 安装/注册为系统服务脚本 (Linux systemd)
# 用法: sudo bash install-linux-server.sh [install|uninstall|status] [二进制路径]
#   - 安装二进制到 /usr/local/bin
#   - 注册 systemd 服务 mocenter-server.service 并设置开机自启
#   - 启动参数从 /etc/mocenter/mocenter-server.conf 读取（首次自动生成）
#
# 典型用法:
#   bash scripts/packaging/build-linux-server.sh amd64          # 先构建
#   sudo bash scripts/packaging/install-linux-server.sh install \
#        ./build/server/linux/mocenter-server-amd64
# =============================================================
set -e

ACTION="${1:-install}"
BIN_NAME="mocenter-server"
INSTALL_DIR="/usr/local/bin"
SERVICE="mocenter-server.service"
CONF_DIR="/etc/mocenter"
CONF_FILE="${CONF_DIR}/mocenter-server.conf"
CONFIG_JSON="${CONF_DIR}/mocenter-server.json"
DATA_DIR="/var/lib/mocenter/data"
PKG_DIR="/var/lib/mocenter/packages"
WEB_DIR="/usr/share/mocenter/web"

# 默认监听与登录参数（可在安装前用环境变量覆盖）
LISTEN="${MOCENTER_LISTEN:-:9000}"
HTTP="${MOCENTER_HTTP:-:8080}"
USER_NAME="${MOCENTER_USER:-admin}"
PASSWORD="${MOCENTER_PASSWORD:-change-me}"

need_root() {
  if [ "$(id -u)" != "0" ]; then
    echo "请以 root 运行: sudo bash $0 $ACTION"
    exit 1
  fi
}

do_install() {
  need_root
  local src="${1:-./mocenter-server}"
  if [ ! -f "$src" ]; then
    echo "未找到 Server 二进制: $src"
    echo "请先构建: bash scripts/packaging/build-linux-server.sh amd64"
    echo "或:       bash server/build.sh"
    exit 1
  fi

  mkdir -p "${INSTALL_DIR}" "${CONF_DIR}" "${DATA_DIR}" "${PKG_DIR}"
  install -m 0755 "$src" "${INSTALL_DIR}/${BIN_NAME}"

  # 生成配置文件（若不存在）——已存在时保留用户修改
  if [ ! -f "${CONF_FILE}" ]; then
    cat > "${CONF_FILE}" <<CONF
# MoCenter Server 配置（由 systemd 注入启动参数）
MOCENTER_LISTEN="${LISTEN}"
MOCENTER_HTTP="${HTTP}"
# 登录用户名与密码（请立即修改）
MOCENTER_USER="${USER_NAME}"
MOCENTER_PASSWORD="${PASSWORD}"
# Web 前端目录（先用 bash web/build.sh 生成 dist，再拷贝到该目录；留空则不托管前端）
MOCENTER_WEB_DIR="${WEB_DIR}"
# 数据目录（审计日志、users.json、agent.key、托管安装包）
MOCENTER_DATA="${DATA_DIR}"
# 服务端全量配置文件路径（首次启动自动生成）
MOCENTER_CONFIG="${CONFIG_JSON}"
# IP 白名单（逗号分隔，留空不限制）
# MOCENTER_WHITELIST="10.0.0.0/8,192.168.0.0/16"
# TLS / mTLS 证书
# MOCENTER_TLS_CERT="${CONF_DIR}/certs/server.crt"
# MOCENTER_TLS_KEY="${CONF_DIR}/certs/server.key"
# MOCENTER_CA_CERT="${CONF_DIR}/certs/ca.crt"
# TOTP 二次校验密钥（base32）
# MOCENTER_TOTP_SECRET=""
CONF
    echo "已生成配置文件: ${CONF_FILE}，请修改服务端地址、口令与白名单"
  fi

  # 写入 systemd 服务单元
  cat > "/etc/systemd/system/${SERVICE}" <<UNIT
[Unit]
Description=MoCenter Server (集中式主机远控管理总控服务端)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/var/lib/mocenter
EnvironmentFile=-${CONF_FILE}
ExecStart=${INSTALL_DIR}/${BIN_NAME} \\
  -listen \${MOCENTER_LISTEN} \\
  -http \${MOCENTER_HTTP} \\
  -user \${MOCENTER_USER} \\
  -password \${MOCENTER_PASSWORD} \\
  -data \${MOCENTER_DATA} \\
  -config \${MOCENTER_CONFIG} \\
  -pkg-dir ${PKG_DIR} \\
  -web \${MOCENTER_WEB_DIR}
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable "${SERVICE}"
  systemctl restart "${SERVICE}"
  echo "✅ MoCenter Server 已安装并注册为系统服务（开机自启）"
  echo "   服务: ${SERVICE}"
  echo "   二进制: ${INSTALL_DIR}/${BIN_NAME}"
  echo "   配置: ${CONF_FILE}（全量参数见 ${CONFIG_JSON}）"
  echo "   数据: ${DATA_DIR}"
  echo "   端口: ${LISTEN} (Agent 长连接) / ${HTTP} (Web 控制台与 REST API)"
  echo "   日志: journalctl -u ${SERVICE} -f"
  systemctl status "${SERVICE}" --no-pager || true
}

do_uninstall() {
  need_root
  if systemctl list-unit-files | grep -q "${SERVICE}"; then
    systemctl disable --now "${SERVICE}" 2>/dev/null || true
    rm -f "/etc/systemd/system/${SERVICE}"
    systemctl daemon-reload
    echo "已停止并移除服务 ${SERVICE}"
  fi
  rm -f "${INSTALL_DIR}/${BIN_NAME}"
  echo "已删除二进制 ${INSTALL_DIR}/${BIN_NAME}"
  echo "配置与数据已保留：${CONF_DIR} / ${DATA_DIR}"
  echo "如需彻底删除请手动移除（注意其中的 agent.key 与审计日志）"
}

do_status() {
  systemctl status "${SERVICE}" --no-pager || true
  systemctl is-enabled "${SERVICE}" 2>/dev/null || echo "服务未启用开机自启"
}

case "${ACTION}" in
  install) do_install "$2" ;;
  uninstall) do_uninstall ;;
  status) do_status ;;
  *) echo "用法: $0 [install <binary>|uninstall|status]"; exit 1 ;;
esac
