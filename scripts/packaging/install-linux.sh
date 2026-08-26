#!/usr/bin/env bash
# =============================================================
# MoCenter Agent 安装/注册为系统服务脚本 (Linux systemd)
# 用法: sudo bash install-linux.sh [install|uninstall|status]
#   - 安装二进制到 /usr/local/bin
#   - 注册 systemd 服务 mocenter-agent.service 并设置开机自启
#   - 启动参数从 /etc/mocenter/mocenter-agent.conf 读取（可选）
# =============================================================
set -e

ACTION="${1:-install}"
BIN_NAME="mocenter-agent"
INSTALL_DIR="/usr/local/bin"
SERVICE="mocenter-agent.service"
CONF_DIR="/etc/mocenter"
CONF_FILE="${CONF_DIR}/mocenter-agent.conf"

# 默认服务端地址（可被配置文件覆盖）
SERVER_ADDR="${MOCENTER_SERVER:-your.server.com:9000}"
AGENT_ID="${MOCENTER_AGENT_ID:-}"
PROXY_IP="${MOCENTER_PROXY_IP:-}"

need_root() {
  if [ "$(id -u)" != "0" ]; then
    echo "请以 root 运行: sudo bash $0 $ACTION"
    exit 1
  fi
}

do_install() {
  need_root
  local src="${1:-./mocenter-agent}"
  if [ ! -f "$src" ]; then
    echo "未找到 Agent 二进制: $src，请先构建（bash agent/build.sh linux）"
    exit 1
  fi

  mkdir -p "${INSTALL_DIR}" "${CONF_DIR}"
  install -m 0755 "$src" "${INSTALL_DIR}/${BIN_NAME}"

  # 生成配置文件（若不存在）
  if [ ! -f "${CONF_FILE}" ]; then
    cat > "${CONF_FILE}" <<EOF
# MoCenter Agent 配置
# 服务端地址 host:port
MOCENTER_SERVER="${SERVER_ADDR}"
# Agent 唯一标识（留空则使用主机名）
MOCENTER_AGENT_ID="${AGENT_ID}"
# 国内加速代理前缀（可选，用于公网 IP 探测与 VNC 安装包下载加速）
# MOCENTER_PROXY_IP="http://fa.mooncn.win/?url="
EOF
    echo "已生成配置文件: ${CONF_FILE}，请根据实际环境修改服务端地址"
  fi

  # 写入 systemd 服务单元
  cat > "/etc/systemd/system/${SERVICE}" <<EOF
[Unit]
Description=MoCenter Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-${CONF_FILE}
ExecStart=${INSTALL_DIR}/${BIN_NAME} -server \${MOCENTER_SERVER} -agentid \${MOCENTER_AGENT_ID} -proxy-ip \${MOCENTER_PROXY_IP} -daemon
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "${SERVICE}"
  systemctl restart "${SERVICE}"
  echo "✅ MoCenter Agent 已安装并注册为系统服务（开机自启）"
  echo "   服务: ${SERVICE}"
  echo "   二进制: ${INSTALL_DIR}/${BIN_NAME}"
  echo "   配置: ${CONF_FILE}"
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
  echo "配置文件 ${CONF_FILE} 已保留，如需彻底删除请手动移除"
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
