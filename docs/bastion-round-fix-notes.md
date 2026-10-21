# 本轮改造问题记录（登录页 + 堡垒机 + beta 推送）

记录本轮执行「完成登录页与堡垒机改造并推送 beta」时实际遇到的 5 个问题，按固定四段说明：报错问题、原因、解决方案、如何避免。

---

## 问题 1：`go build ./...` 链接阶段磁盘空间不足

### 报错问题

```
# github.com/yangguangdijia/xingshu-matrix/server/cmd/matrix-server
D:\GO\pkg\tool\windows_amd64\link.exe: resize output file failed:
truncate $WORK\b207\exe\a.out.exe: There is not enough space on the disk.
```

`go vet ./internal/modules/bastion/` 正常，只有需要链接二进制的 `go build` / `go test` 失败。

### 原因

Go 默认把编译缓存与链接临时目录放在系统盘（`%LOCALAPPDATA%\go-build`、`%TEMP%`）。C 盘剩余空间不足以容纳 `matrix-server` 的链接产物与全量测试二进制，链接器 `truncate` 失败。

不是代码错误：同一个包 `go vet` 通过、`go test ./internal/modules/bastion/` 通过。

### 解决方案

把 Go 的缓存与临时目录整体指到 D 盘工作区（`.evox-cache/`，不提交）：

```bash
cd D:/EvoX/xingshu-matrix
mkdir -p .evox-cache/go-build .evox-cache/go-tmp
cd server
export GOCACHE=$(cygpath -w ../.evox-cache/go-build)
export GOTMPDIR=$(cygpath -w ../.evox-cache/go-tmp)
export GOPATH=$(cygpath -w ../.evox-cache/go-mod)
export GOMODCACHE=$(cygpath -w ../.evox-cache/go-mod/pkg/mod)
go test ./...
go vet ./...
```

关键参数说明：

| 变量 | 值 | 作用 |
|---|---|---|
| `GOCACHE` | `<repo>/.evox-cache/go-build` | 编译产物缓存，占空间最大 |
| `GOTMPDIR` | `<repo>/.evox-cache/go-tmp` | 链接临时文件 `$WORK\...\a.out.exe` |
| `GOPATH` | `<repo>/.evox-cache/go-mod` | 模块工作区根 |
| `GOMODCACHE` | `<repo>/.evox-cache/go-mod/pkg/mod` | 依赖源码缓存 |

Windows 下必须先用 `cygpath -w` 把 POSIX 路径转成 `D:\...`，否则 Windows 原生的 `link.exe` 读不到自定义路径。

### 如何避免

1. 本仓库统一用上面的 4 个变量跑 Go 命令，写进本地脚本而不是每次手敲。
2. 提交前确认 `.evox-cache/` 在 `.gitignore` 内，且 `git status --short` 不出现它。
3. 出现 `resize output file failed` / `not enough space` 时，先怀疑缓存盘，不要先怀疑代码。

---

## 问题 2：计划进度与仓库实际状态不一致

### 报错问题

计划「完成登录页与堡垒机改造并推送 beta」显示 11/21 步完成，但仓库实际已经推进到 `f1b4b7a fix(deploy): 用干净代码重建镜像 v1.62`，`beta` 与 `origin/beta` 同步、工作区干净。计划里标记为未完成的若干步骤（删除重复登录 CSS、抽屉滚动重置、协议感知资产表单）在代码中已经实现。

### 原因

计划是在较早的会话里写的，之后又有多轮提交（包含部署相关），但没有回写勾选状态。计划文件是"当时的快照"，不是"当前的真相"。

### 解决方案

先做一次只读对账，再决定动手范围：

```bash
git status --short
git log --oneline -6
git branch -vv
grep -c "^\s*\.x-auth\s*{" public/index.html          # 期望 1
grep -n "x-dbody.*scrollTop\|scrollTo(0, 0)" public/index.html
grep -n "function showAuth\|function renderTerm\|function basConnect" public/index.html
ls server/internal/modules/bastion/migrations/
```

对账结论（本轮实际缺口）：

