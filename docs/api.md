# API 接口

Moon Bridge 对外暴露 OpenAI Responses 兼容端点和可选的管理端点。

## 基础信息

| 项目 | 默认值 |
|------|--------|
| 监听地址 | `127.0.0.1:38440` |
| 认证 | 默认无本地认证；配置 `server.auth_token` 后要求 `Authorization: Bearer <token>` |

可通过 `-addr` 覆盖监听地址，通过 `config.yml` 的 `server.addr` 配置。

## `POST /v1/responses`

主要端点，Codex CLI 发送对话请求。支持流式和非流式。`POST /responses` 是同一处理器的兼容路径。

### 请求格式

兼容 OpenAI Responses API 格式。完整定义见 `internal/foundation/openai/types.go`。

```json
{
  "model": "moonbridge",
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}
  ],
  "instructions": "System prompt here",
  "max_output_tokens": 65536,
  "temperature": 0.7,
  "tools": [
    {"type": "function", "name": "get_weather", "description": "...", "parameters": {...}},
    {"type": "web_search_preview"},
    {"type": "local_shell"},
    {"type": "custom", "name": "edit", "format": {"definition": "..."}}
  ],
  "tool_choice": "auto",
  "stream": false,
  "reasoning": {"effort": "high"}
}
```

### 支持的字段

| 字段 | 支持情况 | 说明 |
|------|----------|------|
| `model` | ✅ 必填 | 模型别名或 `provider/model` 引用 |
| `input` | ✅ | 消息数组或纯文本字符串 |
| `instructions` | ✅ | 系统指令，与 system prompt 合并 |
| `max_output_tokens` | ✅ | 默认 65536 |
| `temperature` | ✅ | 映射到 Anthropic temperature |
| `top_p` | ✅ | 映射到 Anthropic top_p |
| `stop` | ✅ | 停止序列 |
| `tools` | ✅ | 支持 function / web_search_preview / local_shell / custom |
| `tool_choice` | ✅ | auto / none / required / {"type":"function","name":"..."} |
| `stream` | ✅ | true = SSE 流式 |
| `reasoning` | ✅ | 传递 reasoning.effort 给支持层 |

### 支持的 Tool 类型

#### `function`
标准函数调用，转换为 Anthropic tool_use。

#### `web_search_preview` / `web_search`
根据提供商配置转换为：
- **auto**：启动时探测上游是否支持，支持才注入 Anthropic 原生 `web_search_20250305`
- **enabled**：始终注入 Anthropic 原生 `web_search_20250305` server tool
- **injected**：转换为 `tavily_search` + `firecrawl_fetch` function tools，由服务端执行
- **disabled**：忽略

#### `local_shell`
Codex 本地 shell 执行，转换为 Anthropic `local_shell` tool。

#### `custom`
Codex 自定义工具，根据 grammar 类型转换为：
- **raw**：通用自定义工具
- **apply_patch**：拆分为 `add_file` / `update_file` / `delete_file` / `replace_file` / `batch` 五个子工具
- **exec**：转换为 Code Mode exec 代理工具

### 响应格式（非流式）

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1234567890,
  "status": "completed",
  "model": "moonbridge",
  "output": [
    {
      "type": "message",
      "id": "msg_item_0",
      "status": "completed",
      "role": "assistant",
      "content": [{"type": "output_text", "text": "Hello!"}]
    },
    {
      "type": "reasoning",
      "summary": [{"type": "summary_text", "text": "Thinking..."}]
    },
    {
      "type": "function_call",
      "id": "fc_tooluse_0",
      "call_id": "toolu_...",
      "name": "get_weather",
      "arguments": "{\"location\": \"Beijing\"}",
      "status": "completed"
    }
  ],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 50,
    "input_tokens_details": {"cached_tokens": 30}
  }
}
```

### 流式响应格式（SSE）

SSE 事件格式：

```
event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_...","status":"in_progress",...}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{...}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hel"}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"Hello!"}

