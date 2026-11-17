# 云厂商上游：Vertex AI Gemini、Azure OpenAI、AWS Bedrock，以及 OpenAI 的组织 / 项目参数

2026-11-17，设计稿，未实现。代码依据：develop `10d459e59`（核心最新发布 0.1.302）。文中“核心 0.1.N”指带上第 8 节两个扩展点的下一次核心发布，版本号实现时填。

本文只写设计。没有任何真实云账号可用，所有厂商行为来自 2026-11 的官方文档，第 12 节列出没有实测过的地方。

## 1. 背景与目标

现在能接的上游是 OpenAI、Anthropic、Gemini 三家的官方接口和它们的兼容中转（openai、anthropic、gemini、relay 四个声明式插件），加上 codex、claude-oauth、volcengine 三个代码插件。三大云上的同一批模型接不进来：

- Vertex AI 上的 Gemini：地址带项目和区域，鉴权是服务账号换来的 1 小时令牌。
- Azure OpenAI：资源自己的域名，密钥放在 `api-key` 头，模型名是部署名。
- AWS Bedrock：请求要 SigV4 签名，流式响应是二进制的 `application/vnd.amazon.eventstream`。
- OpenAI 官方的 `OpenAI-Organization`、`OpenAI-Project` 现在只能靠客户端自己带，账号上没地方填。

目标：四项都能建账号、转发、计量、分类错误，账号表单全部用 schema 表单，做法符合六条规范（抽象、复用、解耦、低嵌套、简洁、低侵入）。

## 2. 范围与不做

做：

| 项 | 内容 |
|---|---|
| OpenAI | 账号上可填组织、项目 |
| Azure OpenAI | 自定义资源地址 + API Key；v1 接口和按部署的经典接口 |
| Vertex AI | Gemini 的 generateContent / streamGenerateContent / countTokens；服务账号、API Key、authorized_user 三种鉴权 |
| Bedrock | Claude 走 InvokeModel（anthropic 平台），OpenAI 兼容接口走 `/openai/v1/*`（openai 平台）；API Key、AK/SK、临时凭证三种鉴权 |
| 核心 | 两个与厂商无关的扩展点：请求签名、响应流分帧 |

不做（第 15 节列为后续）：Vertex 上的 Claude（rawPredict）、Azure 的 Entra ID 令牌、Bedrock 的 Converse 接口、Bedrock 自定义端点（VPC 端点）、三家的模型列表拉取、Bedrock 临时凭证自动续期、Claude Code 请求体的深度清洗。

## 3. 现状（都核对过代码）

1. **声明式上游**。账号类型在清单里写 `upstream`，核心自己实现校验、构造请求、错误分类、测试、拉模型五个入口（`server/internal/plugin/declarative/declarative.go:32-37`）；清单没覆盖的入口（刷新、额度等）仍然转给插件进程。
   - 模板变量 `{api_key}`、`{base_url}`、`{model}`、`{credentials.<k>}`、`{client_header.<名>}`。`{credentials.<k>}` 可以用在请求头、路径和查询串（`sdk/upstream/template.go:38-52`），先查凭证再查设置（`sdk/upstream/build.go:64-82`）。
   - 请求头的模板里有变量取不到值，这个头就不写（`sdk/upstream/build.go:136-141`）。
   - 路由只按协议选，不能按账号设置分支（`sdk/upstream/build.go:44-53`）；基地址固定是账号的 `base_url`，不能按别的字段拼主机（`sdk/upstream/build.go:86-89`）。
   - 声明式类型一定要有 `api_key`（`sdk/pluginsdk/apikey/apikey.go:176-200`）。
   - `classify` 只能是 `openai`、`anthropic`、`gemini`（`sdk/upstream/check.go:74-76`）。
   - 清单的路由必须和类型服务的端点一一对应（`sdk/manifest/check/upstream.go:32-44`）。
2. **OpenAI 插件**。只有 `api_key`、`base_url`（`plugins/openai/manifest.json`、`forms/apikey.*`）。`openai-organization`、`openai-project` 在 openai 平台的 `passHeaders` 里，客户端带了就原样转给上游。清单版本 0.4.3，线上是 0.4.2（`plugins/released.json`）。预设的 key 校验要求 `sk-` / `sess-` 开头（`sdk/platforms/upstream/openai.json`），所以 Azure 的密钥填不进现有 openai 账号。
3. **网关热路径**。`attempt` 调插件的 `BuildUpstreamRequest`（`server/internal/gateway/dispatch.go:627`），`forwardBuilt` 应用补丁得到最终请求体（`:755`、`:812`），按层写请求头（`:824-833`），发出（`:876`）。响应只认 `text/event-stream` 和 JSON（`server/internal/gateway/forward.go:63-76`）；流式请求在分流前有一次内容类型嗅探（`dispatch.go:923`）。
4. **插件看不到整个请求体**。`BuildUpstreamRequest` 只收清单声明的 `requestFields`，插件执行路径（`Execute`）收到的也是同一个消息（`sdk/proto/sub2api/plugin/v1/platform.proto:197-200`）。所以插件算不出 SigV4 要的请求体哈希。
5. **令牌刷新（§48）**。类型声明 `refresh`，插件实现 `BuildRefreshRequest` / `ParseRefreshResponse`，核心每 5 分钟扫一遍、经账号代理发送、合并凭证补丁（`server/internal/account/refresh.go:41-56`、`:300-341`）。读不出过期时间的账号不会自动刷新（`refresh.go:194-198`、`:487`）。
6. **校验时归一化**。插件的 `ValidateCredentials` 可以返回归一化后的凭证和设置，核心存归一化结果（`server/internal/account/creds.go:363-395`）。
7. **同一产品的多种鉴权**。同插件内 `creationGroup` 相同的类型在新建账号时合成一项，用 `authMethodLabel` 切换（`web/src/views/accounts/accountTypeChoices.ts`、`AccountEditor.vue:66`、`:575-581`），schema 表单和 native 表单都适用。codex 是现成例子。
8. **错误分类**。`openai` 策略：401/403 禁用、429 按 `retry-after-ms` / `retry-after` 冷却、408 和 5xx 换号冷却 10 秒，其余（包括所有 400）原样回给客户端、不动账号（`sdk/pluginsdk/providers/openai/openai.go:18-29`、`sdk/pluginsdk/classify/classify.go:373`）。`api-key` 已经在凭证头名单里（`sdk/paramoverride/headers.go:42-48`）。
9. **旧实现**。仓库根目录的旧版有 Bedrock 和 Vertex 的实现可以对照：`backend/internal/service/bedrock_stream.go:251`（eventstream 解码）、`bedrock_request.go:167`（URL）、`:197`（请求体改写）、`:482`（beta 白名单）、`vertex_service_account.go:218`（JWT 换令牌）。旧的签名用了 aws-sdk-go-v2（`bedrock_signer.go`）。
10. **手写签名的先例**。volcengine 没引厂商 SDK，手写了 V4 签名，用官方 SDK 录下来的样例做黄金测试（`plugins/volcengine/internal/volcengine/arksign.go`）。这次照做：只用标准库，不引 aws-sdk、google SDK。

