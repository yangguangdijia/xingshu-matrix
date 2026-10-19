# 离线镜像包

本目录存放可直接离线部署的镜像包，避免每次部署都依赖外网。

## 内容

| 文件 | 说明 | 大小 |
| --- | --- | --- |
| `matrix-server-v40.tar.gz` | 平台主镜像，内部 tag `xingshu/matrix-server:v40`，对应代码版本 **v1.3** | 约 19 MB（压缩后） |

格式说明：**gzip 压缩的 `ctr images export` 产物**（OCI tar）。`docker load` 也能直接吃 gzip 流
（`gunzip -c xxx.tar.gz | docker load`）。

## 使用

```bash
# 部署前先跑这个：本地已有镜像就直接返回，不会重复导入/拉取
./deploy/scripts/image.sh ensure

# 只检查
./deploy/scripts/image.sh has

# 从离线包强制导入（并把 tag 别名成目标版本）
IMAGE=docker.io/xingshu/matrix-server:v1.3 ./deploy/scripts/image.sh load

# 把当前集群里的镜像重新导出到本目录
./deploy/scripts/image.sh save
```

`ensure` 的决策顺序固定为：

```
1. 本地已有目标镜像            → 直接使用（不联网、不动文件）
2. 有离线包                    → 导入 + 补 tag 别名
3. 都没有                      → 尝试从仓库拉取
4. 拉取失败                    → 尝试本地 Dockerfile 构建
5. 仍失败                      → 报错退出（非 0）
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `IMAGE` | `docker.io/xingshu/matrix-server:v1.3` | 部署时要用的镜像名 |
| `SRC_TAG` | `docker.io/xingshu/matrix-server:v40` | 离线包内记录的镜像名（不一致时脚本自动 tag 别名） |
| `TAR` | `deploy/images/matrix-server-v40.tar.gz` | 离线包路径 |
| `CONTAINERD_NS` | `k8s.io` | k3s 用 `k8s.io`，原生 containerd 可能是 `default` |
| `VERSION` | `v1.3` | `save`/`build` 时使用的版本号 |

## 注意

- 离线包是**二进制产物**，约 19 MB，提交进 git 会让仓库变大。当前按项目要求随仓库保存；
  若以后改用制品仓库（Harbor/OSS），把 `TAR` 指向下载地址即可，脚本无需改动。
- 包内 tag 与部署清单的 tag 允许不同：`image.sh` 会在导入后自动补 `docker.io/...` 别名，
  避免"导入成功但 kubelet 仍然去拉取"的经典问题。
