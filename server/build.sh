#!/usr/bin/env bash
# MoCenter Server 构建脚本 (Linux/macOS)
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"
echo "[server] go mod download ..."
go mod download
echo "[server] building mocenter-server ..."
go build -ldflags "-s -w" -o mocenter-server .
echo "[server] done -> ./mocenter-server"
