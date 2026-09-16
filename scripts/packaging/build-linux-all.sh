#!/usr/bin/env bash
# =============================================================
# MoCenter Agent Linux 多架构二进制构建脚本
# 一键交叉编译 amd64 / arm64 / arm / 386 二进制
# 产物输出到 build/agent/linux/
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AGENT_DIR="${ROOT_DIR}/agent"
OUT_DIR="${ROOT_DIR}/build/agent/linux"
# 与 Server 版本互相独立，见 agent/internal/version
AGENT_VERSION="${MOCENTER_AGENT_VERSION:-$(sed -n 's/^\s*const\s\+Version\s*=\s*"\([^"]*\)".*/\1/p' "${AGENT_DIR}/internal/version/version.go" 2>/dev/null | head -1)}"
AGENT_VERSION="${AGENT_VERSION:-1.0.1}"
mkdir -p "${OUT_DIR}"

echo "=========================================="
echo " MoCenter Agent Linux 多架构构建"
echo " 输出目录: ${OUT_DIR}"
echo "=========================================="

declare -A ARCHS=(
  [amd64]=amd64
  [arm64]=arm64
  [arm]=arm
  [386]=386
)

for arch in "${!ARCHS[@]}"; do
  echo "[+] 构建 ${arch} ..."
  (cd "${AGENT_DIR}" && GOOS=linux GOARCH="${ARCHS[$arch]}" CGO_ENABLED=0 \
    go build -ldflags "-s -w -X mocenter/agent/internal/version.Version=${AGENT_VERSION}" \
    -o "${OUT_DIR}/mocenter-agent-${arch}" .)
  echo "  -> ${OUT_DIR}/mocenter-agent-${arch}"
done

echo "=========================================="
echo " 二进制构建完成"
ls -lh "${OUT_DIR}"
echo " 如需生成 .deb/.rpm 安装包，请执行:"
echo "   bash scripts/packaging/build-linux-pkg.sh all amd64"
echo "=========================================="
