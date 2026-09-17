#!/usr/bin/env bash
# =============================================================
# MoCenter 统一构建/打包脚本（Linux / macOS 构建机）
#
# 一个入口覆盖：Server / Agent / Web 前端 / Windows 图形安装程序的构建，
# 以及 .deb / .rpm / zip 产物的生成，并按平台分类落到 outdir。
#
# 用法:
#   bash scripts/build-linux-all.sh [选项]
#   常用:
#     bash scripts/build-linux-all.sh                        # 默认：全部类型 + 全部平台 + 全部架构 + 全部包
#     bash scripts/build-linux-all.sh --type=server          # 只出服务端
#     bash scripts/build-linux-all.sh --type=agent --arch=amd64,arm64
#     bash scripts/build-linux-all.sh --package=deb --arch=amd64 --no-binary   # 只要安装包
#     bash scripts/build-linux-all.sh --package=zip --platform=windows         # Windows 免安装 zip
#     bash scripts/build-linux-all.sh --type=none --list     # 只列出可选值
#
# 选项:
#   --type=     agent|server|web|installer|none|all      默认 all（可以逗号组合）
#   --package=  deb|rpm|zip|none|all                     默认 all（按平台过滤：deb/rpm→Linux，zip→Windows）
#   --platform= linux|windows|all                        默认 all（决定产出哪个平台的产物）
#   --arch=     amd64|386|arm64|arm|all                  默认 all
#               （arm 仅 Linux 侧有效；windows/arm 不是 Go 支持的目标，会被自动过滤）
#   --binary=   1|0                                      默认 1，是否输出二进制
#   --web=      1|0|auto                                 默认 auto（有 npm 才构建；type 含 web 时强制 1）
#   --outdir=   目录                                      默认 <项目目录>/build
#   --version=  服务端版本（同 MOCENTER_SERVER_VERSION）   默认读 server/internal/version
#   --agent-version= Agent 版本（同 MOCENTER_AGENT_VERSION）默认读 agent/internal/version
#   --jobs=     N                                        默认 CPU 核数，并行构建/打包的并发度
#   -j N                                                  --jobs 的简写
#   --clean                                               构建前清空 outdir（默认只清理本次涉及的子目录）
#   --list                                                打印支持的取值后退出
#   --help                                                显示本帮助
#
# 产物布局（outdir 下先按平台分类，再按用途分类）:
#   build/linux/binaries/server/linux/mocenter-server-<arch>
#   build/linux/binaries/agent/<linux|windows>/mocenter-agent-<arch>[.exe]
#   build/linux/web/                        Web 前端静态资源
#   build/linux/packages/{deb,rpm}/         .deb / .rpm 安装包
#   build/linux/installer/                  Windows 图形安装程序（在 Linux 上构建时）
#   build/windows/binaries/.../             Windows 免安装 zip 的原始素材
#   build/windows/packages/zip/             Windows 免安装 zip
#   build/windows/installer/                Windows 图形安装程序
#
# 环境变量（与命令行等价，命令行优先）:
#   MOCENTER_SERVER_VERSION / MOCENTER_AGENT_VERSION / MOCENTER_VERSION(兼容旧名)
#   DEBEMAIL / DEBFULLNAME / MOCENTER_PKG_MAINTAINER   .deb 维护者信息（dpkg-deb 分支需要）
#   MOCENTER_DEB_DEPENDS / MOCENTER_RPM_DEPENDS        包依赖声明覆盖
#   MOCENTER_RPM_DEBUGINFO=1                           允许随包产出 -debuginfo 子包
#                                                      （默认 0，见 build_rpm_with_rpmbuild 注释）
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}/.."                      # 统一以仓库根为工作目录
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

# ---------------------------------------------------------------- 默认参数
OPT_TYPE="all"
OPT_PACKAGE="all"
OPT_PLATFORM="all"
OPT_ARCH="all"
OPT_BINARY=1
OPT_WEB="auto"
OPT_OUTDIR="$(mc_root)/build"
OPT_CLEAN=0
OPT_JOBS="$( (nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4) )"
OPT_AGENT_VERSION="$(mc_agent_version)"
OPT_SERVER_VERSION="$(mc_server_version)"

usage() { sed -n '2,60p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

# ---------------------------------------------------------------- 参数解析
# 支持 --key=value 与 --key value 两种写法，兼容 `-j N`。
parse_args() {
  local args=("$@") i=0 a val
  while [ $i -lt ${#args[@]} ]; do
    a="${args[$i]}"
    case "$a" in
      --help|-h) usage ;;
      --list) mc_list_values; exit 0 ;;
      -j) i=$((i+1)); OPT_JOBS="${args[$i]:-}" ;;
      -j*) OPT_JOBS="${a#-j}" ;;
      --*=*) val="${a#*=}"; a="${a%%=*}"; set_opt "$a" "$val" ;;
      --*)
        # 不带 = 的形式：下一个参数作为值（布尔开关除外）
        if mc_in_list "${a#--}" "clean list help"; then set_opt "$a" ""; else
          i=$((i+1)); set_opt "$a" "${args[$i]:-}"
        fi ;;
      *) die "未知参数：$a（用 --help 查看用法）" ;;
    esac
    i=$((i+1))
  done
}

set_opt() {
  local k="${1#--}" v="${2:-}"
  case "$k" in
    type)          OPT_TYPE="$v" ;;
    package|packages) OPT_PACKAGE="$v" ;;
    platform)      OPT_PLATFORM="$v" ;;
    arch|archs)    OPT_ARCH="$v" ;;
    binary)        OPT_BINARY="$v" ;;
    web)           OPT_WEB="$v" ;;
    outdir)        OPT_OUTDIR="$v" ;;
    version)       OPT_SERVER_VERSION="$v" ;;
    agent-version) OPT_AGENT_VERSION="$v" ;;
    jobs)          OPT_JOBS="$v" ;;
    clean)         OPT_CLEAN=1 ;;
    list)          mc_list_values; exit 0 ;;
    help)          usage ;;
    *) die "未知选项：--$k（用 --help 查看用法）" ;;
  esac
}

