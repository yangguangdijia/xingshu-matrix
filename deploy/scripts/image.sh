#!/usr/bin/env bash
# 星枢 Matrix 镜像后勤脚本
#
# 部署时"先看本地有没有镜像，没有才导入/拉取/构建"，避免每次联网拉镜像。
#
# 用法：
#   ./image.sh ensure            # 确保镜像存在（本地 → 离线包 → 拉取 → 构建）
#   ./image.sh has               # 只检查，存在返回 0
#   ./image.sh load              # 强制从离线包导入
#   ./image.sh save              # 把当前镜像导出到 deploy/images/ 下
#   ./image.sh list              # 列出本地相关镜像
#
# 环境变量：
#   IMAGE            目标镜像，默认 docker.io/xingshu/matrix-server:v1.3
#   SRC_TAG          离线包里记录的镜像名，默认 docker.io/xingshu/matrix-server:v40
#   TAR              离线包路径，默认 deploy/images/matrix-server-v40.tar.gz
#   CONTAINERD_NS    containerd 命名空间，k3s 用 k8s.io，默认 k8s.io
#   REGISTRY         拉取时的仓库前缀，默认 docker.io
#   VERSION          构建时写入二进制的版本号

set -euo pipefail

IMAGE="${IMAGE:-docker.io/xingshu/matrix-server:v1.3}"
SRC_TAG="${SRC_TAG:-docker.io/xingshu/matrix-server:v40}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TAR="${TAR:-$HERE/../images/matrix-server-v40.tar.gz}"
CONTAINERD_NS="${CONTAINERD_NS:-k8s.io}"
VERSION="${VERSION:-v1.3}"

# 运行时探测：优先 containerd（k3s），其次 docker，最后 crictl
runtime() {
  if command -v ctr >/dev/null 2>&1; then echo ctr
  elif command -v docker >/dev/null 2>&1; then echo docker
  elif command -v crictl >/dev/null 2>&1; then echo crictl
  else echo none
  fi
}

has_image() {
  case "$(runtime)" in
    ctr)    ctr -n "$CONTAINERD_NS" images ls -q 2>/dev/null | grep -qx "$IMAGE" ;;
    docker) docker image inspect "$IMAGE" >/dev/null 2>&1 ;;
    crictl) [ -n "$(crictl images -q "$IMAGE" 2>/dev/null)" ] ;;
    *)      return 1 ;;
  esac
}

import_tar() {
  [ -f "$TAR" ] || { echo "[image] 离线包不存在：$TAR" >&2; return 1; }
  echo "[image] 从离线包导入：$TAR"
  case "$(runtime)" in
    ctr)
      # 离线包是 gzip 压缩的 OCI tar；containerd 不会自动解压，先判断再解压导入
      case "$TAR" in
        *.gz|*.tgz)
          # containerd 不支持从 stdin 读压缩流，必须先落盘解压再导入
          local tmp; tmp="$(mktemp /tmp/matrix-img-XXXXXX.tar)"
          gzip -dc "$TAR" > "$tmp"
          ctr -n "$CONTAINERD_NS" images import "$tmp" >/dev/null
          rm -f "$tmp" ;;
        *)          ctr -n "$CONTAINERD_NS" images import "$TAR" >/dev/null ;;
      esac
      # 离线包内记录的 tag 与目标 tag 可能不同（例如 v40 → v1.3），这里补齐别名。
      # containerd 里同一个镜像可能以 "xingshu/xxx" 或 "docker.io/xingshu/xxx" 两种名字存在，
      # 因此源 tag 找不到时自动去掉 docker.io/ 前缀再试一次。
      if [ "$SRC_TAG" != "$IMAGE" ] && ! has_image; then
        local src="$SRC_TAG"
        ctr -n "$CONTAINERD_NS" images ls -q | grep -qx "$src" || src="${SRC_TAG#docker.io/}"
        if ctr -n "$CONTAINERD_NS" images tag "$src" "$IMAGE" >/dev/null 2>&1; then
          echo "[image] 已补 tag 别名：$src → $IMAGE"
        else
          echo "[image] 警告：无法把 $src 别名为 $IMAGE" >&2
        fi
      fi
      ;;
    docker) case "$TAR" in *.gz|*.tgz) gunzip -c "$TAR" | docker load ;; *) docker load -i "$TAR" ;; esac ;;
    crictl) case "$TAR" in *.gz|*.tgz) gzip -dc "$TAR" | ctr -n "$CONTAINERD_NS" images import - ;; *) ctr -n "$CONTAINERD_NS" images import "$TAR" ;; esac ;;
    *) return 1 ;;
  esac
}

pull_image() {
  echo "[image] 本地无镜像，尝试拉取：$IMAGE"
  case "$(runtime)" in
    ctr)    ctr -n "$CONTAINERD_NS" images pull "$IMAGE" >/dev/null ;;
    docker) docker pull "$IMAGE" >/dev/null ;;
    crictl) crictl pull "$IMAGE" >/dev/null ;;
    *) return 1 ;;
  esac
}

build_image() {
  local root
  root="$(cd "$HERE/../.." && pwd)"
  [ -f "$root/Dockerfile" ] || { echo "[image] 找不到 Dockerfile，无法构建" >&2; return 1; }
  echo "[image] 尝试本地构建：$IMAGE"
  if command -v docker >/dev/null 2>&1; then
    docker build --build-arg VERSION="$VERSION" -t "$IMAGE" "$root"
  else
    echo "[image] 无 docker，无法本地构建；请准备离线包或可用的镜像仓库" >&2
    return 1
  fi
}

ensure_image() {
  if has_image; then
    echo "[image] 已存在：$IMAGE（跳过导入/拉取）"
    return 0
  fi
  # 顺序：离线包 → 拉取 → 构建
  if import_tar && has_image; then
    echo "[image] 已从离线包导入：$IMAGE"
    return 0
  fi
  if pull_image && has_image; then
    echo "[image] 已拉取：$IMAGE"
    return 0
  fi
  if build_image && has_image; then
    echo "[image] 已构建：$IMAGE"
    return 0
  fi
  echo "[image] 失败：既无法导入、拉取，也无法构建 $IMAGE" >&2
  return 1
}

save_image() {
  local out="$HERE/../images/matrix-server-${VERSION}.tar.gz"
  echo "[image] 导出 $IMAGE → $out"
  case "$(runtime)" in
    ctr)    ctr -n "$CONTAINERD_NS" images export /tmp/_matrix_img.tar "$IMAGE" >/dev/null && gzip -9 -f /tmp/_matrix_img.tar && mv /tmp/_matrix_img.tar.gz "$out" ;;
    docker) docker save "$IMAGE" | gzip -9 > "$out" ;;
    *) echo "[image] 当前运行时无法导出" >&2; return 1 ;;
  esac
  ls -lh "$out" | awk '{print "[image] 已生成 "$5" "$9}'
}

case "${1:-ensure}" in
  ensure) ensure_image ;;
  has)    has_image && echo "present" || { echo "absent"; exit 1; } ;;
  load)   import_tar ;;
  save)   save_image ;;
  list)   case "$(runtime)" in
            ctr) ctr -n "$CONTAINERD_NS" images ls -q | grep matrix-server || true ;;
            docker) docker images | grep matrix-server || true ;;
            crictl) crictl images | grep matrix-server || true ;;
          esac ;;
  *) echo "用法：$0 {ensure|has|load|save|list}" >&2; exit 2 ;;
esac
