# MCP 服务端（Model Context Protocol）

星枢 Matrix 把 AI 工具注册表（与平台内 AI 对话相同的工具）以 MCP 服务端的形式开放给外部 AI 客户端（Claude Code、Cursor 等）。

| 项 | 值 |
|---|---|
| 端点 | `POST /mcp`（与 `/api/v1` 同域，例如 `https://matrix.example.com/mcp`） |
| 传输 | Streamable HTTP，无状态，仅 POST；`GET` 等其他方法返回 `405`（不提供 SSE 流） |
| 协议 | JSON-RPC 2.0，MCP 协议版本 `2025-06-18`（兼容 `2025-03-26`） |
| 请求体 | `Content-Type: application/json`，上限 1 MiB（超出返回 `413`） |
| 批量 | 支持 JSON 数组批量请求；只含通知的请求返回 `202 Accepted` 且无响应体 |

## 鉴权

- 只接受 **API Token**：`Authorization: Bearer <你的 API Token>`。在「个人中心 → 访问令牌」创建。
- 不接受 Cookie 登录态，也不接受网页登录会话令牌，均返回 `401`。
- 令牌不能放在 URL 查询参数里。
- 若请求带 `Origin` 头，必须与服务同源（或在服务端白名单内），否则返回 `403`（防 DNS 重绑定）。
- 每个用户独立令牌桶限速（默认 5 次/秒，突发 20），超出返回 `429` 并带 `Retry-After`。
- 若请求带 `MCP-Protocol-Version` 头，值必须是受支持的版本，否则 `400`。

## 方法

| 方法 | 说明 |
|---|---|
| `initialize` | 返回 `protocolVersion`、`capabilities.tools`、`serverInfo`（`title: 星枢 Matrix`） |
| `notifications/initialized` | 通知，无响应 |
| `ping` | 返回 `{}` |
| `tools/list` | 只列出当前令牌持有者有权限的工具；`annotations.readOnlyHint` / `destructiveHint` 标明是否只读 |
| `tools/call` | 调用工具，见下 |

### tools/call 行为

- 无权限：返回 `isError: true` 的结果「权限不足：调用该工具需要权限 …」，工具不执行。
- 未知工具、参数不是对象：JSON-RPC 错误 `-32602`。
- **需要确认的工具（会改变状态的操作）不会通过 MCP 执行**：返回 `isError: false` 的文本「该操作需在平台内确认」，附平台确认路径（如 `/api/v1/ai/tool-calls/<id>/confirm`），`structuredContent.status = "pending_confirmation"`。请登录星枢 Matrix，在 AI 页面确认或拒绝。
- 工具执行失败：`isError: true`，文本为错误信息。
- 每次调用写审计日志（用户、令牌、工具、脱敏后的参数摘要、结果、耗时）；密码、令牌、密钥类字段记为 `***`。

### 错误码

| 码 | 含义 |
|---|---|
| `-32700` | 解析错误（非法 JSON / 空请求体） |
| `-32600` | 无效请求（`jsonrpc` 不是 `"2.0"`、缺 `method`、`id` 为 null、空批量等） |
| `-32601` | 方法不存在 |
| `-32602` | 参数无效 / 未知工具 |
| `-32603` | 内部错误 |

## 客户端配置

### Claude Code

```bash
claude mcp add --transport http xingshu-matrix https://matrix.example.com/mcp \
  --header "Authorization: Bearer <你的 API Token>"
```

或在项目 `.mcp.json`：

```json
{
  "mcpServers": {
    "xingshu-matrix": {
      "type": "http",
      "url": "https://matrix.example.com/mcp",
      "headers": { "Authorization": "Bearer <你的 API Token>" }
    }
  }
}
```

### Cursor

`~/.cursor/mcp.json`（或项目内 `.cursor/mcp.json`）：

```json
{
  "mcpServers": {
    "xingshu-matrix": {
      "url": "https://matrix.example.com/mcp",
      "headers": { "Authorization": "Bearer <你的 API Token>" }
    }
  }
}
```

### curl 自检

```bash
curl -s https://matrix.example.com/mcp \
  -H "Authorization: Bearer <你的 API Token>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

> 建议为 MCP 单独创建权限最小的令牌，并设置过期时间；令牌泄露时在「访问令牌」页吊销。
