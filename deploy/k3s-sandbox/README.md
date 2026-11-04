# 单节点 k3s 验证环境（受限沙箱）

用途：在没有 Docker、内核裁剪过的 aarch64 沙箱里跑一个最小 Kubernetes，部署星枢 Matrix 并接入该集群做联调。**仅用于开发验证，不是生产部署方式。** 生产请使用 `deploy/k8s/` 与正式镜像。

## 环境限制与绕行

| 问题 | 原因 | 绕行 |
| --- | --- | --- |
| kube-proxy 启动即退出 | 内核无 nf_tables；iptables-legacy 可用但缺 `xt_comment`/`xt_mark`/`xt_statistic` | `--disable-kube-proxy`，用 `svcproxy.py` 按 EndpointSlice 写 DNAT 规则（只取第一个就绪端点，无负载均衡） |
| flannel / kube-router 网络策略不可用 | 同上 | `--flannel-backend=none --disable-network-policy`，CNI 用 bridge + host-local（10.42.0.0/24） |
| 无 Docker、无法构建镜像 | 沙箱限制 | 运行镜像用 `python:3.12-slim-bookworm`，`matrix-server` 二进制与 `public/` 通过 hostPath 挂载 |
| 沙箱会重启、根盘非持久 | 平台限制 | `bootstrap.sh` 一键恢复；重启后 k3s 数据为新集群，需要重新登录改密并接入集群 |

## 使用

```bash
# 1. 构建二进制（arm64）
cd server && CGO_ENABLED=0 go build -o /workspace/tools/xs/bin/matrix-server ./cmd/matrix-server
# 2. 准备 /workspace/tools/xs/{public,config.yaml,manifest.yaml}，manifest 由本目录 manifest.yaml 填好密钥得到
# 3. 启动
sudo ./bootstrap.sh
```

接入本集群：在平台「集群 → 接入集群」粘贴 `/etc/rancher/k3s/k3s.yaml`，并把 `server` 改为 `https://10.43.0.1:443`（Pod 内访问 apiserver）。

## 已知不可用

- NetworkPolicy 不生效；Service 无负载均衡；NodePort 未实现（svcproxy 只处理 ClusterIP）。
- 未安装 metrics-server / Prometheus / Ingress Controller，平台中依赖这些组件的统计与告警页面会显示未接入。