event: response.completed
data: {"type":"response.completed","response":{...}}
```

支持的事件类型：

| 事件 | 说明 |
|------|------|
| `response.created` | 响应创建 |
| `response.in_progress` | 响应处理中 |
| `response.output_item.added` | 新增输出项 |
| `response.output_item.done` | 输出项完成 |
| `response.content_part.added` | 新增内容片段 |
| `response.content_part.done` | 内容片段完成 |
| `response.output_text.delta` | 文本增量 |
| `response.output_text.done` | 文本完成 |
| `response.function_call_arguments.delta` | 函数参数 JSON 增量 |
| `response.function_call_arguments.done` | 函数参数完成 |
| `response.custom_tool_call_input.delta` | 自定义工具输入增量 |
| `response.completed` | 响应完成 |
| `response.incomplete` | 响应不完整 |
| `response.failed` | 响应失败 |

### 错误响应

```json
{
  "error": {
    "message": "提供商错误：rate limit exceeded",
    "type": "server_error",
    "code": "provider_error"
  }
}
```

错误类型映射：

| HTTP 状态码 | 说明 |
|-------------|------|
| 400 | 请求参数错误 |
| 401 | API Key 无效 |
| 403 | 权限不足 |
| 429 | 速率限制 |
| 502 | 上游提供商错误 |
| 504 | 上游超时 |

## `GET /v1/models`

返回模型目录，用于 Codex CLI 的模型发现。

`GET /models` 是同一处理器的兼容路径。

### 响应格式

```json
{
  "models": [
    {
      "slug": "deepseek-v4-pro(deepseek)",
      "display_name": "DeepSeek V4 Pro(deepseek)",
      "description": "DeepSeek V4 with selectable high/xhigh reasoning effort.",
      "default_reasoning_level": "high",
      "supported_reasoning_levels": [
        {"effort": "high", "description": "High reasoning effort"},
        {"effort": "xhigh", "description": "Extra high reasoning effort (maps to DeepSeek max)"}
      ],
      "shell_type": "unified_exec",
      "visibility": "list",
      "supported_in_api": true,
      "supports_reasoning_summaries": true,
      "default_reasoning_summary": "auto",
      "web_search_tool_type": "text",
      "apply_patch_tool_type": "freeform",
      "truncation_policy": {"mode": "tokens", "limit": 10000},
      "supports_parallel_tool_calls": true,
      "context_window": 1000000,
      "max_context_window": 1000000,
      "effective_context_window_percent": 95,
      "input_modalities": ["text"]
    }
  ]
}
```

模型目录的生成逻辑在 `internal/extension/codex/catalog.go` 的 `BuildModelInfosFromConfig()` 中：

1. 优先使用 `provider.providers.<key>.models` 中的模型目录
2. 追加 `provider.routes` 中的别名作为补充
3. 为每个模型生成 `base_instructions`（来自 `default_instructions.txt` 模板）

## 开发中：`GET /v1/admin/metrics`

> 该端点来自 dev 分支的持久化/metrics 工作，不视为稳定公开 API。当 `metrics` 扩展启用并成功绑定数据库 store 时，会注册该管理端点；如果没有可用数据库或 metrics 被禁用，路由不会注册。

启用 `metrics` 扩展并绑定数据库 Provider 后可用。接口返回最近请求指标，支持 `limit`、`offset`、`model`、`status`、`since`、`until`、`order=asc` 查询参数。

### 响应格式

```json
{
  "records": [
    {
      "id": 1,
      "timestamp": "2026-04-30T06:40:00Z",
      "model": "moonbridge",
      "actual_model": "kimi-for-coding",
      "input_tokens": 85822,
      "output_tokens": 145,
      "cache_creation": 0,
      "cache_read": 85248,
      "protocol": "anthropic",
      "usage_source": "anthropic_stream",
      "raw_input_tokens": 85822,
      "raw_output_tokens": 145,
      "raw_cache_creation": 0,
      "raw_cache_read": 85248,
      "normalized_input_tokens": 85822,
      "normalized_output_tokens": 145,
      "normalized_cache_creation": 0,
      "normalized_cache_read": 85248,
      "raw_usage_json": "{\"input_tokens\":85822,\"cache_read_input_tokens\":85248}",
      "cost": 0.0,
      "response_time": 150000000,
      "status": "success"
    }
  ],
  "count": 1
}
```

字段口径：

- `protocol`：请求实际走的 Provider 协议，目前为 `anthropic` 或 `openai-response`。
- `usage_source`：usage 来源，常见值为 `anthropic_response`、`anthropic_stream`、`openai_response`、`openai_sse`；错误或未拿到 usage 时为 `none` 或空值。
- `raw_*`：Provider 原始 telemetry。Anthropic 来自 `usage` / stream event usage，OpenAI Responses 来自响应 body 或 SSE usage。
- `normalized_*`：Moon Bridge 用于 session 统计、费用计算和历史字段兼容的口径。
- `input_tokens`、`output_tokens`、`cache_creation`、`cache_read`：历史兼容字段，当前等同于 normalized 口径。
- `raw_usage_json`：原始 usage JSON 的快照。OpenAI Responses 原生 usage 不提供 cache creation，因此 `raw_cache_creation` 为 0。

## 命令行工具

Moon Bridge 提供以下命令行开关：

| 开关 | 说明 |
|------|------|
| `-config` | 指定配置文件路径；未指定时读取 `${XDG_CONFIG_HOME:-$HOME/.config}/moonbridge/config.yml` |
| `-addr` | 覆盖监听地址 |
| `-mode` | 覆盖运行模式 |
| `-print-addr` | 打印监听地址并退出 |
| `-print-mode` | 打印运行模式并退出 |
| `-print-default-model` | 打印默认模型别名并退出 |
| `-print-codex-model` | 打印 Codex 模型别名并退出 |
| `-print-claude-model` | 打印 Claude model 并退出 |
| `-print-codex-config` | 生成指定模型的 Codex config.toml 并退出 |
| `-codex-base-url` | 生成 config.toml 时使用的 Base URL |
| `-codex-home` | 指定 CODEX_HOME，同时写入 models_catalog.json |
| `-dump-config-schema` | 在配置文件旁生成 JSON Schema 并退出 |

---

## 管理 API (`/api/v1/*`)

管理 API 提供可视化面板和自动化工具所需的 RESTful 端点。所有端点路径前缀为 `/api/v1`（WebUI 自动添加该前缀）。认证方式与主 API 一致（Bearer Token）。

### Provider 管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/providers` | 列出所有 Provider（支持 `limit`、`offset` 分页） |
| `GET` | `/api/v1/providers/{key}` | 获取单个 Provider 详情（API Key 脱敏显示） |
| `PUT` | `/api/v1/providers/{key}` | 创建/替换 Provider（暂存为变更，需 apply 生效） |
| `PATCH` | `/api/v1/providers/{key}` | 部分更新 Provider（API Key 传 `******` 表示保留原值） |
| `DELETE` | `/api/v1/providers/{key}` | 删除 Provider（暂存为变更） |
| `POST` | `/api/v1/providers/{key}/test` | 测试 Provider 连接 |

#### PUT /api/v1/providers/{key}

```json
{
  "base_url": "https://api.example.com",
  "api_key": "sk-...",
  "version": "2024-01-01",
  "protocol": "anthropic",
  "user_agent": ""
}
```

响应（暂存成功）：

```json
{
  "change_id": 42,
  "status": "pending",
  "message": "变更已暂存，请调用 POST /changes/apply 使其生效"
}
```

#### PATCH /api/v1/providers/{key}

对于已有 Provider，API Key 传 `"******"` 可保留原值（WebUI 脱敏显示时使用此约定）：

```json
{
  "base_url": "https://new-endpoint.example.com",
  "api_key": "******"
}
```

#### POST /api/v1/providers/{key}/test

```json
{
  "provider": "anthropic",
  "base_url": "https://api.anthropic.com",
  "success": true,
  "duration": "145ms",
  "timestamp": "2026-05-01T12:00:00Z"
}
```

### Offer 管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/v1/providers/{key}/offers` | 新增 Offer |
| `PATCH` | `/api/v1/providers/{key}/offers/{model}` | 更新 Offer |
| `DELETE` | `/api/v1/providers/{key}/offers/{model}` | 删除 Offer |

### Model 管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/models` | 列出所有 Model |
| `GET` | `/api/v1/models/{slug}` | 获取单个 Model 详情 |
| `PUT` | `/api/v1/models/{slug}` | 创建/更新 Model |
| `DELETE` | `/api/v1/models/{slug}` | 删除 Model（被 Provider Offer 引用时返回 409） |

### Route 管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/routes` | 列出所有 Route |
| `GET` | `/api/v1/routes/{alias}` | 获取单个 Route 详情 |
| `PUT` | `/api/v1/routes/{alias}` | 创建/更新 Route |
| `DELETE` | `/api/v1/routes/{alias}` | 删除 Route |

### 配置管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/config/effective` | 获取当前生效配置（FileConfig 格式） |
| `GET` | `/api/v1/config/export` | 导出配置为 YAML（支持 `?include_secrets=true`） |
| `POST` | `/api/v1/config/import` | 导入 YAML 配置（校验后返回预览，不自动应用） |
| `POST` | `/api/v1/config/validate` | 校验配置格式 |

#### POST /api/v1/config/import

```json
{"yaml": "mode: Transform\nproviders:\n  test:\n    base_url: https://test.api.com\n    api_key: sk-test-key\n    offers:\n      - model: gpt-4o\nmodels:\n  gpt-4o:\n    display_name: GPT-4o\n"}
```

响应：

```json
{
  "providers": 1,
  "models": 1,
  "routes": 0,
  "has_defaults": false,
  "has_web_search": false,
  "warnings": [],
  "message": "配置已通过校验，请确认后通过 changes apply 使其生效"
}
```

### 变更管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/changes` | 列出所有待应用变更 |
| `POST` | `/api/v1/changes/apply` | 应用所有待处理变更（事务性，失败自动回滚） |
| `POST` | `/api/v1/changes/discard` | 丢弃所有待处理变更 |

#### POST /api/v1/changes/apply 响应

```json
{"status": "success", "message": "变更已应用生效"}
```

#### ChangeRow 对象

```json
{
  "id": 42,
  "batch_id": "",
  "action": "create",
  "resource": "provider",
  "target_key": "deepseek-v4",
  "before": "",
  "after": "{\"base_url\":\"https://api.deepseek.com\",\"api_key\":\"sk-...\",\"version\":\"v1\",\"protocol\":\"anthropic\"}",
  "applied": false,
  "error": "",
  "revision": 0,
  "created_at": "2026-05-01T12:00:00Z",
  "applied_at": ""
}
```

### 状态与统计

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/status` | 系统概览（Provider/Route 数量、运行模式、监听地址） |
| `GET` | `/api/v1/status/providers` | Provider 健康状态列表 |
| `GET` | `/api/v1/sessions` | 活跃会话列表（Key 脱敏） |
| `GET` | `/api/v1/stats` | 完整统计信息 |
| `GET` | `/api/v1/stats/summary` | 统计摘要（请求数、Token、缓存命中率、总费用） |
| `GET` | `/api/v1/logs` | 日志条目（当前返回空列表，等待日志环形缓冲区实现） |
| `GET` | `/api/v1/version` | 版本信息 |

### 设置

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/defaults` | 获取默认配置 |
| `PUT` | `/api/v1/defaults` | 更新默认配置 |
| `GET` | `/api/v1/web-search` | 获取 Web Search 配置 |
| `PUT` | `/api/v1/web-search` | 更新 Web Search 配置 |
| `GET` | `/api/v1/extensions` | 列出已注册扩展 |
| `GET` | `/api/v1/extensions/{name}` | 获取扩展详情 |
| `PUT` | `/api/v1/extensions/{name}` | 更新扩展配置 |

### 错误格式

所有管理 API 错误使用统一格式：

```json
{
  "error": {
    "code": "not_found",
    "message": "provider \"nonexistent\" 不存在"
  }
}
```

常见错误码：`invalid_json`、`validation_error`、`not_found`、`referenced`、`stage_error`、`apply_error`、`list_error`、`discard_error`、`export_error`、`parse_error`。

### 分页格式

列表端点支持 `limit`（默认 20，最大 100）和 `offset`（默认 0）查询参数：

```json
{
  "data": [...],
  "total": 42,
  "limit": 20,
  "offset": 0
}
```
