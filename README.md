# Codex Proxy

Codex API 代理服务，提供 OpenAI / Claude 多协议兼容接口，支持多账号轮询、内部重试、自动 Token 刷新。

## 功能

- **多协议兼容** — 同时支持 Chat Completions、Responses API、Responses Compact、Claude Messages API
- **多账号轮询** — Round-Robin + 额度均衡，按使用率排序优先使用剩余额度最多的账号
- **内部重试** — 请求失败时在 executor 内部切换账号重试，流式请求 SSE 头只在成功后才写给客户端，客户端不感知重试过程
- **自动 Token 刷新** — 定期并发刷新 access_token，429 限频设冷却而不是删除账号
- **健康检查** — 定期探测账号状态，自动识别 401/403/429 并处理
- **额度查询** — 支持查询每个账号的剩余额度，按额度使用率排序选号
- **热加载** — 运行时自动扫描新增账号文件，无需重启
- **Tool Schema 自动修复** — 自动补全 `type: array` 缺少 `items` 的 JSON Schema，避免上游 400 错误
- **连接池保活** — 定时 ping 上游保持 TCP+TLS 连接，消除冷启动延迟
- **API Key 鉴权** — 可选的访问密钥保护

## 快速开始

### 1. 准备账号文件

在 `auths/` 目录中放入 JSON 格式的账号文件，每个文件一个账号：

```json
{
  "access_token": "eyJhbGciOi...",
  "refresh_token": "v1.MjE0OT...",
  "id_token": "eyJhbGciOi...",
  "account_id": "org-xxx",
  "email": "user@example.com",
  "type": "codex",
  "expired": "2025-01-01T00:00:00Z"
}
```

### 2. 编辑配置

```yaml
listen: ":8080"
auth-dir: "./auths"
base-url: "https://chatgpt.com/backend-api/codex"
# proxy-url: "http://127.0.0.1:7890"
log-level: "info"
refresh-interval: 3000
max-retry: 2
health-check-interval: 7200
health-check-max-failures: 2
health-check-concurrency: 100
refresh-concurrency: 100
api-keys:
  - "sk-your-custom-key"
```

### 3. 预编译包（推荐）


| ZIP 文件名 | 适用环境 |
|------------|----------|
| `codex-proxy-linux-amd64.zip` | Linux x86_64（主流服务器 / PC） |
| `codex-proxy-linux-386.zip` | Linux 32 位 x86 |
| `codex-proxy-linux-arm64.zip` | Linux AArch64（树莓派 64 位、ARM 服务器等） |
| `codex-proxy-linux-armv7.zip` | Linux 32 位 ARMv7（旧版树莓派等） |
| `codex-proxy-windows-amd64.zip` | Windows 64 位 x86 |
| `codex-proxy-windows-386.zip` | Windows 32 位 x86 |
| `codex-proxy-windows-arm64.zip` | Windows ARM64（Surface Pro X 等） |
| `codex-proxy-darwin-amd64.zip` | macOS Intel |
| `codex-proxy-darwin-arm64.zip` | macOS Apple Silicon (M 系列) |

解压后编辑 `config.yaml`，将账号 JSON 放入 `auth-dir` 指定目录，执行：

- Linux / macOS: `./codex-proxy -config config.yaml`
- Windows: `codex-proxy.exe -config config.yaml`

Release 页附带 `SHA256SUMS.txt` 可校验文件完整性。

### 4. 源码编译

```bash
go build -o codex-proxy .
./codex-proxy -config config.yaml
```

### 5. Docker

镜像发布至 `ghcr.io/XxxXTeam/codex-proxy`（amd64 / arm64）。打标签或默认分支构建时会推送，具体见仓库 Actions。

### 6. 调用接口

**Chat Completions**
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4-high", "messages": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

**Responses API**
```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4", "input": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

**Claude Messages API**
```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4", "max_tokens": 4096, "messages": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

## 思考配置

在模型名中使用连字符后缀控制思考级别，可选 `-fast` 后缀启用快速模式。

| 格式 | 说明 |
|------|------|
| `gpt-5.4-high` | 高思考级别 |
| `gpt-5.4-fast` | fast 服务层级 |
| `gpt-5.4-high-fast` | 高思考 + fast 层级 |
| `gpt-5.4` | 使用请求体中的配置或默认值 |

不同基础模型支持的思考后缀不同

| 基础模型 | 支持的思考后缀 |
|----------|---------------|
| `gpt-5` / `gpt-5-codex` / `gpt-5-mini` | low, medium, high, auto |
| `gpt-5.1` / `gpt-5.1-codex` / `gpt-5.1-mini` | low, medium, high, none, auto（codex 另有 max） |
| `gpt-5.2` / `gpt-5.2-codex` / `gpt-5.2-mini` | low, medium, high, xhigh, none, auto |
| `gpt-5.4` / `gpt-5.4-codex` / `gpt-5.4-mini` | low, medium, high, xhigh, none, auto |

所有组合均支持 `-fast` 后缀（如 `gpt-5.4-codex-high-fast`）。

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | Chat Completions（流式/非流式） |
| POST | `/v1/responses` | Responses API（流式/非流式） |
| POST | `/v1/responses/compact` | Responses Compact API（对话历史压缩） |
| POST | `/v1/messages` | Claude Messages API（流式/非流式） |
| GET | `/v1/models` | 模型列表 |
| GET | `/health` | 健康检查 |
| GET | `/stats` | 账号统计（状态/请求数/错误数/额度） |
| POST | `/refresh` | 手动刷新所有账号 Token |
| POST | `/check-quota` | 查询所有账号额度 |

## 配置说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `listen` | `:8080` | 监听地址 |
| `auth-dir` | `./auths` | 账号文件目录 |
| `base-url` | `https://chatgpt.com/backend-api/codex` | Codex API 基础 URL |
| `proxy-url` | 空 | HTTP/SOCKS5 代理地址 |
| `log-level` | `info` | 日志级别（debug/info/warn/error） |
| `refresh-interval` | `3000` | Token 自动刷新间隔（秒） |
| `max-retry` | `2` | 请求失败最大重试次数（内部切换账号） |
| `health-check-interval` | `300` | 健康检查间隔（秒，0 禁用） |
| `health-check-max-failures` | `3` | 连续失败多少次后禁用账号 |
| `health-check-concurrency` | `5` | 健康检查并发数 |
| `refresh-concurrency` | `50` | Token 并发刷新数 |
| `api-keys` | 空 | API Key 列表（为空则不鉴权） |

## 项目结构

```
codex-proxy/
  |-- main.go                           # 服务入口（入站 HTTP/2 h2c 多路复用）
  |-- config.example.yaml               # 示例配置
  |-- auths/                            # 账号文件目录（自建）
  |-- internal/
  |   |-- config/config.go              # 配置加载
  |   |-- static/                       # 嵌入前端（assets/index.html）
  |   |-- auth/                         # 账号、刷新、健康检查、额度
  |   |-- thinking/                     # 思考后缀与协议转换
  |   |-- executor/codex.go             # 上游请求与连接池保活
  |   `-- handler/                      # HTTP 路由、代理、Claude、中间件
  |-- .github/workflows/release.yml     # 多架构并行打包 + Release + Docker
```