## 4. 总体方案

| 上游 | 账号类型（鉴权） | 插件 | 插件形态 | 需要核心改动 |
|---|---|---|---|---|
| OpenAI | `apikey`（原有，加组织 / 项目） | `openai` 0.4.4 | 只有清单 | 无 |
| Azure OpenAI | `azure_v1`（API Key，v1 接口） | `azure-openai` 0.1.0（新） | 只有清单 | 无 |
| Azure OpenAI | `azure_deployment`（API Key，经典接口） | 同上 | 只有清单 | 无 |
| Vertex AI | `vertex_sa`（服务账号 JSON） | `vertex` 0.1.0（新） | gRPC 进程 | 无（§48 现有能力） |
| Vertex AI | `vertex_authorized_user`（ADC 用户凭证 JSON） | 同上 | gRPC 进程 | 无 |
| Vertex AI | `vertex_apikey`（API Key） | 同上 | gRPC 进程 | 无 |
| Bedrock | `bedrock_apikey`（Bedrock API Key） | `bedrock` 0.1.0（新） | gRPC 进程 | 响应流分帧（8.2） |
| Bedrock | `bedrock_aksk`（长期 AK/SK） | 同上 | gRPC 进程 | 请求签名（8.1）+ 响应流分帧 |
| Bedrock | `bedrock_sts`（临时凭证） | 同上 | gRPC 进程 | 同上 |

三个新插件都只进市场，不进内置列表（`deploy/docker/build-go.sh:165-166` 不改；市场列表按 `plugins/*` 自动发现，`tools/sub2api-plugin/scripts/build-demo.sh:105-118`）。

客户端用别的协议访问时由核心转换（所有账号类型都接受转换，CONVERT-MATRIX §0），插件只管自己的原生协议。

## 5. OpenAI：组织与项目

只改清单和表单，不动核心。

### 5.1 账号表单

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `api_key` | API Key | secret | 凭证（加密） | 无，必填 | 原样 |
| `base_url` | Base URL | url-presets | 设置（明文） | 空 = `https://api.openai.com` | 原样 |
| `organization` | 组织 ID | text | 设置（明文） | 空 | `^[!-~]{0,128}$`，占位 `org-...` |
| `project` | 项目 ID | text | 设置（明文） | 空 | `^[!-~]{0,128}$`，占位 `proj_...` |

两个新字段放在“高级（可选）”分组（`ui:section`，折叠），帮助文案写明：填了就以账号的为准，留空则沿用客户端自己带的头。

### 5.2 清单

```jsonc
"settingsFields": ["base_url", "organization", "project"],
"upstream": {
  "preset": "openai",
  "filterFields": [],
  "credentials": { "fields": ["organization", "project"] },
  "headers": {
    "openai-organization": { "set": "{credentials.organization}" },
    "openai-project":      { "set": "{credentials.project}" }
  }
}
```

行为：引擎先带上客户端的头，再执行清单的写入（`sdk/upstream/build.go:113-133`）。账号填了值，`set` 覆盖客户端的；没填，变量取不到值，这次写入整个跳过，客户端的头保持原样。所以留空时的行为和现在逐字节相同。

写在 openai 插件的清单里，不写进 `openai` 预设：预设还被 Azure 等别的类型引用，这两个头只属于 OpenAI 官方。

WebSocket 路由（`openai.responses_ws`）用同一套共用头，握手也会带上。

### 5.3 其他

- 版本 0.4.3 → 0.4.4（0.4.3 还没发，可以并在一次发布里，实现时看 `released.json` 定）。
- 错误分类、模型、映射都不变。组织或项目填错，上游回 401，按现有规则禁用账号。
- `manifest_test.go` 里钉住的 `declaration` 常量要跟着改。

## 6. Azure OpenAI

新插件 `plugins/azure-openai`，只有清单（`runtime: "none"`），与 openai 插件互不依赖。两个账号类型，`creationGroup: "azure-openai"`。

为什么是两个类型：v1 和经典接口路径不同，而路由不能按账号设置分支（第 3 节第 1 条）。一个类型加 `api_mode` 字段做不到，除非改成代码插件。两个类型能保持只有清单。

### 6.1 账号表单

`azure_v1`（鉴权方式标签“v1 接口”，排第一，默认选中）：

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `base_url` | 资源地址 | url-presets（无预设） | 设置 | 无，必填 | `^https?://[^\s?#]+$`；占位 `https://<资源名>.openai.azure.com` |
| `api_key` | API Key | secret | 凭证（加密） | 无，必填 | 8–512 个可见字符（不要求前缀） |

`azure_deployment`（鉴权方式标签“按部署（经典）”）：上面两项，加

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `api_version` | API 版本 | text | 设置 | `2024-10-21`（schema `default`，必填） | `^\d{4}-\d{2}-\d{2}(-preview)?$` |

地址不设 `guardedSettings`：Azure 每个资源一个域名，没有“官方地址”可以钉。后果和 relay 一样：只有 own 级权限的用户建这类账号需要 `account:relay`（CONTRACTS §21.3.1 第 3 条）。这符合用户的要求“有自定义端点的，我们支持就行了”。

