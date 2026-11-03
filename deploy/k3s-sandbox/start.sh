#!/bin/sh
exec /workspace/tools/k3s/k3s server --snapshotter=native --disable=traefik,servicelb,metrics-server --disable-kube-proxy --disable-network-policy --flannel-backend=none --write-kubeconfig-mode=644 --node-name xs-node --kube-apiserver-arg=enable-aggregator-routing=true
