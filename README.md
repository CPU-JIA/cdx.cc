# >\_ cdx.cc — 把 Codex 装进 Claude Code

Anthropic Messages API → OpenAI Responses API 协议转换代理。让 Codex / GPT 模型在 **Claude Code CLI** 中原生使用。

## 功能

- 完整协议转换：Anthropic Messages API ↔ OpenAI Responses API
- Web 管理面板：动态配置上游、模型映射、认证，无需重启
- 流式 SSE 转换：逐事件实时桥接，支持 thinking / tool_use / web_search
- Token 精确计算：tiktoken BPE (o200k_base)，CC 状态栏上下文百分比依赖此数据
- 服务端 Compaction：Anthropic beta ↔ OpenAI context_management 协议转换
- 热更新配置：JSON 持久化，Docker 友好
- 零配置启动：开箱即用，默认上游 `https://api.openai.com`

## 快速开始

### 1. 构建

```bash
go build -o claude-bridge ./cmd/claude-bridge
```

### 2. 启动

```bash
./claude-bridge
```

Bridge 默认监听 `:8787`，打开 `http://localhost:8787/admin` 配置上游和模型映射。

也可以通过环境变量预配置：

```bash
UPSTREAM_BASE_URL=https://your-api.com \
UPSTREAM_API_KEY=sk-xxx \
AUTH_TOKEN=your-bridge-token \
./claude-bridge
```

### 3. 配置 Claude Code

**Bash / Zsh（推荐写入 `~/.bashrc`）：**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=your-bridge-token
claude
```

**PowerShell：**

```powershell
$env:ANTHROPIC_BASE_URL = "http://localhost:8787"
$env:ANTHROPIC_API_KEY = "your-bridge-token"
claude
```

> **重要**：必须从项目目录内启动 Claude Code（`cd your-project && claude`），否则 sub-agents 和 worktree 无法正常工作。

### 4. Docker 部署

```bash
docker compose up -d
```

生产环境在管理面板设置"服务地址"为你的域名（如 `https://bridge.example.com`），连接配置代码片段会自动更新。

## 管理面板

访问 `http://localhost:8787/admin`，功能包括：

- **上游配置**：Base URL、API Key，实时生效
- **模型映射**：入站 Claude 模型名 → 上游模型 + 推理强度，下拉选择上游真实模型列表
- **连接配置**：Auth Token、服务地址，一键复制客户端配置代码

所有配置保存到 `runtime_config.json`，重启自动恢复。

## 环境变量

| 变量                       | 默认值                   | 说明                                                  |
| -------------------------- | ------------------------ | ----------------------------------------------------- |
| `UPSTREAM_BASE_URL`        | `https://api.openai.com` | 上游 OpenAI 兼容 API 地址                             |
| `UPSTREAM_API_KEY`         | —                        | 上游 API Key                                          |
| `AUTH_TOKEN`               | —                        | Bridge 入站认证 Token                                 |
| `LISTEN_ADDR`              | `:8787`                  | 监听地址                                              |
| `MODE`                     | `best_effort`            | `strict` 或 `best_effort`                             |
| `MODEL_MAP`                | —                        | 模型映射（格式：`claude-opus-4-6=gpt-5.4:xhigh,...`） |
| `UPSTREAM_TIMEOUT_SECS`    | `120`                    | 上游请求超时秒数                                      |
| `MAX_BODY_MB`              | `10`                     | 最大请求体 (MB)                                       |
| `CONTEXT_LIMIT`            | `1048576`                | 上下文窗口大小（token 数），影响 rate-limit 头        |
| `OUTPUT_LIMIT`             | `32000`                  | 最大输出 token 数，影响 rate-limit 头                 |
| `DISABLE_RESPONSE_STORAGE` | —                        | 设置任意值则 `store=false`                            |
| `LOG_LEVEL`                | `info`                   | `debug` / `info` / `warn` / `error`                   |
| `REDIS_URL`                | —                        | Redis 连接字符串（留空用内存存储）                    |

> 所有环境变量均可在管理面板中覆盖，热更新无需重启。

## 协议转换

### API 端点