### 6.2 转发

共同部分：`preset: "openai"`；`baseURL: {default: null, required: true, stripSuffixes: ["/openai/v1", "/openai"]}`（用户连 `/openai/v1` 一起粘贴也行）；`auth: {type: "header", name: "api-key", value: "{api_key}"}`；`credentials.keyValidator: null`；`models: null`；`filterFields: []`；`classify: "openai"`。

| 类型 | 协议 | 上游路径 |
|---|---|---|
| `azure_v1` | `openai.chat` | `/openai/v1/chat/completions` |
| | `openai.responses` | `/openai/v1/responses` |
| | `openai.embeddings` | `/openai/v1/embeddings` |
| | `openai.responses_ws` | 不提供（`null`） |
| `azure_deployment` | `openai.chat` | `/openai/deployments/{model}/chat/completions?api-version={credentials.api_version}` |
| | `openai.embeddings` | `/openai/deployments/{model}/embeddings?api-version={credentials.api_version}` |
| | `openai.responses`、`openai.responses_ws` | 不提供（`null`） |

- 类型的 `platforms[].endpoints` 分别是 `["chat_completions","responses","embeddings"]` 和 `["chat_completions","embeddings"]`，与路由一致（否则清单检查不过）。
- 经典类型的账号遇到 `/v1/responses` 请求时，核心把它转成 chat 再发（CONVERT-MATRIX：responses → chat，good）。
- 经典路由设 `model: {required: true}`；`{model}` 是映射后的模型，按路径转义。请求体里的 `model` 保留，Azure 经典接口忽略它。
- 流式 chat 沿用预设的补丁 `stream_options.include_usage = true`。
- `services.ai.azure.com` 域名只是另一个 `base_url`，不需要特别处理。

### 6.3 令牌与错误分类

没有令牌。分类直接用 `openai` 策略，不新增 `azure` 策略：

| 上游回答 | 结果 | 依据 |
|---|---|---|
| 400 `content_filter` 及其他 400 | 原样回客户端，不换号、不动账号 | 没有规则命中 400 |
| 401、403 | 换号，禁用账号 | `Credentials` 规则 |
| 429 | 换号，冷却到 `retry-after-ms` / `retry-after` 指的时间，没有就 60 秒 | `RateLimit` + `RetryAfter` |
| 404（部署不存在） | 原样回客户端 | 与 OpenAI 的 `model_not_found` 一致 |
| 408、5xx | 换号，冷却 10 秒 | `ServerErrors` |

### 6.4 模型与映射

上游的“模型”是部署名。清单不预填 `defaultModels`（部署名是用户自己起的）；用账号的模型列表和模型映射把客户端模型名对到部署名（CONTRACTS §18）。测试默认模型 `gpt-4o-mini`，没有同名部署时在测试对话框里指定。

## 7. Vertex AI（Gemini）

新插件 `plugins/vertex`，gRPC 进程，平台 `gemini`，端点 `generate_content`、`stream_generate_content`、`count_tokens`。三个账号类型，`creationGroup: "vertex"`，`upstreamFields: []`（主机由区域决定，用户不能自己填地址），需要 `accounts.credentials` 授权（代码类型要读凭证）。

三个类型都由插件代码实现，共用一个 URL 构造函数。API Key 类型不做成声明式：基地址不能按区域拼主机，路由也不能按“有没有填项目”分支（第 3 节第 1 条）；拆成声明加代码两处反而重复。

### 7.1 账号表单

`vertex_sa`（服务账号，排第一）：

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `service_account_json` | 服务账号密钥 | secret + textarea（多行、等宽） | 凭证（加密） | 无，必填 | 插件解析：`type` 为 `service_account`，`client_email`、`private_key` 齐全且私钥能解析，`token_uri` 必须是 `https://oauth2.googleapis.com/token` |
| `project_id` | 项目 ID | text | 设置 | 空 = 取 JSON 里的 `project_id`（保存时由插件填进设置） | `^[a-z][a-z0-9-]{4,28}[a-z0-9]$` |
| `location` | 区域 | select（可搜索，可输入） | 设置 | `global` | `^[a-z0-9-]{2,32}$` |
| `access_token`、`expires_at`、`token_for` | — | hidden | 凭证（加密） | 由核心刷新写入 | 用户不可见 |

`vertex_authorized_user`（用户凭证）：同上，把第一项换成 `authorized_user_json`（校验：`type` 为 `authorized_user`，`client_id`、`client_secret`、`refresh_token` 齐全）；`project_id` 必填（这种 JSON 里的 `quota_project_id` 只作为默认值）。

`vertex_apikey`（API Key）：

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `api_key` | API Key | secret | 凭证（加密） | 无，必填 | 8–512 个可见字符 |
| `project_id` | 项目 ID（可选） | text | 设置 | 空 | 同上，允许空 |
| `location` | 区域（可选） | select | 设置 | 空 | 同上，允许空 |

`project_id` 和 `location` 都填了才走带项目的完整地址，否则走 express 地址。只填一个时保存报错，指出要么都填要么都不填。

隐藏字段的用处：编辑账号时表单把整个凭证对象带回来（`AccountEditor.vue:471`），敏感字段回传掩码、核心还原（`creds.go:118`），所以改别的字段不会把已有令牌冲掉。它们必须写进 schema（`additionalProperties: false`），用 `ui:widget: "hidden"` 不显示（`web/packages/ui/src/schema/SchemaObject.vue:42-45`）。

### 7.2 转发

主机：`location` 为 `global` 时 `aiplatform.googleapis.com`，否则 `{location}-aiplatform.googleapis.com`。

| 情况 | URL |
|---|---|
| 服务账号、用户凭证、填了项目和区域的 API Key | `https://{主机}/v1/projects/{project_id}/locations/{location}/publishers/google/models/{model}:{动作}` |
| 没填项目和区域的 API Key（express） | `https://aiplatform.googleapis.com/v1/publishers/google/models/{model}:{动作}` |

