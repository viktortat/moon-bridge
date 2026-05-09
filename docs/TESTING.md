# Testing

Moon Bridge 使用 Go 标准库 `testing` 包，无外部测试框架依赖。

## 运行测试

```bash
# 全量测试
go test ./...

# 包级别
go test ./internal/protocol/anthropic/...

# 详细输出
go test -v -count=1 ./internal/protocol/...

# 测试覆盖率
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

## 测试层级

### 1. 单元测试

位置与被测试包同目录。包括 Adapter 转换测试、服务器路由处理测试、各 extension 和基础包下的测试。使用 Mock HTTP 服务器，不依赖真实 API。

### 2. 协议转换 E2E 测试

位置：`internal/e2e/`

包含 6 个独立测试文件，覆盖所有 4 条转换路径 + 插件 + Web Search：

| 测试文件 | 覆盖范围 |
|-----------|---------|
| `anthropic_e2e_test.go` | Anthropic Messages 协议转换 |
| `google_genai_e2e_test.go` | Google Gemini 协议转换 |
| `openai_chat_e2e_test.go` | OpenAI Chat 协议转换 |
| `openai_response_e2e_test.go` | OpenAI Responses 直通 |
| `plugin_hooks_e2e_test.go` | CorePluginHooks 全链路集成 |
| `websearch_injection_e2e_test.go` | Web Search 注入路径 |

支持 Mock 模式（默认）和真实 Provider 模式（需配置 `.env.test`）。

### 3. 服务层 E2E 测试

位置：`internal/service/e2e/` — 完整 HTTP 请求/响应链路测试。

### 4. 管理 API 测试

位置：`internal/service/api/` — 管理 API 端点功能测试和集成测试。

## 运行 E2E 测试

```bash
# Mock 模式（无需 API Key）
go test ./internal/e2e/... -v -count=1

# 真实 Provider 模式
cd internal/e2e && PROVIDER=deepseek go test -v -count=1 -run TestAnthropicE2E
cd internal/e2e && PROVIDER=gemini go test -v -count=1 -run TestGoogleGenAIE2E
cd internal/e2e && PROVIDER=openai-chat go test -v -count=1 -run TestOpenAIChatE2E
cd internal/e2e && PROVIDER=openai go test -v -count=1 -run TestOpenAIResponseE2E
cd internal/e2e && PROVIDER=plugin-websearch go test -v -count=1 -run TestPluginHooksE2E
```

## 编写测试

- 使用 `httptest.NewServer` 模拟上游 API
- 协议转换测试通过 Core 格式 ⇄ 协议格式的相互转换验证正确性
- 覆盖率目标：单元测试 ≥ 95%，E2E 覆盖所有协议路径
