#!/usr/bin/env bash
set -euo pipefail
DIR=${1:-/opt/xingshu-matrix}
rm -f /etc/nginx/conf.d/xingshu-matrix.conf && systemctl reload nginx || true
rm -rf "$DIR"
echo "已卸载星枢 Matrix（Nginx 本身保留）"
