#!/usr/bin/env bash
# =============================================================
# MoCenter 一键构建脚本 (Linux/macOS)
# 构建 web / server / agent 三大部分
#
# 用法: bash scripts/build-all.sh [linux|windows|darwin]
#       参数只影响 Agent 的目标平台（Server 与 Web 是平台无关的），
#       缺省构建当前系统平台对应的 agent 二进制
#
# 产物:
#   build/mocenter-server[.exe]   服务端
#   build/mocenter-agent[.exe]    Agent
#   build/web/                    Web 前端静态资源
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${ROOT_DIR}/build"
PLATFORM="${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
# Agent 版本：从 agent 源码的 internal/version 读取（唯一事实来源），
# 可用 MOCENTER_AGENT_VERSION 覆盖。它与 Server 版本互相独立，无需保持一致。
AGENT_VERSION="${MOCENTER_AGENT_VERSION:-$(sed -n 's/^\s*const\s\+Version\s*=\s*"\([^"]*\)".*/\1/p' "${ROOT_DIR}/agent/internal/version/version.go" 2>/dev/null | head -1)}"
AGENT_VERSION="${AGENT_VERSION:-1.0.1}"

echo "=========================================="
echo " MoCenter 构建开始"
echo " 根目录: ${ROOT_DIR}"
echo " 输出目录: ${OUT_DIR}"
echo " Agent 目标平台: ${PLATFORM}"
echo "=========================================="

mkdir -p "${OUT_DIR}"

# ---------- 1. Server ----------
# Server 只在本机运行，因此按其自身平台编译 —— 绝不能把 Agent 的目标平台
# 传给它，否则在 Linux 上执行 `build-all.sh windows` 会误产出 linux 二进制，
# 而在 macOS 上则相反。它的平台参数只作用于 Agent。
echo ""
echo "[1/3] 构建 Server ..."
if command -v go >/dev/null 2>&1; then
  case "$(uname -s)" in
    Darwin) SERVER_GOOS=darwin ;;
    MINGW*|MSYS*|CYGWIN*) SERVER_GOOS=windows ;;
    *) SERVER_GOOS=linux ;;
  esac
  SERVER_OUT="mocenter-server"
  [ "${SERVER_GOOS}" = "windows" ] && SERVER_OUT="mocenter-server.exe"
  (cd "${ROOT_DIR}/server" && \
    GOOS="${SERVER_GOOS}" GOARCH=amd64 CGO_ENABLED=0 go mod download && \
    GOOS="${SERVER_GOOS}" GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" \
      -o "${OUT_DIR}/${SERVER_OUT}" .)
  echo "  -> ${OUT_DIR}/${SERVER_OUT}  (GOOS=${SERVER_GOOS} GOARCH=amd64)"
else
  echo "  !! 未安装 Go，跳过 Server 构建"
fi

# ---------- 2. Agent ----------
echo ""
echo "[2/3] 构建 Agent ..."
if command -v go >/dev/null 2>&1; then
  AGENT_OUT="mocenter-agent"
  AGENT_GOOS=linux
  AGENT_GOARCH=amd64
  case "${PLATFORM}" in
    linux)  AGENT_GOOS=linux ;;
    windows) AGENT_GOOS=windows; AGENT_OUT="mocenter-agent.exe" ;;
    darwin) AGENT_GOOS=darwin ;;
    *) echo "  !! 未知平台 ${PLATFORM}，按 linux 处理"; AGENT_GOOS=linux ;;
  esac
  (cd "${ROOT_DIR}/agent" && \
    GOOS="${AGENT_GOOS}" GOARCH="${AGENT_GOARCH}" CGO_ENABLED=0 go mod download && \
    GOOS="${AGENT_GOOS}" GOARCH="${AGENT_GOARCH}" CGO_ENABLED=0 \
    go build -ldflags "-s -w -X mocenter/agent/internal/version.Version=${AGENT_VERSION}" \
    -o "${OUT_DIR}/${AGENT_OUT}" .)
  echo "  -> ${OUT_DIR}/${AGENT_OUT}  (GOOS=${AGENT_GOOS} GOARCH=${AGENT_GOARCH} agent_version=${AGENT_VERSION})"
else
  echo "  !! 未安装 Go，跳过 Agent 构建"
fi

# ---------- 3. Web ----------
echo ""
echo "[3/3] 构建 Web ..."
if command -v npm >/dev/null 2>&1; then
  (cd "${ROOT_DIR}/web" && npm install --no-audit --no-fund && npm run build)
  # 将前端静态资源拷贝到 build/web
  rm -rf "${OUT_DIR}/web"
  cp -r "${ROOT_DIR}/web/dist" "${OUT_DIR}/web"
  echo "  -> ${OUT_DIR}/web"
else
  echo "  !! 未安装 Node.js/npm，跳过 Web 构建"
fi

echo ""
echo "=========================================="
echo " 构建完成，产物位于: ${OUT_DIR}"
ls -lh "${OUT_DIR}"
echo ""
echo " Linux Server 多架构二进制与 .deb/.rpm 安装包请用:"
echo "   bash scripts/packaging/build-linux-server-all.sh all amd64"
echo "=========================================="
