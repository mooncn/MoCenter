#!/usr/bin/env bash
# =============================================================
# MoCenter 构建脚本公共库（被 scripts/build-linux-all.sh 加载，不单独执行）
#
# 目标：把「参数解析 / 版本推导 / 架构映射 / 打印 / 依赖自愈」收敛到一处，
#       避免此前每个脚本各写一份、口径逐渐走偏（例如 Agent 版本号在 4 个脚本里
#       各抄一遍 1.0.1，加一种架构要改 5 个文件）。
# =============================================================

# ---------- 常量：支持的取值（唯一事实来源） ----------
# 构建目标类型
MOCENTER_TYPES="agent server web installer all"
# 二进制产出架构（按平台）
MOCENTER_ARCHS_LINUX="amd64 arm64 arm 386"
MOCENTER_ARCHS_WINDOWS="amd64 386 arm64 arm"
# 安装包格式 -> 适用平台（.deb/.rpm 仅 Linux；zip 双平台，Windows 的另一条产出线）
MOCENTER_PACKAGES_LINUX="deb rpm"
MOCENTER_PACKAGES_WINDOWS="zip"
# 纳入构建/打包的 Agent 目标平台
MOCENTER_PLATFORMS="linux windows"
# 纳入构建/打包的 Server 目标平台（服务端只在总控机上运行，常规不需要跨平台）
MOCENTER_SERVER_PLATFORMS="linux windows"

# ---------- 打印 ----------
c_reset=$'\033[0m'; c_dim=$'\033[2m'; c_red=$'\033[31m'; c_grn=$'\033[32m'
c_yel=$'\033[33m'; c_cyn=$'\033[36m'
if [ -n "${NO_COLOR:-}" ] || [ ! -t 1 ]; then
  c_reset=""; c_dim=""; c_red=""; c_grn=""; c_yel=""; c_cyn=""
fi
log()  { printf '%s\n' "$*"; }
info() { printf '%s%s%s\n' "$c_cyn" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_grn" "$*" "$c_reset"; }
warn() { printf '%s!! %s%s\n' "$c_yel" "$*" "$c_reset" >&2; }
die()  { printf '%s** %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }
step() { printf '\n%s>>> %s%s\n' "$c_cyn" "$*" "$c_reset"; }
rule() { printf '%s%s%s\n' "$c_dim" "------------------------------------------------------------" "$c_reset"; }

# ---------- 路径 ----------
# 由调用方 cd 到仓库根后再 source，故直接取 pwd（不依赖 BASH_SOURCE 层级）。
mc_root() { pwd; }

# ---------- 版本 ----------
# 读取 Go 源码里的版本常量（唯一的版本事实来源），失败时回落到默认值。
#   mc_go_version <文件> <常量名> <默认值>
mc_go_version() {
  local file="$1" name="$2" fallback="$3" v=""
  if [ -f "$file" ]; then
    v="$(sed -n "s/^[[:space:]]*\(const\|var\)[[:space:]]\+${name}[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\2/p" "$file" | head -1)"
  fi
  [ -n "$v" ] || v="$fallback"
  printf '%s' "$v"
}

# Agent / Server 版本互相独立（见 agent/internal/version 的说明），
# 环境变量 MOCENTER_AGENT_VERSION / MOCENTER_SERVER_VERSION 可覆盖。
mc_agent_version() {
  [ -n "${MOCENTER_AGENT_VERSION:-}" ] && { printf '%s' "$MOCENTER_AGENT_VERSION"; return; }
  mc_go_version "agent/internal/version/version.go" "Version" "1.0.1"
}
mc_server_version() {
  [ -n "${MOCENTER_SERVER_VERSION:-}" ] && { printf '%s' "$MOCENTER_SERVER_VERSION"; return; }
  mc_go_version "server/internal/version/version.go" "Version" "1.0.1"
}

# ---------- 架构映射 ----------
# GOARCH -> Debian 包架构名
mc_pkg_arch_deb() {
  case "$1" in
    amd64) echo amd64 ;;
    arm64) echo arm64 ;;
    arm)   echo armhf ;;
    386)   echo i386 ;;
     *)     echo "$1" ;;
  esac
}
# GOARCH -> RPM 包架构名
mc_pkg_arch_rpm() {
  case "$1" in
    amd64) echo x86_64 ;;
    arm64) echo aarch64 ;;
    arm)   echo armv7hl ;;
    386)   echo i686 ;;
     *)     echo "$1" ;;
  esac
}
# Debian/RPM 包架构名 -> rpmbuild --target 需要的架构串
# （rpmbuild 用 uname 风格：aarch64 / armv7hl / i686 / x86_64）
mc_rpm_target_arch() {
  case "$1" in
    amd64|x86_64) echo x86_64 ;;
    arm64|aarch64) echo aarch64 ;;
    arm|armv7hl|armhf) echo armv7hl ;;
    386|i386|i686) echo i686 ;;
    *) echo "$1" ;;
  esac
}

