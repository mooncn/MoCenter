#!/usr/bin/env bash
# =============================================================
# MoCenter Linux 一键交付脚本：Server 多架构 + Agent 多架构 + 安装包
#
# 用法: bash scripts/packaging/build-linux-all.sh [deb|rpm|all|none] [arch]
#   缺省: all amd64
#   示例: bash scripts/packaging/build-linux-all.sh all amd64
#         bash scripts/packaging/build-linux-all.sh deb arm64
#         bash scripts/packaging/build-linux-all.sh none   # 只出二进制，不打包
#
# 产物:
#   build/server/linux/mocenter-server-<arch>   服务端二进制
#   build/agent/linux/mocenter-agent-<arch>     Agent 二进制
#   build/packages/                             .deb / .rpm 安装包
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PKG_TYPE="${1:-all}"
ARCH="${2:-amd64}"

echo "##########################################"
echo " MoCenter Linux 一键交付"
echo " 打包类型: ${PKG_TYPE}"
echo " 打包架构: ${ARCH}"
echo "##########################################"

# ---- 1. Server 多架构二进制 ----
echo ""
echo ">>> [1/4] 构建 Server（linux 全架构）"
bash "${ROOT_DIR}/scripts/packaging/build-linux-server.sh" all

# ---- 2. Agent 多架构二进制 ----
echo ""
echo ">>> [2/4] 构建 Agent（linux 全架构）"
bash "${ROOT_DIR}/scripts/packaging/build-linux-all.sh"

# ---- 3. Web 前端（可选，缺失不影响服务端运行）----
echo ""
echo ">>> [3/4] 构建 Web 前端"
if command -v npm >/dev/null 2>&1; then
  (cd "${ROOT_DIR}/web" && npm install --no-audit --no-fund && npm run build)
  echo "  -> ${ROOT_DIR}/web/dist（打包 Server 安装包时会一并装入）"
else
  echo "  !! 未安装 Node.js/npm，跳过 Web 构建（安装包将不含 Web 界面）"
fi

# ---- 4. 安装包 ----
echo ""
echo ">>> [4/4] 生成安装包"
if [ "${PKG_TYPE}" = "none" ]; then
  echo "  已指定 none，跳过打包"
else
  bash "${ROOT_DIR}/scripts/packaging/build-linux-server-pkg.sh" "${PKG_TYPE}" "${ARCH}"
  bash "${ROOT_DIR}/scripts/packaging/build-linux-pkg.sh" "${PKG_TYPE}" "${ARCH}"
fi

echo ""
echo "##########################################"
echo " 完成。产物目录:"
echo "   ${ROOT_DIR}/build/server/linux/"
echo "   ${ROOT_DIR}/build/agent/linux/"
echo "   ${ROOT_DIR}/build/packages/"
echo "##########################################"