mc_list_values() {
  cat <<'EOF'
可选取值（脚本会校验，填错会直接报错并列出可选值）:

  --type       agent | server | web | installer | none | all
  --package    deb | rpm | zip | none | all
  --platform   linux | windows | all
  --arch       amd64 | 386 | arm64 | arm | all
               （arm 仅 Linux；windows/arm 不受 Go 支持，构建时按平台自动过滤）
  --binary     1 | 0
  --web        1 | 0 | auto

平台与打包格式的对应关系（其余组合会被自动过滤，不会报错）:
  linux    -> deb / rpm    （Windows 图形安装程序只在 --platform=windows 下构建）
  windows  -> zip          （Linux 构建机上为交叉编译产物，不含 .deb/.rpm）
EOF
}

# ---------------------------------------------------------------- 参数校验与展开
expand_lists() {
  # ---- type ----
  local types
  if [ "$OPT_TYPE" = "all" ]; then types="$MOCENTER_TYPES"; else types="$(mc_split_list "$OPT_TYPE")"; fi
  TYPES=()
  for t in $types; do
    [ "$t" = "none" ] && continue
    mc_in_list "$t" "$MOCENTER_TYPES" || die "--type=$t 不支持，可选：$(echo "$MOCENTER_TYPES" | tr ' ' '|')"
    TYPES+=("$t")
  done

  # ---- platform ----
  if [ "$OPT_PLATFORM" = "all" ]; then PLATFORMS="$MOCENTER_PLATFORMS"
  else
    PLATFORMS="$(mc_split_list "$OPT_PLATFORM")"
    for p in $PLATFORMS; do
      mc_in_list "$p" "$MOCENTER_PLATFORMS" || die "--platform=$p 不支持，可选：$(echo "$MOCENTER_PLATFORMS" | tr ' ' '|')|all"
    done
  fi

  # ---- package ----
  if [ "$OPT_PACKAGE" = "all" ]; then PKGS="$(echo "$MOCENTER_PACKAGES_LINUX $MOCENTER_PACKAGES_WINDOWS")"
  elif [ "$OPT_PACKAGE" = "none" ]; then PKGS=""
  else
    PKGS="$(mc_split_list "$OPT_PACKAGE")"
    local all="$(echo "$MOCENTER_PACKAGES_LINUX $MOCENTER_PACKAGES_WINDOWS")"
    for p in $PKGS; do
      mc_in_list "$p" "$all" || die "--package=$p 不支持，可选：deb|rpm|zip|none|all"
    done
  fi

  # ---- arch ----
  if [ "$OPT_ARCH" = "all" ]; then ARCHS="$(echo "$MOCENTER_ARCHS_LINUX $MOCENTER_ARCHS_WINDOWS" | tr ' ' '\n' | awk '!seen[$0]++' | tr '\n' ' ')"
  else
    ARCHS="$(mc_split_list "$OPT_ARCH")"
    for a in $ARCHS; do
      mc_check_arch "$a" || die "--arch=$a 不支持，可选：amd64|386|arm64|arm|all"
    done
  fi

  # ---- binary / web ----
  case "$OPT_BINARY" in 1|0) ;; *) die "--binary=$OPT_BINARY 不支持，可选：1|0" ;; esac
  case "$OPT_WEB" in 1|0|auto) ;; *) die "--web=$OPT_WEB 不支持，可选：1|0|auto" ;; esac
  [ -n "$OPT_JOBS" ] && [ "$OPT_JOBS" -ge 1 ] 2>/dev/null || die "--jobs 需要是 >=1 的整数"

  # ---- type=web 时强制构建前端（否则该参数没有意义） ----
  if mc_in_list "web" "${TYPES[*]:-}"; then OPT_WEB=1; fi
}

