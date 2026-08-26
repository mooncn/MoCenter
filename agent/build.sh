#!/usr/bin/env bash
# MoCenter Agent 构建脚本 (Linux/macOS)
# 用法: bash build.sh [linux|windows|darwin] [amd64|386|arm64|arm]
#       默认构建当前平台；构建 windows 时输出 .exe
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"

PLATFORM="${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
ARCH="${2:-amd64}"
OUT="mocenter-agent"
GOOS="linux"; GOARCH="${ARCH}"

case "${PLATFORM}" in
  linux)
    GOOS=linux
    case "${ARCH}" in
      amd64|x86_64) GOARCH=amd64 ;;
      arm64|aarch64) GOARCH=arm64 ;;
      arm) GOARCH=arm; export GOARM=7 ;;
      386) GOARCH=386 ;;
      *) GOARCH=amd64 ;;
    esac
    ;;
  windows)
    GOOS=windows
    OUT="mocenter-agent.exe"
    case "${ARCH}" in
      amd64|x86_64) GOARCH=amd64 ;;
      arm64|aarch64) GOARCH=arm64 ;;
      arm) GOARCH=arm; export GOARM=7 ;;
      386) GOARCH=386 ;;
      *) GOARCH=amd64 ;;
    esac
    ;;
  darwin)
    GOOS=darwin
    case "${ARCH}" in
      arm64) GOARCH=arm64 ;;
      amd64) GOARCH=amd64 ;;
      *) GOARCH=amd64 ;;
    esac
    ;;
  *) GOOS=linux GOARCH=amd64 ;;
esac

export GOOS GOARCH CGO_ENABLED=0
echo "[agent] go mod download ..."
go mod download
echo "[agent] building ${OUT} (GOOS=${GOOS} GOARCH=${GOARCH}) ..."
go build -ldflags "-s -w" -o "${OUT}" .
echo "[agent] done -> ./${OUT}"