| 计划步骤 | 实际状态 |
|---|---|
| 删除重复 `.x-auth` / `.x-ashell` / `.x-art` / `.x-formp` | 已完成，各只有 1 处声明 |
| 抽屉重绘后 `.x-dbody.scrollTo(0,0)` | 已在 9 处实现 |
| 协议感知资产表单 / `BAS_PROTO` | 已实现 |
| 登录/改密 label `for`/`id`、`#cerr` 的 `aria-live` | 缺失，本轮补齐 |
| 终端空态访问工作台 | 只有一行"还没有会话"，本轮重做 |
| `BAS_CONN()` 动态探测 + 真实 SSH | 硬编码全 `false`，本轮重做 |
| 协议参数持久化 | 只在前端内存，本轮加 `options` |
| `docs/bastion-product-patterns.md` | 不存在，本轮新增 |
| 堡垒机测试扩展 | 只有 5 个用例，本轮补 7 个 |

### 如何避免

1. 每次续做计划前先跑"对账四件套"：`git status`、`git log`、关键符号 `grep`、目录 `ls`。
2. 计划是快照，勾选状态以代码为准；发现计划超前或滞后，先修计划再动手。
3. 提交信息里写清"本轮改了什么/没改什么"，便于下次对账。

---

## 问题 3：`BAS_CONN()` 硬编码全 false，live 模式下 SSH 也无法连接

### 报错问题

```js
const BAS_CONN = () => ({ ssh: false, sftp: false, rdp: false, vnc: false, mysql: false, postgresql: false, redis: false });
```

`basConnect()` 里 `if (!demo && !BAS_CONN()[proto.toLowerCase()]) return basNoConnector(...)`。`demo` 恒为 `false`，所以**任何**协议都会被判定为"连接器未配置"，即使后端 bastion 模块与 SSH WebSocket 完全可用。

### 原因

这段代码是为"静态预览下不伪造连接"写的，但把"后端不可用"和"后端可用"合并成了同一个常量，等于把安全默认值写死成了永久状态。后端的 `GET /bastion/sessions/ssh/ws` 已经实现，前端却永远走不到它。

### 解决方案

1. 引入真实探测 `basProbeConnectors()`：
   - `GET /bastion/my/assets` → 200 记 `可用`，401/403 记 `未授权`，其他记 `未连接`；
   - `GET /bastion/guacd/health` → `reachable:true` 记 `可用`，`false` 记 `未配置`，异常记 `无法确认`；
   - 未接入 `/api/v1` 时统一记 `未连接`。
2. `BAS_CONN()` 改为读取探测结果，而不是常量；SFTP 显式保持 `false`（后端没有 SFTP 接口）。
3. live 模式 `basConnect()` 走 `basLiveConnect()` → `basLiveSSH()`，用 `XS.ws("/bastion/sessions/ssh/ws", {asset, account, cols, rows})` 建立真实会话。
4. 会话失败时显示真实原因（`s.err`），不渲染成功态；关闭时断开 WebSocket，审计由后端 `bastion_sessions` 记录，前端不补写演示行。

消息契约（与 `ssh.go` 一致）：

| 方向 | 消息 |
|---|---|
| 浏览器 → 服务端 | `{"type":"input","data":"ls\r"}`、`{"type":"resize","cols":120,"rows":32}` |
| 服务端 → 浏览器 | `{"type":"output","data":"..."}`、`{"type":"error","data":"..."}`、`{"type":"closed","data":"killed"}` |

### 如何避免

1. 安全默认值写成"未探测"，不要写成 `false` 常量；未探测 → 不允许连接，探测成功 → 允许。
2. 任何 `xxx: false` 的能力表都要写明"什么时候会变 true"，否则就是死代码。
3. 新增协议时同时更新：`BAS_CONN_STATE`、`BAS_CONN()`、`basNoConnector()` 文案、`docs/api/bastion.md`。

---

## 问题 4：协议参数只存在前端内存，刷新即丢，详情抽屉取不到

### 报错问题

`basNewAsset()` 把域、TLS、库名、只读、跳板、资产组等写入 `BAS_META[name]`（页面内对象），资产行写入 `adm("资产").rows`（演示数组）。后端 `bastion_assets` 只有 `name/host/port/protocol/env/description/host_key` 七列。后果：

- 刷新页面后协议参数全部丢失；
- `basAssetDrawer()` 在 live 模式下按名字查 `adm("资产").rows`，查不到就 `toast("资产不存在或已移除")`；
- 资产页显示的是演示数组，不是后端真实资产。

### 原因

早期只做交互原型，协议字段没有对应列；后来补了表单但没有补持久化路径。

### 解决方案

1. 新增向后兼容迁移 `0002_asset_options.up.sql`：