# ---------------------------------------------------------------- 输出目录
setup_outdir() {
  case "$OPT_OUTDIR" in
    /*) OUT="$OPT_OUTDIR" ;;
    ./*|../*) OUT="$(mc_root)/${OPT_OUTDIR#./}" ;;
    *) OUT="$(mc_root)/$OPT_OUTDIR" ;;
  esac
  mkdir -p "$OUT"

  if [ "$OPT_CLEAN" = 1 ]; then
    info "  清理输出目录：$OUT（--clean）"
    rm -rf "$OUT"; mkdir -p "$OUT"
    return 0
  fi

  # 清理粒度：只删掉「本次确实会重新生成」的目录/文件。
  # 之前是按 (平台, 类型) 整目录清理，跑 --type=agent 会把同平台下 server 的
  # 产物一起删掉 —— 参数组合之间会互相破坏，不符合"可重复执行"的预期。
  local p a role path n
  # 1) 二进制：精确到文件。
  #    注意 --binary=0 时必须**保留**已有二进制 —— 它正是"复用已有产物只做打包"
  #    这一用法的前提，清掉就等于把打包的输入删了。
  if [ "$OPT_BINARY" = 1 ]; then
    for p in $PLATFORMS; do
      for role in server agent; do
        mc_in_list "$role" "${TYPES[*]:-}" || continue
        for a in $ARCHS; do
          n="mocenter-${role}-${a}"; [ "$p" = "windows" ] && n="${n}.exe"
          rm -f "$OUT/$p/binaries/$role/$p/$n"
        done
        rmdir "$OUT/$p/binaries/$role/$p" "$OUT/$p/binaries/$role" "$OUT/$p/binaries" 2>/dev/null || true
      done
    done
  else
    info "  --binary=0：保留已有二进制（复用为打包输入）"
  fi
  # 2) Web 前端
  if mc_in_list "web" "${TYPES[*]:-}"; then
    for p in $PLATFORMS; do rm -rf "$OUT/$p/web"; done
  fi
  # 3) 安装包：只清本次要产出的格式
  if mc_in_list "deb" "$PKGS" && mc_in_list "linux" "$PLATFORMS"; then rm -rf "$OUT/linux/packages/deb"; fi
  if mc_in_list "rpm" "$PKGS" && mc_in_list "linux" "$PLATFORMS"; then rm -rf "$OUT/linux/packages/rpm"; fi
  if mc_in_list "zip" "$PKGS" && mc_in_list "windows" "$PLATFORMS"; then rm -rf "$OUT/windows/packages/zip"; fi
  # 4) 安装程序
  if mc_in_list "installer" "${TYPES[*]:-}" && mc_in_list "windows" "$PLATFORMS"; then
    rm -rf "$OUT/windows/installer"
  fi
  # 5) 上次异常中断遗留的中间目录
  find "$OUT" -maxdepth 4 -name '.stage-*' -o -maxdepth 4 -name '.rpmbuild' 2>/dev/null | while read -r d; do rm -rf "$d"; done
}

# ---------------------------------------------------------------- 构建：Go 二进制
# 统一产物路径：<out>/<platform>/binaries/{server,agent}/{linux,windows}/<name>-<arch>[.exe]
build_go() {
  local mod="$1" plat="$2" arch="$3" out="$4" ldflags="$5"
  mc_go_build "$mod" "$out" "$plat" "$arch" "$ldflags"
  local size; size="$(du -h "$out" | cut -f1)"
  ok "$(basename "$out")  ($(mc_arch_label "$arch"), ${size})"
}

build_binaries() {
  local do_server=0 do_agent=0 do_installer=0
  mc_in_list "server" "${TYPES[*]:-}" && do_server=1
  mc_in_list "agent" "${TYPES[*]:-}" && do_agent=1
  mc_in_list "installer" "${TYPES[*]:-}" && do_installer=1
  [ "$OPT_BINARY" = 1 ] || { [ "$do_server$do_agent$do_installer" != "000" ] && info "  已指定 --binary=0，跳过二进制构建"; return 0; }

  # Server：只在能原生/交叉编译的平台上产出；.deb/.rpm 需要 linux 版二进制，
  # 因此打包 linux 包时会隐式补齐 linux 二进制（即使 --platform 没写 linux）。
  local server_plats="" p
  for p in $PLATFORMS; do
    mc_in_list "$p" "$MOCENTER_SERVER_PLATFORMS" && server_plats="$server_plats $p"
  done

  if [ "$do_server" = 1 ]; then
    for p in $server_plats; do
      local port_archs="$ARCHS"
      # windows server 只支持 amd64/386（Windows on ARM 上服务端为可选形态，暂不产出）
      [ "$p" = "windows" ] && port_archs="$(echo "$port_archs" | tr ' ' '\n' | grep -Ex 'amd64|386' | tr '\n' ' ')"
      [ -n "$port_archs" ] || continue
      step "构建 Server 二进制（$p，版本 ${OPT_SERVER_VERSION}）"
      local out_dir="$OUT/$p/binaries/server/$p"
      local outs=()
      for a in $port_archs; do
        local name="mocenter-server-${a}"; [ "$p" = "windows" ] && name="${name}.exe"
        outs+=("$out_dir/$name")
      done
      run_go_batch server "$p" "$port_archs" "" "${outs[@]}"
    done
  fi

  if [ "$do_agent" = 1 ]; then
    for p in $PLATFORMS; do
      # --arch=all 取的是"两个平台架构的并集"，但并非每个架构在两边都受支持：
      # windows/arm（32 位 ARM）不是 Go 支持的目标（broken port，go.dev/issue/68552），
      # 按并集直接构建会报 "go: unsupported GOOS/GOARCH pair windows/arm" 并中断整批。
      # 因此这里按平台取交集，只构建该平台真正支持的架构。
      local archs; archs="$(mc_platform_archs_filtered "$p" "$ARCHS")"
      [ -n "$archs" ] || { info "  $p 没有任何可构建架构（请求：$ARCHS），跳过"; continue; }
      step "构建 Agent 二进制（$p，版本 ${OPT_AGENT_VERSION}）"
      local out_dir="$OUT/$p/binaries/agent/$p"
      local outs=()
      for a in $archs; do
        local name="mocenter-agent-${a}"; [ "$p" = "windows" ] && name="${name}.exe"
        outs+=("$out_dir/$name")
      done
      run_go_batch agent "$p" "$archs" "-X mocenter/agent/internal/version.Version=${OPT_AGENT_VERSION}" "${outs[@]}"
    done
  fi

  # Windows 图形安装程序：只能在 Windows 构建机上编译（ISCC 是 Windows 程序），
  # 但 --type=installer 在 Linux 上会准备 installer/payload 素材并给出指引。
  if [ "$do_installer" = 1 ]; then
    step "准备 Windows 图形安装程序素材（installer/payload）"
    if ! mc_in_list "windows" "$PLATFORMS"; then
      info "  已指定 --platform 不含 windows，跳过"
    else
      prepare_installer_payload
    fi
  fi
}

# 并行执行构建任务：<函数> <平台> <空格分隔架构> <空格分隔输出文件...>
# 串行（而非 |xargs -P 批量）派发构建：Go 自身会用满所有核，
# 再叠加一层并发只会互相抢 CPU 并让日志交错难读。--jobs 作为上限保留给未来扩展。
run_go_batch() {
  # run_go_batch <模块> <平台> <架构列表> <ldflags> <输出文件列表...>
  local mod="$1" plat="$2" archs="$3" ld="$4"; shift 4
  local outs=("$@") i=0 a
  for a in $archs; do
    local out="${outs[$i]}"; i=$((i+1))
    build_go "$mod" "$plat" "$a" "$out" "$ld"
  done
}

# ---------------------------------------------------------------- 构建：Web
build_web() {
  mc_in_list "web" "${TYPES[*]:-}" || return 0
  step "构建 Web 前端"
  local need=0
  case "$OPT_WEB" in
    1) need=1 ;;
    0) info "  已指定 --web=0，跳过"; return 0 ;;
    auto) [ "$OPT_CLEAN" = 1 ] && need=1 || need=0 ;;   # 默认复用 web/dist
  esac

  if [ "$need" = 0 ] && [ -f web/dist/index.html ]; then
    ok "复用已有 web/dist（如需强制重建：--web=1 或 --clean）"
  else
    if ! mc_have_npm; then
      if mc_in_list "web" "${TYPES[*]:-}"; then
        die "未找到 npm，但本次 --type 含 web。请安装 Node.js 18+，或改用 --type 不含 web（服务端将不托管页面）。"
      fi
      warn "未安装 Node.js/npm，跳过 Web 构建"
      return 0
    fi
    ( cd web && npm install --no-audit --no-fund && npm run build )
  fi

  [ -f web/dist/index.html ] || die "web/dist/index.html 不存在，前端构建未产出可用静态资源"
  local dest
  for p in $PLATFORMS; do
    dest="$OUT/$p/web"
    rm -rf "$dest"; mkdir -p "$dest"
    cp -r web/dist/. "$dest/"
    ok "Web 前端 -> $dest"
  done
}

# ---------------------------------------------------------------- Windows 安装程序素材
prepare_installer_payload() {
  command -v go >/dev/null 2>&1 || die "未找到 go，无法准备安装程序素材"
  local payload="installer/payload"
  mkdir -p "$payload"
  local ld="-X mocenter/agent/internal/version.Version=${OPT_AGENT_VERSION}"
  build_go agent  windows amd64 "$payload/mocenter-agent.exe"  "$ld" >/dev/null
  build_go server windows amd64 "$payload/mocenter-server.exe" "" >/dev/null
  build_go installer windows amd64 "$payload/mocenter-deploy.exe" "" >/dev/null
  ok "mocenter-agent.exe / mocenter-server.exe / mocenter-deploy.exe -> $payload"
  if [ -f web/dist/index.html ]; then
    rm -rf "$payload/web"; mkdir -p "$payload/web"; cp -r web/dist/. "$payload/web/"
    ok "Web 前端 -> $payload/web"
  else
    warn "未发现 web/dist，安装包将不含 Web 界面（可加 --web=1 一并构建）"
  fi
  rule
  warn "Windows 图形安装程序需要在 Windows 上编译（ISCC 为 Windows 程序）："
  log  "      scripts\\build-all.bat --type=installer      # 已含素材准备与 ISCC 调用"
  log  "    cross build 只能到这里：素材已就位，拷到 Windows 执行上面一行即可。"
}

# ---------------------------------------------------------------- 打包：Linux .deb/.rpm
# 组装安装包文件树（与旧 build-linux-pkg.sh / build-linux-server-pkg.sh 完全一致）
stage_linux_pkg() {
  # stage_linux_pkg <role> <arch> <stage目录>
  local role="$1" arch="$2" stage="$3" bin="$4"
  mc_reset_dir "$stage"
  case "$role" in
    agent)
      mkdir -p "$stage/usr/local/bin" "$stage/etc/mocenter" "$stage/lib/systemd/system"
      install -m 0755 "$bin" "$stage/usr/local/bin/mocenter-agent"
      cp scripts/deploy/mocenter-agent.conf  "$stage/etc/mocenter/mocenter-agent.conf"
      cp scripts/deploy/mocenter-agent.service "$stage/lib/systemd/system/mocenter-agent.service"
      ;;
    server)
      mkdir -p "$stage/usr/local/bin" "$stage/etc/mocenter" "$stage/lib/systemd/system" \
               "$stage/var/lib/mocenter/data" "$stage/var/lib/mocenter/packages" "$stage/usr/share/mocenter"
      install -m 0755 "$bin" "$stage/usr/local/bin/mocenter-server"
      cp scripts/deploy/mocenter-server.conf "$stage/etc/mocenter/mocenter-server.conf"
      cp scripts/deploy/mocenter-server.service "$stage/lib/systemd/system/mocenter-server.service"
      if [ -f web/dist/index.html ]; then
        mkdir -p "$stage/usr/share/mocenter/web"
        cp -r web/dist/. "$stage/usr/share/mocenter/web/"
      else
        rmdir "$stage/usr/share/mocenter" 2>/dev/null || true
      fi
      cat > "$stage/etc/mocenter/README.server" <<'EOF'
MoCenter Server 安装说明
========================

1. 修改配置：/etc/mocenter/mocenter-server.conf
   至少修改 MOCENTER_PASSWORD（默认 change-me 仅为占位）。
   所有可调参数（TLS、白名单、TOTP、Token 有效期等）见启动后自动生成的
   /etc/mocenter/mocenter-server.json。

2. 启动服务：systemctl enable --now mocenter-server
   查看日志：journalctl -u mocenter-server -f

3. 端口：:9000 供 Agent 长连接，:8080 供 Web 控制台 / REST API。
   请按需放行防火墙；生产环境请配置 TLS 证书（MOCENTER_TLS_*）并限制来源网段。

4. Agent 注册密钥：首次启动生成 /var/lib/mocenter/data/agent.key（权限 0600）。
   分发给被控机时请走安全通道。

5. 卸载：systemctl disable --now mocenter-server
   配置与数据默认保留（/etc/mocenter、/var/lib/mocenter）。
EOF
      ;;
    *) die "未知打包角色：$role" ;;
  esac

  # 安装脚本（postinst / prerm），两种包格式共用
  cat > "$stage/postinst.sh" <<EOF
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable mocenter-${role}.service
  systemctl restart mocenter-${role}.service || true
fi
exit 0
EOF
  cat > "$stage/prerm.sh" <<EOF
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop mocenter-${role}.service 2>/dev/null || true
  systemctl disable mocenter-${role}.service 2>/dev/null || true
fi
exit 0
EOF
  chmod +x "$stage/postinst.sh" "$stage/prerm.sh"
  # systemd 单元里引用的目录必须存在，否则服务端会往 root 工作目录写数据
  [ "$role" = "server" ] || true
}

pkg_depends_deb() {
  case "$1" in
    agent)  echo "${MOCENTER_DEB_DEPENDS:-libc6 (>= 2.17)}" ;;
    server) echo "${MOCENTER_DEB_DEPENDS:-libc6 (>= 2.17)}" ;;
  esac
}

build_deb() {
  # build_deb <文件树> <角色> <架构>
  local stage="$1" role="$2" arch="$3" pkg="mocenter-${role}"
  local out="$OUT/linux/packages/deb/${pkg}_${OPT_SERVER_VERSION}_$(mc_pkg_arch_deb "$arch")_${arch}.deb"
  local tool; tool="$(mc_pkg_tool_deb)"
  [ -n "$tool" ] || { warn "缺少 dpkg-deb 与 fpm，跳过 .deb（$pkg/$arch）"; return 1; }
  local pkgarch; pkgarch="$(mc_pkg_arch_deb "$arch")"
  mkdir -p "$(dirname "$out")"
  if [ "$tool" = "dpkg-deb" ]; then
    local d="$stage-deb"
    rm -rf "$d"; mkdir -p "$d/DEBIAN"
    ( cd "$stage" && find . -type d -exec mkdir -p "$d/{}" \; )
    ( cd "$stage" && find . -type f ! -name 'postinst.sh' ! -name 'prerm.sh' \
        -exec cp --parents {} "$d/" \; )
    cat > "$d/DEBIAN/control" <<EOF
Package: ${pkg}
Version: ${OPT_SERVER_VERSION}
Section: net
Priority: optional
Architecture: ${pkgarch}
Depends: $(pkg_depends_deb "$role")
Maintainer: ${MOCENTER_PKG_MAINTAINER:-MoCenter <admin@example.com>}
Description: $(deb_description "$role" | head -1)
$(deb_description "$role" | tail -n +2 | sed 's/^/ /')
EOF
    cp "$stage/postinst.sh" "$d/DEBIAN/postinst"
    cp "$stage/prerm.sh"    "$d/DEBIAN/prerm"
    chmod 0755 "$d/DEBIAN/postinst" "$d/DEBIAN/prerm" "$d/usr/local/bin/mocenter-${role}"
    # dpkg-deb 在没有 DEBEMAIL/DEBFULLNAME 时会打印噪声警告，这里显式提供
    DEBEMAIL="${DEBEMAIL:-admin@example.com}" DEBFULLNAME="${DEBFULLNAME:-MoCenter}" \
      dpkg-deb --build "$d" "$out" >/dev/null
    rm -rf "$d"
  else
    fpm -s dir -t deb -n "$pkg" -v "$OPT_SERVER_VERSION" -a "$pkgarch" \
      --depends "$(pkg_depends_deb "$role")" \
      --after-install "$stage/postinst.sh" --before-remove "$stage/prerm.sh" \
      --config-files "/etc/mocenter/mocenter-${role}.conf" \
      -C "$stage" -p "$out" . >/dev/null
  fi
  [ -f "$out" ] || { warn "deb 未产出：$out"; return 1; }
  ok "$(basename "$out")  ($(du -h "$out" | cut -f1))"
  return 0
}

deb_description() {
  case "$1" in
    agent)  printf 'MoCenter Agent - 集中式主机远控管理被控端\n自动注册 systemd 服务并设置开机自启。\n' ;;
    server) printf 'MoCenter Server - 集中式主机远控管理总控服务端\n提供 Agent 长连接接入、命令路由、探针聚合、端口映射与 Web 控制台。\n自动注册 systemd 服务并设置开机自启。\n' ;;
  esac
}

rpm_description() {
  case "$1" in
    agent)  printf 'MoCenter Agent - remote management client' ;;
    server) printf 'MoCenter Server - centralized control server' ;;
  esac
}

build_rpm() {
  local stage="$1" role="$2" arch="$3" pkg="mocenter-${role}"
  local out="$OUT/linux/packages/rpm/${pkg}-${OPT_SERVER_VERSION}-1.$(mc_pkg_arch_rpm "$arch").rpm"
  local tool; tool="$(mc_pkg_tool_rpm)"
  [ -n "$tool" ] || { warn "缺少 fpm 与 rpmbuild，跳过 .rpm（$pkg/$arch）"; return 1; }
  local pkgarch; pkgarch="$(mc_pkg_arch_rpm "$arch")"
  mkdir -p "$(dirname "$out")"
  if [ "$tool" = "rpmbuild" ]; then
    build_rpm_with_rpmbuild "$stage" "$role" "$pkgarch" "$out"
  else
    fpm -s dir -t rpm -n "$pkg" -v "$OPT_SERVER_VERSION" -a "$pkgarch" \
      --after-install "$stage/postinst.sh" --before-remove "$stage/prerm.sh" \
      --config-files "/etc/mocenter/mocenter-${role}.conf" \
      -C "$stage" -p "$out" . >/dev/null
  fi
  ok "$(basename "$out")  ($(du -h "$out" | cut -f1))"
  return 0
}

# 用 rpmbuild 直接组包（不依赖 fpm，便于在只装了 rpm 工具的镜像里出包）
build_rpm_with_rpmbuild() {
  local stage="$1" role="$2" pkgarch="$3" out="$4"
  local pkg="mocenter-${role}" top="$OUT/linux/packages/.rpmbuild"
  rm -rf "$top"
  mkdir -p "$top"/{BUILD,RPMS,SOURCES,SPECS,SRPMS,BUILDROOT}
  # 把 stage 的文件树打成一个 tar 作为 source，再写 spec 指定安装路径
  local tarname="${pkg}-${OPT_SERVER_VERSION}"
  ( cd "$stage" && tar --exclude=postinst.sh --exclude=prerm.sh \
      -czf "$top/SOURCES/${tarname}.tar.gz" . )

  local dirs="" files=""
  while IFS= read -r d; do dirs="$dirs%dir /$d"$'\n'; done < <(cd "$stage" && find . -type d ! -name '.' | sed 's|^\./||')
  while IFS= read -r f; do
    case "$(basename "$f")" in postinst.sh|prerm.sh) continue ;; esac
    files="$files/$f"$'\n'
  done < <(cd "$stage" && find . -type f | sed 's|^\./||')

  cat > "$top/SPECS/${pkg}.spec" <<EOF
Name:           ${pkg}
Version:        ${OPT_SERVER_VERSION}
Release:        1
Summary:        $(rpm_description "$role")
License:        Proprietary
AutoReqProv:    no
Requires:       ${MOCENTER_RPM_DEPENDS:-glibc}

%description
$(rpm_description "$role")

%prep
%setup -q -c -T
tar -xzf %{_sourcedir}/${tarname}.tar.gz

%install
mkdir -p %{buildroot}
cp -a . %{buildroot}/

%post
%{buildroot}/../etc/mocenter/README.server >/dev/null 2>&1 || true
if [ -x "%{_prefix}/../$(basename %{buildroot})" ]; then :; fi
%{buildroot}/lib/systemd/system/mocenter-${role}.service >/dev/null 2>&1 || true

%files
${dirs}${files}
EOF

  # %post / %preun 用包内脚本（rpmbuild 通过 spec 内的脚本段执行）
  cat > "$top/SPECS/post.sh" <<EOF
#!/bin/sh
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable mocenter-${role}.service
  systemctl restart mocenter-${role}.service || true
fi
exit 0
EOF
  cat > "$top/SPECS/preun.sh" <<EOF
#!/bin/sh
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop mocenter-${role}.service 2>/dev/null || true
  systemctl disable mocenter-${role}.service 2>/dev/null || true
fi
exit 0
EOF
  # 把 %post/%preun 换成脚本内容（spec 里直接内联，避免额外机制）
  python3 - "$top/SPECS/${pkg}.spec" <<'PY'
import sys, re
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
post = open(p.rsplit('/', 2)[0] + '/SPECS/post.sh', encoding='utf-8').read().split('\n', 1)[1]
preun = open(p.rsplit('/', 2)[0] + '/SPECS/preun.sh', encoding='utf-8').read().split('\n', 1)[1]
s = re.sub(r"%post\n.*?\n\n%files", "%post\n" + post + "\n%preun\n" + preun + "\n%files", s, flags=re.S)
open(p, "w", encoding="utf-8").write(s)
PY
  rm -f "$top/SPECS/post.sh" "$top/SPECS/preun.sh"

  # rpmbuild 默认只允许构建「本机架构」；交叉出任意外架构必须用 --target 声明，
  # 否则报 "No compatible architectures found for build"。
  # rpm_arch_of_target 把 Go 的 arm64/arm/386 映射成 rpm 认识的 aarch64/armv7hl/i686。
  local rpmmacro_arch; rpmmacro_arch="$(mc_rpm_target_arch "$pkgarch")"
  # ── 关闭 debuginfo 子包（否则整批 .rpm 必失败）────────────────────────
  # 发行版的 rpm 默认已定义 debug_package 宏（rpm>=4.14 中该宏自带
  # %ifnarch noarch，Fedora/CentOS/openSUSE 均默认生效）。它会让 rpmbuild 多出
  # 一个 -debuginfo 子包，其文件清单来自 %__debug_install_post 调用的
  # find-debuginfo 所生成的 debugfiles.list。
  #
  # 而 find-debuginfo 只把「file 判定为 ELF 且 not stripped」的可执行文件写进清单：
  #     find "$RPM_BUILD_ROOT" -perm -0111 -type f | file -N -f - |
  #       sed -n 's/^\(.*\):.*ELF.*, not stripped.*/\1/p'
  # 本项目的二进制在构建期（--binary=1）或 --ldflags "-s -w"（CI）下已被 strip，
  # file 输出为 "... stripped"，清单因此恒为空，报：
  #     error: Empty %files file .../debugfiles.list
  # 这是本仓库 .rpm 长期构建失败的真实原因，与代码/依赖无关。
  #
  # 即使把 file 装上，带 debug info 的二进制也只会额外产出一个本包不需要的
  # -debuginfo 子包（里面只有 /usr/lib/debug 下的符号）。本包是「已 strip 的
  # 发布二进制 + 服务单元 + 配置」的交付包，不需要调试符号，所以默认明确关闭。
  # 需要调试符号时用 MOCENTER_RPM_DEBUGINFO=1 打开。
  local -a rpmdebug_define=()
  if ! mc_is_true "${MOCENTER_RPM_DEBUGINFO:-0}"; then
    rpmdebug_define=(--define "debug_package %{nil}")
  fi
  # 注意：本函数只拿到 pkgarch（rpm 风格架构名），Go 风格的 "$arch" 在此作用域内
  # 并不存在。日志里曾写 "$pkg/$arch"，在 set -u 下会直接以
  #   bash: arch: unbound variable
  # 终止 —— rpmbuild 的真实报错被完全盖住，且是「整批中断」而非「跳过单个包」。
  if ! rpmbuild --define "_topdir $top" --define "_rpmdir $top/RPMS" \
      "${rpmdebug_define[@]}" \
      --target "$rpmmacro_arch" \
      -bb "$top/SPECS/${pkg}.spec" >"$top/build.log" 2>&1; then
    warn "rpmbuild 失败（$pkg/$pkgarch），日志尾部："
    tail -20 "$top/build.log" >&2 || true
    # 最常见的一类失败是「环境缺工具」而非本项目的 spec 有问题：rpmbuild 的
    # find-debuginfo 依赖 file(1)，缺失时只会在日志里留下 "file: not found"，
    # 与源码毫无关系。这类信息必须显式提示，否则会被当成代码问题排查半天。
    if grep -qE "^(RPM build errors:)?[[:space:]]*(Empty %files file .*debugfiles.list|.*: file: not found|.*find-debuginfo: command not found)" "$top/build.log"; then
      warn "  上述报错来自 rpmbuild 的 debuginfo 阶段（非本项目代码）："
      warn "   · 缺 file(1) 时 rpmbuild 的 find-debuginfo 无法识别 ELF，转而报 Empty %files"
      warn "   · 本脚本已默认关闭 debuginfo 子包（--define 'debug_package %{nil}'）"
      warn "   · 修复：安装 file 与 rpm 工具（apt-get install -y rpm file / dnf install -y rpm-build file）"
      warn "   · 如需产物带 -debuginfo：MOCENTER_RPM_DEBUGINFO=1 重新构建"
    fi
    return 1
  fi
  # 关掉 debuginfo 后 RPMS 下只应有一个 rpm；开启时才可能是两个（主包 +
  # -debuginfo）。取「非 -debuginfo 的那个」作为交付产物，避免把子包搬到
  # 包名 mocenter-*.rpm 的路径下张冠李戴。
  local built
  built="$(find "$top/RPMS" -name '*.rpm' ! -name '*-debuginfo-*.rpm' | head -1)"
  [ -n "$built" ] || built="$(find "$top/RPMS" -name '*.rpm' | head -1)"
  [ -n "$built" ] || { warn "rpmbuild 未产出 rpm（$pkg/$pkgarch）"; return 1; }
  mv "$built" "$out"
  rm -rf "$top"
  return 0
}

