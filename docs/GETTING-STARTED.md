# Getting Started

> 5 分钟跑通第一个对话。更多用法见 [CookBook.md](../CookBook.md)。

## 1. 安装

```bash
git clone <repo> && cd moonbridge
go mod download
go build -o moonbridge ./cmd/moonbridge
```

或下载预编译二进制（GitHub Releases）。

## 2. 创建配置

```bash
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/moonbridge"
cp config.example.yml "${XDG_CONFIG_HOME:-$HOME/.config}/moonbridge/config.yml"
```

编辑 `config.yml`，填入至少一个 Provider 的 `api_key`：

```yaml
providers:
  my-provider:
    base_url: "https://api.example.com"
    api_key: "sk-your-api-key-here"

routes:
  default:
    model: my-model
    provider: my-provider

defaults:
  model: default
```

## 3. 启动

```bash
go run ./cmd/moonbridge
```

默认监听 `127.0.0.1:38440`。

## 4. 验证

```bash
# 查看可用模型
curl http://localhost:38440/v1/models

# 发送测试请求
curl http://localhost:38440/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "default",
    "input": "你好，用一句话介绍自己。",
    "max_output_tokens": 100
  }'
```

## 5. 与 Codex CLI 集成

```bash
./scripts/start_codex_with_moonbridge.sh --project-directory "$PWD"
```

或手动配置 `~/.codex/config.toml`。详见 [README.md](../README.md#与-codex-cli-一起使用)。

## 下一步

- [完整配置指南](CONFIGURATION.md)
- [架构说明](architecture.md)
- [API 参考](api.md)