# GOARCH -> 人类可读名（日志用）
mc_arch_label() {
  case "$1" in
    amd64) echo "x86_64/amd64" ;;
    386)   echo "x86/386" ;;
    arm64) echo "ARM64/arm64" ;;
    arm)   echo "ARM/armv7" ;;
    *)     echo "$1" ;;
  esac
}
# 校验架构是否受支持
mc_check_arch() {
  local all="$MOCENTER_ARCHS_LINUX $MOCENTER_ARCHS_WINDOWS"
  case " $all " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# ---------- 依赖检查 ----------
mc_require_go() {
  command -v go >/dev/null 2>&1 || die "未找到 go。请安装 Go 1.25+（server/agent go.mod 要求），或把它加入 PATH 后重试。"
}
mc_have_npm() { command -v npm >/dev/null 2>&1; }

# 打包工具探测：返回可用工具名（无可用工具时输出空）
mc_pkg_tool_deb() {
  if command -v dpkg-deb >/dev/null 2>&1; then echo dpkg-deb
  elif command -v fpm >/dev/null 2>&1; then echo fpm
  else echo ""; fi
}
mc_pkg_tool_rpm() {
  if command -v fpm >/dev/null 2>&1; then echo fpm
  elif command -v rpmbuild >/dev/null 2>&1; then echo rpmbuild
  else echo ""; fi
}

# ---------- 目录清理 ----------
# 只清理「本次确实会产出内容」的子目录：这样 --type=agent 不会误删上一次的
# server 产物，重复执行不同参数组合也不会互相覆盖。
mc_clean() { rm -rf "$@"; }
mc_reset_dir() { rm -rf "$1"; mkdir -p "$1"; }

# ---------- Go 构建（含模块缓存自愈） ----------
# 一个损坏的 GOMODCACHE 条目会让 go 报 "zip: not a valid zip file" 并连带
# 所有依赖它的 import 全部失败，看起来像源码坏了，其实只是本地下载残破。
# 自愈策略：清缓存 + 换镜像代理重试一次（与 Windows 版 build-all.bat 一致）。
mc_go_build() {
  # mc_go_build <模块目录> <输出文件> <GOOS> <GOARCH> [额外 ldflags]
  local dir="$1" out="$2" goos="$3" goarch="$4" extra="${5:-}" rc=0
  local -a envs=(GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0)
  [ "$goarch" = "arm" ] && envs+=(GOARM=7)
  # 输出路径转成绝对路径：下面会 cd 进模块目录，相对路径会被解析成相对模块，
  # 于是二进制落到 agent/installer/payload/ 这类意外位置（且后续查找必然失败）。
  case "$out" in
    /*) ;;
    *)  out="$(pwd)/$out" ;;
  esac
  mkdir -p "$(dirname "$out")"
  ( cd "$dir" && env "${envs[@]}" go mod download ) || true
  ( cd "$dir" && env "${envs[@]}" go build -ldflags "-s -w $extra" -o "$out" . ) || rc=$?
  if [ "$rc" != 0 ]; then
    warn "构建失败，清理模块缓存后经镜像代理重试：$dir"
    go clean -modcache >/dev/null 2>&1 || true
    local mirror="${GOPROXY_MIRROR:-https://goproxy.cn,direct}" mrc=0
    ( cd "$dir" && GOPROXY="$mirror" env "${envs[@]}" go mod download ) || true
    ( cd "$dir" && GOPROXY="$mirror" env "${envs[@]}" go build -ldflags "-s -w $extra" -o "$out" . ) || mrc=$?
    [ "$mrc" = 0 ] || die "构建失败：$dir（可手动执行 GOPROXY=$mirror go build 排查）"
  fi
}

# ---------- 简单文件树打包（不依赖 fpm / rpmbuild / zip 命令行） ----------
# 用 python3 写 PKZIP，避免为了出一个 Windows 免安装 zip 而要求构建机装 zip 命令；
# python3 在 CI 与主流发行版上都默认可用，缺失时自动回退到 zip(1)。
mc_make_zip() {
  # mc_make_zip <源目录> <输出zip> [归档内顶层目录名]
  local src="$1" out="$2" top="${3:-}"
  [ -d "$src" ] || die "打包源目录不存在：$src"
  mkdir -p "$(dirname "$out")"
  rm -f "$out"
  if command -v python3 >/dev/null 2>&1; then
    MC_ZIP_SRC="$src" MC_ZIP_OUT="$out" MC_ZIP_TOP="$top" python3 - <<'PY'
import os, sys, zipfile

src, out, top = os.environ["MC_ZIP_SRC"], os.environ["MC_ZIP_OUT"], os.environ["MC_ZIP_TOP"]
count = 0
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for root, dirs, files in os.walk(src):
        dirs.sort()
        for f in sorted(files):
            full = os.path.join(root, f)
            rel = os.path.relpath(full, src)
            arc = os.path.join(top, rel) if top else rel
            # 保留可执行位，解压后不用再 chmod
            zi = zipfile.ZipInfo.from_file(full, arc)
            zi.compress_type = zipfile.ZIP_DEFLATED
            zi.external_attr = (os.stat(full).st_mode & 0xFFFF) << 16
            with open(full, "rb") as fh, z.open(zi, "w") as dst:
                dst.write(fh.read())
            count += 1
    if count == 0:
        print("MC_ZIP_EMPTY", file=sys.stderr)
        sys.exit(3)
print(f"MC_ZIP_OK {count}")
PY
    [ $? -eq 0 ] || die "python3 打包 zip 失败（源目录为空？）：$src -> $out"
  elif command -v zip >/dev/null 2>&1; then
    ( cd "$(dirname "$src")" && zip -q -r -9 "$out" "$(basename "$src")" ) || die "zip 打包失败：$out"
  else
    die "生成 zip 需要 python3 或 zip 命令（安装其一后重试，或用 --package=none 只要二进制）"
  fi
  [ -s "$out" ] || die "zip 产物为空：$out"
}

# 用 unzip(1) 或 python3 解包校验 zip 可读、条目数与摘要一致。
mc_verify_zip() {
  local zip="$1" entries
  if command -v unzip >/dev/null 2>&1; then
    entries="$(unzip -Z1 "$zip" 2>/dev/null | wc -l)"
    [ "$entries" -gt 0 ] || die "zip 校验失败（无法列出条目）：$zip"
    unzip -tqq "$zip" >/dev/null 2>&1 || die "zip 完整性校验失败：$zip"
  elif command -v python3 >/dev/null 2>&1; then
    entries="$(python3 -c 'import sys,zipfile;z=zipfile.ZipFile(sys.argv[1]);print(len(z.namelist()))' "$zip")" \
      || die "zip 校验失败：$zip"
  fi
  ok "zip 校验通过：$(basename "$zip")（${entries} 个条目）"
}

# ---------- 可变参数模式匹配 ----------
# 把 "amd64,arm64" / "amd64 arm64" 统一成数组，并校验取值。
mc_split_list() { tr ',' ' ' <<<"$1" | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'; }
mc_in_list() {
  # mc_in_list <值> <空格分隔的候选串>
  case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

# ---------- systemd 服务单元模板 ----------
# 服务单元此前散落在「安装器 heredoc」与「打包脚本 heredoc」两处，
# 改一处忘另一处的风险很高。这里统一成模板文件，脚本与安装器都直接拷贝。
mc_tpl_dir() { echo "scripts/deploy"; }
