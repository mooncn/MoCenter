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

# Agent 版本：从 internal/version 读取（唯一事实来源），可用 MOCENTER_AGENT_VERSION 覆盖。
# 它与 Server 版本互相独立，不需要跨端保持一致。
AGENT_VERSION="${MOCENTER_AGENT_VERSION:-$(sed -n 's/^[[:space:]]*\(const\|var\)[[:space:]]\+Version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\2/p' "$(dirname "${BASH_SOURCE[0]}")/internal/version/version.go" 2>/dev/null | head -1)}"
AGENT_VERSION="${AGENT_VERSION:-1.0.3}"

export GOOS GOARCH CGO_ENABLED=0
echo "[agent] go mod download ..."
go mod download
echo "[agent] building ${OUT} (GOOS=${GOOS} GOARCH=${GOARCH}) ..."
go build -ldflags "-s -w -X mocenter/agent/internal/version.Version=${AGENT_VERSION}" -o "${OUT}" .
echo "[agent] done -> ./${OUT}"
