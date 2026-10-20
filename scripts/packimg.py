#!/usr/bin/env python3
"""把 rootfs 目录打包成 docker-archive 镜像 tar，供 ctr images import 使用。

用法：IMG_REF=xingshu/matrix-server:v1.53 python scripts/packimg.py <rootfs_dir> <out.tar>
说明：Windows 文件系统没有执行位，/app/bin 下的二进制会被强制设为 0755。
"""
import hashlib
import io
import json
import os
import sys
import tarfile
import time

root = sys.argv[1]
out = sys.argv[2]
ref = os.environ.get("IMG_REF", "xingshu/matrix-server:dev")

config = {
    "architecture": "amd64",
    "os": "linux",
    "config": {
        "Env": [
            "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "TZ=Asia/Shanghai",
            "MATRIX_STATIC_DIR=/app/public",
        ],
        "Entrypoint": ["/app/bin/matrix-server"],
        "Cmd": ["serve", "-config", "/etc/matrix/config.yaml"],
        "ExposedPorts": {"8080/tcp": {}},
        "WorkingDir": "/app",
        "User": "matrix",
    },
    "rootfs": {"type": "layers", "diff_ids": []},
    "history": [{"created": "2026-10-20T00:00:00Z", "created_by": "xingshu packimg"}],
}


def add_dir(tf, path, arc):
    for name in sorted(os.listdir(path)):
        full = os.path.join(path, name)
        target = arc + "/" + name
        if os.path.islink(full):
            tf.addfile(tf.gettarinfo(full, target))
        elif os.path.isdir(full):
            tf.addfile(tf.gettarinfo(full, target))
            add_dir(tf, full, target)
        else:
            ti = tf.gettarinfo(full, target)
            if target.startswith("./app/bin/"):
                ti.mode = 0o755
            elif ti.mode & 0o400 == 0:
                ti.mode = 0o644
            tf.addfile(ti, open(full, "rb"))


# 构建前校验：前端静态目录必须完整，缺文件会退化成"无 API 客户端"的演示外壳
public_dir = os.path.join(root, "app", "public")
if os.path.isdir(public_dir):
    files = [f for f in os.listdir(public_dir) if os.path.isfile(os.path.join(public_dir, f))]
    if len(files) < 10 or "xs-api.js" not in files:
        sys.exit("packimg: app/public 不完整（%d 个文件，缺 xs-api.js？），拒绝打包" % len(files))

layer = out + ".layer.tar"
with tarfile.open(layer, "w", format=tarfile.PAX_FORMAT) as tf:
    add_dir(tf, root, ".")

h = hashlib.sha256()
with open(layer, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
config["rootfs"]["diff_ids"] = ["sha256:" + h.hexdigest()]
config["created"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

cfg = json.dumps(config).encode()
cfg_name = hashlib.sha256(cfg).hexdigest()[:24] + ".json"
manifest = [{"Config": cfg_name, "RepoTags": [ref], "Layers": ["layer.tar"]}]

with tarfile.open(out, "w") as tf:
    def add(name, data):
        ti = tarfile.TarInfo(name)
        ti.size = len(data)
        ti.mtime = int(time.time())
        tf.addfile(ti, io.BytesIO(data))

    add(cfg_name, cfg)
    add("manifest.json", json.dumps(manifest).encode())
    tf.add(layer, arcname="layer.tar")

os.remove(layer)
print("image tar: %s (%d bytes) ref: %s" % (out, os.path.getsize(out), ref))
