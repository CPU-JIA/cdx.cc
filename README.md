# Claude Bridge

Anthropic Messages API → OpenAI Responses API 协议转换代理。让 GPT / Codex 模型在 **Claude Code CLI** 中原生使用。

## 快速开始

### 1. 构建

```bash
go build -o claude-bridge ./cmd/claude-bridge
```

### 2. 启动 Bridge

```bash
UPSTREAM_BASE_URL=https://your-openai-api.com \
UPSTREAM_API_KEY=sk-xxx \
./claude-bridge
```

Bridge 默认监听 `:8787`。

### 3. 配置 Claude Code CLI

**Bash / Zsh（推荐写入 `~/.bashrc`）：**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=any-key-here
export ANTHROPIC_MODEL=gpt-5.4
export DISABLE_INTERLEAVED_THINKING=1

claude
```

**PowerShell：**

```powershell
$env:ANTHROPIC_BASE_URL = "http://localhost:8787"
$env:ANTHROPIC_API_KEY = "any-key-here"
$env:ANTHROPIC_MODEL = "gpt-5.4"
$env:DISABLE_INTERLEAVED_THINKING = "1"

claude
```

> `ANTHROPIC_API_KEY` 可以是任意值 — 当 bridge 配置了 `UPSTREAM_API_KEY` 时，客户端的 key 不会被转发。

### 4. 完事

Claude Code CLI 现在使用 GPT-5.4（或任意 OpenAI 兼容模型）作为后端。

## 环境变量

| 变量                       | 必需   | 默认值        | 说明                                                         |
| -------------------------- | ------ | ------------- | ------------------------------------------------------------ |
| `UPSTREAM_BASE_URL`        | **是** | —             | 上游 OpenAI 兼容 API 地址                                    |
| `UPSTREAM_API_KEY`         | 否     | —             | 上游 API Key（留空则转发客户端的 Authorization / X-API-Key） |
| `LISTEN_ADDR`              | 否     | `:8787`       | 监听地址                                                     |
| `MODE`                     | 否     | `best_effort` | `strict` 拒绝不支持的字段；`best_effort` 静默跳过            |
| `DISABLE_RESPONSE_STORAGE` | 否     | —             | 设置任意值则请求中 `store=false`                             |
| `UPSTREAM_TIMEOUT_SECS`    | 否     | `120`         | 上游请求超时秒数                                             |
| `MAX_BODY_MB`              | 否     | `10`          | 最大请求体大小 (MB)                                          |
| `REDIS_URL`                | 否     | —             | Redis 连接字符串（留空用内存存储）                           |
| `LOG_LEVEL`                | 否     | `info`        | `debug` / `info` / `warn` / `error`                          |

## 支持的转换

| Anthropic (入)                     | OpenAI (出)                              | 状态 |
| ---------------------------------- | ---------------------------------------- | ---- |
| `/v1/messages` JSON                | `/v1/responses` JSON                     | ✅   |
| `/v1/messages` SSE 流式            | `/v1/responses` SSE 流式                 | ✅   |
| `tool_use` / `tool_result`         | `function_call` / `function_call_output` | ✅   |
| `thinking` (extended thinking)     | `reasoning` (effort + summary)           | ✅   |
| `image` (base64 / URL)             | `input_image` (image_url 平铺字符串)     | ✅   |
| `document` 块                      | `input_text` 占位 (best_effort)          | ✅   |
| `system` (string / block array)    | `instructions`                           | ✅   |
| `metadata`                         | 剥离（上游不支持）                       | ✅   |
| `cache_control`                    | 剥离（OpenAI 无此概念）                  | ✅   |
| 历史中 `thinking` / `signature` 块 | 剥离（OpenAI 不需要）                    | ✅   |

### Thinking → Reasoning 映射

```
Anthropic thinking                →  OpenAI reasoning
──────────────────────────────────────────────────────
thinking.type = "enabled"         →  reasoning.effort 按 budget_tokens 推算：
  budget_tokens ≤ 2048            →  "low"
  budget_tokens ≤ 8192            →  "medium"
  budget_tokens ≤ 32768           →  "high"
  budget_tokens > 32768           →  "xhigh"
  无 budget_tokens                →  "xhigh"（默认拉满）
thinking.type = "disabled"        →  reasoning.effort = "none"
无 thinking 字段                   →  不设 reasoning
```

### Stop Reason 映射

```
OpenAI                             →  Anthropic
───────────────────────────────────────────────
incomplete: max_output_tokens      →  "max_tokens"
incomplete: content_filter         →  "end_turn"
status: "incomplete"               →  "max_tokens"
status: "failed" / "cancelled"     →  "end_turn"
有 function_call 输出               →  "tool_use"
正常完成                            →  "end_turn"
```

### 流式事件映射

```
OpenAI SSE 事件                        →  Anthropic SSE 事件
─────────────────────────────────────────────────────────────
response.created                       →  message_start
response.content_part.added            →  content_block_start (text)
response.output_text.delta             →  content_block_delta (text_delta)
response.output_text.done              →  content_block_stop
response.output_item.added (tool)      →  content_block_start (tool_use)
response.function_call_arguments.delta →  content_block_delta (input_json_delta)
response.function_call_arguments.done  →  content_block_stop
response.reasoning_summary_text.delta  →  content_block_delta (thinking_delta)
response.reasoning_text.delta          →  content_block_delta (thinking_delta)
response.completed                     →  message_delta + message_stop
response.incomplete                    →  message_delta (max_tokens) + message_stop
response.failed / cancelled            →  message_delta (end_turn) + message_stop
response.refusal.delta                 →  content_block_delta (text_delta)
```

## 架构

```
Claude Code CLI
  │ Anthropic Messages API (/v1/messages)
  ▼
┌──────────────┐
│ Claude Bridge │  :8787
│              │
│  Decode      │  Anthropic JSON → 内部结构
│  Transform   │  内部结构 → OpenAI Responses 格式
│  Forward     │  → upstream /v1/responses
│  Stream      │  OpenAI SSE → Anthropic SSE 逐事件转换
└──────────────┘
  │ OpenAI Responses API (/v1/responses)
  ▼
Upstream (GPT / Codex / 兼容 API)
```

## 已验证

- [x] 纯文本对话（流式 + 非流式）
- [x] 工具调用（Bash / Read / Glob 等 70+ 工具）
- [x] 多模态图片（URL + base64）
- [x] Thinking/Reasoning 双向转换
- [x] 多轮对话（历史 thinking/signature 自动剥离）
- [x] Claude Code CLI 端到端完整运行
- [x] metadata 剥离（防止上游 502）

## License

MIT
