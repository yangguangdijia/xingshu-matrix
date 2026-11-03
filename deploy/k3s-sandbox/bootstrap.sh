#!/bin/sh
# 沙箱重启后一键恢复：k3s(无 kube-proxy) + bridge CNI + svcproxy + 星枢 Matrix
set -e
K=/workspace/tools/k3s/k3s
grep -q xs-node /etc/hosts || echo "127.0.0.1 xs-node" >> /etc/hosts
hostname xs-node || true
sysctl -qw net.ipv4.ip_forward=1
modprobe br_netfilter 2>/dev/null || true
mkdir -p /etc/cni/net.d /opt/cni/bin /var/lib/rancher/k3s/agent/etc/cni/net.d
CONF='{"cniVersion":"1.0.0","name":"xsnet","plugins":[{"type":"bridge","bridge":"cni0","isGateway":true,"ipMasq":false,"hairpinMode":true,"ipam":{"type":"host-local","ranges":[[{"subnet":"10.42.0.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}},{"type":"loopback"}]}'
echo "$CONF" > /etc/cni/net.d/05-bridge.conflist
echo "$CONF" > /var/lib/rancher/k3s/agent/etc/cni/net.d/05-bridge.conflist
rm -f /var/lib/rancher/k3s/agent/etc/cni/net.d/10-flannel.conflist
# 平台文件放本地盘（hostPath）
mkdir -p /opt/xs/bin
cp /workspace/tools/xs/bin/matrix-server /opt/xs/bin/matrix-server.new && mv -f /opt/xs/bin/matrix-server.new /opt/xs/bin/matrix-server
rm -rf /opt/xs/public && cp -r /workspace/work/beta/public /opt/xs/public
cp /workspace/tools/xs/config.yaml /opt/xs/config.yaml
chown -R 10001:10001 /opt/xs
# containerd 放在 /workspace 上的 ext4 镜像（loop 挂载）：本地盘只有 6G，且 overlayfs 不能叠在根 overlay 上
# node-exporter 以 HostToContainer 传播挂载根目录，需要 / 为 shared mount（生产节点默认如此）
mount --make-rshared / || true
mkdir -p /var/lib/rancher/k3s/agent/containerd
[ -f /workspace/tools/k3s-disk.img ] || { command -v mkfs.ext4 >/dev/null || { apt-get update -q; apt-get install -y -q e2fsprogs; }; truncate -s 24G /workspace/tools/k3s-disk.img; mkfs.ext4 -q -F /workspace/tools/k3s-disk.img; }
mountpoint -q /var/lib/rancher/k3s/agent/containerd || mount -o loop /workspace/tools/k3s-disk.img /var/lib/rancher/k3s/agent/containerd
# 沙箱 CI/CD 验证用的 http 镜像仓库（deploy/k3s-sandbox/cicd-demo.yaml，hostNetwork 5000）
mkdir -p /etc/rancher/k3s
printf 'mirrors:\n  "10.42.0.1:5000":\n    endpoint:\n      - "http://10.42.0.1:5000"\n' > /etc/rancher/k3s/registries.yaml
pgrep -x k3s-server >/dev/null || pgrep -f "^/workspace/tools/k3s/k3s server" >/dev/null || setsid sh -c '/workspace/tools/k3s/start.sh > /tmp/k3s.log 2>&1' </dev/null >/dev/null 2>&1 &
for i in $(seq 1 60); do $K kubectl get nodes >/dev/null 2>&1 && break; sleep 3; done
for i in $(seq 1 30); do [ -e /var/lib/rancher/k3s/data/current/bin/cni ] && break; sleep 2; done
for p in bridge host-local loopback portmap; do ln -sf /var/lib/rancher/k3s/data/current/bin/cni /opt/cni/bin/$p; done
sysctl -qw net.bridge.bridge-nf-call-iptables=1 || true
IPT=/var/lib/rancher/k3s/data/current/bin/aux/iptables-legacy
$IPT -t nat -C POSTROUTING -s 10.42.0.0/24 ! -d 10.42.0.0/24 -j MASQUERADE 2>/dev/null || $IPT -t nat -A POSTROUTING -s 10.42.0.0/24 ! -d 10.42.0.0/24 -j MASQUERADE
pgrep -f svcproxy.py >/dev/null || setsid sh -c 'python3 /workspace/tools/k3s/svcproxy.py > /tmp/svcproxy.log 2>&1' </dev/null >/dev/null 2>&1 &
$K kubectl apply -f /workspace/tools/xs/manifest.yaml
echo bootstrap-done
