# WebUI 管理面板使用指南

Moon Bridge 内置了一个基于 Preact 的 WebUI 管理面板，通过 Go embed 集成到二进制中。通过 WebUI，运维人员可以可视化地管理 Provider、Model、Route，暂存并应用配置变更。

## 访问方式

启动 Moon Bridge 后，在浏览器中打开：

```
http://<server-addr>/
```

默认地址为 `http://127.0.0.1:38440/`。

## 登录

如果服务配置了 `server.auth_token`，访问 WebUI 时会跳转到登录页面：

1. 输入 Auth Token（与 API 请求使用的 `Authorization: Bearer` 令牌相同）
2. 点击 **Sign In**
3. 登录成功后 Token 会保存在 `sessionStorage` 中，关闭标签页后需重新登录

若未配置 Auth Token，WebUI 直接进入 Dashboard，无需认证。

## 页面导航

WebUI 采用侧栏 + 顶栏布局。侧栏导航项：

| 导航 | 说明 |
|------|------|
| Dashboard | 概览面板：Provider/Model/Route 总数、活跃会话、最近变更、健康概览 |
| Providers | Provider 列表与编辑 |
| Models | Model 定义管理 |
| Routes | 路由规则管理 |
| Settings | 默认设置、Web Search、Extensions 配置 |
| Import | YAML 配置导入 |
| Changes | 变更暂存区管理 |
| Logs | 日志查看 |

---

## Dashboard（仪表盘）

Dashboard 是运维人员的"作战室"，按区域展示：

- **顶部统计卡片**：Provider 总数（含健康/降级/离线数量）、Model 总数、Route 总数、活跃会话数
- **最近变更**：最近 5 条变更记录（时间、操作、资源、目标、状态）
- **Provider 健康概览**：表格展示每个 Provider 的名称、状态、延迟、最后错误、可用率，提供 [Test] 和 [Edit] 操作

## Providers（Provider 管理）

### Provider 列表

展示所有已配置的 Provider，每一行包含：名称、协议、Model 数量、状态、延迟、最后检查时间、操作按钮。

- 支持搜索（按名称过滤）、按状态筛选、按协议筛选
- 提供列配置菜单（显示/隐藏列）
- 支持分页

### Provider 编辑

点击列表中的 Provider 名称或编辑按钮进入编辑页面。编辑页分两栏：

**左侧 - 基本信息**：
- 名称（Key，不可编辑）
- Base URL（必填）
- API Key（密码输入框，支持显示/隐藏切换，始终脱敏显示）
- 协议（anthropic / openai-response 单选）
- User Agent（可选）
- **Test Connection** 按钮：点击后测试与上游的连接，显示结果（成功/失败 + 延迟）

**右侧 - Offers（关联的 Model）**：
- 列出该 Provider 提供的所有 Model Offers
- 支持添加、编辑、删除 Offer

页面底部提供 [Save] 按钮（暂存变更）和 [Cancel] 按钮（返回列表）。

### 添加 Provider

点击列表页的 [+ Add Provider] 按钮，进入空白的编辑页面。

## Models（Model 管理）

### Model 列表

展示所有已注册的 Model 定义，包含：Slug、显示名称、上下文窗口、关联的 Provider 列表。支持搜索和分页。

### Model 编辑

- **Display Name**：模型显示名称
- **Context Window**：上下文窗口大小
- **Max Output Tokens**：最大输出 Token 数
- **Description**：描述

保存后变更会进入暂存区。

## Routes（路由管理）

### Route 列表

展示所有路由，包含：别名（Alias）、Model、Provider、显示名称。

### Route 编辑

- **Alias**：路由别名（不可编辑）
- **Model Slug**：目标 Model
- **Provider Key**：目标 Provider

保存后变更进入暂存区。

## Settings（设置）

### Defaults

配置默认值：
- **Model**：默认模型别名
- **Max Tokens**：默认最大输出 Token 数（0 表示不限制）
- **System Prompt**：默认系统提示词

### Web Search

- **Support**：模式（auto / enabled / disabled / injected）
- **Max Uses**：最大使用次数
- **Tavily API Key** / **Firecrawl API Key**：搜索服务密钥（安全脱敏显示）
- **Search Max Rounds**：最大搜索轮次

### Extensions

列出已注册的扩展/插件，支持查看详情和编辑。

## Import（配置导入）

将完整的 YAML 配置导入到暂存区：

1. 在文本区域粘贴 YAML 配置内容，或上传文件
2. 点击 **Validate** 校验配置格式
3. 查看预览（解析出的 Provider、Model、Route 数量）
4. 确认后生成变更，自动跳转到 Changes 页面进行 Apply

## Changes（变更管理）

### Pending Changes

列出所有未应用的变更，每条包含：时间、操作（CREATE / UPDATE / DELETE）、资源类型、目标名称。

- **View Before / After**：点击查看变更前后的 JSON 对比
- **[×]** 单条丢弃
- **[Discard All]** 丢弃所有暂存变更
- **[Apply All]** 应用所有暂存变更（会触发运行时重载）

### Applied Changes

已应用的变更历史（折叠区域），默认收起。

## Logs（日志查看）

实时日志查看器，支持：
- **过滤**：按关键词搜索、按级别筛选（DEBUG / INFO / WARN / ERROR）、按组件筛选
- **暂停/继续**：⏸ Pause 按钮冻结自动滚动
- **导出**：⬇ Export 下载日志文件

日志使用等宽字体显示在暗色背景上，不同级别有颜色编码。

## 导入导出

### 配置导出

通过 `GET /config/export` 或 WebUI 的导出功能，可将当前生效配置导出为 YAML 文件：

```
GET /config/export?include_secrets=false
```

- `include_secrets=false`（默认）：API Key 会被脱敏
- `include_secrets=true`：包含原始 API Key（需谨慎使用）

### 配置导入

通过 WebUI Import 页面或 API：

```
POST /config/import
Content-Type: application/json

{"yaml": "mode: Transform\nproviders:\n  ..."}
```

导入支持完整的 YAML 配置格式，包含 providers、models、routes 等所有字段。导入后不会自动应用，需到 Changes 页面确认后 Apply。

## 与 API 的关系

WebUI 的所有操作均通过 Management API（`/api/v1/*`）完成。WebUI 是 API 的可视化封装，两者功能等价。例如：

| WebUI 操作 | 对应 API |
|-----------|----------|
| 添加 Provider | `PUT /api/v1/providers/{key}` |
| 查看变更 | `GET /api/v1/changes` |
| 应用变更 | `POST /api/v1/changes/apply` |
| 导入配置 | `POST /api/v1/config/import` |

详见 [API 文档](api.md#管理-api-v1)。