build_linux_packages() {
  [ -n "$PKGS" ] || { info "  已指定 --package=none，跳过安装包"; return 0; }
  local want_deb=0 want_rpm=0
  mc_in_list "deb" "$PKGS" && want_deb=1
  mc_in_list "rpm" "$PKGS" && want_rpm=1
  [ "$want_deb$want_rpm" = "00" ] && return 0

  # .deb/.rpm 的二进制必须是 linux；--platform=windows 时跳过（会给出提示）
  if ! mc_in_list "linux" "$PLATFORMS"; then
    info "  已指定 --platform 不含 linux，跳过 .deb/.rpm（--package=deb/rpm 需要 linux 产物）"
    return 0
  fi

  local roles=""
  mc_in_list "agent"  "${TYPES[*]:-}" && roles="$roles agent"
  mc_in_list "server" "${TYPES[*]:-}" && roles="$roles server"
  [ -n "$roles" ] || { info "  --type 不含 agent/server，无需打包"; return 0; }

  step "生成 Linux 安装包（${PKGS// /,}，架构：$ARCHS）"
  local pkgarchs="$ARCHS"
  local role arch bin stage failed=0
  local -a jobs=()
  for role in $roles; do
    for arch in $pkgarchs; do
      bin="$OUT/linux/binaries/$role/linux/mocenter-${role}-${arch}"
      if [ ! -f "$bin" ]; then
        warn "缺少二进制 $bin，跳过 $role/$arch 的打包；先执行：
         bash scripts/build-linux-all.sh --type=$role --platform=linux --arch=$arch --package=none"
        failed=1; continue
      fi
      stage="$OUT/linux/packages/.stage-${role}-${arch}"
      stage_linux_pkg "$role" "$arch" "$stage" "$bin"
      for fmt in deb rpm; do
        [ "$fmt" = deb ] && [ "$want_deb" = 0 ] && continue
        [ "$fmt" = rpm ] && [ "$want_rpm" = 0 ] && continue
        build_pkg_job "$fmt" "$stage" "$role" "$arch" &
        jobs+=($!)
      done
      # 串行等待：stage 目录会被复用，避免并发覆盖
      local pid; for pid in "${jobs[@]}"; do wait "$pid" || failed=1; done
      jobs=()
      rm -rf "$stage"
    done
  done
  [ "$failed" = 0 ] || warn "部分安装包未生成（原因见上）"
}

