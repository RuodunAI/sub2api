# 五份 `manifest_test.go` 里依赖引擎的断言（W2 导出）

导出自 `feat/forwarding-v2` 的 `2b0a41d00`。这五个文件今天用 `sdk/upstream` 的 `Resolve`、`Profile` 作断言；引擎删除后，W10a 到 W10e 把下面每一条改写成对新插件（`sdk/pluginsdk/standard` 的类型配置）的断言。azure_openai 没有黄金文件，这份清单就是它的验收依据（设计稿 7.3 节第 3、4 条）。

标 **保留** 的断言不依赖引擎，原样留下（只需去掉 `runtime none`、`upstream` 块相关的部分）；标 **移植** 的要改写。

## azure_openai（`plugins/azure-openai/manifest_test.go`，W10d）

两个类型 `azure_v1`、`azure_deployment`，都从 openai 预设出发。公共差异：`base_url` 必填且没有默认值，去掉后缀 `/openai/v1`、`/openai`；密钥不校验前缀；鉴权头是 `api-key: {api_key}`（不是 `Authorization`）；没有模型列表；`openai.responses_ws` 不服务。客户端请求头只转发 openai 平台的清单去掉 `openai-organization`、`openai-project` 之后的十个。

测试用的常量：资源地址 `https://res.openai.azure.com`，密钥 `0123456789abcdef0123456789abcdef`。

### `TestManifest`

- 保留：键、发布者、两个类型及顺序、`creationGroup`、`authMethodLabel` 中英文、各自服务的端点（v1：`chat_completions`、`responses`、`embeddings`；deployment：`chat_completions`、`embeddings`）、`passHeaders` 等于平台清单减两项、`sensitiveFields` 只有 `api_key`、没有 `guardedSettings`、`CustomUpstream()`、没有 `defaultModels`。
- 移植：版本号、`runtime` 改成 `grpc`；`assertJSON(at.Upstream.Layer(), declarations[...])` 改成对类型配置的断言（`upstream` 块不再存在）。

### `TestRequests`（移植，逐项）

输入：协议见下表，`meta.model = "prod/gpt 4o"`，流式与非流式各一次，账号设置 `api_version = 2025-04-01-preview`，客户端请求头 `user-agent: cli/1`、`openai-beta: assistants=v2`、`x-stainless-lang: python`、`openai-organization: org-client`、`openai-project: proj_client`、`authorization: Bearer sk-client`。

| 类型 | 协议 | 期望的 URL（空 = 没有路由，必须报错） |
|---|---|---|
| `azure_v1` | `openai.chat` | `<资源>/openai/v1/chat/completions` |
| `azure_v1` | `openai.responses` | `<资源>/openai/v1/responses` |
| `azure_v1` | `openai.embeddings` | `<资源>/openai/v1/embeddings` |
| `azure_v1` | `openai.responses_ws` | 空 |
| `azure_deployment` | `openai.chat` | `<资源>/openai/deployments/prod%2Fgpt%204o/chat/completions?api-version=2025-04-01-preview` |
| `azure_deployment` | `openai.embeddings` | `<资源>/openai/deployments/prod%2Fgpt%204o/embeddings?api-version=2025-04-01-preview` |
| `azure_deployment` | `openai.responses` | 空 |
| `azure_deployment` | `openai.responses_ws` | 空 |

有路由的每一项还断言：

- 方法 `POST`，上游模型等于部署名 `prod/gpt 4o`（部署名作为一个路径段转义）。
- 请求头恰好是 `api-key: <密钥>`、`content-type: application/json`、`user-agent: cli/1`、`openai-beta: assistants=v2`、`x-stainless-lang: python`。没有 `authorization`、`openai-organization`、`openai-project`。移植后 `api-key` 在权威头里，其余在默认头里。
- 补丁：只有 `openai.chat` 且流式时有一个，`stream_options.include_usage = true`；其余没有。移植后断言最终请求体。

### `TestBodyFilter`（移植）

