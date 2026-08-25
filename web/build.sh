#!/usr/bin/env bash
# MoCenter Web 前端构建脚本 (Linux/macOS)
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"
echo "[web] npm install ..."
npm install --no-audit --no-fund
echo "[web] vite build ..."
npm run build
echo "[web] done -> ./dist"
