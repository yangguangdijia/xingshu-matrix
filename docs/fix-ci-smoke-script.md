# 修复记录：CI 引用了不存在的测试脚本（`scripts/smoke-test.mjs`）

> 修复版本：v1.16
> 涉及文件：`.github/workflows/docker.yml`
> 影响范围：GitHub Actions 的 `build-image` 工作流（push `main`、打 `v*` 标签、手工触发）

---

## 一、报错问题

### 1.1 现象

工作流在第一步就失败，镜像从未被自动构建：

```
Run cd scripts && npm i jsdom@24 --no-save && node smoke-test.mjs
Could not find 'smoke-test.mjs'
Error: Process completed with exit code 1.
```

### 1.2 复现方式

```bash
# 在原仓库根目录执行工作流里的那一行
cd scripts && npm i jsdom@24 --no-save && node smoke-test.mjs
```

`scripts/` 目录的真实内容只有两个文件：

| 文件 | 语言 | 用途 |
| --- | --- | --- |
| `scripts/package.sh` | bash | 打前端离线包，产物 `dist/xingshu-matrix-<VER>.tar.gz` |
| `scripts/smoke-test.sh` | bash | 部署后冒烟（`/healthz`、首页、静态资源），需要 curl + 运行中的服务 |

即：**`smoke-test.mjs` 在整个仓库历史里从未存在过**，它不是被误删，而是从第一次提交
（`57ff18e`，2026-10-15）起就写错了路径。

### 1.3 完整报错上下文

工作流定义（修复前）：

```yaml
steps:
  - uses: actions/checkout@v4
  - name: Smoke test
    run: cd scripts && npm i jsdom@24 --no-save && node smoke-test.mjs
  - uses: docker/login-action@v3
    ...
```

由于该步骤在 `docker/login-action` 与 `docker/build-push-action` 之前，**后续步骤全部被跳过**，
所以 `ghcr.io/<owner>/xingshu-matrix:latest` 一直没有更新，且失败与代码质量无关，容易被当成"CI 抖动"忽略。

---

## 二、原因

| # | 直接原因 | 具体证据 |
| --- | --- | --- |
| 1 | 脚本路径写错 | 工作流指向 `scripts/smoke-test.mjs`；实际的前端测试在 `tests/frontend/`（`smoke.mjs`、`xs-api.test.mjs`、`mock-server.mjs`），部署冒烟在 `scripts/smoke-test.sh`（`.sh` 不是 `.mjs`） |
| 2 | 测试文件后来搬了家，工作流没跟着改 | `tests/frontend/` 下的测试均为 `.mjs`，与工作流里 `cd scripts` 的工作目录不一致 |
| 3 | 安装了一个用不到的依赖 | `npm i jsdom@24` 是为浏览器 DOM 模拟准备的，但 `xs-api.test.mjs` 只用 `node:test` / `node:assert/strict` / `node:fs` / `node:vm`，**没有任何第三方依赖**（`tests/frontend/xs-api.test.mjs:1-4`） |
| 4 | CI 从未包含后端检查 | 整个工作流只做前端检查，`go build` / `go vet` / `go test` 从来没在 CI 里跑过，后端回归只能靠人工 |
| 5 | 浏览器级冒烟不适合放进 CI 默认步骤 | `tests/frontend/smoke.mjs` 需要真实 Chrome（默认路径 `C:\Program Files\Google\Chrome\Application\chrome.exe`）与 CDP 端口 19222，还要先起 `mock-server.mjs`（端口 18080）；GitHub 托管 runner 没有现成的 Chrome 路径，必须显式安装与配置 |

结论：这不是环境问题，而是**工作流脚本与仓库实际文件布局脱节**，且无人验证过 CI 是否绿。

---

## 三、解决方案

### 3.1 改后的工作流（完整）

```yaml
name: build-image
on:
  push:
    branches: [main]
    tags: ["v*"]
  workflow_dispatch:
permissions:
  contents: read
  packages: write
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.23"
          cache-dependency-path: server/go.sum
      - name: Go build & test
        working-directory: server
        run: |
          go build ./...
          go vet ./...
          go test ./...
      - name: Frontend unit test
        run: node --test tests/frontend/xs-api.test.mjs
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ghcr.io/${{ github.repository_owner }}/xingshu-matrix:latest
```

### 3.2 每一处参数的确切含义

