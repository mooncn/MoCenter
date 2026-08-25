#!/usr/bin/env bash
# =============================================================
# 生成 MoCenter TLS 自签名证书（用于 Agent<->Server 全链路加密）
# 用法: bash scripts/gen-cert.sh [输出目录，默认 ./certs]
# 生成:
#   server.crt / server.key   — 服务端 TLS 证书
#   ca.crt                    — 自签 CA（Agent 端可选校验用）
# =============================================================
set -e

OUT_DIR="${1:-certs}"
mkdir -p "${OUT_DIR}"

# 需要 openssl
if ! command -v openssl >/dev/null 2>&1; then
  echo "!! 未安装 openssl，无法生成证书"
  exit 1
fi

echo "==> 生成 CA 证书 (ca.crt)"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "${OUT_DIR}/ca.key" -out "${OUT_DIR}/ca.crt" \
  -days 3650 -subj "/CN=MoCenter-CA" 2>/dev/null

echo "==> 生成服务端证书 (server.crt/server.key)"
openssl req -newkey rsa:2048 -nodes \
  -keyout "${OUT_DIR}/server.key" -out "${OUT_DIR}/server.csr" \
  -subj "/CN=mocenter-server" 2>/dev/null

cat > "${OUT_DIR}/server-ext.cnf" <<EOF
subjectAltName=DNS:mocenter-server,IP:127.0.0.1,IP:0.0.0.0
EOF

openssl x509 -req -in "${OUT_DIR}/server.csr" \
  -CA "${OUT_DIR}/ca.crt" -CAkey "${OUT_DIR}/ca.key" -CAcreateserial \
  -out "${OUT_DIR}/server.crt" -days 3650 \
  -extfile "${OUT_DIR}/server-ext.cnf" 2>/dev/null

rm -f "${OUT_DIR}/server.csr" "${OUT_DIR}/server-ext.cnf" "${OUT_DIR}/ca.srl"

echo ""
echo "==> 证书已生成到 ${OUT_DIR}/"
ls -1 "${OUT_DIR}"
echo ""
echo "服务端启动（启用 TLS）:"
echo "  ./mocenter-server -listen :9000 -http :8080 -apikey xxx -tls-cert ${OUT_DIR}/server.crt -tls-key ${OUT_DIR}/server.key"
echo "Agent 启动（TLS 连接）:"
echo "  ./mocenter-agent -server host:9000 -agentid host-01 -tls -insecure"
echo ""
echo "说明: 生产环境建议使用受信任 CA 签发的证书，并将 Agent 端的 -insecure 替换为 CA 校验。"