build_pkg_job() {
  local fmt="$1" stage="$2" role="$3" arch="$4"
  if [ "$fmt" = "deb" ]; then build_deb "$stage" "$role" "$arch"; else build_rpm "$stage" "$role" "$arch"; fi
}

# ---------------------------------------------------------------- 打包：Windows zip
build_windows_zip() {
  mc_in_list "zip" "$PKGS" || return 0
  if ! mc_in_list "windows" "$PLATFORMS"; then
    info "  已指定 --platform 不含 windows，跳过 zip"
    return 0
  fi
  local roles=""
  mc_in_list "agent"  "${TYPES[*]:-}" && roles="$roles agent"
  mc_in_list "server" "${TYPES[*]:-}" && roles="$roles server"
  [ -n "$roles" ] || { info "  --type 不含 agent/server，无需打包"; return 0; }

  # 只打包该平台实际构建出来的架构：--arch=all 的并集里含 arm，
  # 但 windows/arm 不受 Go 支持（见 mc_platform_archs_filtered 的说明），
  # 二进制阶段已按平台过滤，这里同步过滤，避免刷"缺少文件"的噪声。
  local win_archs; win_archs="$(mc_platform_archs_filtered windows "$ARCHS")"
  step "生成 Windows 免安装 zip（架构：${win_archs:-无}）"
  local role arch src stage zip
  for role in $roles; do
    for arch in $win_archs; do
      # Windows Server 只产出 amd64/386（与二进制构建阶段的过滤保持一致）。
      if [ "$role" = "server" ]; then
        case "$arch" in amd64|386) ;; *) continue ;; esac
      fi
      src="$OUT/windows/binaries/$role/windows/mocenter-${role}-${arch}.exe"
      if [ ! -f "$src" ]; then
        warn "缺少 $src，跳过 $role/$arch 的 zip；先执行：
         bash scripts/build-linux-all.sh --type=$role --platform=windows --arch=$arch --package=none"
        continue
      fi
      stage="$OUT/windows/packages/.stage-${role}-${arch}"
      mc_reset_dir "$stage"
      mkdir -p "$stage/bin"
      cp "$src" "$stage/bin/"
      # 免安装包要能直接跑起来，因此带上配置模板与部署脚本
      cp scripts/deploy/mocenter-agent.conf "$stage/bin/" 2>/dev/null || true
      cp scripts/deploy/mocenter-server.conf "$stage/bin/" 2>/dev/null || true
      cat > "$stage/README.txt" <<EOF
