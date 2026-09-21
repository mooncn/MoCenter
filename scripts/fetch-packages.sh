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
VDD_URL="https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/25.7.23/VirtualDisplayDriver-x86.Driver.Only.zip"

# 待取包：名称 | 下载地址 | SHA-256
#
# 名称须与 agent/internal/command/vncinstall_common.go 的包名常量一致（Agent 按名索取）；
# 摘要须与同文件的 installPins 一致（Agent 在执行/解压前比对）。
# 摘要来源：UltraVNC 取自 winget-pkgs 官方清单的 InstallerSha256，VDD 为 GitHub Release 直下后计算。
# 校验摘要而不是只看 MZ 头：托管目录里的包随后会在每台 Agent 上以 SYSTEM 执行。
# VDD 驱动包只有「无头（无物理显示器）的 Windows 虚拟机」才用得到，但取回托管目录没有副作用。
FILES=(
  "UltraVNC_1824_x64_Setup.exe|$UVNC_BASE/UltraVNC_1824_x64_Setup.exe|9ef5c8275aa0431f058369305c444d7303adc807a9e4b80bc9ffad7c3bf210b2"
  "UltraVNC_1824_x86_Setup.exe|$UVNC_BASE/UltraVNC_1824_x86_Setup.exe|3354dca22896104c0018bb74fff7028fb5c8e2a497a49e811b529444cf487bfa"
  "VirtualDisplayDriver-x86.Driver.Only.zip|$VDD_URL|e24210692b442b39af763536330ce78b423f19342b7a7792c26de3944e418b3a"
)

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

mkdir -p "$DEST"

if [ "$WHICH" = "all" ] || [ "$WHICH" = "win" ]; then
  for entry in "${FILES[@]}"; do
    IFS='|' read -r f url want <<<"$entry"
    if [ -s "$DEST/$f" ]; then
      echo "已存在，跳过: $f"
      continue
    fi
    echo "下载 $f ..."
    curl -fL --retry 3 --connect-timeout 20 -o "$DEST/$f.part" "$url"
    got="$(sha256_of "$DEST/$f.part")"
    if [ "$got" != "$want" ]; then
      rm -f "$DEST/$f.part"
      echo "错误: $f 摘要不符（期望 $want，实际 $got），已丢弃。" >&2
      echo "      要么下载链路被替换，要么上游已换版本 —— 请核对厂商/winget 清单后再同步" >&2
      echo "      scripts/fetch-packages.sh 与 agent/internal/command/vncinstall_common.go 的钉扎摘要。" >&2
      exit 1
    fi
    # 再确认是可执行安装包/驱动包的正确格式：摘要已足够，这一步只为给出更直白的报错。
    case "$f" in
      *.exe) [ "$(head -c 2 "$DEST/$f.part")" = "MZ" ] || { rm -f "$DEST/$f.part"; echo "错误: $f 不是 PE 可执行文件" >&2; exit 1; } ;;
      *.zip) [ "$(head -c 2 "$DEST/$f.part")" = "PK" ] || { rm -f "$DEST/$f.part"; echo "错误: $f 不是 ZIP 包" >&2; exit 1; } ;;
    esac
    mv "$DEST/$f.part" "$DEST/$f"
    echo "完成: $f ($(du -h "$DEST/$f" | cut -f1)，摘要已核对)"
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
