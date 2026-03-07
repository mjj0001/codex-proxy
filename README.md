# Codex Proxy
 Codex API 代理服务，提供 OpenAI 兼容接口，支持多账号轮询和自动 Token 刷新。

## 功能

- **OpenAI 兼容 API** — 接受 `/v1/chat/completions` 格式请求
- **多账号轮询** — Round-Robin 方式在多个 Codex 账号间分发请求
- **自动 Token 刷新** — 定期刷新 access_token，失败自动禁用账号
- **思考配置** — 使用连字符格式控制模型思考级别（如 `gpt-5-xhigh`）
- **429 冷却** — 自动检测限频，冷却期间跳过该账号
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
log-level: "info"
refresh-interval: 3000
```

### 3. 编译运行

```bash
go build -o codex-proxy .
./codex-proxy -config config.yaml
```

### 4. 调用接口

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

## 思考配置

使用连字符格式在模型名中指定思考级别，不再使用括号格式。

| 格式 | 说明 |
|------|------|
| `gpt-5-xhigh` | 超高思考级别 |
| `gpt-5-high` | 高思考级别 |
| `gpt-5-medium` | 中等思考级别 |
| `gpt-5-low` | 低思考级别 |
| `gpt-5-none` | 禁用思考 |
| `gpt-5-auto` | 自动思考 |
| `gpt-5-16384` | 指定 token 预算 |
| `gpt-5` | 使用请求体中的配置或默认值 |

支持的级别：`minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `none`, `auto`

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | Chat Completions（流式/非流式） |
| GET | `/v1/models` | 模型列表 |
| GET | `/health` | 健康检查 |

## 项目结构

```
codex-proxy/
├── main.go                          # 服务入口
├── config.yaml                      # 配置文件
├── auths/                           # 账号文件目录
│   └── account1.json
├── internal/
│   ├── config/config.go             # 配置加载
│   ├── auth/
│   │   ├── types.go                 # Token 数据结构
│   │   ├── refresh.go               # Token 刷新（OAuth）
│   │   ├── selector.go              # 多号轮询选择器
│   │   └── manager.go               # 账号管理器
│   ├── thinking/
│   │   ├── types.go                 # 思考类型定义
│   │   ├── suffix.go                # 连字符格式解析
│   │   └── apply.go                 # 思考配置应用
│   ├── translator/
│   │   ├── request.go               # OpenAI → Codex 请求转换
│   │   └── response.go              # Codex → OpenAI 响应转换
│   ├── executor/codex.go            # Codex API 执行器
│   └── handler/proxy.go             # HTTP 代理处理器
```