MoCenter ${role} ${arch} 免安装包
=================================
目录内容:
  bin/mocenter-${role}.exe        主程序（直接运行即为便携形态，配置落在同目录）
  bin/mocenter-${role}.conf       配置模板（按需改名/调整）
  install-windows.bat             注册为系统服务（需管理员）
  uninstall-windows.bat           卸载服务

注册为服务（管理员 PowerShell / CMD）:
  install-windows.bat install <服务端地址> [agentid] [proxy]
EOF
      # 服务注册/卸载脚本：Windows 侧不支持符号链接，直接生成副本
      cp scripts/deploy/install-windows.bat "$stage/install-windows.bat" 2>/dev/null || true
      cp scripts/deploy/uninstall-windows.bat "$stage/uninstall-windows.bat" 2>/dev/null || true
      zip="$OUT/windows/packages/zip/mocenter-${role}-${OPT_SERVER_VERSION}-windows-${arch}.zip"
      mc_make_zip "$stage" "$zip" "mocenter-${role}"
      rm -rf "$stage"
      ok "$(basename "$zip")  ($(du -h "$zip" | cut -f1))"
      mc_verify_zip "$zip"
    done
  done
}

# ---------------------------------------------------------------- 汇总
summary() {
  rule
  info " 产物汇总（$OUT）"
  rule
  local found=0 f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    found=1
    printf '  %-8s %s\n' "$(du -h "$f" | cut -f1)" "${f#"$OUT/"}"
  done < <(find "$OUT" -type f ! -name '.gitkeep' ! -path '*/.stage-*/*' ! -path '*/.rpmbuild/*' 2>/dev/null | sort)
  [ "$found" = 1 ] || warn "  没有产出任何文件（检查 --type/--package/--binary 组合）"
  rule
  log " 提示:"
  log "   · Linux 安装:  sudo dpkg -i <deb>    /  sudo rpm -ivh <rpm>"
  log "   · 安装后请修改 /etc/mocenter/mocenter-{agent,server}.conf"
  log "   · Windows 图形安装程序需在 Windows 上编译: scripts\\build-all.bat --type=installer"
}