| 参数 | 值 | 为什么这么设 |
| --- | --- | --- |
| `actions/setup-go@v5` → `go-version` | `"1.23"` | 与 `Dockerfile` 的 `FROM golang:1.23-alpine`、`server/go.mod` 的 `go` 指令保持一致；本地实测 Go 1.25 也能构建，但 CI 对齐构建镜像版本 |
| `cache-dependency-path` | `server/go.sum` | Go 模块缓存的 key 来源；仓库的 `go.mod` 在 `server/` 而不是根目录，不写这项会导致缓存命中率为 0 |
| `working-directory` | `server` | 所有 go 命令必须在 `server/` 下执行，否则 `go build ./...` 找不到 `go.mod` |
| `go build ./...` | — | 编译全部包，包括 `cmd/matrix-server` 与 `cmd/matrix-agent` |
| `go vet ./...` | — | 静态检查，`printf` 参数、锁拷贝、无用赋值等问题 |
| `go test ./...` | — | 当前 7 个包有测试（`cmd/matrix-agent` + bastion / cicd / cluster / hosts / middleware / observability），其余为 `[no test files]`，不会失败 |
| `node --test` | `tests/frontend/xs-api.test.mjs` | Node 18+ 内置测试运行器；该文件零依赖，无需 `npm i` |
| 去掉 `npm i jsdom@24` | — | 没有任何测试使用 jsdom，安装只会拖慢 CI 并引入供应链风险 |

### 3.3 为什么浏览器冒烟没进 CI

`tests/frontend/smoke.mjs` 是"真实浏览器 + mock API"的端到端冒烟，它的前置条件是：

1. `CHROME_PATH` 指向可执行 Chrome（默认 Windows 路径，Linux runner 上不存在）；
2. 先起 `node tests/frontend/mock-server.mjs`（`MOCK_PORT` 默认 18080）；
3. `SMOKE_URL` 指向该服务（默认 `http://127.0.0.1:18080/`）；
4. CDP 调试端口可占用（`CDP_PORT` 默认 19222）；
5. 首次运行会写入基线截图 `tests/frontend/smoke-login.png`，比较逻辑对字体渲染敏感。

托管 runner 的字体与本地不同，容易假失败，因此**保留为手工执行**，并在 README「质量门禁」里写明命令。
如果以后要进 CI，建议单独一个 `smoke` job，用 `ubuntu-latest` + `browser-actions/setup-chrome`，
并允许 3% 像素差异阈值。

### 3.4 本地等价验证（本次实际执行结果）

在 Windows + Git Bash 下逐条跑过与 CI 相同的命令：

```
cd server && go build ./...        → 无输出，退出码 0
cd server && go vet ./...          → 无输出，退出码 0
cd server && go test ./...         → cmd/matrix-agent + bastion/cicd/cluster/hosts/middleware/observability 七个包 ok
node --test tests/frontend/xs-api.test.mjs
                                   → tests 5 / pass 5 / fail 0
```

> Windows 提示：在 Git Bash 里 `node public/xs-live-core.js` 可能报 `EISDIR: lstat 'D:'`，
> 那是 MSYS 路径转换问题，用 `node "$(cygpath -m "$PWD")/public/xs-live-core.js"` 传绝对路径即可。

---

## 四、如何避免

1. **CI 不在本地跑通就不许合入**：把上面的命令固化成本地脚本（`make test` 已存在 `go test ./...`，
   建议再加 `make ci` 串联 `build / vet / test / node --test`），提交前跑一次。
2. **工作流里禁止出现未经验证的路径**：任何 `run:` 里引用的文件，都必须在同一个 PR 里被 `ls` 或测试使用过；
   评审时把"路径存在性"当硬性检查项。
3. **测试文件只有一个位置**：前端测试统一放 `tests/frontend/`，不要同时出现在 `scripts/`；
   部署用脚本统一 `.sh`，测试统一 `.mjs`，靠后缀区分。
4. **不装用不到的依赖**：`npm i` 前先确认测试 import 里真的用到了它；零依赖能跑就不要引入。
5. **给 CI 加"会失败"的守门**：分支保护要求 `build-image` 必须绿；同时在 README 放 CI 徽章，
   让坏掉的工作流立刻可见。
6. **打 tag 前先看 Actions**：`v*` 标签会触发构建，发布前确认最近一次 `main` 的流程是绿的，
   否则会推出一个旧镜像。
7. **镜像 tag 与版本号绑定**（尚未完成，见 `docs/PROGRESS.md §9.1`）：当前只推 `:latest`，
   出问题无法回滚定位；建议按 `v1.16` 打 tag，并用 `org.opencontainers.image.revision` 记录 commit。
8. **CI 里保留一次"从零开始"的验证**：删除缓存后手动 `workflow_dispatch` 跑一次，
   确认不是靠缓存掩盖了缺失依赖。
