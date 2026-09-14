#!/usr/bin/env bash
# 下载 MoCenter 需要的安装包到「服务端托管目录」，供 Agent 直接拉取。
#
# 为什么需要：Agent 一键安装 VNC 时若从公网下载，可能遇到网速慢或被劫持/篡改。
# 把文件放到服务端托管目录后，Agent 会优先通过它与服务端之间既有的长连接拉取
# （该通道可启用 TLS/mTLS），既快又不会被中间人替换。
#
# 用法：
#   bash scripts/fetch-packages.sh [目标目录] [win|linux|all]
#   例：bash scripts/fetch-packages.sh ./packages win
#
# 目标目录需与服务端启动参数一致：
#   - 未指定 -pkg-dir 时，服务端使用 <数据目录>/packages（-data 默认为服务端运行目录）
#   - 指定了 -pkg-dir 时，用该目录
# 下载完成后启动服务端，日志会打印「[pkg] 安装包目录 ... 已托管: ...」以便确认。
#
# 关于 Linux：x11vnc 官方（LibVNC/x11vnc）**只发布源码，release 里没有任何预编译产物**，
# 因此本脚本无法代为下载。Linux 侧的正确做法是：
#   1) 首选：让 Agent 用发行版包管理器安装 —— RHEL/Rocky/Alma 走 EPEL（实测提供 0.9.17），
#      Debian/Ubuntu 官方源直接有 x11vnc；
#   2) 备选：目标机无法访问软件源时，自行构建或自备 x11vnc 二进制，
#      按 x11vnc-linux-<arch> 命名放入本目录，Agent 即会从服务端拉取。
#      例：x11vnc-linux-amd64 / x11vnc-linux-arm64 / x11vnc-linux-arm / x11vnc-linux-386
set -euo pipefail

DEST="${1:-./packages}"
WHICH="${2:-all}"

UVNC_BASE="https://uvnc.eu/download/1800"
# 文件名须与 agent/internal/command/vncinstall_ultravnc_windows.go 的常量一致（有单测保护）
WIN_FILES=(
  "UltraVNC_1824_x64_Setup.exe"
  "UltraVNC_1824_x86_Setup.exe"
)

mkdir -p "$DEST"

if [ "$WHICH" = "all" ] || [ "$WHICH" = "win" ]; then
  for f in "${WIN_FILES[@]}"; do
    if [ -s "$DEST/$f" ]; then
      echo "已存在，跳过: $f"
      continue
    fi
    echo "下载 $f ..."
    curl -fL --retry 3 --connect-timeout 20 -o "$DEST/$f.part" "$UVNC_BASE/$f"
    # 校验确实是 PE 可执行文件（MZ 开头）：既能发现 URL 失效下回的 HTML 错误页，
    # 也能发现内容被中间人替换 —— 与 Agent 侧的魔数校验一致。
    if [ "$(head -c 2 "$DEST/$f.part")" != "MZ" ]; then
      rm -f "$DEST/$f.part"
      echo "错误: $f 的内容不是可执行文件（URL 已失效或被劫持），已丢弃" >&2
      exit 1
    fi
    mv "$DEST/$f.part" "$DEST/$f"
    echo "完成: $f ($(du -h "$DEST/$f" | cut -f1))"
  done
fi

if [ "$WHICH" = "all" ] || [ "$WHICH" = "linux" ]; then
  echo
  echo "关于 Linux（x11vnc）：官方只发布源码、无预编译产物，本脚本不代为下载。"
  echo "  首选：让 Agent 用发行版包管理器安装"
  echo "        RHEL/Rocky/Alma: dnf install -y epel-release && dnf install -y x11vnc"
  echo "        Debian/Ubuntu  : apt-get install -y x11vnc"
  echo "  备选：自行构建/自备二进制，按 x11vnc-linux-<amd64|arm64|arm|386> 命名放入 $DEST"
fi

echo
echo "完成。请确认服务端指向该目录（-pkg-dir \"$DEST\" 或默认 <data>/packages）。"
