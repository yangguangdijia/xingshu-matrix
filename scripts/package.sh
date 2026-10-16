#!/usr/bin/env bash
# 打离线包：用于无外网的物理机环境
set -euo pipefail
cd "$(dirname "$0")/.."
VER=${1:-$(date +%Y%m%d)}
mkdir -p dist
tar -czf "dist/xingshu-matrix-$VER.tar.gz" --transform "s#^public#xingshu-matrix#" public
echo "dist/xingshu-matrix-$VER.tar.gz"
