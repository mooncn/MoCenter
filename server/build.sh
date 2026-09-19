#!/usr/bin/env bash
# MoCenter Server 构建脚本 (Linux/macOS)
# 用法: bash build.sh [linux|darwin|windows] [amd64|arm64|arm|386]
#       默认按当前系统与 amd64 构建；构建 windows 时输出 .exe
# 注意: server 依赖 modernc.org/sqlite（纯 Go），因此无需 CGO，
#       可放心交叉编译到 arm64 / arm 等目标架构。
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"

PLATFORM="${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
ARCH="${2:-$(uname -m 2>/dev/null || echo amd64)}"
OUT="mocenter-server"

case "${ARCH}" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  armv7l|arm) GOARCH=arm; export GOARM=7 ;;
  i386|i686|386) GOARCH=386 ;;
  *) GOARCH=amd64 ;;
esac

case "${PLATFORM}" in
  linux)   GOOS=linux ;;
  darwin|macos) GOOS=darwin ;;
  windows) GOOS=windows; OUT="mocenter-server.exe" ;;
  *) GOOS=linux ;;
esac

export GOOS GOARCH CGO_ENABLED=0
echo "[server] go mod download ..."
go mod download
echo "[server] building ${OUT} (GOOS=${GOOS} GOARCH=${GOARCH}) ..."
go build -ldflags "-s -w" -o "${OUT}" .
echo "[server] done -> ./${OUT}"
