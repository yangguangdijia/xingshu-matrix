#!/usr/bin/env bash
# 部署后冒烟测试：bash scripts/smoke-test.sh http://127.0.0.1:8080
set -euo pipefail
URL=${1:-http://127.0.0.1:8080}
curl -fsS "$URL/healthz" | grep -q ok && echo "healthz OK"
curl -fsS "$URL/" | grep -q "星枢" && echo "index OK"
for f in hark.css hark.js chart.js; do curl -fsS -o /dev/null "$URL/$f" && echo "$f OK"; done