动作按 `meta.protocol`：`gemini.generate` → `generateContent`，`gemini.stream_generate` → `streamGenerateContent?alt=sse`，`gemini.count_tokens` → `countTokens`。模型取 `meta.model`，去掉 `models/` 前缀，按路径转义。

请求头：`content-type: application/json`；服务账号和用户凭证加 `authorization: Bearer {access_token}`，API Key 加 `x-goog-api-key`。请求体与 Gemini Developer API 相同，不打补丁；用量规则沿用 gemini 平台的（`usageMetadata.*`）。

账号还没有令牌（刚建，等第一次刷新）时，`BuildUpstreamRequest` 返回 `FAILED_PRECONDITION`，网关换下一个账号，不惩罚这个账号（`dispatch.go:634-636`）。

测试请求：`generateContent` + `gemini.GenerateContentPingBody(1)`，默认模型 `gemini-2.5-flash`。模型列表：返回 `UNIMPLEMENTED`，靠清单的 `defaultModels`。

### 7.3 令牌

两个带令牌的类型声明 `"refresh": {}`（默认字段 `expires_at`，提前 1800 秒）。令牌有效 1 小时，所以大约每半小时换一次，失败后每 5 分钟重试，有 6 次机会。

**请求**（`BuildRefreshRequest`，纯计算）：

- 服务账号：用私钥签 RS256 JWT（`iss` = `client_email`，`scope` = `https://www.googleapis.com/auth/cloud-platform`，`aud` = `https://oauth2.googleapis.com/token`，`exp` = `iat` + 3600，头里带 `kid`），`POST https://oauth2.googleapis.com/token`，表单体 `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=…`，`Content-Type: application/x-www-form-urlencoded`。只用 `crypto/rsa`、`crypto/x509`。
- 用户凭证：同一地址，`grant_type=refresh_token&client_id=…&client_secret=…&refresh_token=…`。

**回答**（`ParseRefreshResponse`）：200 → 补丁 `{access_token, expires_at: 现在 + expires_in, token_for: 指纹}`；400 `invalid_grant`、`invalid_client`、401、403 → `AUTH_REJECTED`（密钥被删、被停用、刷新令牌作废）；其他 → `TRANSIENT`。

`token_uri` 钉死为 Google 官方地址：这个请求由核心替插件发，不能让粘贴进来的 JSON 指到别处。

**第一枚令牌**。codex、claude-oauth 的第一枚令牌是授权流程里插件自己换来的；Vertex 用 schema 表单，没有授权流程。做法：

1. `ValidateCredentials` 算出密钥指纹（`client_email` + `private_key_id` 的 SHA-256；用户凭证是 `client_id` + `refresh_token`）。提交的凭证里没有令牌，或者 `token_for` 和指纹不同（换了密钥），就在归一化结果里去掉旧令牌并写 `expires_at: 1`。
2. `expires_at: 1` 是一个读得出来的、已经过期的时间，下一轮扫描（最多 5 分钟）就会刷新。不依赖任何核心改动。
3. 等不及可以在账号详情点“立即刷新令牌”（`POST /accounts/:id/refresh-credentials`，现有功能）。
4. 核心 0.1.N 顺带加一个小改进（任务包 C2，Vertex 不依赖它）：保存账号后，类型声明了 `refresh` 就异步调一次 `refreshAccount(id, false)`。非强制调用只刷新已到期的，所以对 codex、claude-oauth 没有影响，Vertex 账号则保存后几秒内就有令牌。

没有采用的做法：插件在校验时自己联网换令牌（要给插件 `net` 权限，而且走不了账号代理）。

### 7.4 错误分类

在 `gemini.Policy` 前面加一条规则（`Policy.With`），其余沿用：

| 上游回答 | 服务账号 / 用户凭证 | API Key |
|---|---|---|
| 401 UNAUTHENTICATED | 换号，冷却 60 秒，不禁用（令牌问题，等下次刷新；密钥真失效时刷新状态会显示 `auth_rejected`） | 换号，禁用 |
| 403 PERMISSION_DENIED | 换号，禁用 | 同左 |
| 400 INVALID_ARGUMENT | 原样回客户端 | 同左（key 无效的 400 按现有规则禁用） |
| 404 | 原样回客户端 | 同左 |
| 429 RESOURCE_EXHAUSTED | 换号，按 `retry-after` / `RetryInfo` 冷却 | 同左 |
| 500、503 | 换号，不动账号 | 同左 |

### 7.5 模型与映射

`defaultModels` 预填 Vertex 上的 Gemini 模型（与 gemini 插件的列表一致，实现时按 Vertex 文档核对哪些有 `global` 区域）。不预填映射：Vertex 的 Gemini 模型名与 Developer API 相同。

## 8. 核心扩展点

两处都是给 `BuildUpstreamRequestResponse` 加可选字段，插件不填时行为完全不变。它们不带任何厂商判断：核心只认“方案名”和“格式名”，按名字查表。

```protobuf
// common.proto
message RequestSigning {
  string scheme = 1;             // "aws-sigv4"
  string region = 2;
  string service = 3;
  string access_key_id = 4;
  string secret_access_key = 5;
  string session_token = 6;      // 可选
}

// platform.proto, BuildUpstreamRequestResponse
RequestSigning signing = 6;      // 请核心在发送前对最终请求签名
string response_stream = 7;      // 响应流的分帧格式，如 "aws-eventstream"
```

清单不加字段。要不要签名、响应是什么分帧都取决于这一次请求（鉴权方式、是不是流式），放在构造结果里比放在清单里准确。

### 8.1 请求签名

**为什么在核心**：签名要对最终请求体取哈希，而插件拿不到请求体（第 3 节第 4 条）；请求体在插件返回之后还会被补丁和强制流式改写。

**库**：新包 `sdk/reqsign`，只用标准库。

```go
type Directive struct{ Scheme, Region, Service, AccessKeyID, SecretAccessKey, SessionToken string }
func Sign(req *http.Request, payload []byte, d Directive, now time.Time) error   // 未知方案返回错误
```

内部是一张“方案名 → 签名函数”的表，现在只有 `aws-sigv4`。放在 SDK 而不是核心内部，是因为插件的测试请求也要用它（见下）。