```sql
ALTER TABLE bastion_assets ADD COLUMN IF NOT EXISTS options JSONB NOT NULL DEFAULT '{}'::jsonb;
```

2. `Asset` 与 `assetIn` 增加 `Options map[string]any`，`createAsset` / `updateAsset` 的 SQL 增加 `options=$8/$9`。
3. `assetIn.check()` 增加三条约束：
   - `nil` → 归一化为 `{}`（避免 NOT NULL 失败）；
   - 序列化后 > 4096 字节 → 400 `options is too large (max 4096 bytes)`；
   - 键名（大小写不敏感）命中 `secret`/`password`/`passphrase`/`private_key`/`privatekey`/`token`/`kubeconfig`/`credential` → 400 `options must not contain credentials: <key>`。
4. 前端 `basNewAsset()` 在 live 模式真实提交：`POST /bastion/assets`（带 `options`）→ `POST /bastion/accounts`（凭据单独提交）→ `basLiveAssets(true)` 刷新缓存。
5. live 资产页改读 `GET /bastion/assets?size=200`，状态列显示「已登记」而不是「在线」；详情用 `basLiveAssetDlg()` 只展示后端已持久化字段。

### 如何避免

1. 表单新增字段时，同时回答三个问题：存哪列、谁读它、没权限时显示什么。
2. 任何"前端看起来有、后端其实没有"的字段，都必须在 UI 上标注"仅本地"。
3. 迁移必须用 `ADD COLUMN IF NOT EXISTS` + `DEFAULT`，保证老数据可读。
4. 凭据类字段永远不进入 `options`，服务端做关键字拒绝，不能只靠前端自觉。

---

## 问题 5：前端脚本解析把 JSON 数据块当成 JS，误报语法错误

### 报错问题

```
script#2 near line 1747: Unexpected token ':'
script#3 near line 1748: Unexpected token ':'
```

### 原因

`public/index.html` 里有两段 `<script type="application/json" id="ops-demo">` 与 `id="obs-demo"`，用于承载演示数据。解析脚本只按 `<script>` 标签提取内容并 `new Function(code)`，把 JSON 当 JavaScript 解析，必然在第一个 `:` 报错。

### 解决方案

解析时排除非 JS 的 `type` 与带 `src` 的标签：

```js
const re = /<script([^>]*)>([\s\S]*?)<\/script>/gi;
while ((m = re.exec(html))) {
  const attrs = m[1] || "";
  if (/\bsrc=/.test(attrs)) continue;
  const type = (attrs.match(/type\s*=\s*"([^"]*)"/) || [])[1] || "";
  if (type && !/javascript|module/i.test(type)) continue;
  new Function(m[2]); // 只解析真实 JS
}
```

修正后：`public/index.html` 4 个 JS 块、`hark.js`、`chart.js`、`datalabels.js`、`hark-charts.js`、`xs-api.js`、`xs-live-core.js`、`xs-live-ops.js` 全部 `failed=0`。

### 如何避免

1. 脚本解析永远按 `type` 过滤，不假设所有 `<script>` 都是 JS。
2. 把解析脚本固化为提交前检查项，输出 `js blocks=N failed=0` 作为通过条件。
3. 解析失败先看报错行号对应的标签类型，再怀疑模板字符串。

---

## 本轮最终验证清单

| 检查项 | 命令 | 结果 |
|---|---|---|
| 堡垒机单测 | `go test ./internal/modules/bastion/ -count=1` | 通过（12 个用例） |
| 后端全量测试 | `go test ./...` | 见提交说明（使用 D 盘缓存变量） |
| 静态检查 | `go vet ./...` | 见提交说明 |
| 前端脚本解析 | 上述 Node 脚本 | `failed=0` |
| 空白/冲突标记 | `git diff --check` | 无输出 |
| 敏感信息扫描 | `git diff --cached` 人工复查 | 无密码/私钥/kubeconfig/token |

未验证（本轮明确不做）：真实 PostgreSQL、Kubernetes、Prometheus、日志、链路、Jenkins、Kafka、真实 SSO、真实 SSH 目标机、guacd RDP/VNC 代理。

部署侧的问题（集群仍跑 v1.62、Argo `Synced` 但清单指向旧 tag、kubeconfig 证书不匹配、scratch 镜像无 shell、
`last-applied-configuration` 漂移到 v1.57）单独记录在
[deploy-v1.63-bastion-workbench.md](deploy-v1.63-bastion-workbench.md)，本文不重复。
