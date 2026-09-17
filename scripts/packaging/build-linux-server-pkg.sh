#!/usr/bin/env bash
# =============================================================
# MoCenter Server Linux 打包脚本
# 支持构建 .deb 与 .rpm 安装包，内置 systemd 服务与开机自启，
# 可选随包附带 Web 前端（web/dist）。
#
# 依赖（任一）:
#   - fpm  https://github.com/jordansissel/fpm
#   - 或 dpkg-deb (Debian/Ubuntu) 与 rpmbuild (RHEL/Fedora)
#
# 用法: bash build-linux-server-pkg.sh [deb|rpm|all] [arch]
#   示例: bash build-linux-server-pkg.sh all amd64
#   示例: bash build-linux-server-pkg.sh deb arm64
#
# 说明：Web 前端未随包时，服务端启动后仅提供 :9000 / REST API，不托管页面。
#       先执行 bash web/build.sh 再打包即可把 dist 一并装入 /usr/share/mocenter/web。
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SERVER_DIR="${ROOT_DIR}/server"
OUT_DIR="${ROOT_DIR}/build/packages"
PKG_TYPE="${1:-all}"
ARCH="${2:-amd64}"

# 目标架构 -> Go GOARCH 与包架构名
case "${ARCH}" in
  amd64)  GOARCH=amd64; PKG_ARCH=amd64 ;;
  x86_64) GOARCH=amd64; PKG_ARCH=amd64 ;;
  arm64)  GOARCH=arm64; PKG_ARCH=arm64 ;;
  aarch64)GOARCH=arm64; PKG_ARCH=arm64 ;;
  arm)    GOARCH=arm;   PKG_ARCH=armhf ;;
  386)    GOARCH=386;   PKG_ARCH=i386 ;;
  *) echo "不支持的架构: ${ARCH}（可用 amd64/arm64/arm/386）"; exit 1 ;;
esac

VERSION="${MOCENTER_VERSION:-1.0.0}"
PKG_NAME="mocenter-server"
STAGE="${OUT_DIR}/stage-server"
WEB_INSTALL_DIR="/usr/share/mocenter/web"

echo "=============================================="
echo " MoCenter Server Linux 打包"
echo " 架构: ${ARCH} (GOARCH=${GOARCH}, pkg=${PKG_ARCH})"
echo " 版本: ${VERSION}"
echo " 输出: ${OUT_DIR}"
echo "=============================================="

if ! command -v go >/dev/null 2>&1; then
  echo "!! 未找到 Go，请先安装 Go 1.22+（服务端 go.mod 要求）" >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"
rm -rf "${STAGE}"
mkdir -p "${STAGE}"

# 1. 构建 Server 二进制（server 依赖 modernc.org/sqlite，纯 Go，可 CGO_ENABLED=0）
echo "[1/3] 构建 Server (linux/${GOARCH}) ..."
(cd "${SERVER_DIR}" && GOOS=linux GOARCH="${GOARCH}" CGO_ENABLED=0 go build -ldflags "-s -w" -o "${STAGE}/mocenter-server" .)

# 2. 组装目录结构
echo "[2/3] 组装文件树 ..."
mkdir -p \
  "${STAGE}/usr/local/bin" \
  "${STAGE}/etc/mocenter" \
  "${STAGE}/lib/systemd/system" \
  "${STAGE}/var/lib/mocenter/data" \
  "${STAGE}/var/lib/mocenter/packages" \
  "${STAGE}/usr/share/mocenter"
cp "${STAGE}/mocenter-server" "${STAGE}/usr/local/bin/mocenter-server"

# Web 前端（可选）
if [ -f "${ROOT_DIR}/web/dist/index.html" ]; then
  mkdir -p "${STAGE}${WEB_INSTALL_DIR}"
  cp -r "${ROOT_DIR}/web/dist/." "${STAGE}${WEB_INSTALL_DIR}/"
  echo "  已随包附带 Web 前端 -> ${WEB_INSTALL_DIR}"
else
  echo "  !! 未发现 web/dist，安装包不含 Web 界面（可先执行 bash web/build.sh）"
  echo "     服务端仍会正常提供 :9000 长连接与 REST API"
  rmdir "${STAGE}${WEB_INSTALL_DIR}" 2>/dev/null || true