**挂点**：`forwardBuilt` 里所有请求头写完之后、发送之前，即 `dispatch.go:833` 之后加一处调用（实现放在新文件 `gateway/upstream_ext.go`，`dispatch.go` 只多两行）。这时请求体、URL、所有请求头层都已定，重定向本来就不跟随。插件执行路径也经过 `forwardBuilt`，自动覆盖。

**SigV4 的细节**（都要有测试钉住）：

- 签名的头：`host`、`x-amz-date`，有的话加 `content-type`、`x-amz-security-token`。别的头不签，所以透传、参数覆盖加的头不影响签名。
- 规范路径：对 `req.URL.EscapedPath()` 的每一段再转义一次（S3 以外的服务都是双重转义）。模型 ID 里的 `:` 在线上是 `%3A`，在规范路径里是 `%253A`。
- 主机取 `req.Host`，为空取 `req.URL.Host`。
- 写入 `X-Amz-Date`、`Authorization`，有临时凭证时写 `X-Amz-Security-Token`；先删掉已有的 `Authorization`。
- 签名失败或方案名不认识：这次尝试按插件不可用换号，绝不发出未签名的请求。
- 签名指令带着密钥，不得进日志。

**黄金样例**（照 volcengine 的做法，两个来源）：

1. AWS 公布的 SigV4 测试套件样例。
2. 用旧版已经依赖的 aws-sdk-go-v2 签名器，对 Bedrock 形状的请求（模型 ID 带 `:`、ARN 带 `/`、带临时凭证）固定时间和密钥录下 `Authorization`，存进 `sdk/reqsign/testdata`。新代码不依赖 SDK，只比对录下的结果。

**测试请求**：账号测试的请求体是插件自己给的（`server/internal/account/testreq.go:211-245` 原样发送），插件在 `BuildTestRequest` 里直接调 `reqsign.Sign` 即可，账号测试的代码不用改。

**声明式扩展模式**：`declarative.merge`（`declarative.go:95-116`）要把这两个新字段从插件的修改结果里带过去，否则以后用 extend 的类型会丢掉它们。

volcengine 的素材库签名（插件自己发请求、算法细节不同）以后可以作为第二个方案并进这张表，这次不动。

### 8.2 响应流分帧

**库**：新包 `sdk/awseventstream`，只用标准库（`hash/crc32`、`encoding/binary`）：帧解码器，外加给测试和模拟上游用的编码器。逻辑从 `backend/internal/service/bedrock_stream.go:230-398` 移植，两处校验和都验。

**适配器**：一个 `io.Reader`，读 eventstream 帧，吐 SSE 文本：

| 帧 | 输出 |
|---|---|
| `:message-type` = `event`，负载 `{"bytes":"<base64>"}` | 解出 base64 得到模型原生事件 JSON，输出 `event: <JSON 的 type>\ndata: <JSON>\n\n`（没有 `type` 就只有 `data:` 行） |
| `:message-type` = `exception` 或 `error` | 输出 `event: error\ndata: {"type":"error","error":{"type":"<异常类型>","message":"<消息>"}}\n\n`，然后结束 |
| 其他事件、没有 `bytes` 的负载 | 跳过 |
| 帧损坏（长度不合法、校验和不对、超过 16 MiB） | 读取返回错误，按上游中断处理 |

**挂点**：`dispatch.go:923` 的嗅探之前。插件的构造结果里 `response_stream` 非空、上游状态是 2xx、并且响应的 `Content-Type` 就是该格式的媒体类型时，把 `resp.Body` 换成适配器，把 `Content-Type` 改成 `text/event-stream`。此后的一切（SSE 转发、用量规则、协议转换、首 token 计时、空闲超时、断开计费）都不用改，看到的就是普通 SSE。

格式名查不到：按插件不可用换号。内容类型对不上（上游回了 JSON 错误或者普通 SSE）：不处理，照原样走。

错误事件的形状选的是用量规则已经认识的那种（事件名 `error`、`error.message`，`server/internal/usagerules/usagerules.go:284-293`），所以流中途的异常会被记成上游流错误并交给故障策略。

### 8.3 兼容

- 旧插件不填新字段，不受影响；旧核心遇到新字段会直接忽略，结果是不签名就发出去、被 AWS 回 403、账号被禁用。所以 bedrock 插件的 `hostCompat` 必须写 `>=0.1.N`，装不到旧核心上。
- 不加能力 ID、不改 Host API 版本：两个字段是构造结果的可选部分，`hostCompat` 足够表达依赖。
- 网关二进制有变更，按核心发布流程走。

## 9. AWS Bedrock

新插件 `plugins/bedrock`，gRPC 进程。三个账号类型，`creationGroup: "bedrock"`，`upstreamFields: []`，需要 `accounts.credentials` 授权，`hostCompat: ">=0.1.N <0.2.0"`。

平台：

| 平台 | 端点 | 上游 |
|---|---|---|
| `anthropic` | `messages`（`passHeaders: ["anthropic-beta"]`，`requestFields: ["model"]`） | InvokeModel / InvokeModelWithResponseStream |
| `openai` | `chat_completions`、`responses` | `/openai/v1/chat/completions`、`/openai/v1/responses` |

`count_tokens` 不声明，由核心本地估算（CONVERT-MATRIX §0 第 6 条）。

### 9.1 账号表单

所有类型共有：

| 字段 | 标题 | 控件 | 存储 | 默认 | 校验 |
|---|---|---|---|---|---|
| `region` | 区域 | select（可搜索，可输入） | 设置 | `us-east-1` | `^[a-z]{2}(-[a-z]+)+-\d$`（它会拼进主机名，必须严格） |
| `inference_profile_prefix` | 跨区域推理前缀 | select：不加 / us / eu / apac / jp / au / global | 设置 | 不加 | 枚举 |

各类型自己的：