两个类型都过滤 `service_tier`、`safety_identifier`（插件的 `azure()` 里写明，预设不带过滤字段）。输入 `{"model":"d","service_tier":"priority","safety_identifier":"u1","user":"u1","messages":[]}`，结果必须是 `{"model":"d","user":"u1","messages":[]}`（逐字节）。

这正是 README 里“顺序例外”实际涉及的两个字段：移植后过滤发生在运营方的参数覆盖之后。

### `TestDeploymentModelRequired`（移植）

`azure_deployment`、`openai.chat`、没有模型：必须报错，消息含 `model is required`（不能发往 `/openai/deployments//...`）。

### `TestResourceEndpoint`（移植）

两个类型各自：

- `base_url` 填 `<资源>`、`<资源>/`、`<资源>/openai`、`<资源>/openai/v1`、`<资源>/openai/v1/` 五种：凭证校验没有错误，保存的 `base_url`（`normalized_settings_json`）是 `<资源>`；账号测试请求的 URL 以 `<资源>/openai/` 开头，且不含 `/openai/openai`。
- `base_url` 不填：校验恰好一个错误，字段 `base_url`、代码 `required`、消息 `Resource endpoint is required / 请填写资源地址`；构造请求必须报错。

### `TestResourceEndpointForm`（表单部分保留，校验与测试请求部分移植）

- 保留：两份表单的 `base_url` 正则相同；`rejected` 列表里的每个值都不匹配，`accepted` 里的每个值都匹配；`ui:errorMessages.pattern` 的中英文文案。
- 移植：`accepted` 里每个输入，校验没有错误、保存的地址是表中的“基础地址”、账号测试请求以 `<基础地址>/openai/` 开头。

`accepted`（输入 → 基础地址）与 `rejected` 两张表很长，以测试文件里的为准，移植时整表照搬。

### `TestAccountTest`（移植）

账号设置 `api_version = 2024-10-21`：

| 类型 | 测试的模型 | URL |
|---|---|---|
| `azure_v1` | 空、`my-gpt` | 都是 `<资源>/openai/v1/chat/completions` |
| `azure_deployment` | 空 | `<资源>/openai/deployments/gpt-4o-mini/chat/completions?api-version=2024-10-21` |
| `azure_deployment` | `my-gpt` | `<资源>/openai/deployments/my-gpt/chat/completions?api-version=2024-10-21` |

每项：方法 `POST`，请求头恰好是 `api-key`、`content-type: application/json`，`usage_protocol` 是 `openai.chat`。两个类型的模型列表请求都必须报错。

### `TestClassify`（移植）

openai 的分类策略原样，没有 Azure 特有的规则。时钟 `1800000000`，`meta.protocol = openai.chat`，类型 `azure_v1`：

| 情形 | 状态码 | 响应头 | 动作 | 账号影响 | 冷却秒数 |
|---|---|---|---|---|---|
| `content_filter` | 400 | — | 返回客户端 | 无 | — |
| `BadRequest` | 400 | — | 返回客户端 | 无 | — |
| `DeploymentNotFound` | 404 | — | 返回客户端 | 无 | — |
| 密钥无效 | 401 | — | 换号 | 禁用 | — |
| 禁止 | 403 | — | 换号 | 禁用 | — |
| 限流 | 429 | `retry-after-ms: 30000`、`retry-after: 31` | 换号 | 冷却 | 30 |
| 限流 | 429 | `retry-after: 7` | 换号 | 冷却 | 7 |
| 限流 | 429 | — | 换号 | 冷却 | 60 |
| 超时 | 408 | — | 换号 | 冷却 | 10 |
| 服务器错误 | 500 | — | 换号 | 冷却 | 10 |
| 不可用 | 503 | — | 换号 | 冷却 | 10 |

响应体原文以测试文件为准。

### `TestForms`（保留）

只读表单文件，不依赖引擎。

## openai（`plugins/openai/manifest_test.go`，W10a）

### `TestManifest`