# ---------------------------------------------------------------- 主流程
main() {
  parse_args "$@"
  expand_lists
  # Go 只在真的要构建二进制时才必需：--binary=0（复用已有产物只做打包）
  # 或 --type 不含 agent/server 时都不该因为缺 Go 而失败。
  need_go=0
  if [ "$OPT_BINARY" = 1 ]; then
    for t in "${TYPES[@]:-}"; do
      case "$t" in agent|server|installer) need_go=1 ;; esac
    done
  fi
  [ "$need_go" = 1 ] && mc_require_go
  OPT_OUTDIR="${OPT_OUTDIR%/}"
  # 相对 outdir 统一落到仓库根下，避免受调用者当前目录影响
  case "$OPT_OUTDIR" in
    /*) OUT="$OPT_OUTDIR" ;;
    ./*|../*) OUT="$(mc_root)/${OPT_OUTDIR#./}" ;;
    *) OUT="$(mc_root)/$OPT_OUTDIR" ;;
  esac

  info "============================================================"
  info " MoCenter 统一构建"
  info "============================================================"
  log  " 项目目录 : $(mc_root)"
  log  " 输出目录 : $OUT"
  log  " 构建类型 : ${TYPES[*]:-none}"
  log  " 目标平台 : $PLATFORMS"
  log  " 目标架构 : $ARCHS"
  log  " 安装包   : ${PKGS:-none}"
  log  " 二进制   : $([ "$OPT_BINARY" = 1 ] && echo 是 || echo 否)"
  log  " Web 前端 : $OPT_WEB"
  log  " 并发度   : $OPT_JOBS"
  log  " 版本     : server ${OPT_SERVER_VERSION} / agent ${OPT_AGENT_VERSION}"
  info "============================================================"

  setup_outdir
  build_binaries
  build_web
  build_linux_packages
  build_windows_zip
  summary
}

main "$@"