| 类型（标签） | 字段 | 控件 | 存储 | 默认 / 校验 |
|---|---|---|---|---|
| `bedrock_apikey`（API Key） | `api_key` | secret | 凭证 | 必填，8–2048 个可见字符；帮助写明短期 Key 最长 12 小时且绑定区域 |
| `bedrock_aksk`（Access Key） | `access_key_id` | text | 凭证 | 必填，`^[A-Z0-9]{16,128}$` |
| | `secret_access_key` | secret | 凭证 | 必填 |
| `bedrock_sts`（临时凭证） | `access_key_id`、`secret_access_key` | 同上 | 凭证 | 同上 |
| | `session_token` | secret + textarea | 凭证 | 必填 |
| | `session_expires_at` | date-time | 设置 | 可选，只作提示，不参与任何逻辑；帮助写明到期后要手动换凭证 |

后两个类型在代码里是同一条路径，区别只是有没有 `session_token`。分成两个类型是为了让表单把“会过期、不会自动续”说清楚。

### 9.2 转发

主机固定 `https://bedrock-runtime.{region}.amazonaws.com`。

**anthropic 平台**（`anthropic.messages`）：

- URL：`/model/{模型}/invoke`，`meta.stream` 为真时 `/model/{模型}/invoke-with-response-stream`。
- 模型：取 `meta.model`（已经过映射和账号参数覆盖）。设置了前缀、模型不是 ARN、也没有已知前缀（`us.` `eu.` `apac.` `jp.` `au.` `us-gov.` `global.`）时，在前面加 `<前缀>.`。整个 ID 作为一个路径段转义，`:` 再换成 `%3A`（同 `bedrock_request.go:167-180`）。
- 请求体补丁：删 `model`、删 `stream`、设 `anthropic_version` 为 `"bedrock-2023-05-31"`；客户端带了 `anthropic-beta` 头时，把其中 Bedrock 支持的令牌写进请求体的 `anthropic_beta` 数组（白名单从 `bedrock_request.go:482-492` 移植），头本身不转发。
- 请求头：`content-type: application/json`、`accept: application/json`。
- 流式时构造结果带 `response_stream: "aws-eventstream"`。
- 删掉 `model` 不触发补丁守卫：守卫只比对声明的附加模型引用（`fallbacks`、advisor），不含主模型（`server/internal/gateway/model_references.go:326-342`）。

**openai 平台**（`openai.chat`、`openai.responses`）：URL `/openai/v1/chat/completions`、`/openai/v1/responses`；请求体不动（模型在请求体里），只在流式 chat 时加 `stream_options.include_usage = true`；响应本来就是 SSE，不需要分帧。

**鉴权**：

| 类型 | 做法 |
|---|---|
| `bedrock_apikey` | 请求头 `authorization: Bearer {api_key}` |
| `bedrock_aksk`、`bedrock_sts` | 构造结果带 `signing {scheme: "aws-sigv4", region, service: "bedrock", access_key_id, secret_access_key, session_token}`，核心签名 |

测试请求：非流式 InvokeModel，请求体是去掉 `model` 的 Anthropic ping 加 `anthropic_version`；AK/SK 类型由插件调 `reqsign.Sign` 签好。默认测试模型在清单里定。模型列表：`UNIMPLEMENTED`（列表在控制面 `bedrock.{region}`，这次不做）。

### 9.3 令牌

没有自动刷新。短期 API Key 和临时凭证到期后上游回 403，按 9.4 处理，管理员手动换。

### 9.4 错误分类

插件自带策略。错误类型取响应头 `x-amzn-errortype`（冒号前的部分），没有就取响应体的 `__type`；消息取 `message`。`/openai/v1/*` 的回答如果是 OpenAI 形状（`error` 对象）按 OpenAI 形状解析，状态码规则相同。客户端错误类型按 `meta.protocol` 选 Anthropic 或 OpenAI 的词表。

| 上游回答 | 结果 |
|---|---|
| 429 ThrottlingException、ModelNotReadyException | 换号，冷却（`retry-after` 没有就 60 秒） |
| 503 ServiceUnavailableException、529、500 | 换号，不动账号 |
| 408 ModelTimeoutException | 换号，不动账号 |
| 424 ModelErrorException | 原样回客户端 |
| 400 ValidationException | 原样回客户端 |
| 404 ResourceNotFoundException | 原样回客户端 |
| 403 UnrecognizedClientException、InvalidClientTokenId、签名不匹配的 InvalidSignatureException | 换号，禁用（密钥不对） |
| 403 ExpiredTokenException | 换号，禁用，原因写“临时凭证已过期” |
| 403 InvalidSignatureException 且消息含 `Signature expired` / `not yet current`；400 RequestExpired | 换号，不动账号，原因写“节点时钟偏差” |
| 403 AccessDeniedException | 换号，冷却 60 秒，不禁用（见第 10 节偏差 6） |
| 400 IncompleteSignatureException | 换号，不动账号（是核心签名的问题，不是账号的） |
| 流中途的异常帧 | 响应已经开始，客户端收到 `error` 事件；记为上游流错误，交给故障策略 |

### 9.5 模型与映射

客户端用的是 Anthropic 的模型名，Bedrock 要的是 `anthropic.claude-…-v1:0` 这样的 ID、带区域前缀的推理配置 ID 或 ARN。做法：清单 `defaultModels` 列 Anthropic 模型名，`defaultModelMapping` 预填“Anthropic 名 → Bedrock ID”（不带区域前缀），新建账号时带进表单，管理员可以改（CONTRACTS §41）。前缀由 `inference_profile_prefix` 统一加。映射表的具体 ID 实现时从 AWS 文档抄，逐个核对，本文不列。

插件不内置别名表：映射只有账号上那一份，看得见、改得了。

### 9.6 没有采用：`/anthropic/v1/messages`

bedrock-runtime 上还有一个 Anthropic 原生接口 `POST /anthropic/v1/messages`：请求体带 `model`、响应是 SSE、不需要分帧，用 Bedrock Key 时只要清单就能接。没有选它作主路径：文档只写了 `x-api-key` 加 Bedrock Key 的用法，SigV4 能不能用没有确认，AK/SK 和临时凭证两种账号就没着落；InvokeModel 是三种鉴权都确定可用的路径。等有真实账号验证后，可以给 `bedrock_apikey` 加这条路（第 15 节）。