- 保留：键、发布者、一个类型 `apikey` 服务内置 `openai` 平台。
- 移植：版本号；`runtime` 改成 `grpc`；`assertJSON(at.Upstream.Layer(), declaration)` 改成对类型配置的断言；`guardedSettings` 恰好一项，`base_url` 只允许默认上游地址（今天从 `p.Upstream().BaseURL.Default` 取，值是 `https://api.openai.com`）。

### `TestOrganizationAndProject`（移植）

线上的类型比黄金样例用的类型多两处：凭证字段 `organization`、`project`，以及两个头 `openai-organization: {credentials.organization}`、`openai-project: {credentials.project}`（`set`）。

对每个路由（`openai.chat`、`openai.responses`、`openai.responses_ws`、`openai.embeddings`，含 WebSocket 握手），`meta.model = gpt-4o-mini`、流式，密钥 `sk-test-0123456789`，客户端请求头取“无”和 `openai-organization: org-client`、`openai-project: proj_client`、`user-agent: cli/1` 两种：

- 账号设置为空或 `{"organization":"","project":""}`：回答与不带这两处差异的类型**完全相同**（所以客户端自己的两个头照常透传）。这一条保证了黄金样例对线上的类型配置成立。
- `{"organization":"org-acct","project":"proj_acct"}`：两个头是 `org-acct`、`proj_acct`。
- 只设 `organization`：`openai-organization` 是 `org-acct`，`openai-project` 是客户端的值（没有就没有）。
- 只设 `project`：对称。

即：账号上设置的值覆盖客户端的头，没设置的保持透传。

## anthropic（`plugins/anthropic/manifest_test.go`，W10e）

`TestManifest` 里依赖引擎的只有三处，其余（数据库、平台、表单、权限、路由、升级夹具）保留：

- 移植：`assertJSON(at.Upstream.Layer(), declaration)`（`{"preset":"anthropic","filterFields":[]}`）改成对类型配置的断言；特别是过滤字段为空这一点要保住。
- 移植：`guardedSettings` 恰好一项，`base_url` 只允许默认上游地址（`https://api.anthropic.com`）。
- **要反过来**：今天断言清单**不**声明 `platform.execute.v1`（“进程只提供目录路由”），也**不**持有 `accounts.credentials`（“凭证留在核心里，由核心构造请求”）。改写后插件自己构造请求：`platform.execute.v1` 这个能力不再存在（设计稿 D6）；插件要读到账号的凭证，今天凭证只发给持有 `accounts.credentials` 的插件（`common.proto` 里 `Account.credentials_json` 的注释），这一条多半要反过来，以 W1、W9 定下的规则为准。这两条断言要按新契约重写，不能照搬。

## gemini（`plugins/gemini/manifest_test.go`，W10b）

`TestManifest`：

- 保留：键、发布者、一个类型 `apikey` 服务内置 `gemini` 平台。
- 移植：版本号；`runtime` 改成 `grpc`；声明 `{"preset":"gemini","filterFields":[]}` 改成对类型配置的断言；`guardedSettings` 的 `base_url` 只允许 `https://generativelanguage.googleapis.com`。

## relay（`plugins/relay/manifest_test.go`，W10c）

`TestManifest`：

- 保留：键、发布者、没有平台、一个类型 `relay_key` 服务内置 `anthropic` 平台、`passHeaders` 恰好是 `anthropic-version`、`anthropic-beta`。
- 移植：版本号；`runtime` 改成 `grpc`；声明与 anthropic 预设的四处差异改成对类型配置的断言：`base_url` 没有默认值且必填、文案 `baseURLRequired` 是 `Base URL of the relay is required / 请填写中转站的 Base URL`、没有默认协议、过滤字段为空；类型配置能通过检查（今天是 `upstream.Resolve` 不报错）。

## 五份文件共有的 `load`

每个文件的 `load` 用 `check.Validate(..., Tooling: true)` 检查清单，并要求 `check.Warnings` 为空。今天这一步顺带用引擎解析 `upstream` 块；W1 之后清单里没有这一块，`load` 本身保留。
