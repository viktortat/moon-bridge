# 贡献指南

感谢您对 Moon Bridge 的关注！欢迎通过 Issue 和 Pull Request 参与贡献。

## 报告问题

- 使用 GitHub Issues 提交
- 请包含：运行环境、配置（脱敏后）、复现步骤、预期行为与实际行为
- 如果涉及 API 错误，请附上请求跟踪日志（启用 `trace.enabled: true`）

## 提交代码

### 分支策略

- `main` — 稳定版本
- `dev` — 开发分支，所有 PR 合入此分支
- `fix/*` — 修复分支
- `feat/*` — 功能分支

### 开发流程

1. Fork 仓库并创建功能分支：`git checkout -b feat/my-feature`
2. 编写代码并添加测试
3. 运行全量测试：`go test ./...`
4. 提交 PR 到 `dev` 分支

### 代码规范

- 使用 `log/slog` 进行结构化日志
- 文件名反映职责（如 `candidate_routing_test.go`），不使用项目管理编号
- 协议转换统一使用 `format.CoreRequest` / `CoreResponse` 作为中间表示
- 新增 Adapter 必须同时实现 `ProviderAdapter` 和 `ProviderStreamAdapter`

### 测试要求

- 单元测试覆盖新增代码
- 协议转换必须包含 E2E 测试
- 运行全量测试确保无回归

## 添加新 Provider

1. 在 `config.go` 中添加 `ProviderDef` 字段
2. 实现 Protocol Adapter
3. 注册到 `format.Registry`
4. 在 `dispatch.go` 中添加协议分支
5. 添加 E2E 测试

## 许可证

本项目采用 [GPL v3](LICENSE) 许可证。提交代码即表示您同意代码在此许可证下发布。
