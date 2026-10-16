# http-relay

一个 **HTTP 中继服务端**：将收到的请求原样透传到请求头 `X-Upstream-Target`（或查询参数 `upstream_target`）指定的上游地址，并把上游响应流式写回客户端。

**核心定位：纯粹 HTTP 透传，无业务处理。** 不限定转发内容，任何路径、任何上游均可。

- ✅ 转发目标由请求头 `X-Upstream-Target` 或查询参数 `upstream_target` 指定（**必传**，请求头优先），支持**多个目标**：每个中继节点消费一个，剩余的传给下一跳，形成链式中继
- ✅ SSE 流式响应完整透传（读到一块立刻写回，不缓存响应体）
- ✅ 原样转发请求/响应 Header 与 Body，保留 HTTP 状态码
- ✅ 可选 Token 访问认证（`X-Relay-Token` 头 / `relay_token` 查询参数），token 支持**自动生成**与**手动录入**两种签发方式，支持**过期时间**与**使用次数上限**，配 Web 管理界面签发/删除
- ✅ 仅使用 Go 标准库，无第三方依赖

**明确不做的事：**

- ❌ 不解析、不修改请求/响应 JSON
- ❌ 不做 token 统计、prompt 改写、协议转换
- ❌ 不做负载均衡、多上游路由策略（多目标为**链式逐跳消费**，不是多上游选择/故障转移）
- ❌ 认证只区分"是否放行"，不区分 token 权限、不限制可访问的上游

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
| `--listen-addr` | `127.0.0.1:8080` | 监听地址（host:port）；可用 `0.0.0.0` / `::` / 空 host 绑定所有网卡 |
| `--enable-https` | `false` | 显式启用入站 HTTPS；启用时必须提供 `--tls-cert` 与 `--tls-key` |
| `--tls-cert` | 空 | 入站 HTTPS 证书文件（PEM）；仅在 `--enable-https` 时生效 |
| `--tls-key` | 空 | 入站 HTTPS 私钥文件（PEM）；仅在 `--enable-https` 时生效 |
| `--enable-auth` | `false` | **显式启用中继 token 认证**（校验 `X-Relay-Token`）；不启用则不校验 token |
| `--admin-addr` | 空 | 管理界面监听地址；**非空即启用管理界面**（如 `127.0.0.1:8081`） |
| `--admin-user` | `admin` | 管理界面管理员账号 |
| `--admin-password` | 空 | 管理界面管理员密码；为空则启动时**随机生成并打印**；也可用环境变量 `RELAY_ADMIN_PASSWORD` |

日志使用 `log` 包全局日志输出到 stderr，每请求一条访问日志（方法、路径、上游目标、状态码、耗时）。

## 访问认证（可选）

`--enable-auth` 显式启用认证后：客户端每个请求必须携带已签发的 token，否则返回 `401`。token 走**独立请求头** `X-Relay-Token`，与客户端透传给上游的 `Authorization`（如 `Bearer sk-xxx`）互不冲突：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "X-Relay-Token: relay_xxxxxxxx" \
  -H "X-Upstream-Target: https://api.anthropic.com" \
  -H "Authorization: Bearer sk-xxx" \
  -d '{"model":"...","messages":[...]}'
```

token 支持**两种签发方式**（管理台或 API 均可）：

- **自动生成**（默认）：`relay_` + 32 字节 `crypto/rand` 随机数的 base64url（43 字符）
- **手动录入**：管理员指定 token 明文（如与既有系统约定的密钥）。要求仅可见 ASCII 字符（0x21-0x7E，不含空格与控制字符）、最长 128 字符，且不可与已签发的 token 重复（重复返回 400）

token 为**不透明 token**（opaque token，类 GitHub PAT / Stripe 风格），本身不携带信息，验证即查表：

- 过期时间、使用次数上限、撤销状态都在服务端签发记录里；每次通过校验会计数，达到上限后拒绝（401）
- 查表按 token 的 SHA-256 进行，避免以明文作 key 的时序泄露
- 请求头优先，查询参数 `relay_token` 同作用（不便设头的场景），二者都不会转发给上游
- **纯内存存储**：签发记录只在内存中，**服务重启后所有已签发 token 全部失效**，需重新登录管理台签发（未来计划支持 sqlite 持久化）
- 认证失败返回 `401 Unauthorized`；不开 `--enable-auth` 则中继完全不校验 token

## 管理界面

`--admin-addr` **非空即启用管理界面**（可与 `--enable-auth` 独立组合；例如只想用管理台签发 token、但暂不开启中继认证）：

```bash
./http-relay --listen-addr 127.0.0.1:8080 \
  --enable-auth \
  --admin-addr 127.0.0.1:8081 \
  --admin-password your-admin-password
```

浏览器打开 `http://127.0.0.1:8081`，用管理员账号（默认 `admin`）登录后即可：

- **签发 token**：填备注名、有效期（小时，0 = 永不过期）、使用次数上限（0 = 不限）；Token 值留空则**自动生成**，填入自定义值则**手动录入**（仅可见 ASCII 字符、最长 128、不可重复）
- **查看列表**：备注名、完整 token（可见可复制）、过期时间、已用次数/上限、最后使用时间
- **删除 token**：立即失效（撤销）

签发 API（登录后）也可直接调用：`POST /api/tokens`，body 为 `{"token": "自定义值，留空自动生成", "name": "备注", "ttl_hours": 0, "max_uses": 0}`。

