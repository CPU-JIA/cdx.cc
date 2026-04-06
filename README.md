# >\_ cdx.cc — 把 Codex 接到 Claude Code

`cdx.cc` 是一个 **Anthropic Messages API ↔ OpenAI Responses API** 协议桥。

它的目标很直接：

- 让 **Claude Code CLI** 按 Anthropic 协议接入
- 实际把请求转发到 **OpenAI / Codex / 兼容 Responses API 上游**
- 尽量保持 Claude Code 侧的原生体验，包括流式、工具调用、多模态、token 计数、压缩和 `/fast`

## 功能特性

- Anthropic `POST /v1/messages` → OpenAI `POST /v1/responses`
- 流式 SSE 桥接，支持文本、reasoning、工具调用、citations 等事件
- 非流式响应转换，保留 usage / service tier / context management
- `tool_use` / `tool_result`、web search、tool search、local shell、custom tool 映射
- 多模态内容映射：text / image / document / search_result
- `POST /v1/messages/count_tokens` 精确估算，兼容 Claude Code 上下文显示
- `POST /v1/responses/compact` 远程压缩透传
- 自动压缩：`context_management` 或 `/v1/responses/compact`
- Prompt Cache 策略：`off` / `auto` / `force_24h`，支持自动生成稳定 `prompt_cache_key`
- Web 管理面板：上游、模型映射、连接信息、自动压缩、Prompt Cache 热更新
- `runtime_config.json` 持久化运行时配置
- `/fast` 兼容脚本与 bridge 端点支持

## 架构

```text
Claude Code CLI
  │  Anthropic Messages API
  ▼
┌────────────────────────────────┐
│            cdx.cc              │
│                                │
│  /v1/messages               ─┐ │
│  /v1/messages/count_tokens  ─┼─┼─→ OpenAI Responses API
│  /v1/responses/compact      ─┘ │
│                                │
│  /admin       管理面板          │
│  /health      健康检查          │
│  /v1/models   入站模型列表      │
└────────────────────────────────┘
```

## 快速开始

### 1. 构建

```bash
go build -o claude-bridge ./cmd/claude-bridge
```

### 2. 运行

```bash
./claude-bridge
```

默认监听 `:8787`。

- 管理面板：`http://localhost:8787/admin`
- 健康检查：`http://localhost:8787/health`

也可以通过环境变量直接启动：

```bash
UPSTREAM_BASE_URL=https://api.openai.com \
UPSTREAM_API_KEY=sk-xxx \
AUTH_TOKEN=sk-cdx.cc-demo \
./claude-bridge
```

### 3. 首次启动

如果没有现成的运行时配置，bridge 会自动生成：

- `auth_token`
- `admin_password`

并写入：

- 当前目录下的 `runtime_config.json`
- 或 `DATA_DIR/runtime_config.json`

### 4. 打开管理面板

在 `/admin` 中完成这些配置：

- 上游 Base URL / API Key
- 模型映射
- 客户端连接 Token
- 管理密码
- 服务地址
- 自动压缩
- Prompt Cache

## 连接 Claude Code

Claude Code 连接这个 bridge 时，使用的是 **Anthropic 风格的客户端环境变量**。

### Bash / Zsh

```bash
export ANTHROPIC_AUTH_TOKEN="sk-cdx.cc-demo"
export ANTHROPIC_BASE_URL="http://localhost:8787"
# 可选：指定默认模型
# export ANTHROPIC_MODEL="claude-sonnet-4-6"

claude
```

### PowerShell

```powershell
$env:ANTHROPIC_AUTH_TOKEN = "sk-cdx.cc-demo"
$env:ANTHROPIC_BASE_URL = "http://localhost:8787"
# 可选：指定默认模型
# $env:ANTHROPIC_MODEL = "claude-sonnet-4-6"

claude
```

### Claude Code `settings.json`

```json
{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "sk-cdx.cc-demo",
    "ANTHROPIC_BASE_URL": "http://localhost:8787"
  }
}
```

> 请从项目目录内启动 Claude Code，例如 `cd your-project && claude`。

## 管理面板

访问 `http://localhost:8787/admin`。

支持的主要配置项：

- **Upstream**
  - Base URL
  - API Key
- **Models**
  - 入站模型名 → 上游模型名
  - 可选覆盖 `reasoning_effort`
  - 可从上游拉取模型列表