同样没有采用：SigV4 的 `UNSIGNED-PAYLOAD`（只有 S3 有文档支持）；在插件里引 aws-sdk（违背第 3 节第 10 条的先例）。

## 10. 与初始决定的偏差

| # | 初始决定 | 改成 | 原因 |
|---|---|---|---|
| 1 | Azure 用一个 `api_mode` 选项，或两个类型 | 确定两个类型 | 路由不能按设置分支，一个类型就不是只有清单了 |
| 2 | 考虑新增 `azure` 错误策略 | 不新增，用 `openai` | 400 `content_filter` 现在就是原样回客户端、不动账号；`retry-after-ms` 现在就读。新增策略还得改 SDK 的校验 |
| 3 | Azure 经典接口也提供 responses | 经典类型不提供 | 经典路径下 responses 只有预览版本；这类账号的 responses 请求由核心转成 chat |
| 4 | Vertex API Key 类型可能纯声明式 | 三个类型都用代码 | 主机随区域变、“填没填项目”要分支，声明式都表达不了 |
| 5 | Vertex 首枚令牌“设计一下” | `expires_at: 1` + 现有扫描，另加一个可选的核心小改进 | 现有扫描跳过读不出过期时间的账号，所以要写一个读得出的过期值；这样 Vertex 不依赖核心发布 |
| 6 | Bedrock 403 AccessDenied 算账号问题（禁用） | 冷却 60 秒，不禁用 | Bedrock 的模型访问是按模型开通的，一个模型没开通不该停掉整个账号。**这条需要用户定** |
| 7 | 400 IncompleteSignature / RequestExpired 算账号配置问题 | 换号，不动账号 | 签名和时间戳是核心节点生成的，出错是节点时钟或核心的问题，禁用账号会把所有 Bedrock 账号一起停掉 |
| 8 | 响应流格式可以写在平台端点声明里 | 只放在构造结果里 | 同一个类型同一个协议，只有流式请求才是 eventstream |
| 9 | 账号测试也由核心签名 | 插件在 `BuildTestRequest` 里自己签 | 测试请求体是插件给的，插件算得出哈希；签名库放在 SDK，账号测试的代码不用动 |
| 10 | Bedrock 临时凭证的 `expires_at` | 改名 `session_expires_at` | 避免和 §48 的默认字段名 `expires_at` 混淆 |
| 11 | 插件处理 Claude Code 的各种不兼容字段 | 首版不做深度清洗 | 插件看不到请求体，补丁只能按固定路径改，做不了“每个工具里删某字段”。见第 12 节第 7 条 |

另有一条不算偏差的发现：Azure 的 v1 接口文档示例用的是 OpenAI SDK，即 `Authorization: Bearer <key>`，按理现有 openai 账号把 Base URL 填成 `https://<资源>.openai.azure.com/openai` 就能用，但被 `sk-` 前缀校验挡住了。这次仍然做独立插件（`api-key` 头两种接口都有文档），不去放宽 openai 插件的校验。

## 11. 任务拆分

每个任务包只动自己列出的文件，可以分给不同的代理同时做。

| 包 | 内容 | 文件 | 验收 | 依赖 |
|---|---|---|---|---|
| **W0 脚手架** | 三个新插件目录的 `go.mod`（两条 `replace`）、`go.work` 三行 | `go.work`、`plugins/{azure-openai,vertex,bedrock}/go.mod` | `go work sync` 通过 | 无 |
| **W1 OpenAI** | 5 节 | `plugins/openai/manifest.json`、`forms/apikey.*`、`manifest_test.go` | 清单检查过；填了组织时覆盖客户端头、留空时与现有黄金样例输出相同（两个用例都要有） | 无 |
| **W2 Azure** | 6 节 | `plugins/azure-openai/**`（清单、两套表单、图标、`manifest_test.go`） | 清单检查过；两个类型每条路由的 URL、`api-key` 头、缺 `base_url` 报错、`/openai/v1` 后缀被去掉；`e2e/ac29_azure_test.go` 对模拟上游跑通 chat 流式与非流式、用量入账 | W0；e2e 依赖 W8 |
| **W3 Vertex** | 7 节 | `plugins/vertex/**` | 校验（三种 JSON 的好坏样例、指纹变了重置令牌、项目区域成对）；URL 表全覆盖；JWT 用测试密钥验签、声明字段正确；刷新回答的三类结果；分类表逐行；对假上游的往返测试 | W0 |
| **S1 签名库** | 8.1 的库 | `sdk/reqsign/**` | 两个来源的黄金样例逐字节相同；改一个字节的请求体签名就变；未知方案报错 | 无 |
| **S2 分帧库** | 8.2 的库 | `sdk/awseventstream/**` | 编码再解码还原；两处校验和错、截断帧、超长帧都报错；异常帧转成 `error` 事件；旧版测试里的帧样例移植过来 | 无 |
| **C1 核心接入** | proto 两个字段与生成代码；`gateway/upstream_ext.go` 与 `dispatch.go` 两处调用；`declarative.merge` 带上新字段 | `sdk/proto/**`、`sdk/gen/**`、`server/internal/gateway/upstream_ext.go`、`dispatch.go`、`server/internal/plugin/declarative/declarative.go`、`gateway/upstream_ext_test.go` | 假插件返回签名指令，假上游独立重算签名通过（含参数覆盖改了请求体、强制流式的情况）；假上游回 eventstream，客户端收到 SSE、用量入账、转换路由可用；未知方案和未知格式都换号且没有请求发出；不带新字段的全部现有测试不变；日志里没有密钥 | S1、S2 |
| **C2 保存后刷新** | 7.3 第 4 点 | `server/internal/account/handlers.go`、`refresh.go`、对应测试 | 保存到期的账号后触发一次刷新；未到期的不触发 | 无 |
| **W4 Bedrock** | 9 节 | `plugins/bedrock/**` | 校验；URL 与模型编码表（含前缀、ARN）；补丁与 beta 白名单；三种鉴权的构造结果；测试请求的签名能被独立重算；分类表逐行；对假上游的往返测试（测试里用 S1、S2 充当核心） | W0、S1、S2、C1（proto 字段） |
| **W8 模拟上游** | Azure 两套路径；供本地联调的 Bedrock invoke（eventstream）与 Vertex 路径 | `deploy/mock-upstream/**` | 各路径的单测 | S2 |
| **D1 文档** | CONTRACTS 新增一节（两个扩展点、三个插件、OpenAI 字段），§48 补“保存后刷新”；PLUGIN-DEVELOPMENT §3 表格加三行；UPSTREAM-DECLARATIVE 记 extend 带新字段 | `docs/CONTRACTS.md`、`docs/PLUGIN-DEVELOPMENT.md`、`docs/UPSTREAM-DECLARATIVE.md` | 与实现一致 | 全部完成后 |