管理员密码为空时启动会随机生成并打印一次到日志。管理端口建议只绑 `127.0.0.1`，通过 SSH 隧道等方式访问；session cookie 为 HttpOnly + SameSite=Strict，12 小时有效。管理员密码也可用环境变量 `RELAY_ADMIN_PASSWORD` 传入，避免出现在命令行历史里。

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

### 多目标链式转发（每到一个中继节点消费一个）

`X-Upstream-Target` 可携带**多个目标**（逗号或空白分隔，也可重复设置该头）。每个中继节点**消费第一个目标**作为本跳上游，剩余目标继续随 `X-Upstream-Target` 传给下一跳——下一跳若也是 http-relay 节点就继续消费，最后一个目标即最终上游：

```bash
# A、B 都是 http-relay 节点，C 是最终上游
curl http://A:8080/v1/chat/completions \
  -H "X-Upstream-Target: https://B:8080, https://C" \
  -H "Authorization: Bearer sk-xxx" \
  -d '{"model":"...","messages":[...]}'
```

链路行为：`A` 节点消费 `B`，转发到 `B` 并携带 `X-Upstream-Target: https://C`；`B` 节点消费 `C`，转发到 `C` 且**不再携带**该头。规则：

- 目标列表中**每一项都会先校验**，任一非法（非 http/https、缺 host）即返回 `400`
- 无剩余目标时该头不转发给上游；查询参数 `upstream_target` 指定多目标时，剩余目标改用请求头转发（参数本身仍剥除）
- ⚠️ 目标以逗号/空白为分隔约定，目标 URL 自身含逗号（如 `?ids=1,2`）时需改用单目标
- 多跳时每跳的访问日志会带 `next=[剩余目标]`，便于排查链路

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
| 消费 `X-Upstream-Target` / `upstream_target` | 中继控制头/控制参数，每跳消费第一个目标；无剩余目标时剥除，有剩余时把剩余目标写入 `X-Upstream-Target` 转发给下一跳 |
| 删除 `X-Relay-Token` / `relay_token` | 中继认证头/认证参数，不转发给上游 |
| 改写 `Host` | 改为上游目标域名 |
| 删除逐跳头 | `Connection` / `Keep-Alive` / `Transfer-Encoding` / `Upgrade` 等（含 `Connection` 头中声明的扩展头） |
| 重算 `Content-Length` | 由 net/http 按实际 Body 计算，避免冲突 |
| 其余 Header / 查询参数 | 原样转发（`Authorization`、`x-api-key`、`anthropic-version` 等） |

## 错误处理

| 场景 | 行为 |
|---|---|
| 启用认证后缺少/错误 token（`X-Relay-Token` 头与 `relay_token` 参数） | `401 Unauthorized` |
| 缺少 `X-Upstream-Target` 头与 `upstream_target` 查询参数 | `400 Bad Request` |
| `X-Upstream-Target` 任一目标非法 URL（解析失败 / 非 http-https / 缺 host） | `400 Bad Request` + 错误说明（含目标序号） |
| 管理台手动录入 token 非法（含空格/控制字符/超 128 字符）或重复 | `400 Bad Request` |
| 上游连接失败 | `502 Bad Gateway` + 错误说明 |
| 上游返回任意状态码 | 原样回传（含 4xx/5xx） |

## 测试

```bash
go test -race ./...
```

覆盖：基础正向转发（Header/Body/状态码透传、Host 改写、控制头清理）、上游基路径拼接、查询参数指定目标（含参数剥除、请求头优先级）、多目标链式转发（两跳 relay 串联消费、逗号/空白/重复头/查询参数多种分隔、最后一跳剥除控制头、任一目标非法 400）、缺目标 400、非法目标 400、上游不可达 502、出站 HTTPS 上游、入站 TLS 监听、SSE 流式透传（验证未缓冲响应体）、监听地址校验、token 认证（缺失/错误 401、过期、次数上限、请求头/查询参数、控制头剥除、无认证兼容行为）、token 签发与校验（自动生成与手动录入、非法/重复录入拒绝、未知/篡改/跨 store 拒绝、删除即撤销、不限次/永不过期、唯一性、并发）、管理 API（账号密码登录、未登录 401、手动录入 token 200/重复 400/非法 400、路由 405/404、随机密码生成）、跨包集成（管理台签发 → relay 认证 → 次数用尽/删除拒绝、编译期接口解耦检查）。

## 项目结构

管理面与转发面通过接口解耦：`relay` 只依赖自己的 `Authorizer` 接口（"放不放行"），`admin` 只依赖 `TokenManager` 接口（签发/删除/列表）；`auth` 包同时实现两者，`main` 只做装配。换认证实现（如未来的 sqlite 版）只需替换 `auth` 包。

```
http-relay/
├── main.go           # 装配入口：flag 解析、relay + admin 双 server、优雅退出
├── relay/            # 转发面（数据面）
│   ├── relay.go      #   Relay 中继、Authorizer 接口、Header 处理、SSE 流式透传
│   └── relay_test.go #   转发与认证行为测试（认证用 fake Authorizer，不依赖实现）
├── auth/             # 认证实现（同时满足两个面的接口）
│   ├── auth.go       #   不透明 token 签发/校验/删除（SHA-256 查表，纯内存）
│   └── auth_test.go  #   token 签发与校验测试
├── admin/            # 管理面（控制面）
│   ├── admin.go      #   TokenManager 接口、账号密码登录、ServeMux 路由、token API
│   ├── admin.html    #   管理界面前端（go:embed 内嵌，无外部依赖）
│   └── admin_test.go #   管理 API 测试
├── main_test.go      # 跨包集成测试（装配根上的端到端）
├── go.mod
└── README.md
```
