#!/usr/bin/env bash
# =============================================================
# MoCenter Agent Linux 打包脚本
# 支持构建 .deb 与 .rpm 安装包，并内置 systemd 服务与开机自启。
#
# 依赖（任一）:
#   - fpm  https://github.com/jordansissel/fpm
#   - 或 dpkg-deb (Debian/Ubuntu) 与 rpmbuild (RHEL/Fedora)
#
# 用法: bash build-linux-pkg.sh [deb|rpm|all] [arch]
#   示例: bash build-linux-pkg.sh all amd64
#   示例: bash build-linux-pkg.sh deb arm64
# =============================================================
set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AGENT_DIR="${ROOT_DIR}/agent"
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
PKG_NAME="mocenter-agent"
STAGE="${OUT_DIR}/stage"

echo "=============================================="
echo " MoCenter Agent Linux 打包"
echo " 架构: ${ARCH} (GOARCH=${GOARCH}, pkg=${PKG_ARCH})"
echo " 版本: ${VERSION}"
echo " 输出: ${OUT_DIR}"
echo "=============================================="

mkdir -p "${STAGE}" "${OUT_DIR}"
rm -rf "${STAGE}"

# 1. 构建 Agent 二进制
echo "[1/3] 构建 Agent (linux/${GOARCH}) ..."
(cd "${AGENT_DIR}" && GOOS=linux GOARCH="${GOARCH}" CGO_ENABLED=0 go build -ldflags "-s -w" -o "${STAGE}/mocenter-agent" .)

# 2. 组装目录结构
echo "[2/3] 组装文件树 ..."
mkdir -p "${STAGE}/usr/local/bin" "${STAGE}/etc/mocenter" "${STAGE}/lib/systemd/system"
cp "${STAGE}/mocenter-agent" "${STAGE}/usr/local/bin/mocenter-agent"

# systemd 服务单元
cat > "${STAGE}/lib/systemd/system/mocenter-agent.service" <<EOF
[Unit]
Description=MoCenter Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/mocenter/mocenter-agent.conf
ExecStart=/usr/local/bin/mocenter-agent -server \${MOCENTER_SERVER} -agentid \${MOCENTER_AGENT_ID} -proxy-ip \${MOCENTER_PROXY_IP} -daemon
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

# 默认配置
cat > "${STAGE}/etc/mocenter/mocenter-agent.conf" <<EOF
# MoCenter Agent 配置
MOCENTER_SERVER=your.server.com:9000
# MOCENTER_AGENT_ID=
# MOCENTER_PROXY_IP=http://fa.mooncn.win/?url=
EOF

# 后置安装脚本：启用并启动服务（开机自启）
cat > "${STAGE}/postinst.sh" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable mocenter-agent.service
  systemctl restart mocenter-agent.service || true
fi
exit 0
EOF
chmod +x "${STAGE}/postinst.sh"

# 卸载脚本
cat > "${STAGE}/prerm.sh" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop mocenter-agent.service 2>/dev/null || true
  systemctl disable mocenter-agent.service 2>/dev/null || true
fi
exit 0
EOF
chmod +x "${STAGE}/prerm.sh"

build_deb() {
  echo "[3/3] 构建 .deb ..."
  if command -v dpkg-deb >/dev/null 2>&1; then
    local D="${STAGE}/deb/${PKG_NAME}_${VERSION}_${PKG_ARCH}"
    mkdir -p "${D}/DEBIAN" "${D}/usr/local/bin" "${D}/etc/mocenter" "${D}/lib/systemd/system"
    cp -r "${STAGE}/usr/local/bin" "${D}/usr/local/"
    cp -r "${STAGE}/etc/mocenter" "${D}/etc/"
    cp -r "${STAGE}/lib/systemd/system" "${D}/lib/systemd/"
    cat > "${D}/DEBIAN/control" <<EOF
Package: ${PKG_NAME}
Version: ${VERSION}
Section: net
Priority: optional
Architecture: ${PKG_ARCH}
Depends: libc6 (>= 2.17)
Maintainer: MoCenter <admin@example.com>
Description: MoCenter Agent - 集中式主机远控管理客户端
 自动注册 systemd 服务并设置开机自启。
EOF
    cat > "${D}/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable mocenter-agent.service
  systemctl restart mocenter-agent.service || true
fi
exit 0
EOF
    chmod +x "${D}/DEBIAN/postinst"
    cat > "${D}/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop mocenter-agent.service 2>/dev/null || true
  systemctl disable mocenter-agent.service 2>/dev/null || true
fi
exit 0
EOF
    chmod +x "${D}/DEBIAN/prerm"
    dpkg-deb --build "${D}" "${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb"
    echo "  -> ${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb"
  elif command -v fpm >/dev/null 2>&1; then
    fpm -s dir -t deb -n "${PKG_NAME}" -v "${VERSION}" -a "${PKG_ARCH}" \
      --after-install "${STAGE}/postinst.sh" --before-remove "${STAGE}/prerm.sh" \
      -C "${STAGE}" -p "${OUT_DIR}/${PKG_NAME}_${VERSION}_${PKG_ARCH}.deb" \
      usr/local/bin/mocenter-agent etc/mocenter/mocenter-agent.conf lib/systemd/system/mocenter-agent.service
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
      -C "${STAGE}" -p "${OUT_DIR}/${PKG_NAME}-${VERSION}.${PKG_ARCH}.rpm" \
      usr/local/bin/mocenter-agent etc/mocenter/mocenter-agent.conf lib/systemd/system/mocenter-agent.service
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
