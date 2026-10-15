# http-relay

一个 **HTTP 中继服务端**：将收到的请求原样透传到请求头 `X-Upstream-Target`（或查询参数 `upstream_target`）指定的上游地址，并把上游响应流式写回客户端。

**核心定位：纯粹 HTTP 透传，无业务处理。** 不限定转发内容，任何路径、任何上游均可。

- ✅ 转发目标由请求头 `X-Upstream-Target` 或查询参数 `upstream_target` 指定（**必传**，请求头优先）
- ✅ SSE 流式响应完整透传（读到一块立刻写回，不缓存响应体）
- ✅ 原样转发请求/响应 Header 与 Body，保留 HTTP 状态码
- ✅ 仅使用 Go 标准库，无第三方依赖

**明确不做的事：**

- ❌ 不解析、不修改请求/响应 JSON
- ❌ 不做 token 统计、prompt 改写、协议转换
- ❌ 不做鉴权（纯透传）
- ❌ 不做负载均衡、多上游路由策略

## 构建

```bash
go build -o http-relay .
```

## 启动

```bash
./http-relay --listen-addr 127.0.0.1:8080

# Windows
http-relay.exe --listen-addr 127.0.0.1:8080
```

### 命令行参数

| Flag | 默认值 | 说明 |
|---|---|---|
| `--listen-addr` | `127.0.0.1:8080` | 监听地址；`0.0.0.0` / `::` / 空 host 会拒绝启动，需指定具体 IP |
| `--enable-https` | `false` | 显式启用入站 HTTPS；启用时必须提供 `--tls-cert` 与 `--tls-key` |
| `--tls-cert` | 空 | 入站 HTTPS 证书文件（PEM）；仅在 `--enable-https` 时生效 |
| `--tls-key` | 空 | 入站 HTTPS 私钥文件（PEM）；仅在 `--enable-https` 时生效 |

日志使用 `log` 包全局日志输出到 stderr，每请求一条访问日志（方法、路径、上游目标、状态码、耗时）。

## 调用示例

### 指定上游地址（X-Upstream-Target，必传）

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "X-Upstream-Target: https://api.anthropic.com" \
  -H "Authorization: Bearer sk-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"...","messages":[...]}'
```

中继读取 `X-Upstream-Target`，把整个请求转发到 `https://api.anthropic.com/v1/chat/completions`。不带该头返回 `400`。

### 通过查询参数指定上游（upstream_target，同作用）

不便设请求头时（如浏览器/简单 GET 工具），可用查询参数：

```bash
curl "http://127.0.0.1:8080/v1/models?limit=1&upstream_target=https%3A%2F%2Fapi.openai.com"
```

规则：

- 请求头与查询参数**同时存在时，请求头优先**
- `upstream_target` 与 `X-Upstream-Target` 一样不会转发给上游（从转发查询串中剥除）
- 注意：使用查询参数指定目标时，其余查询参数会被重新编码并按 key 排序（不影响语义，仅字节表示可能变化）

### SSE 流式

```bash
curl -N http://127.0.0.1:8080/v1/messages \
  -H "X-Upstream-Target: https://api.anthropic.com" \
  -H "x-api-key: sk-ant-xxx" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-5","stream":true,"messages":[...]}'
```

流式响应逐块透传，`curl -N`（或客户端 SDK 的 stream 模式）可实时收到每个 SSE chunk。

## HTTPS 支持说明

两个方向都支持：

1. **出站 HTTPS（中继 → 上游）**：默认支持。`X-Upstream-Target` 使用 `https://` 地址即可（如 `https://api.anthropic.com`），走标准 TLS + HTTP/2。
2. **入站 HTTPS（客户端 → 中继）**：`--enable-https` + 证书启用，例如：

```bash
./http-relay --listen-addr 127.0.0.1:8443 \
  --enable-https \
  --tls-cert /etc/http-relay/server.pem \
  --tls-key  /etc/http-relay/server.key
```

之后客户端用 `https://127.0.0.1:8443/...` 访问（证书域名需匹配，或客户端侧信任该证书）。

## 超时行为

中继**不设置任何超时**，入站 `http.Server` 与出站 `http.DefaultClient` 均使用 Go 默认行为，以完整支持 SSE 长流式响应不被中途切断。如需限流/超时，建议在部署层按需控制。

## 头部处理规则

| 处理 | 说明 |
|---|---|
| 删除 `X-Upstream-Target` / `upstream_target` | 中继控制头/控制参数，不转发给上游 |
| 改写 `Host` | 改为上游目标域名 |
| 删除逐跳头 | `Connection` / `Keep-Alive` / `Transfer-Encoding` / `Upgrade` 等（含 `Connection` 头中声明的扩展头） |
| 重算 `Content-Length` | 由 net/http 按实际 Body 计算，避免冲突 |
| 其余 Header / 查询参数 | 原样转发（`Authorization`、`x-api-key`、`anthropic-version` 等） |

## 错误处理

| 场景 | 行为 |
|---|---|
| 缺少 `X-Upstream-Target` 头与 `upstream_target` 查询参数 | `400 Bad Request` |
| `X-Upstream-Target` 非法 URL（解析失败 / 非 http-https / 缺 host） | `400 Bad Request` + 错误说明 |
| 上游连接失败 | `502 Bad Gateway` + 错误说明 |
| 上游返回任意状态码 | 原样回传（含 4xx/5xx） |

## 测试

```bash
go test ./...
```

覆盖：基础正向转发（Header/Body/状态码透传、Host 改写、控制头清理）、上游基路径拼接、查询参数指定目标（含参数剥除、请求头优先级）、缺目标 400、非法目标 400、上游不可达 502、出站 HTTPS 上游、入站 TLS 监听、SSE 流式透传（验证未缓冲响应体）、监听地址校验。
