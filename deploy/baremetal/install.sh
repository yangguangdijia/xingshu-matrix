#!/usr/bin/env bash
# 星枢 Matrix 物理机 / 虚拟机一键安装（CentOS/RHEL/Rocky/openEuler/Kylin、Ubuntu/Debian）
# 用法: sudo bash install.sh [--port 8080] [--dir /opt/xingshu-matrix] [--offline ./xingshu-matrix.tar.gz]
set -euo pipefail
PORT=8080; DIR=/opt/xingshu-matrix; PKG=""
while [ $# -gt 0 ]; do case "$1" in
  --port) PORT="$2"; shift 2;; --dir) DIR="$2"; shift 2;; --offline) PKG="$2"; shift 2;;
  *) echo "未知参数 $1"; exit 1;; esac; done
[ "$(id -u)" = 0 ] || { echo "请使用 root 运行"; exit 1; }
SRC="$(cd "$(dirname "$0")/../.." && pwd)"

echo "==> 安装 Nginx"
if ! command -v nginx >/dev/null; then
  if command -v dnf >/dev/null; then dnf install -y nginx
  elif command -v yum >/dev/null; then yum install -y nginx
  elif command -v apt-get >/dev/null; then apt-get update && apt-get install -y nginx
  else echo "未识别的包管理器，请先手动安装 nginx"; exit 1; fi
fi

echo "==> 部署静态文件到 $DIR"
mkdir -p "$DIR"
if [ -n "$PKG" ]; then tar -xzf "$PKG" -C "$DIR" --strip-components=1; else cp -r "$SRC/public/." "$DIR/"; fi

echo "==> 写入 Nginx 配置（端口 $PORT）"
CONF=/etc/nginx/conf.d/xingshu-matrix.conf
sed -e "s#listen 80;#listen $PORT;#" -e "s#/usr/share/nginx/html#$DIR#" "$SRC/deploy/nginx.conf" > "$CONF"
nginx -t
systemctl enable --now nginx
systemctl reload nginx

if command -v firewall-cmd >/dev/null && systemctl is-active -q firewalld; then
  firewall-cmd --permanent --add-port="$PORT"/tcp && firewall-cmd --reload
fi
if command -v getenforce >/dev/null && [ "$(getenforce)" = Enforcing ]; then
  chcon -R -t httpd_sys_content_t "$DIR" || true
  semanage port -a -t http_port_t -p tcp "$PORT" 2>/dev/null || true
fi

curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null && echo "==> 安装完成：http://<服务器IP>:$PORT  默认账号 admin（首次登录强制改密）"