依赖图：`W0 → W2、W3、W4`；`S1、S2 → C1 → W4`；`S2 → W8 → W2 的 e2e`；W1、C2、S1、S2 可以立刻开始。发布后改 `plugins/released.json`（集成时一次做完，不属于任何一个包）。

## 12. 测试方案

三层，都不需要真实云账号：

1. **单元与黄金样例**。签名、分帧、JWT、URL 与模型编码、补丁、错误分类表、清单检查。黄金样例的来源见 8.1。
2. **网关层**（`server/internal/gateway/*_test.go` 现有的环境：假插件 + `httptest` 假上游）。验证两个扩展点在真实的请求体改写、请求头分层、SSE 转发、用量、协议转换之下工作。假上游自己重算签名，不复用被测代码。
3. **插件对假上游**。Vertex、Bedrock 的主机是写死的，测试里用可替换的端点（codex 的 `NewWithEndpoints` 先例，`plugins/codex/internal/codex/plugin.go:76-126`）指向 `httptest`。Azure 的地址本来就是账号填的，走 docker e2e（`next/e2e`，同 `ac20_openai_gemini_test.go`）。

Vertex 和 Bedrock 首版没有 docker e2e：那需要一个在生产包里也生效的端点覆盖开关，为了测试开这个口子不值得。它们的全链路由第 2、3 层合起来覆盖。

### 没有验证过的事项

下面这些只有文档依据，或者文档也没说清，要等有真实凭证时逐条验证。验证之前不要对外说“已支持”。

1. Bedrock：ARN 作模型 ID 时的路径编码（`/` → `%2F`、`:` → `%3A`）以及它的 SigV4 规范路径。
2. Bedrock：`/openai/v1/*` 用 SigV4 签名、流式时是否接受 `stream_options`、支持哪些模型。
3. Bedrock：beta 白名单是否还是 2026-11 的现状；流的最后一个事件里的 `amazon-bedrock-invocationMetrics` 字段原样透给客户端是否有客户端在意（首版不删）。
4. Vertex：绑定服务账号的 API Key 能不能用在带项目和区域的完整地址上；express 地址支持哪些模型。
5. Vertex：请求体与 Developer API 的细小差别（已知 Vertex 对 `contents[].role` 更严格）。首版不改请求体，客户端不带 `role` 时可能被上游拒绝。
6. Azure：`services.ai.azure.com` 域名下 v1 路径是否完全一致；经典接口 `2024-10-21` 是否接受 `stream_options`；v1 的 `/embeddings`。
7. Bedrock + Claude Code：Claude Code 会在工具定义里带 `custom`、在 `cache_control` 里带 `scope` / `ttl`，旧版为此做了深度清洗（`bedrock_request.go:197-265`）。首版插件做不了。可以在账号的参数覆盖（§84，带通配路径的删除和 `prune_objects`）里手工配，但这套配置没有实测过。**如果 Bedrock 账号主要给 Claude Code 用，这一条要先解决再上线**。
8. 所有错误分类表都来自文档里的错误码，没有用真实回答核对过。

## 13. 发布与回滚

顺序：

1. openai 0.4.4、azure-openai 0.1.0、vertex 0.1.0：不依赖核心改动，可以先发。只进市场，管理员自己安装。
2. 核心 0.1.N（S1、S2、C1、C2）：网关二进制有变更，按核心发布流程。没有迁移。
3. bedrock 0.1.0：核心 0.1.N 上线之后发，`hostCompat` 挡住旧核心。

回滚：

- 插件：卸载或回到上一版本。openai 回到 0.4.3 后，账号设置里多出的 `organization`、`project` 两个键没人读，不影响请求；重新升级后继续生效。
- 核心：两个扩展点只在插件填了新字段时才执行，回滚核心前先停用 bedrock 插件即可。C2 回滚后 Vertex 退回“最多等 5 分钟”。
- 没有数据迁移，没有要清理的数据。

## 14. 需要用户决定的事

1. Bedrock 的 403 AccessDenied：按本文冷却不禁用，还是按初始决定禁用（第 10 节偏差 6）。
2. Bedrock 给 Claude Code 用的优先级：高的话，深度清洗要在首版解决（第 12 节第 7 条，候选做法见第 15 节）。
3. Azure 经典类型要不要：用户的原话是“有自定义端点就行”，只留 `azure_v1` 也满足；经典类型只是多一份清单，本文保留了。
4. Bedrock 的 openai 平台要不要进首版：它依赖的行为没验证的最多（第 12 节第 2 条），去掉后插件只剩 anthropic 一条路，更小。

## 15. 后续

- Vertex 上的 Claude（rawPredict / streamRawPredict），放进 vertex 插件，复用同一套令牌。
- Azure 的 Entra ID 令牌：可以照 Vertex 的做法用 §48。
- Bedrock：Converse 接口；`bedrock_apikey` 走 `/anthropic/v1/messages`；自定义端点；临时凭证用 AssumeRole 自动续期（刷新请求本身要签名，核心的刷新发送处要接上 8.1）。
- 让代码类型也能在清单里声明固定的参数覆盖（现在只有声明式类型可以），Bedrock 的深度清洗就能写成清单的一部分。
- 三家的模型列表拉取。
- volcengine 的素材库签名并入 `sdk/reqsign`。
