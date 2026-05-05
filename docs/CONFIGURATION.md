# Configuration

> See also: `config.example.yml` for a complete annotated example.

MoonBridge 使用 YAML 配置文件，默认路径为 `${XDG_CONFIG_HOME:-$HOME/.config}/moonbridge/config.yml`。通过 `-config <path>` 可指定任意路径。

## 顶层结构

```yaml
mode: "Transform"         # Transform | CaptureResponse | CaptureAnthropic
server:
  addr: "127.0.0.1:38440"
  auth_token: ""           # Bearer token（可选）
models:                    # 模型元数据定义
providers:                 # 上游 Provider 连接信息
routes:                    # 模型别名 → Provider 映射
defaults:                  # 默认值
extensions:                # 插件配置
cache:                     # 缓存策略
web_search:                # Web Search 配置
log:
  level: "info"            # debug | info | warn | error
  format: "text"           # text | json
system_prompt: ""          # 全局系统提示词
trace:
  enabled: false           # 请求跟踪
```

## Mode

| 值 | 说明 |
|---|------|
| `Transform` | 默认模式。接收 OpenAI Responses 请求，转换为 Anthropic Messages，转发上游，再转换回 OpenAI 格式 |
| `CaptureResponse` | 透明代理 OpenAI Responses 流量 |
| `CaptureAnthropic` | 透明代理 Anthropic Messages 流量 |

## Server

```yaml
server:
  addr: "127.0.0.1:38440"       # 监听地址
  auth_token: "sk-..."           # Bearer token 认证（可选）
```

## Models

共享模型元数据定义，以 slug 为 key：

```yaml
models:
  deepseek-v4-pro:
    context_window: 1000000
    max_output_tokens: 384000
    display_name: "DeepSeek V4 Pro"
    default_reasoning_level: "high"
    supported_reasoning_levels:
      - effort: "high"
        description: "High reasoning effort"
    extensions:
      deepseek_v4:
        enabled: true
```

## Providers

上游 Provider 连接信息和协议声明：

```yaml
providers:
  deepseek:
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-..."
    protocol: "anthropic"          # anthropic（默认）| openai-response | openai-chat
    version: "2023-06-01"          # Anthropic API 版本（anthropic 协议）
    offers:
      - model: deepseek-v4-pro     # 引用 models 中的 slug
        upstream_name: ""           # 可选，上游真实模型名
        priority: 0                 # offer 优先级（越小越高）
        pricing:                    # 可选，定价（元/M tokens）
          input_price: 2
          output_price: 8
          cache_write_price: 1
          cache_read_price: 0.2
```

## Routes

客户端模型别名到 Provider + model 的映射：

```yaml
routes:
  moonbridge:
    model: deepseek-v4-pro
    provider: deepseek
```

API 请求中也可直接使用 `provider/model` 或 `model(provider)` 格式。

## Web Search

```yaml
web_search:
  support: "auto"           # auto | enabled | disabled | injected
  max_uses: 8
  tavily_api_key: "tvly-..."
  firecrawl_api_key: "fc-..."
  search_max_rounds: 5
```

## Cache

```yaml
cache:
  mode: "explicit"          # off | automatic | explicit | hybrid
  ttl: "5m"                 # 5m | 1h
  prompt_caching: true
  automatic_prompt_cache: false
  explicit_cache_breakpoints: true
  max_breakpoints: 4
  min_cache_tokens: 1024
```

## Extensions

```yaml
extensions:
  deepseek_v4:
    config:
      reinforce_instructions: true
      reinforce_prompt: "..."

  visual:
    config:
      provider: "kimi"
      model: "kimi-for-coding"
      max_rounds: 4
      max_tokens: 2048

  db_sqlite:
    config:
      path: "./data/moonbridge.db"

  metrics:
    enabled: true
```

## Proxy (Capture 模式)

```yaml
proxy:
  response:                    # CaptureResponse 模式
    model: "gpt-5.5"
    base_url: "https://api.openai.com"
    api_key: "sk-..."

  anthropic:                   # CaptureAnthropic 模式
    model: "claude-sonnet-4-6"
    base_url: "https://api.anthropic.com"
    api_key: "sk-ant-..."
    version: "2023-06-01"
```

## CLI 标志

| 标志 | 说明 |
|------|------|
| `-config <path>` | 指定配置文件路径 |
| `-addr <host:port>` | 覆盖监听地址 |
| `-mode <mode>` | 覆盖运行模式 |
| `-print-addr` | 打印监听地址并退出 |
| `-print-mode` | 打印运行模式并退出 |
| `-dump-config-schema` | 生成 config.schema.json |
| `-print-codex-config <model>` | 生成指定模型的 Codex config.toml |
| `-codex-home <dir>` | 指定 CODEX_HOME 并写入 models_catalog.json |