fi

# 服务端配置文件：由 systemd 的 EnvironmentFile 提供给 ExecStart，
# 初次安装请务必修改 MOCENTER_USER / MOCENTER_PASSWORD 与防火墙放行的端口。
# 注意：服务端自身会生成 mocenter-server.json（含 TLS/白名单等全量参数），
#       本文件只负责在服务注册阶段传入首次启动所需的最小参数。
cat > "${STAGE}/etc/mocenter/mocenter-server.conf" <<EOF
# MoCenter Server 配置（由 systemd 注入启动参数）
# Agent TCP 长连接监听地址
MOCENTER_LISTEN=:9000
# Web 控制台 / REST API 监听地址
MOCENTER_HTTP=:8080
# 登录用户名与密码（首次安装后请立即修改）
MOCENTER_USER=admin
MOCENTER_PASSWORD=change-me
# Web 前端目录（与安装包内一致；未随包附带前端时留空即可）
MOCENTER_WEB_DIR=${WEB_INSTALL_DIR}
# 数据目录（审计日志、users.json、agent.key、托管安装包等）
MOCENTER_DATA=/var/lib/mocenter/data
# 显式指定配置文件路径，避免落到来回变化的当前工作目录
MOCENTER_CONFIG=/etc/mocenter/mocenter-server.json
# IP 白名单（逗号分隔，留空不限制；生产环境建议限制管理网段）
# MOCENTER_WHITELIST=10.0.0.0/8,192.168.0.0/16
# TLS / mTLS（证书就绪后取消注释启用；详见 scripts/gen-cert.sh）
# MOCENTER_TLS_CERT=/etc/mocenter/certs/server.crt
# MOCENTER_TLS_KEY=/etc/mocenter/certs/server.key
# MOCENTER_CA_CERT=/etc/mocenter/certs/ca.crt
EOF

# systemd 服务单元
cat > "${STAGE}/lib/systemd/system/mocenter-server.service" <<EOF
[Unit]
Description=MoCenter Server (集中式主机远控管理总控服务端)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 数据目录与配置目录必须先存在，否则服务端会以 root 工作目录写入
WorkingDirectory=/var/lib/mocenter
EnvironmentFile=-/etc/mocenter/mocenter-server.conf
ExecStart=/usr/local/bin/mocenter-server \\
  -listen \${MOCENTER_LISTEN} \\
  -http \${MOCENTER_HTTP} \\
  -user \${MOCENTER_USER} \\
  -password \${MOCENTER_PASSWORD} \\
  -data \${MOCENTER_DATA} \\
  -config \${MOCENTER_CONFIG} \\
  -pkg-dir /var/lib/mocenter/packages \\
  -web \${MOCENTER_WEB_DIR}
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

# 说明文件：写清安装后必须做的几件事，避免直接对外裸奔
cat > "${STAGE}/etc/mocenter/README.server" <<EOF
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

# 后置安装脚本：启用并启动服务（开机自启）
cat > "${STAGE}/postinst.sh" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable mocenter-server.service
  systemctl restart mocenter-server.service || true
fi
exit 0
EOF
chmod +x "${STAGE}/postinst.sh"

# 卸载脚本
cat > "${STAGE}/prerm.sh" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop mocenter-server.service 2>/dev/null || true
  systemctl disable mocenter-server.service 2>/dev/null || true
fi
exit 0
EOF
chmod +x "${STAGE}/prerm.sh"

# deb 需要带路径的 file:dest 对，动态拼装（前端可能不存在）
deb_files() {
  local D="$1"
  {
    printf 'usr/local/bin/mocenter-server %s/usr/local/bin/\n' "${D}"
    printf 'etc/mocenter/mocenter-server.conf %s/etc/mocenter/\n' "${D}"
    printf 'etc/mocenter/README.server %s/etc/mocenter/\n' "${D}"
    printf 'lib/systemd/system/mocenter-server.service %s/lib/systemd/system/\n' "${D}"
    if [ -d "${STAGE}${WEB_INSTALL_DIR}" ]; then
      printf 'usr/share/mocenter/web %s/usr/share/mocenter/\n' "${D}"
    fi
  }
}

