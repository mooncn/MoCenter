#!/usr/bin/env bash
# =============================================================
# MoCenter Server Linux 多架构二进制构建脚本
# 一键交叉编译 amd64 / arm64 / arm / 386 二进制
# 产物输出到 build/server/linux/
#
# 用法: bash scripts/packaging/build-linux-server.sh [amd64|arm64|arm|386|all]
#       缺省 all
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SERVER_DIR="${ROOT_DIR}/server"
OUT_DIR="${ROOT_DIR}/build/server/linux"
TARGET="${1:-all}"

echo "=========================================="
echo " MoCenter Server Linux 多架构构建"
echo " 输出目录: ${OUT_DIR}"
echo "=========================================="

if ! command -v go >/dev/null 2>&1; then
  echo "!! 未找到 Go，请先安装 Go 1.22+（服务端 go.mod 要求）" >&2
  exit 1
fi

ALL_ARCHS=(amd64 arm64 arm 386)
case "${TARGET}" in
  all)     ARCHS=("${ALL_ARCHS[@]}") ;;
  amd64)   ARCHS=(amd64) ;;
  arm64)   ARCHS=(arm64) ;;
  arm)     ARCHS=(arm) ;;
  386)     ARCHS=(386) ;;
  *) echo "不支持的架构: ${TARGET}（可用 amd64/arm64/arm/386/all）" >&2; exit 1 ;;
esac

mkdir -p "${OUT_DIR}"

# server 依赖 modernc.org/sqlite（纯 Go），因此 CGO_ENABLED=0 亦可交叉编译
for arch in "${ARCHS[@]}"; do
  echo "[+] 构建 linux/${arch} ..."
  (cd "${SERVER_DIR}" && \
    GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 go mod download && \
    GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 go build -ldflags "-s -w" \
      -o "${OUT_DIR}/mocenter-server-${arch}" .)
  echo "  -> ${OUT_DIR}/mocenter-server-${arch}"
done

echo "=========================================="
echo " 二进制构建完成"
ls -lh "${OUT_DIR}"
echo " 如需生成 .deb/.rpm 安装包，请执行:"
echo "   bash scripts/packaging/build-linux-server-pkg.sh all amd64"
echo "=========================================="
