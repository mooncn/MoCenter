#!/usr/bin/env bash
# =============================================================
# MoCenter 一键构建脚本 (Linux/macOS)
# 构建 web / server / agent 三大部分
# 用法: bash scripts/build-all.sh [linux|windows]
#       默认构建当前系统平台对应的 agent 二进制
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
echo ""
echo "[1/3] 构建 Server ..."
if command -v go >/dev/null 2>&1; then
  (cd "${ROOT_DIR}/server" && go mod download && go build -ldflags "-s -w" -o "${OUT_DIR}/mocenter-server" .)
  echo "  -> ${OUT_DIR}/mocenter-server"
else
  echo "  !! 未安装 Go，跳过 Server 构建"
fi

# ---------- 2. Agent ----------
echo ""
echo "[2/3] 构建 Agent ..."
if command -v go >/dev/null 2>&1; then
  AGENT_OUT="mocenter-agent"
  case "${PLATFORM}" in
    linux)  GOOS=linux GOARCH=amd64 ;;
    windows) GOOS=windows GOARCH=amd64; AGENT_OUT="mocenter-agent.exe" ;;
    darwin) GOOS=darwin GOARCH=amd64 ;;
    *) GOOS=linux GOARCH=amd64 ;;
  esac
  export GOOS GOARCH CGO_ENABLED=0
  (cd "${ROOT_DIR}/agent" && go mod download && \
    go build -ldflags "-s -w -X mocenter/agent/internal/version.Version=${AGENT_VERSION}" \
    -o "${OUT_DIR}/${AGENT_OUT}" .)
  echo "  -> ${OUT_DIR}/${AGENT_OUT}  (GOOS=${GOOS} GOARCH=${GOARCH} agent_version=${AGENT_VERSION})"
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
echo "=========================================="