build_deb() {
  echo "[3/3] 构建 .deb ..."
  if command -v dpkg-deb >/dev/null 2>&1; then
    local D="${OUT_DIR}/deb/${PKG_NAME}_${VERSION}_${PKG_ARCH}"
    rm -rf "${D}"
    mkdir -p "${D}/DEBIAN"
    # 按 stage 的文件树拷贝（保留目录结构），再用 dpkg-deb 打包
    (cd "${STAGE}" && find . -type d -exec mkdir -p "${D}/{}" \;)
    (cd "${STAGE}" && find . -type f \
      ! -name 'postinst.sh' ! -name 'prerm.sh' ! -name 'mocenter-server' \
      -exec cp --parents {} "${D}/" \;)
    cp "${STAGE}/mocenter-server" "${D}/usr/local/bin/mocenter-server"
    chmod 0755 "${D}/usr/local/bin/mocenter-server"
    cat > "${D}/DEBIAN/control" <<EOF
Package: ${PKG_NAME}
Version: ${VERSION}
Section: net
Priority: optional
Architecture: ${PKG_ARCH}
Depends: libc6 (>= 2.17)
Maintainer: MoCenter <admin@example.com>
Description: MoCenter Server - 集中式主机远控管理总控服务端
 提供 Agent 长连接接入、命令路由、探针聚合、端口映射与 Web 控制台。
 自动注册 systemd 服务并设置开机自启。
EOF
    cp "${STAGE}/postinst.sh" "${D}/DEBIAN/postinst"
    cp "${STAGE}/prerm.sh" "${D}/DEBIAN/prerm"
    chmod +x "${D}/DEBIAN/postinst" "${D}/DEBIAN/prerm"
    dpkg-deb --build "${D}" "${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb"
    echo "  -> ${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb"
  elif command -v fpm >/dev/null 2>&1; then
    fpm -s dir -t deb -n "${PKG_NAME}" -v "${VERSION}" -a "${PKG_ARCH}" \
      --after-install "${STAGE}/postinst.sh" --before-remove "${STAGE}/prerm.sh" \
      --config-files /etc/mocenter/mocenter-server.conf \
      -C "${STAGE}" -p "${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb" \
      usr/local/bin/mocenter-server \
      etc/mocenter/mocenter-server.conf \
      etc/mocenter/README.server \
      lib/systemd/system/mocenter-server.service \
      usr/share/mocenter
    echo "  -> ${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb (fpm)"
  else
    echo "  !! 缺少 dpkg-deb 与 fpm，跳过 .deb 构建"
  fi
}

build_rpm() {
  echo "[3/3] 构建 .rpm ..."
  if command -v fpm >/dev/null 2>&1; then
    fpm -s dir -t rpm -n "${PKG_NAME}" -v "${VERSION}" -a "${PKG_ARCH}" \
      --after-install "${STAGE}/postinst.sh" --before-remove "${STAGE}/prerm.sh" \
      --config-files /etc/mocenter/mocenter-server.conf \
      -C "${STAGE}" -p "${OUT_DIR}/${PKG_NAME}-${VERSION}.${PKG_ARCH}.rpm" \
      usr/local/bin/mocenter-server \
      etc/mocenter/mocenter-server.conf \
      etc/mocenter/README.server \
      lib/systemd/system/mocenter-server.service \
      usr/share/mocenter
    echo "  -> ${OUT_DIR}/${PKG_NAME}-${VERSION}.${PKG_ARCH}.rpm"
  else
    echo "  !! 缺少 fpm，跳过 .rpm 构建（可安装 fpm 后重试）"
  fi
}

case "${PKG_TYPE}" in
  deb) build_deb ;;
  rpm) build_rpm ;;
  all) build_deb; build_rpm ;;
  *) echo "用法: $0 [deb|rpm|all] [arch]"; exit 1 ;;
esac

echo "=============================================="
echo " 打包完成，产物目录: ${OUT_DIR}"
ls -lh "${OUT_DIR}"
echo "=============================================="