| 端点                                | 说明                        |
| ----------------------------------- | --------------------------- |
| `POST /v1/messages`                 | 主消息 API（流式 + 非流式） |
| `POST /v1/messages/count_tokens`    | Token 计数（tiktoken BPE）  |
| `GET /api/claude_code_penguin_mode` | /fast 模式支持              |
| `GET /admin`                        | 管理面板                    |
| `GET /health`                       | 健康检查                    |

### 请求转换

| Anthropic (入)                    | OpenAI (出)                                  |
| --------------------------------- | -------------------------------------------- |
| `/v1/messages`                    | `/v1/responses`                              |
| `tool_use` / `tool_result`        | `function_call` / `function_call_output`     |
| `thinking.type = enabled`         | `reasoning.effort` (按 budget_tokens 推算)   |
| `thinking.type = adaptive`        | `reasoning.effort = high` + `summary = auto` |
| `thinking.type = disabled`        | `reasoning.effort = none`                    |
| `image` (base64 / URL)            | `input_image`                                |
| `system` (string / blocks)        | `instructions`                               |
| `web_search` 工具                 | `web_search` (OpenAI GA)                     |
| `cache_control`                   | 剥离                                         |
| `context_management` (compaction) | OpenAI `context_management` (compaction)     |
| 历史 `thinking` / `signature` 块  | 剥离                                         |

### 流式事件映射

| OpenAI SSE                               | Anthropic SSE                                |
| ---------------------------------------- | -------------------------------------------- |
| `response.created`                       | `message_start`                              |
| `response.output_text.delta`             | `content_block_delta` (text_delta)           |
| `response.output_item.added` (tool)      | `content_block_start` (tool_use)             |
| `response.function_call_arguments.delta` | `content_block_delta` (input_json_delta)     |
| `response.reasoning_summary_text.delta`  | `content_block_delta` (thinking_delta)       |
| `response.reasoning_text.delta`          | `content_block_delta` (thinking_delta)       |
| `response.web_search_call.completed`     | `server_tool_use` + `web_search_tool_result` |
| `response.completed`                     | `message_delta` + `message_stop`             |
| `response.refusal.delta`                 | `content_block_delta` (text_delta)           |

### 响应头

Bridge 设置 Anthropic 兼容的 rate-limit 响应头（`anthropic-ratelimit-*`），Claude Code 依赖这些头显示状态栏的上下文百分比和 Token 计数。上下文窗口大小通过 `CONTEXT_LIMIT` 和 `OUTPUT_LIMIT` 环境变量配置。

## 架构

```
Claude Code CLI
  │ Anthropic Messages API
  ▼
┌────────────────────────────────┐
│         cdx.cc Bridge          │  :8787
│                                │
│  /v1/messages          → 协议转换 → /v1/responses
│  /v1/messages/count_tokens → tiktoken BPE 精确计算
│  /admin                → 管理面板 (嵌入式 HTML)
│  /health               → 健康检查
│                                │
│  RuntimeConfig: JSON 持久化，热更新
└────────────────────────────────┘
  │ OpenAI Responses API
  ▼
Upstream (Codex / GPT / 兼容 API)
```

## 已验证

- [x] 纯文本对话（流式 + 非流式）
- [x] 工具调用（70+ 工具完整支持）
- [x] 多模态图片（URL + base64）
- [x] Thinking/Reasoning（enabled / adaptive / disabled）
- [x] Sub-agents（普通模式 + worktree 隔离）
- [x] /fast 模式
- [x] Web Search 工具映射
- [x] 模型名称保持（CC 功能检测依赖模型名子字符串）
- [x] Token 计数端点（tiktoken BPE 精确计算）
- [x] 服务端 Compaction 协议转换（Anthropic beta ↔ OpenAI）
- [x] 管理面板热更新配置
- [x] Docker 部署

## /fast 模式

需要 patch Claude Code CLI 解锁 /fast（penguin mode 硬编码到 api.anthropic.com）：

```bash
bash scripts/patch-fast-mode.sh
```

恢复：`bash scripts/patch-fast-mode.sh --restore`

## License

MIT