- **Connection**
  - `auth_token`
  - `admin_password`
  - `service_url`
- **Auto Compact**
  - 模式
  - 阈值
- **Prompt Cache**
  - `off`
  - `auto`
  - `force_24h`
  - 自动生成 `prompt_cache_key`

所有配置都会保存到 `runtime_config.json`，无需重启即可生效。

## Prompt Cache

bridge 支持把 Claude Code / Anthropic 风格缓存习惯兼容到 OpenAI / Codex 的 prompt caching。

### 策略

- `off`
  - 不自动注入 `prompt_cache_retention`
  - 不自动生成 `prompt_cache_key`
  - 仅保留客户端显式传入的缓存字段
- `auto`
  - 检测到缓存信号时才注入缓存策略
  - 例如：
    - 客户端显式传了 `prompt_cache_key`
    - bridge 能从 Claude Code metadata 安全派生稳定 key
    - Anthropic 请求里存在 `cache_control`
- `force_24h`
  - 默认强制注入 `prompt_cache_retention: "24h"`

### 自动生成 `prompt_cache_key`

启用后，如果客户端没有显式传 `prompt_cache_key`，bridge 会自动生成稳定 key。

优先使用 Claude Code metadata 中可稳定识别会话/设备的信息：

- `account_uuid`
- `device_id`
- `session_id`

并结合：

- auth token
- user-agent

做哈希生成，尽量提高缓存命中率并降低冲突概率。

### 24h retention 与自动降级

当策略允许注入 retention 时，bridge 会优先使用：

- `prompt_cache_retention: "24h"`

如果上游 / 模型不支持该字段，bridge 会自动重试一次并移除它。

### 与 Anthropic 1h / 5m 的关系

Anthropic 的 `cache_control.ttl=1h|5m` 与 OpenAI 的 prompt caching 不是完全同构。

bridge 的兼容策略是：

- 优先提升到 OpenAI 侧可用的高缓存模式
- 使用 `24h` 作为最佳实践默认值
- 在 usage 上尽量回填为 Claude Code 可消费的缓存字段

### Cache usage 回填

OpenAI / Codex 原生返回的主要是：

- `input_tokens_details.cached_tokens`

bridge 会把它兼容回填到 Anthropic usage：

- `cache_read_input_tokens`
- `cache_creation.ephemeral_1h_input_tokens`
- `cache_creation.ephemeral_5m_input_tokens`

这是**兼容近似值**，用于让 Claude Code 的 UI、状态栏和上下文显示更贴近原生；不是对上游真实 cache creation 计费桶的 1:1 还原。

## 自动压缩

支持 3 种模式：

- `off`
- `context_management`
- `responses_compact`

### `context_management`

达到阈值后，bridge 会在消息请求里自动注入 compaction 相关上下文字段。

适合上游已经支持 `context_management` / `compaction` 的场景。

### `responses_compact`

达到阈值后，bridge 会：

1. 先调用 `POST /v1/responses/compact`
2. 用压缩后的历史重新构造输入
3. 再继续正式 `POST /v1/responses`

### 与 Claude Code `/compact` 的区别

- **Claude Code `/compact`**：客户端本地命令
- **bridge 自动压缩**：服务端阈值触发

它们是两套机制，可以同时存在。

## `/fast` 支持

Claude Code 的 `/fast` 检测并不完全走 `ANTHROPIC_BASE_URL`，因此需要两部分配合：

1. bridge 暴露 `GET /api/claude_code_penguin_mode`
2. 对本地 Claude Code CLI 做一次 patch

仓库内提供了两个脚本：

- `scripts/setup-claude-code-bridge.sh`
- `scripts/setup-claude-code-bridge.ps1`

### Bash / Zsh / Git Bash

```bash
bash scripts/setup-claude-code-bridge.sh
```

### PowerShell

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\setup-claude-code-bridge.ps1
```

> `setup-claude-code-bridge.ps1` 是 PowerShell 启动器，底层仍调用 Bash 脚本；Windows 上请准备好 Git Bash 或可用的 `bash`。

### 同时配置 `/fast` 和自动压缩

```bash
bash scripts/setup-claude-code-bridge.sh \
  --bridge http://localhost:8787 \
  --admin-password 'your-admin-password' \
  --compact-mode responses_compact \
  --compact-threshold 180000
