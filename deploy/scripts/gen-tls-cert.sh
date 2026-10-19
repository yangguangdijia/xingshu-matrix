#!/usr/bin/env bash
# 为星枢 Matrix 生成自签名 TLS 证书并写入 K8s Secret。
#
#   CN=xingshu matrix（证书主题），SAN 覆盖平台域名与常用入口，有效期默认 3650 天（10 年）。
#
# 用法：
#   bash deploy/scripts/gen-tls-cert.sh                     # 默认参数
#   DOMAIN=xingshu-matrix.com DAYS=3650 bash deploy/scripts/gen-tls-cert.sh
#   NAMESPACE=xingshu-system SECRET=xingshu-matrix-tls bash deploy/scripts/gen-tls-cert.sh
#
# 说明：
#   - 生成的文件放在 $OUT（默认 ./certs），私钥请不要提交到 git。
#   - 已存在同名 Secret 时用 `kubectl apply` 覆盖（证书轮换）。
#   - 兼容 OpenSSL 1.0.2（CentOS 7 自带）：用配置文件声明 SAN，而不是 -addext。
set -Eeuo pipefail

DOMAIN="${DOMAIN:-xingshu-matrix.com}"
CN="${CN:-xingshu matrix}"
ORG="${ORG:-Xingshu Matrix}"
DAYS="${DAYS:-3650}"
BITS="${BITS:-4096}"
OUT="${OUT:-./certs}"
NAMESPACE="${NAMESPACE:-xingshu-system}"
SECRET="${SECRET:-xingshu-matrix-tls}"
EXTRA_IPS="${EXTRA_IPS:-192.168.6.3,127.0.0.1}"   # 逗号分隔，方便直接用 IP 访问

command -v openssl >/dev/null || { echo "需要 openssl" >&2; exit 1; }
mkdir -p "$OUT"

# ---- SAN 列表 ----
alt="DNS.1 = ${DOMAIN}
DNS.2 = *.${DOMAIN}
DNS.3 = localhost"
i=4; ips=""
IFS=',' read -ra _ips <<< "$EXTRA_IPS"
n=1
for ip in "${_ips[@]}"; do
  [ -z "$ip" ] && continue
  ips+="IP.${n} = ${ip}"$'\n'; n=$((n+1))
done

cat > "$OUT/openssl.cnf" <<EOF
[req]
default_bits = ${BITS}
prompt = no
distinguished_name = dn
x509_extensions = v3

[dn]
CN = ${CN}
O = ${ORG}
OU = Platform

[v3]
subjectAltName = @alt
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth

[alt]
${alt}
${ips}
EOF

openssl req -x509 -newkey rsa:${BITS} -sha256 -days "${DAYS}" -nodes \
  -keyout "$OUT/tls.key" -out "$OUT/tls.crt" -config "$OUT/openssl.cnf" 2>/dev/null
chmod 600 "$OUT/tls.key"

echo "== 证书信息 =="
openssl x509 -in "$OUT/tls.crt" -noout -subject -issuer -dates
openssl x509 -in "$OUT/tls.crt" -noout -text | grep -A2 "Subject Alternative Name" || true

if command -v kubectl >/dev/null && kubectl get ns "$NAMESPACE" >/dev/null 2>&1; then
  kubectl -n "$NAMESPACE" create secret tls "$SECRET" \
    --cert="$OUT/tls.crt" --key="$OUT/tls.key" --dry-run=client -o yaml | kubectl apply -f -
  echo "已写入 Secret ${NAMESPACE}/${SECRET}"
else
  echo "跳过写入 Secret（未找到 kubectl 或命名空间 ${NAMESPACE}）；手动："
  echo "  kubectl -n ${NAMESPACE} create secret tls ${SECRET} --cert=${OUT}/tls.crt --key=${OUT}/tls.key"
fi
