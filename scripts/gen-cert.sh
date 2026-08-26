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

echo "==> 生成客户端证书 (client.crt/client.key) — 用于 mTLS 双向认证"
openssl req -newkey rsa:2048 -nodes \
  -keyout "${OUT_DIR}/client.key" -out "${OUT_DIR}/client.csr" \
  -subj "/CN=mocenter-agent" 2>/dev/null

cat > "${OUT_DIR}/client-ext.cnf" <<EOF
subjectAltName=DNS:agent,IP:127.0.0.1
EOF

openssl x509 -req -in "${OUT_DIR}/client.csr" \
  -CA "${OUT_DIR}/ca.crt" -CAkey "${OUT_DIR}/ca.key" -CAcreateserial \
  -out "${OUT_DIR}/client.crt" -days 3650 \
  -extfile "${OUT_DIR}/client-ext.cnf" 2>/dev/null

rm -f "${OUT_DIR}/client.csr" "${OUT_DIR}/client-ext.cnf" "${OUT_DIR}/ca.srl"

echo ""
echo "==> 证书已生成到 ${OUT_DIR}/"
ls -1 "${OUT_DIR}"
echo ""
echo "服务端启动（启用 TLS 单向加密）:"
echo "  ./mocenter-server -listen :9000 -http :8080 -apikey xxx -tls-cert ${OUT_DIR}/server.crt -tls-key ${OUT_DIR}/server.key"
echo ""
echo "服务端启动（启用 mTLS 双向认证，Agent 必须携带客户端证书）:"
echo "  ./mocenter-server -listen :9000 -http :8080 -apikey xxx -tls-cert ${OUT_DIR}/server.crt -tls-key ${OUT_DIR}/server.key -ca-cert ${OUT_DIR}/ca.crt"
echo ""
echo "Agent 启动（单向 TLS 连接，自签名跳过校验）:"
echo "  ./mocenter-agent -server host:9000 -agentid host-01 -tls -insecure"
echo ""
echo "Agent 启动（mTLS 双向认证，携带客户端证书并校验服务端 CA）:"
echo "  ./mocenter-agent -server host:9000 -agentid host-01 -tls -client-cert ${OUT_DIR}/client.crt -client-key ${OUT_DIR}/client.key -ca-cert ${OUT_DIR}/ca.crt"
echo ""
echo "说明: 生产环境建议使用受信任 CA 签发的证书。Agent 端 -ca-cert 用于校验服务端证书（非必须，可用 -insecure 跳过）。"