```

### 恢复 CLI patch

```bash
bash scripts/setup-claude-code-bridge.sh --restore
```

### 脚本参数

```text
--bridge URL
--admin-password PASS
--compact-mode off|context_management|responses_compact
--compact-threshold N
--skip-fast
--skip-compact
--restore
```

## API 端点

| 端点 | 说明 |
| --- | --- |
| `GET /health` | 健康检查 |
| `GET /v1/models` | Anthropic 风格模型列表 |
| `POST /v1/messages` | 主消息接口，支持流式和非流式 |
| `POST /v1/messages/count_tokens` | Token 计数 |
| `POST /v1/responses/compact` | OpenAI / Codex 远程 compact 透传 |
| `POST /responses/compact` | `POST /v1/responses/compact` 别名 |
| `GET /api/claude_code_penguin_mode` | `/fast` 所需状态端点 |
| `GET /admin` | 管理面板 |

## 协议兼容范围

当前 bridge 覆盖的是“Claude Code 常用 Anthropic 语义”与“OpenAI Responses/Codex item 模型”之间的双向映射。

### 请求级字段映射

| Anthropic Messages | OpenAI Responses |
| --- | --- |
| `model` | `model` |
| `max_tokens` | `max_output_tokens` |
| `messages` | `input` |
| `system` | `instructions` |
| `tools` | `tools` |
| `tool_choice` | `tool_choice` |
| `stream` | `stream` |
| `temperature` | `temperature` |
| `top_p` | `top_p` |
| `stop_sequences` | `stop` |
| `metadata` | `metadata` |
| `thinking` | `reasoning` |
| `speed=fast` | `service_tier=priority` |
| `context_management` | `context_management` |

### 透传字段

这些字段会从 Anthropic 请求体直接带到 OpenAI Responses：

- `include`
- `parallel_tool_calls`
- `previous_response_id`
- `prompt_cache_key`
- `prompt_cache_retention`
- `service_tier`
- `store`
- `text`

除此之外，未显式识别的额外字段也会尽量保存在请求的 `ExtraFields` 中继续向上游透传。

### 输入内容块与历史消息映射

#### 基础内容

| Anthropic block / message | OpenAI input item / content |
| --- | --- |
| `message(role=user/assistant)` | `message(role=user/assistant)` |
| `text` | `input_text` / `output_text` |
| `image` | `input_image` |
| `document` | `input_file` |
| `thinking` / `signature`（历史） | 剥离，不继续上传上游 |
| `phase` / `end_turn` | 保存在 `message.phase` / `message.end_turn` |

#### 工具调用

| Anthropic | OpenAI Responses |
| --- | --- |
| `tool_use` | `function_call` |
| `tool_use(name=Bash)` | `local_shell_call` |
| `tool_use(name=ToolSearch)` | `tool_search_call` |
| `tool_use(responses_type=custom_tool_call)` | `custom_tool_call` |
| `server_tool_use(name=web_search)` | `web_search_call` |

其中这些字段会尽量保留：

- `id` ↔ `call_id`
- `name`
- `namespace`
- `status`
- `execution`
- `action`
- `raw_input`

`local_shell_call` 还会额外映射：

- `command`
- `timeout` / `timeout_ms`
- `cwd` / `working_directory`
- `env`
- `user`
- `run_in_background`

#### 工具结果

| Anthropic | OpenAI Responses |
| --- | --- |
| `tool_result` | `function_call_output` |
| `tool_result`（custom tool） | `custom_tool_call_output` |
| `tool_search_tool_result` | `tool_search_output` |
| `web_search_tool_result` | 保留为历史结果块；web_search 调用由 `server_tool_use` 对应 |

`tool_result.content` 支持：

| `tool_result.content` block | OpenAI output payload |
| --- | --- |
| `text` | `input_text` / 字符串输出 |
| `image` | `input_image` |
| `document` | `input_file` |
| `search_result` | 结构化 `input_text` |

#### 其他历史块

| Anthropic | OpenAI Responses |
| --- | --- |
| `compaction` | `compaction` |
| `image_generation_call` | `image_generation_call` |
| `responses_output_item` | 未知/未来 output item 原样回放 |

### 响应输出项映射

#### 非流式输出

| OpenAI Responses output item | Anthropic content block |
| --- | --- |
| `message` | `text` / 带 citations 的文本块 |
| `reasoning` | `thinking` |
| `function_call` | `tool_use` |
| `local_shell_call` | `tool_use(name=Bash)` |
| `tool_search_call` | `tool_use(name=ToolSearch)` |
| `tool_search_output` | `tool_search_tool_result` |
| `custom_tool_call` | `tool_use` + `responses_type=custom_tool_call` |
| `web_search_call` | `server_tool_use` + `web_search_tool_result` |
| `compaction` / `compaction_summary` | `compaction` |
| `image_generation_call` | `image_generation_call` |
| 未知 output item | `responses_output_item` |

#### usage / message 级元数据

| OpenAI Responses | Anthropic |
| --- | --- |
| `usage.input_tokens` | `usage.input_tokens` |
| `usage.output_tokens` | `usage.output_tokens` |
| `usage.input_tokens_details.cached_tokens` | `usage.cache_read_input_tokens` |
| `prompt_cache_retention=24h` | 优先回填到 `usage.cache_creation.ephemeral_1h_input_tokens` |
| `prompt_cache_retention=in_memory` | 优先回填到 `usage.cache_creation.ephemeral_5m_input_tokens` |
| `service_tier` | `usage.service_tier` |
| `priority` | `usage.speed=fast` |
| `context_management` | `message.context_management` |
| web search 次数 | `usage.server_tool_use.web_search_requests` |

### 流式 SSE 事件映射

bridge 会把 OpenAI Responses SSE 转成 Anthropic 风格流式事件。

| OpenAI SSE | Anthropic SSE |
| --- | --- |
| `response.output_item.added`（message text） | `content_block_start` |
| `response.output_text.delta` | `content_block_delta(text_delta)` |
| `response.output_text.done` / `response.content_part.done` | `content_block_stop` |
| `response.content_part.done` with annotations | `content_block_delta(citations_delta)` |
| `response.function_call_arguments.delta` | `content_block_delta(input_json_delta)` |
| `response.output_item.done(function_call)` | `tool_use` block 完成 |
| `response.output_item.done(local_shell_call)` | `tool_use(name=Bash)` |
| `response.output_item.done(tool_search_call)` | `tool_use(name=ToolSearch)` |
| `response.output_item.done(tool_search_output)` | `tool_search_tool_result` |
| `response.reasoning_text.delta` / `response.reasoning_summary_text.delta` | `content_block_delta(thinking_delta)` |
| `response.reasoning_text.done` / `response.reasoning_summary_text.done` | `signature_delta` + `content_block_stop` |
| `response.web_search_call.completed` | `server_tool_use` + `web_search_tool_result` |
| `response.output_item.done(compaction)` | `compaction` |
| `response.output_item.done(image_generation_call)` | `image_generation_call` |
| `response.completed` / `response.incomplete` / `response.cancelled` / `response.failed` | `message_delta` + `message_stop` |
| `error` | Anthropic `error` 事件 |

说明：

- `thinking` 会插入占位 `signature`，以兼容 Claude Code 的折叠 UI。
- `message done` 会避免与已经流式发出的文本块重复。
- 未识别的 output item 会尽量用 `responses_output_item` 发回，避免流式阶段直接丢失。

### Web search / Tool search / Local shell 细节

#### Web search

- OpenAI `web_search_call.action.query` → Anthropic `server_tool_use.input.query`
- `url_citation` annotations → `web_search_tool_result.content`
- `usage.server_tool_use.web_search_requests` 会累计回填

#### Tool search

- `tool_search_call.arguments` ↔ `tool_use(name=ToolSearch).input`
- `tool_search_output.tools` ↔ `tool_search_tool_result`
- 原始 `tools` JSON 会优先保留，无法精确恢复时退回 `tool_reference`

#### Local shell

- `local_shell_call.action` ↔ `tool_use(name=Bash).input`
- 如果上游是 `["bash","-lc","pwd"]` 这类命令，bridge 会尽量还原成更自然的 `command: "pwd"`

### Thinking / Reasoning

- Anthropic `thinking` 会映射到 OpenAI `reasoning`
- OpenAI `reasoning.summary` / `reasoning_text` 会映射回 Anthropic `thinking`
- 历史里的 `thinking` / `signature` 块不会再原样发回上游

### Compaction / 上下文压缩

| 场景 | 行为 |
| --- | --- |
| Anthropic `context_management` | 转成 OpenAI `context_management` compaction 项 |
| Anthropic `compaction` 历史块 | 转成 OpenAI `compaction` input item |
| OpenAI `compaction` / `compaction_summary` 输出 | 转成 Anthropic `compaction` |
| `POST /v1/responses/compact` | 原样透传到上游 |
| 自动压缩 `context_management` 模式 | 超阈值后注入 `compact_threshold` |
| 自动压缩 `responses_compact` 模式 | 超阈值后先调远程 compact，再重建 input |

### 未知 / 未来类型保留策略

对于当前未显式支持的 OpenAI output item：

- 非流式：转成 Anthropic `responses_output_item`
- 流式 done：同样尽量发成 `responses_output_item`
- 下一轮历史回放时再还原回 OpenAI input item

这可以减少新 item 类型导致的字段丢失。

## Token 计数与响应头

Claude Code 对 token 计数和 rate-limit 头比较敏感。

bridge 会处理：

- `POST /v1/messages/count_tokens`
- `anthropic-ratelimit-*`
- `anthropic-ratelimit-unified-*`
- 自动压缩前后的重新估算
- 图片、文档、搜索结果等多模态内容的计数近似
- cached token / reasoning token 相关 usage 映射
- prompt cache usage 的 Anthropic 兼容回填

这会影响：

- 状态栏上下文百分比
- 自动压缩触发时机
- usage / cost 展示
- request / quota 提示

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8787` | 监听地址 |
| `UPSTREAM_BASE_URL` | `https://api.openai.com` | 上游基础地址 |
| `UPSTREAM_API_KEY` | — | 上游 API Key |
| `AUTH_TOKEN` | 自动生成 | bridge 入站鉴权 token |
| `MODE` | `best_effort` | `strict` 或 `best_effort` |
| `MODEL_MAP` | — | 模型映射，如 `claude-sonnet-4-6=gpt-5.4:high` |
| `UPSTREAM_TIMEOUT_SECS` | `120` | 上游请求超时秒数 |
| `MAX_BODY_MB` | `10` | 最大请求体大小 |
| `CONTEXT_LIMIT` | `1048576` | 上下文窗口大小，用于兼容头和估算 |
| `OUTPUT_LIMIT` | `32000` | 最大输出 token 数 |
| `AUTO_COMPACT_MODE` | `off` | `off` / `context_management` / `responses_compact` |
| `AUTO_COMPACT_THRESHOLD_TOKENS` | — | 自动压缩阈值 |
| `PROMPT_CACHE_MODE` | `force_24h` | `off` / `auto` / `force_24h` |
| `PROMPT_CACHE_AUTO_KEY` | `true` | 是否自动生成稳定 `prompt_cache_key` |
| `DISABLE_RESPONSE_STORAGE` | — | 设置任意值时向上游发送 `store=false` |
| `LOG_LEVEL` | `info` | 日志级别 |
| `REDIS_URL` | — | Redis 存储地址；留空时使用内存 |
| `DATA_DIR` | 当前目录 | `runtime_config.json` 持久化目录 |

## Docker

仓库包含 `docker-compose.yml`，可以直接启动：

```bash
docker compose up -d
```

启动前请根据你的上游服务修改 compose 中的环境变量，例如：

- `UPSTREAM_BASE_URL`
- `UPSTREAM_API_KEY`
- `MODEL_MAP`

## 代码结构

```text
cmd/claude-bridge/main.go         启动入口
internal/server/                  HTTP 路由、消息处理、自动压缩
internal/transform/               Anthropic ↔ OpenAI 请求/响应/SSE 转换
internal/admin/                   管理面板与接口
internal/config/                  环境变量与 runtime_config
internal/tokenizer/               token 计数
internal/types/                   协议结构体
internal/upstream/                上游 HTTP 客户端
scripts/                          /fast 与自动压缩辅助脚本
```

## 开发与测试

运行测试：

```bash
go test ./...
```

如果修改了 `/fast` 或自动压缩安装脚本，也可以单独检查：

```bash
bash -n scripts/setup-claude-code-bridge.sh
```

## License

MIT
