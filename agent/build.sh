#!/usr/bin/env bash
# MoCenter Agent 构建脚本 (Linux/macOS)
# 用法: bash build.sh [linux|windows|darwin]
#       默认构建当前平台；构建 windows 时输出 .exe
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"

PLATFORM="${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
OUT="mocenter-agent"
GOOS="linux"; GOARCH="amd64"

case "${PLATFORM}" in
  linux)   GOOS=linux GOARCH=amd64 ;;
  windows) GOOS=windows GOARCH=amd64; OUT="mocenter-agent.exe" ;;
  darwin)  GOOS=darwin GOARCH=amd64 ;;
  *) GOOS=linux GOARCH=amd64 ;;
esac

export GOOS GOARCH CGO_ENABLED=0
echo "[agent] go mod download ..."
go mod download
echo "[agent] building ${OUT} (GOOS=${GOOS} GOARCH=${GOARCH}) ..."
go build -ldflags "-s -w" -o "${OUT}" .
echo "[agent] done -> ./${OUT}"
