# codex 插件：ChatGPT 账号（Codex 设备码 / Sign in with Codex）

日期：2026-11-08（额度窗口 2026-11-09，官方用量重置次数 2026-11-10）。状态：已实现（插件 0.1.1 已上线；0.1.2 增加 `codex_device` 的 5h / 周额度窗口，见第 9 节，需核心 ≥0.1.257；0.1.3 接入官方用量重置次数，见第 10 节，需核心 ≥0.1.269；0.1.4 随权限加固发布，只改授权相关行为，见第 11 节，需核心 ≥0.1.286；2026-11-17 新增 Sign in with Codex（`codex_oauth`）并删除 Sign in with ChatGPT（`chatgpt_siwc`），见第 13 节）。核心契约见 CONTRACTS §83（WebSocket 每轮 patch 见 §83.6），额度见 §44、§69。

## 1. 目标与边界

- 新增内置并默认启用的插件 `codex`，让运营者用**自己的** ChatGPT 订阅为**自己的**应用提供 OpenAI 兼容接口：`/v1/chat/completions`、`/v1/responses`、`/v1/responses`（WebSocket 模式）。
- 两种建号方式（同一 `creationGroup: "codex"` 下的两个账号类型）：
  1. `codex_device`：Codex 设备码登录（与 Codex CLI `--device-auth`、OpenCode 等第三方工具相同的公开流程）。
  2. `codex_oauth`：Sign in with Codex，Codex CLI 自己的浏览器登录（授权码 + PKCE，2026-11-17 新增，见第 13 节）。
  - ~~`chatgpt_siwc`：OpenAI 官方第三方入口 Sign in with ChatGPT · ChatGPT plan usage~~（2026-11-17 删除，见第 13 节；下文涉及 `chatgpt_siwc` 的内容只作历史记录）。
- 守线（写进插件说明，不做技术强制）：
  - 请求如实标明来源：`originator: sup2api`、`User-Agent: sup2api-codex/<版本>`；**不**伪装 Codex CLI 的 UA、TLS 指纹、`x-codex-*` 头，**不**注入 Codex 官方系统提示词。
  - 仅供账号所有者自用；把多人 ChatGPT 账号做成号池对外出售违反 OpenAI 条款（付费/托管应用需先通过 OpenAI 的 Sign in with ChatGPT 候补审批）。
- 不做：连接池/预热、`previous_response_id` 跨连接恢复、`chatgpt_siwc` 的额度窗口（见第 9 节）。

## 2. 官方协议事实

### 2.1 Codex 设备码（`codex_device`）

来源：openai/codex `codex-rs/login/src/device_code_auth.rs`。issuer = `https://auth.openai.com`，client_id = `app_EMoamEEZ73f0CkXaXp7hrann`（Codex 公开客户端）。

1. `POST https://auth.openai.com/api/accounts/deviceauth/usercode`，JSON `{"client_id": "..."}` → `{"device_auth_id", "user_code"（或 "usercode"）, "interval"（字符串秒数）}`；404 = 该账号/工作区未开启设备码登录（ChatGPT 设置 → 安全 → Device code authorization；企业工作区需管理员开启）。
2. 展示验证地址 `https://auth.openai.com/codex/device` 和 `user_code`，15 分钟有效。
3. 轮询 `POST https://auth.openai.com/api/accounts/deviceauth/token`，JSON `{"device_auth_id", "user_code"}`：403/404 = 尚未授权，按 interval 继续；2xx → `{"authorization_code", "code_challenge", "code_verifier"}`；其他状态 = 失败。
4. 换令牌：`POST https://auth.openai.com/oauth/token`，form：`grant_type=authorization_code`、`code`、`redirect_uri=https://auth.openai.com/deviceauth/callback`、`client_id`、`code_verifier` → `{id_token, access_token, refresh_token, expires_in}`。
5. 刷新：`POST https://auth.openai.com/oauth/token`，JSON `{"client_id", "grant_type":"refresh_token", "refresh_token", "scope":"openid profile email"}`（与旧版 `backend/internal/pkg/openai/oauth.go` 一致）；refresh token 会轮换。
6. 身份：id_token 的 `https://api.openai.com/auth` 声明里有 `chatgpt_account_id`、`chatgpt_plan_type`；`email` 在顶层。
7. 上游：`POST https://chatgpt.com/backend-api/codex/responses`，头 `authorization: Bearer <access_token>`、`chatgpt-account-id`、`originator: sup2api`、`user-agent`、`accept: text/event-stream`；请求体须 `store:false`、`stream:true`、带 `instructions` 字段（缺省时补空串）。WebSocket：`wss://chatgpt.com/backend-api/codex/responses` + `openai-beta: responses_websockets=2026-02-06`。

### 2.2 Sign in with ChatGPT（`chatgpt_siwc`，已删除，见第 13 节）

来源：developers.openai.com/siwc/token-sharing-open-source/*。

1. 宿主 ID：每个账号生成一次 `ext_agent_host_id = urn:uuid:<UUIDv4>`，存进凭证，以后不变（两节点共用同一账号即同一 host）。
2. 授权地址 `https://auth.openai.com/api/accounts/authorize`，参数：`client_id=dynamic_agent_client`（首次；重新授权用已发放的 client_id 且不带 agent_name_hint）、`agent_name_hint=sup2api`、`ext_agent_host_id`、`response_type=code`、`redirect_uri=http://127.0.0.1:1455/auth/callback`、`scope=openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`、`resource=https://api.openai.com/v1`、`state`、`nonce`、`code_challenge_method=S256`、`code_challenge`。
3. 服务器不在本地：浏览器跳到 `127.0.0.1:1455` 会打不开，运营者把地址栏完整网址粘回后台。插件解析 `code`、`state`、`client_id`、`scope`、`error`；校验 state；`error=access_denied` 直接失败；新注册缺 `client_id` 视为未完成；重新授权时返回的 client_id 与已存的不同要拒绝。
4. 换令牌：`POST https://auth.openai.com/api/accounts/oauth/token`，form：`grant_type=authorization_code`、`client_id`（已发放的）、`code`、`code_verifier`、`redirect_uri`（同上）、`resource=https://api.openai.com/v1`。必须确认授予的 scope 含 `chatgpt.tokens.use.direct`。
5. 刷新：同一 token 端点，form：`grant_type=refresh_token`、`client_id`（已发放的）、`refresh_token`、`resource`；不带 scope。终止性错误：`invalid_grant`、`invalid_refresh_token`、`token_expired`、`refresh_token_expired`、`refresh_token_invalidated`、`refresh_token_reused`（需重新授权）；`invalid_client`（配置错误）。其他错误保留凭证、稍后重试。
6. 上游：`POST https://api.openai.com/v1/responses`，`authorization: Bearer <access_token>`；必须 `store:false`、`stream:true`，`input` 必须是数组，不支持 `temperature`、`top_p`、`max_output_tokens`、`metadata`、`user`、`truncation`，HTTP 下不能用 `previous_response_id`，不能有 system 角色的 input 项（改用 `instructions`）。禁止调用 `backend-api`。WebSocket：`wss://api.openai.com/v1/responses`，连接内可续接。
7. 模型：`GET https://api.openai.com/v1/models` → `{"models":[{"slug","display_name","visibility"}]}`，取 `visibility=="list"` 的 `slug`。
8. 错误码（结构化 Responses 错误）：`subscription_sharing_usage_limit_exceeded`（429，冷却账号，不推断重置时间）、`subscription_sharing_usage_unavailable`/`subscription_sharing_user_unavailable`（503，短暂退避）、`subscription_sharing_user_not_eligible`（403，禁用账号）、`subscription_sharing_unsupported_capability`（400，原样返回客户端）、`subscription_sharing_route_not_supported`（403）、`subscription_sharing_invalid_user`（401）、`chatpass_v2_scope_not_authorized`/`chatpass_v2_invalid_authorization_context`（403，禁用）。

## 3. 核心改动：上游只接受流式（`streamOnly`）

两类账号的上游都**只接受 `stream:true`**。核心目前按客户端的 `stream` 原样转发，非流式客户端（以及被转换成 Responses 的非流式 chat 请求）会失败。新增：

- `manifest.AccountPlatform.StreamOnly []string`（JSON `streamOnly`）：列出该账号类型上游只接受流式的上游协议（本插件填 `["openai.responses"]`）。
- 请求选中此类账号、上游协议在列表里、且本次为非流式时，核心：把上游请求体的 `streamPath` 置 `true`；按 SSE 读上游；从 `response.completed`（或 `response.incomplete`）事件取 `response` 对象作为非流式 JSON 响应体；若其 `output` 为空，用沿途 `response.output_item.done` 的 `item` 按 `output_index` 补齐；`response.failed`/`error` 事件按上游错误处理（交给插件 `ClassifyError`，状态码取事件里的值，缺省 502）。之后走原有非流式路径（JSON 用量规则、协议转换成 chat JSON、记录、计费）。
- 使用记录的 `stream` 字段仍记客户端视角（false）。
- 只对声明了 `streamOnly` 的账号类型生效，其他插件行为不变。CONTRACTS 增加一节。

## 4. 插件 `codex`

- 目录 `next/plugins/codex`，key `codex`，runtime grpc，能力 `platform.adapter.v1`、`platform.execute.v1`、`platform.websocket.v1`、`http.routes.v1`。
- 两个账号类型都挂内置 `openai` 平台，`endpoints: ["responses", "responses_websocket"]`（chat 由核心转换成 Responses），`streamOnly: ["openai.responses"]`，`refresh: {}`，`sensitiveFields: ["access_token","refresh_token","id_token"]`，`settingsFields: ["proxy_id"]`，表单 `mode: "native"`、组件 `CodexAuthForm`。
- `BuildUpstreamRequest`：按账号类型选 URL 和头；patch `store=false`；字符串 `input` 转数组（空白字符串转 `[]`，其余包成一条 user 消息）；`role:"system"` 的 input 项改成 `developer`；`codex_device` 缺 `instructions` 时补空串；删除不支持字段；WebSocket 协议换成 `wss` + `GET` + `openai-beta`，并对每轮 `response.create` 消息返回同样的 patch（不含 stream 相关字段，`chatgpt_siwc` 保留 `previous_response_id`；核心每轮应用，CONTRACTS §83.6）。
- `ClassifyError`：reason 一律纯英文（与 SDK `classify` 一致）；401 → 冷却并提示重新授权（刷新由核心按 `refresh` 声明处理）；429 且响应头显示某个用量窗口 ≥100% 并给出重置时刻 → 自动禁用到最晚的重置时刻（0.1.7 起只看响应头，见第 13 节；0.1.2–0.1.6 见第 9 节）；其他 429 原样返回客户端、不冷却不禁用（0.1.2 起；0.1.1 为冷却）；其他 403 → 禁用；400 原样返回。
- 刷新：`BuildRefreshRequest` / `ParseRefreshResponse`，按账号类型选端点和格式；轮换后的 refresh token 一并保存。
- 管理路由（`scope: admin`，权限 `codex:manage`）。响应按 CONTRACTS §3.1：成功为 `{"data": ...}`（`pluginsdk.DataResponse`，下面写的是 `data` 的内容，控制台 `pluginApi` 自动解包）；请求本身不合法（缺 `session_id`、请求体不是合法 JSON）为 400 `invalid_argument`，`details.fields = [{field, code, message}]`（`pluginsdk.BadRequest`；field 为 `session_id` / `body`，code 为 `required` / `invalid_json`）；授权流程本身的拒绝保留各自的错误码（`device_auth_disabled`、`invalid_callback`、`state_mismatch`、`access_denied`、`authorization_failed` 为 400，`session_expired` 为 404，`upstream_unavailable` / `upstream_error` / `token_exchange_failed` 为 502）。所有 message 为“English / 中文”双语（CONTRACTS §10）。
  - `POST /auth/device/start` → `{session_id, verification_url, user_code, interval, expires_in}`；会话存 KV（含 device_auth_id，15 分钟过期）。
  - `POST /auth/device/poll` `{session_id}` → `{status: "pending"|"done"|"expired"|"error", credentials?, message?, interval?}`；`interval` 为 pending 时的轮询间隔（秒，OpenAI 返回 429 时加长）；done 时返回完整凭证对象（见下），同时删除会话。同一会话的“读会话 → 问 OpenAI → 写回/删除”在集群锁（`Host.Locks`，锁名为会话 id 的哈希，ttl 50 秒）下进行：两个标签页或两个节点并发轮询时，拿不到锁的一方直接回 `pending`，不会两边都去用一次性授权码换令牌；锁服务不可用时回 503 `unavailable`。
  - `POST /auth/codex/start`（请求体忽略） → `{session_id, authorize_url}`；PKCE verifier、state 存 KV（30 分钟）。2026-11-17 起取代 `/auth/siwc/start`，见第 13 节。
  - `POST /auth/codex/exchange` `{session_id, callback_url}` → `{credentials}` 或错误。
  - 两种会话都绑定发起它的登录用户（`pluginsdk.SaveCallerSession`，2026-11-12）：其他用户拿到 `session_id` 也无法继续，得到的回答与会话不存在时相同（轮询 `expired`，exchange 404 `session_expired`）；没有登录用户时 start 回 401。
- 凭证对象：
  - 共有：`access_token`、`refresh_token`、`id_token`、`expires_at`（Unix 秒）、`email`、`plan_type`（从令牌读出，两种类型都可能有）；除 `access_token` 外缺值时省略。
  - `chatgpt_account_id`（两种类型都有）。
  - ~~`chatgpt_siwc`：`client_id`、`ext_agent_host_id`、`scopes`~~（已删除）。
- 网络权限（与 manifest 一致）：`net` 只有 `auth.openai.com`（插件自己只在授权时访问授权服务器）；`chatgpt.com`、`api.openai.com` 只列在 `externalServices`，上游请求、模型列表、额度查询（0.1.2）与令牌刷新都由核心发出。
- 其他宿主权限：`platform.register`、`accounts.credentials`（`types: own`）、`kv`（授权会话）、`lock`（设备码轮询的会话锁，见上）、`routes.admin`、`ui.native`；0.1.3 起另有 `accounts.auto_state`（`types: own`），见第 10 节。
- 日志：`Init` 取 `Host.Logger()`；授权失败、令牌换取失败、删除会话失败、锁获取/释放失败记 warn，只记状态码、OAuth 错误码、错误文本；绝不记录令牌、授权码、PKCE verifier、state 或会话 id。
- `ValidateCredentials` 会解析调用方提交的 `id_token`（不验签），用来补缺的 `email`、`plan_type` 与 `chatgpt_account_id`，后者会作为上游头 `chatgpt-account-id` 发送。凭证是运营者自己的（只有有账号权限的管理员能提交），上游会用 access token 校验账号 id，填错只会让运营者自己的请求失败，sup2api 也不据此做任何授权判断，所以不验签（`identity.go` `jwtClaims` 注释）。
- 原生表单 `CodexAuthForm`：设备码页签（开始 → 显示链接与验证码、复制按钮、倒计时、自动轮询 → 成功后填入凭证并显示邮箱/套餐）；Sign in with Codex 页签（2026-11-17 起取代 Sign in with ChatGPT 页签；生成链接 → 新窗口打开 → 粘贴回调网址输入框 → 换取 → 填入凭证）；编辑已有账号时显示当前邮箱与“重新授权”。按账号类型只显示对应页签。中英文文案。回调网址的格式提示与插件 `parseCallback` 一致（完整网址、不带 scheme 的地址、只有查询串 `?code=…` / `code=…`、片段里的参数都接受；只有插件也解析不了的——非法 `%` 转义或分号——才禁用“完成授权”）。失败时优先显示 `details.fields` 里的字段消息，否则显示错误的 message。

## 5. 内置

`deploy/docker/build-go.sh` 的 `BUILTIN_PLUGINS` 加 `codex`（安装即启用）。web mock 加 codex 插件与两个账号类型。

## 6. 实现记录（2026-11-08）

- 核心 §83：`streamOnly`（非流式请求强制上游流式、合成 JSON）；同平台转换 `openai.chat → openai.responses`（2026-11-08 改为宽松：旧式 functions、custom 工具、allowed_tools、web_search_options 等能等价的都转换；`stop`、`seed`、罚分、logprobs、消息 `name`、未知字段等静默丢弃并在网关 debug 日志记下字段路径；只有 `n>1`、音频输出/输入、空 `messages` 等无法保留含义的返回 400 `unsupported_conversion`；对话中间的 system/developer 消息转成 `developer` 角色。详见 CONTRACTS §83.2）；manifest 校验对用到这两项的插件要求 hostCompat ≥ `StreamOnlyFirstCore`（占位 0.1.257，发布时定号）。
- 插件：`codex_device` 也删除 Codex 后端已知不接受的字段（`max_output_tokens`、`max_completion_tokens`、`temperature`、`top_p`、`frequency_penalty`、`presence_penalty`、`user`、`metadata`、`safety_identifier`、`stream_options`、`truncation`、`prompt_cache_retention`、`stop_sequences`；WebSocket 消息保留 `stream_options`，见 `upstream.go` `unsupportedDevice`）；`chatgpt_siwc` 必须有 `client_id`。net 权限只给 `auth.openai.com`（上游与刷新由核心发出）。
- 已知限制：~~WebSocket 消息核心不做 patch，客户端需自带 `store:false`（device 还需 `instructions`）；SIWC 下对话中间的 system/developer 消息会被上游拒绝；~~（0.1.1 已解决，见下）device 账号无在线模型列表；授权请求不走账号代理；路由要求 `codex:manage` 权限。

## 7. 0.1.1（2026-11-08，用户要求 sup2api 兜住客户端差异）

- 核心 CONTRACTS §83.6：WebSocket 每轮消息都调用插件 `BuildUpstreamRequest` 取 body patch（首轮复用握手那次调用，后续轮只取 patch、忽略 URL/头），在钩子与模型映射之后应用；空 patch 时消息逐字节不变；patch 失败回该轮错误事件、不断开。属于同一未发布核心（占位 0.1.257），`hostCompat` 不变。
- 插件：WebSocket 返回与 HTTP 相同的 patch（`store=false`、删除不支持字段、device 补 `instructions`、字符串 input 转数组），不含 stream 相关字段，SIWC 保留 `previous_response_id`。客户端不再需要自带 `store:false`/`instructions`。
- 插件：两种账号类型、HTTP 与 WebSocket 都把 `role:"system"` 的 input 项改成 `role:"developer"`；`requestFields` 新增 `input.#.[role].#(=="system")#`（按项序 `["system"]`/`[]`，每项约 3 字节），据此生成 `input.<i>.role` 的 `OP_SET`，每请求最多 256 个。
- 仍有限制：字符串 input 字段回显整段字符串，超过插件 gRPC 接收上限（4 MiB）会使调用失败；device 账号无在线模型列表；授权请求不走账号代理；路由要求 `codex:manage` 权限。

## 8. 编码规范审核修复（2026-11-08，插件版本仍为 0.1.1）

- 用户可见文案一律“English / 中文”：路由错误、`readToken`、`internalError`、刷新结果（令牌端点状态、响应过大、无法解析、缺 `access_token`、连接失败）。`ClassifyError` 的 reason 改为纯英文。
- 路由：成功统一 `{"data": ...}`；请求参数错误统一 400 `invalid_argument` + `details.fields`（替换原自定义 `invalid_request`、`invalid_client_id`、`invalid_host_id`）；流程业务码不变（见 §4）。web mock（`web/mock/codex.ts`）同步为相同的响应格式与错误码（为便于试用，回调缺 `client_id` 时 mock 仍代发一个，不报 `registration_incomplete`）。
- 设备码轮询加会话锁（新增宿主权限 `lock`），授权失败等记 warn 日志（不含敏感值），见 §4。

## 9. 0.1.2：`codex_device` 的 5 小时 / 周额度窗口（2026-11-09，用户要求）

目标：和 Claude 账号一样在控制台显示 5 小时窗口（key `5h`）和周窗口（key `7d`）的用量与重置时间；窗口真满时按项目规则自动禁用到重置，其他上游 429 原路返回（CONTRACTS §69 末两条，CCGateway 0.1.127 同一规则）。

> **0.1.7 起本节 9.2–9.4 已被第 13 节取代**（9.1 里按 ±5% 容差定 key、`reset-after-seconds`、`wham/usage` 的内容和 9.5 的测试清单同样作废）：窗口只来自响应头，`wham/usage` 查询已删除，判满只看 429 的响应头。下面保留的是 0.1.2–0.1.6 的历史设计。

### 9.1 协议事实

- 响应头：`x-codex-primary-used-percent`（百分比）、`x-codex-primary-window-minutes`、`x-codex-primary-reset-at`（Unix 秒，新后端）或 `x-codex-primary-reset-after-seconds`（旧后端），`secondary` 同名三项。
- 用量接口：`GET https://chatgpt.com/backend-api/wham/usage` → `{"plan_type", "rate_limit": {"allowed", "limit_reached", "primary_window": {"used_percent", "limit_window_seconds", "reset_after_seconds", "reset_at"} | null, "secondary_window": … | null} | null, "credits", "additional_rate_limits"}`。未使用的槽位为 `null`。
- **primary / secondary 与 5h / 周的对应关系不固定**（旧版 sub2api 注释写 primary = 周、secondary = 5h，Codex CLI 里 primary 通常是 300 分钟），所以插件一律按窗口时长定 key（`windowKey`）：300 分钟 ±5% → `5h`，10080 分钟 ±5% → `7d`；其他时长按实际长度给 key（整天 `<n>d`、整小时 `<n>h`，否则 `<n>m`，控制台按 key 原样显示）；没有时长或时长不可信（≤0 或超过一年）的窗口忽略。两个窗口时长相同时保留用得多的那个。
- `reset-at` / `reset_at` 优先（大于 1e11 视为毫秒），没有时用 `reset-after-seconds` / `reset_after_seconds` 加当前时间；都没有时重置时间未知（0）。没有用量百分比的窗口记为 `utilization_unknown`。用量 ≥100% 的窗口状态记 `rejected`，其他不写状态（后端不报）。
- `additional_rate_limits`（按功能的额外限额，如 Codex Spark）与 SSE 里的 `codex.rate_limits` 事件不读：前者不是账号级窗口，后者核心不交给插件。

### 9.2 manifest（只给 `codex_device`）（已被第 13 节取代）

```json
"quota": {
  "query": true,
  "autoRefresh": true,
  "windows": [
    { "key": "5h", "label": { "en": "5-hour usage", "zh": "5 小时用量" }, "shortLabel": { "en": "5h", "zh": "5 小时" } },
    { "key": "7d", "label": { "en": "Weekly usage", "zh": "周用量" }, "shortLabel": { "en": "7d", "zh": "7 天" } }
  ]
}
```

- 不声明 `quota.headers`：声明式被动采样把固定的头映射到固定的 key（CONTRACTS §44.2），表达不了“按 window-minutes 选 key”，而核心目前没有让插件在成功响应上解析额度头的钩子（只有 `ClassifyError` 的 `quota`，见 9.4）。所以数据来自主动查询：`autoRefresh` 让核心后台每 30 分钟查一次（新账号录入后立即查一次，升级后已有账号由核心的补建扫描建立查询计划），点击控制台用量单元格强制刷新。窗口重置时间在 30 分钟内时，插件建议在重置后 1 分钟复查（`next_check_at_unix`）。
- `chatgpt_siwc` 不声明额度：Sign in with ChatGPT 官方文档没有额度头，并且禁止调用 `backend-api`，只靠 `subscription_sharing_usage_limit_exceeded` 冷却。`BuildQuotaRequest` 对它回 `Unimplemented`。
- 不声明 `modelAvailability`：额度窗口不参与调度准入（同 CCGateway 0.1.112 起的做法），真满时由下面的自动禁用把账号拿出调度。
- `autoRefresh` 需要 Host API 7（SDK 在旧核心上拒绝初始化）；`hostCompat` 仍为 `>=0.1.257 <0.2.0`，所用能力（`quota.query/autoRefresh/windows`、`automatic_state`、Host API 7）0.1.257 都有。控制台已认识 `5h` / `7d`，前端不用改。

### 9.3 查询（插件描述、核心发送）（已被第 13 节取代，查询已删除）

- `BuildQuotaRequest`：`GET <chatgpt>/backend-api/wham/usage`，头 `authorization: Bearer <access_token>`、`chatgpt-account-id`（凭证里有时）、`originator: sup2api`、`user-agent: sup2api-codex/<版本>`、`accept: application/json`。不发任何伪装 Codex 客户端的头（`openai-beta`、`oai-language`、`sec-fetch-*` 等旧版用过的都不发）。凭证无 `access_token` → `FailedPrecondition`，凭证不是 JSON → `InvalidArgument`。
- 核心经账号代理发送，所以插件 `net` 权限不变（仍只有 `auth.openai.com`），`chatgpt.com` 已在 `externalServices`。
- `ParseQuotaResponse`：401/403 → `auth_rejected`；连接失败、其他非 2xx、响应过大、无法解析 → `transient`（只记录在快照上，不冷却、不禁用账号）；`rate_limit` 为 `null` 或两个窗口都为 `null` → 成功、没有窗口。

### 9.4 `ClassifyError`：窗口用满才禁用（已被第 13 节取代）

- **窗口用满**（`windowFull`）：上游错误 type/code 为 `usage_limit_reached`（任何状态码），或 429 且 `x-codex-*` 头里 `5h` / `7d` 窗口用量 ≥100%。其他时长的窗口满、只有重置时间或 `retry-after`，都不算。
- 用满时：`action = FAILOVER`，`automatic_state = DISABLE`、`until_unix` = 重置时刻，reason `Codex usage limit reached until <时间> (<来源>)`。重置时刻依次取：错误体 `resets_at`（秒或毫秒）/ `resets_in_seconds`（错误对象内或顶层）→ 头里已满的 5h/周窗口中最晚的重置时刻 → `retry-after(-ms)` → 5 分钟；限定在 [现在+1 秒, 现在+7 天]。核心不论是否发布了故障策略都执行这个临时禁用，到点自动恢复（CONTRACTS §69）；账号不允许自动禁用时，核心退回冷却到同一时刻（`cooldown_until_unix`，仅无故障策略时）。
- **其他 429**（包括带窗口头但都没满的）：`action = RETURN_TO_CLIENT`，不冷却、不禁用，reason `upstream 429 without a used-up usage window`，上游错误码照常作为 `client_error_code` 返回。0.1.1 对所有 429 冷却（默认 5 分钟），这是行为变化。
- 错误响应带 `x-codex-*` 窗口头时，`ClassifyErrorResponse.quota` 附上这些窗口（已满的标 `rejected`）。注意：核心只在声明了 `modelAvailability` 的类型上保存这份观测（准入时要有快照 generation，CONTRACTS §44.7），本插件不声明，所以目前不会写入快照，控制台的用量仍以查询为准。
- `subscription_sharing_usage_limit_exceeded`（`chatgpt_siwc`）保持冷却，不推断重置时间、不自动禁用。发布了故障策略时核心不采用插件给的冷却（§69），这类账号的处理由策略决定。

### 9.5 上线

- 只需在线升级插件到 0.1.2；不需要升级核心（线上 0.1.258 ≥ 0.1.257）。
- 测试：`quota_test.go`（时长 → key、两种窗口顺序、reset-at 与 reset-after、毫秒、缺字段、null 窗口、错误映射、查询请求构造、经 SDK 服务端调用）、`classify_test.go`（`usage_limit_reached` 与头部判满的自动禁用和观测、其他 429 原样返回、SIWC 仍为冷却）、`manifest_test.go`（只有 `codex_device` 有额度声明、窗口标签中英文、`net` 不变）。

## 10. 0.1.3：官方用量重置次数（2026-11-10，用户要求）

目标：ChatGPT 套餐会发放少量“用量重置次数”（rate-limit reset credits），每次可把账号已用满的 Codex 用量窗口立即重置一次。控制台上能查看某个 `codex_device` 账号剩余的重置次数，并由管理员手动对该账号使用一次；成功后账号立即恢复调度。仍只用于账号主人自己的订阅。

### 10.1 官方接口（openai/codex `codex-rs/backend-client` 的 `rate_limit_resets.rs`、`types.rs`）

- 根为 `https://chatgpt.com/backend-api`，鉴权头与 `wham/usage` 完全相同。只有 `codex_device` 能用：Sign in with ChatGPT 禁止调用 `backend-api`。
- `GET /backend-api/wham/rate-limit-reset-credits` → `{"available_count": int, "credits": [{"id", "reset_type", "status": "available"|"redeeming"|"redeemed"|…, "granted_at": RFC3339, "expires_at": RFC3339|null, "title": str|null, "description": str|null}]}`。
- `POST /backend-api/wham/rate-limit-reset-credits/consume`，body `{"redeem_request_id": "<uuid>", "credit_id"?: "<id>"}`（不带 `credit_id` 时后端挑下一个）→ `{"code": "reset"|"nothing_to_reset"|"no_credit"|"already_redeemed", "windows_reset": int}`。`redeem_request_id` 是幂等键：同一个 id 重复提交只扣一次（第二次答 `already_redeemed`）。
- `wham/usage` 里也有 `rate_limit_reset_credits: {available_count}`（可能缺失或为 `null`）：0.1.3–0.1.6 用它缓存次数；0.1.7 删除了 `wham/usage` 查询，不再读（见 10.6）。

### 10.2 发送方式

- 插件不直连 `chatgpt.com`（`net` 仍只有 `auth.openai.com`）：两个请求都经 `pluginsdk.SendAccountRequest` 由核心通过账号代理发出（CONTRACTS §91，URL 主机须在 `externalServices`，`chatgpt.com` 已在；核心 0.1.269 起提供，旧核心答 `Unimplemented`，路由回 503 并提示需要的核心版本）。
- 头为 `backendHeaders`（0.1.2–0.1.6 额度查询用的同一组）：`authorization: Bearer <access_token>`、`chatgpt-account-id`（有时）、`originator: sup2api`、`user-agent: sup2api-codex/0.1.3`、`accept: application/json`，consume 另加 `content-type: application/json`。不发 `x-codex-*` 或任何伪装 Codex CLI 的头。

### 10.3 路由（`scope: admin`，新权限 `codex:reset`）

成功为 `{"data": ...}`，错误为 `{"error": {"code", "message"}}`（message “English / 中文”），请求参数错误为 400 `invalid_argument` + `details.fields`，与授权路由一致（§4）。

- `POST /reset-credits/list`，body `{"account_id": int}` → `{"available_count": n, "credits": [{"id", "title", "description", "status", "granted_at": Unix 秒, "expires_at": Unix 秒|null}]}`。
  - `credits` 按 `expires_at` 升序，`null`（不过期或时间无法解析）排最后；顺序稳定。
  - 解析宽松：未知 `status` / `reset_type` 原样透传（`reset_type` 不输出）；`title` / `description` 为 `null` 时输出空串；`granted_at` 解析失败为 0，`expires_at` 解析失败为 `null`。响应不是 JSON → 502 `upstream_error`。
- `POST /reset-credits/consume`，body `{"account_ids": [int], "credit_id"?: str}`。`account_ids` 是控制台声明式行操作的固定形状，本接口**只接受恰好 1 个**正整数 id，否则 400（field `account_ids`，code `count`）。`credit_id` 去空白，空时不发给后端。
  - `reset` / `already_redeemed` → 200 `{"outcome": "reset"|"already_redeemed", "windows_reset": n, "account_restored": bool}`。随后调用 `SetAccountAutomaticState(CLEAR)` 清除账号的自动停用与冷却（§9.4 窗口用满时设置的，核心本版起 CLEAR 也清冷却）；CLEAR 失败只记 warn，请求仍为 200，`account_restored: false`（状态到窗口重置时刻自然解除）。
  - `nothing_to_reset` → 409 `nothing_to_reset`（未消耗次数）；`no_credit` → 409 `no_credit`。
- `POST /reset-credits/summary`，body `{"account_ids": [int, ...]}` → `{"accounts": {"29": {"available_count": 2, "updated_at": 1793491200}}}`（键为账号 id 字符串，`updated_at` 为写入缓存的 Unix 秒）。只读插件 KV、不访问 ChatGPT、不读凭证；没有缓存的账号不出现在结果里（结果可以是 `{}`）。`account_ids` 先只留正整数并去重，之后超过 `manifest.MaxAccountBatch`（500）→ 400（field `account_ids`，code `max`）。供账号列表的次数装饰器使用（列表每 10 秒刷新）。
- list / consume 共同的错误：账号不存在或不属于本插件 → 404 `account_not_found`；账号类型不是 `codex_device` → 400 `unsupported_account_type`；凭证无 `access_token` 或不是 JSON → 409 `not_authorized`（提示重新授权）；上游 401/403 → 409 `auth_rejected`（提示重新授权）；连接失败、其他非 2xx、响应过大、未知 `code` → 502 `upstream_error`（带状态码和截断到 200 字节的响应片段；片段中出现的 access token 替换为 `[redacted]`，绝不回显令牌）；读账号或发请求的宿主调用失败 → 503 `unavailable`（只有 `Unimplemented` 才提示需要核心 0.1.269）。

### 10.4 锁与幂等

- consume 在每账号一把集群锁下进行（`Host.Locks`，锁名 `codex-reset:<account_id>`，ttl 90 秒（两次各至多 30 秒的发送加余量，确保重试仍在进行时锁不会过期），结束即释放，释放失败等 ttl 过期）：双击或两个节点同时提交时，拿不到锁的一方回 409 `busy`，不发请求。锁服务不可用 → 503 `unavailable`。
- 每次 consume 新生成一个 UUIDv4 作 `redeem_request_id`；传输失败（status 0）时用**同一个** id 重试一次，所以即使第一次其实已到达后端也只扣一次（后端答 `already_redeemed`，按成功处理）。再失败回 502，不再重试；非 0 状态码不重试。
- list 只读，不加锁、不重试。

### 10.5 manifest 与守线

- `version` 0.1.3，`hostCompat` `>=0.1.269 <0.2.0`（`SendAccountRequest`）。
- 新增 userPermission `codex:reset`（说明写明会消耗账号由 OpenAI 发放的重置次数、不可撤回），三条路由（list、consume、summary）都用它，与授权用的 `codex:manage` 分开。
- 新增宿主权限 `accounts.auto_state`（`types: own`，用于成功后 CLEAR）；`accounts.credentials`、`kv`（次数缓存）、`lock`、`routes.admin` 的 reason 补上本功能。控制台入口（UI 3.8，CONTRACTS §91b，`hostUICompat` `^3.8`）：`account.list.quota` 次数装饰 `CodexResetBadge`、`account.detail.tabs` 标签页 `CodexResetTab`、行操作 `reset-credit`（`POST /reset-credits/consume`，`danger`、带确认、不设 `bulk`），都要 `codex:reset` 且只对 `codex_device` 显示。
- 守线：只由管理员对单个账号手动触发，插件从不自动消耗重置次数；请求如实标 sup2api；只用于账号主人自己的订阅。
- 测试：`resetcredits_test.go`（假 chatgpt.com + 包一层 harness 宿主 fake `AccountCredentials` / `SendAccountRequest` / `SetAccountAutomaticState`）：list 解析与排序、各 `code` 与状态码的映射、传输失败重试复用同一 `redeem_request_id` 且最多两次、锁被占回 409 且不发请求、SIWC / 无令牌 / 未知账号、`account_ids` 数量不对、reset 后 CLEAR 及 CLEAR 失败仍 200、请求头含 `originator: sup2api` 且无 `x-codex-*`、错误不回显令牌、旧核心回 503；`manifest_test.go`（新权限、路由与权限对应、`accounts.auto_state` 范围）。

### 10.6 次数缓存（账号列表装饰器）

- 位置：插件 KV，namespace `reset_count`，key 为账号 id，值 `{"available_count", "updated_at"}`，TTL 7 天（键名与读写封装都在 `resetcache.go`）。
- 写入：`/reset-credits/list` 成功时用应答的 `available_count` 写入。0.1.3–0.1.6 还在 `ParseQuotaResponse` 解析 `wham/usage` 时写入，0.1.7 随查询一起删除（第 13 节），所以次数只由 list 刷新、由 consume 删除。
- 失效：consume 得到 `reset` / `already_redeemed` 或未知 `code`（可能已扣）后**删除**该账号的缓存键，而不是写“原数 − 1”（原数可能已过时，`already_redeemed` 也未必是这次扣的），也不为此再调一次 list（多一次上游请求）。装饰器在下次打开明细（list）后恢复显示。`nothing_to_reset` / `no_credit` 不动缓存。
- KV 读写失败一律只记 warn：不让 list / consume 失败；summary 中读失败的账号视为没有缓存（一次请求只记一条 warn，带失败个数）。
- summary 的 KV 只有单键 `Get`，所以并发读：最多 16 个同时进行，整次请求限时 5 秒（超时后未读到的账号视为没有缓存）。
- 测试：`resetcache_test.go`：summary（去重与非正整数过滤、缺失账号不出现、500 个通过、501 个 400、坏 JSON、KV 失败仍 200、不发上游请求）、list 写入与 consume 后删除（拒绝的 consume 不删，KV 失败不影响 consume）。

## 11. 0.1.4：权限加固（2026-11-13，随核心权限加固发布）

功能与上游协议不变，只改授权相关行为；版本在期 4 T84 统一升级（[DESIGN-2026-11-12-AUTHZ-HARDENING.md](DESIGN-2026-11-12-AUTHZ-HARDENING.md)“期 4 T84”）。

- **`hostCompat` `>=0.1.286 <0.2.0`**：`defaultRoles` 显式写出（旧核心把省略与 `[]` 混淆）和路由的 `accounts` 声明都需要权限加固后的核心（0.1.286）。
- **路由的账号范围（T25）**：`POST /reset-credits/list` 声明 `{"action":"update","from":["body:account_id"]}`，`POST /reset-credits/summary` 声明 `{"action":"read","from":["body:account_ids"]}`，核心按调用者对这些账号的范围放行（只有自己创建的账号的角色操作他人账号回 404，CONTRACTS §21）；`/reset-credits/consume` 不声明，由 `ui.accountActions` 推断为 `update` + `body:account_ids`。持有 `codex:reset` 但没有任何范围的 `account:update` 的角色调用 list / consume 由 200 变为 403。
- **默认角色（T58）**：`codex:manage`、`codex:reset` 声明 `defaultRoles: ["admin", "operator"]`。新安装时授给这两个内置角色；已安装的库不回填（D9），升级时若新授予需要同意（敏感权限或授给 user、自定义角色），codex 作为内置插件会暂缓这些授予，由超管在插件页确认（P4C；授给内置角色只有超管能确认）。
- **授权会话绑定发起人（T42）**：设备码与 Sign in with ChatGPT 的会话改用 `pluginsdk.SaveCallerSession` / `LoadCallerSession` / `UpdateCallerSession`，存 `{"uid", "v"}`。他人拿到 `session_id`：设备码轮询回 200 `expired`（与会话不存在相同），SIWC `exchange` 回 404 `session_expired`。升级或回滚时进行中的授权会话（不超过 15 分钟 / SIWC 会话有效期）需重新发起。重置次数缓存不是会话，改名 `loadCached` / `saveCached`，行为不变。
- **账号类型（T26）**：`codex_device`、`chatgpt_siwc` 声明 `upstreamFields: []`（没有可改的上游地址），控制台据 `custom_upstream=false` 不显示上游地址字段。
- **访问密钥（S4）**：`plugin.codex:codex:manage` 与 `plugin.codex:codex:reset` 可以加入访问密钥的权限；`consume` 会花掉 OpenAI 发放的次数，不可撤销，授给密钥前确认用途。
- 回滚：装回 0.1.3（核心回滚时随之回滚）；会话格式不兼容，进行中的授权需重新发起。

## 12. 0.1.6：默认模型换成上游现有模型（2026-11-17，用户要求）

- 用户反映内置模型和上游对不上，`gpt-6-astra` 请求在账号模型里没有它时被调度拒绝（`model_not_served`，503）。在 ec2pool 测试实例上对 Sign in with ChatGPT 账号获取模型（`GET /v1/models`，`visibility=="list"`）得到：`gpt-5.6-luna`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-6-astra`、`gpt-6-luna`、`gpt-6-sol`、`gpt-6.1-sol`、`gpt-6.1-terra`。两种账号类型的 `defaultModels` 都换成这 8 个（新到旧），`DefaultTestModel` 由已不在列表里的 `gpt-5.1-codex-mini` 改为最便宜的 `gpt-6-luna`。`codex_device` 没有在线模型列表，沿用同一份；测试实例上没有设备码账号，未单独核实。
- 生图：同一账号带 `tools:[{"type":"image_generation"}]` 请求，上游回 400 `subscription_sharing_unsupported_capability`（“User subscription sharing currently supports only 'web_search' and developer-defined 'function' and 'custom' tools … tool 'image_generation' is not supported”），所以 SIWC 账号不能生图，不加生图模型。`codex_device`（ChatGPT Codex 后端）是否支持需要真实设备码账号验证，见 [DESIGN-2026-11-17-OPENAI-IMAGE-BILLING.md](DESIGN-2026-11-17-OPENAI-IMAGE-BILLING.md)。
- 计费为 0 的根因不在插件：上游 SSE 标成 `application/json`，由核心按正文嗅探修复（CONTRACTS §104.1）。

## 13. 0.1.7：用量窗口以响应头为准（2026-11-17，用户要求）

用户要求“以响应头为准，不要乱加兜底”“429 + 响应头判断”。本节取代 §9.2–9.4（以及 §9.1 中按 ±5% 容差定 key、`reset-after-seconds`、`wham/usage` 的内容）。

- 协议事实（openai/codex `codex-rs/codex-api/src/rate_limits.rs`）：`x-codex-{primary,secondary}-used-percent`（百分比）、`-window-minutes`、`-reset-at`（Unix 秒），没有别的。哪个槽位是 5 小时、哪个是周不固定。
- manifest（`codex_device`）：`quota` 改为声明这 6 个头，每个槽位一条，用 `windowMinutes` 按窗口时长命名（CONTRACTS §105：300 → `5h`，10080 → `7d`，其他时长为 `<n>d` / `<n>h` / `<n>m`，没有容差；同一响应两个槽位同名时保留用得多的）；`windows` 保留 5 小时 / 周的标签；删掉 `query`、`autoRefresh`。核心的被动采样（CONTRACTS §44.3）在每个 2xx、429、101 响应上读这些头，所以每个响应都会更新窗口。没有了主动查询，控制台的用量单元格也不能再强制刷新；核心按旧声明建的查询计划因声明里没有 `query` 而跳过。
- 删除 `wham/usage` 查询：`BuildQuotaRequest`、`ParseQuotaResponse` 及其解析、按时长定 key 的容差、窗口合并、`reset-after-seconds` 处理全部删除，插件不再实现 `pluginsdk.QuotaReader`（SDK 对查询调用回 `Unimplemented`）。`backendHeaders` 只剩重置次数接口在用，移到 `resetcredits.go`。
- 头名只写在 manifest：`main.go` 把嵌入的 manifest 传给 `codex.New(manifestJSON)`，插件读出 `codex_device` 的 `quota.headers`，`ClassifyError` 用这份声明调 `manifest.ReadQuotaHeaders`（与核心采样同一个函数）；manifest 里没有这份声明时插件启动失败。
- `ClassifyError`：
  - 窗口用满：状态 429，且响应头里某个窗口（任何时长都算，不只 5 小时 / 周）用量 ≥100% 并有晚于现在的重置时刻。此时 `FAILOVER`，`automatic_state = DISABLE`、`until_unix` 为这些已满窗口中最晚的重置时刻（限定在 7 天内），冷却到同一时刻作为核心不能自动禁用时的后备，reason `Codex usage limit reached until <时间> (x-codex window reset)`。到点由核心自动恢复（CONTRACTS §69）。
  - 只看响应头：不再读错误体的 `resets_at` / `resets_in_seconds`，不用 `retry-after`，没有 5 分钟默认值。窗口满但没有重置时刻（或重置时刻已过）→ 当作普通 429，原样返回客户端。
  - 删除“错误 type/code 为 `usage_limit_reached`（任何状态码）即算用满”：只有这个错误体、没有满窗口的头 → 原样返回客户端。上游错误码仍作为 `client_error_code` 返回（type `usage_limit_reached` 照旧当作 code）。
  - 不再在 `ClassifyErrorResponse.quota` 里附窗口：429 的头由核心自己采样。
  - 其他 429 → `RETURN_TO_CLIENT`，不冷却、不禁用；其他状态码从不因窗口头禁用。401、403、5xx、传输错误的规则不变。
  - Sign in with ChatGPT（`chatgpt_siwc`）整体删除（同期另一项改动），它的错误规则（`subscription_sharing_*`、`chatpass_v2_*`）随之从 `ClassifyError` 删除。
- 重置次数缓存（§10.6）不再由用量查询写入，只由 `/reset-credits/list` 成功时写入、consume 后删除。
- `version` 0.1.7，`hostCompat` `>=0.1.297 <0.2.0`（`windowMinutes` 随核心 0.1.297 发布，旧核心因缺 `key` 拒绝安装）。宿主权限 `accounts.credentials`、`net` 的说明去掉“用量查询”。
- 测试：`quota_test.go`（manifest 的声明经 `ReadQuotaHeaders` 在两种槽位顺序下都得到 `5h` / `7d`；判满：任一窗口满且有未来重置时刻、取最晚、无重置时刻或已过、未满、没有时长头；manifest 缺声明时启动失败）、`classify_test.go`（429 满窗口 → 禁用到最晚重置并冷却到同一时刻；满但无 `reset-at`、未满、没有头、只有 `usage_limit_reached` 错误体 → 原样返回；200 / 503 / 401 带满窗口头不禁用；不附 `quota`）、`manifest_test.go`（`codex_device` 声明 `windowMinutes` 头、没有查询）。

## 14. Sign in with Codex（2026-11-17，用户要求）

用户要三种 ChatGPT 登录方式，随后改为只保留两种：“Device Code”（`codex_device`）与新增的“Sign in with Codex”（`codex_oauth`）；Sign in with ChatGPT（`chatgpt_siwc`）整体删除。插件版本号不在本改动里升（同期另一项改动统一升到 0.1.7）。

### 14.1 协议（与旧版 Sub2API `backend/internal/pkg/openai/oauth.go` 及其服务代码一致）

- Codex CLI 的浏览器登录：公开客户端 `client_id = app_EMoamEEZ73f0CkXaXp7hrann`（与设备码相同），授权码 + PKCE（S256），没有动态客户端注册。
- 授权地址 `https://auth.openai.com/oauth/authorize`，参数：`client_id`、`response_type=code`、`redirect_uri=http://localhost:1455/auth/callback`（Codex 客户端登记的回调，必须是 `localhost`）、`scope=openid profile email offline_access`、`state`、`code_challenge_method=S256`、`code_challenge`，以及 Codex CLI 同样带的 `id_token_add_organizations=true`（id_token 带上工作区信息）、`codex_cli_simplified_flow=true`（只走 ChatGPT 授权页，不进入 API 组织设置）。旧版还会发的 `originator` 等 Codex 标识不发。
- 服务器不在本地：浏览器最后跳到 `localhost:1455/auth/callback?...` 打不开，运营者把地址栏完整网址粘回表单（解析与原 SIWC 相同：完整网址、无 scheme、只有查询串、片段都接受）。
- 换令牌：`POST https://auth.openai.com/oauth/token`，form：`grant_type=authorization_code`、`code`、`redirect_uri`（同上）、`client_id`、`code_verifier`。与设备码登录的最后一步完全相同（只有 `redirect_uri` 不同），代码共用 `redeemCode`。
- 得到的令牌与设备码登录相同：上游 `chatgpt.com/backend-api/codex/responses`、`chatgpt-account-id` 取自 id_token、刷新用同一个 Codex 客户端（`POST /oauth/token` JSON，scope `openid profile email`，`refresh.go`）、`x-codex-*` 用量头、429 处理、官方重置次数都一样。插件里两种类型没有任何分支，只在 `knownType` 里一起列出。

### 14.2 插件改动

- manifest：新账号类型 `codex_oauth`（`authMethodLabel` “Sign in with Codex”，表单、敏感字段、设置字段、`refresh`、默认模型、`requestFields`、`streamOnly` 与 `codex_device` 相同）；额度用 `quota.headers` 按 `windowMinutes` 从每次应答的 `x-codex-primary-*` / `x-codex-secondary-*` 头命名窗口（CONTRACTS §105），窗口标签与 `codex_device` 相同；`codex_device` 的 `authMethodLabel` 改为 “Device Code”。路由 `POST /auth/codex/start`、`POST /auth/codex/exchange`（`scope: admin`，`codex:manage`）。重置次数的两个 UI 插槽与行操作的 `accountTypes` 加上 `codex_oauth`。宿主权限说明改为设备码与 Sign in with Codex。
- 会话：KV namespace `codex_session`，`{state, code_verifier}`，30 分钟，绑定发起人（`SaveCallerSession`，他人或过期都回 404 `session_expired`）。exchange 的错误码沿用原 SIWC：`invalid_callback`、`state_mismatch`、`access_denied`（结束会话）、`authorization_failed`（400），`session_expired`（404），`upstream_unavailable`、`token_exchange_failed`（502）。
- 表单：`SiwcFlow.vue` 改为 `OAuthFlow.vue`（生成链接 → 新窗口打开 → 粘贴 `localhost:1455` 回调网址 → 换取），重新授权走同一流程；文案 `oauth.*`。

### 14.3 删除 Sign in with ChatGPT（`chatgpt_siwc`）

- 删掉账号类型、`/auth/siwc/*` 路由、动态客户端注册（`dynamic_agent_client`、`ext_agent_host_id`、`resource`、`nonce`、`chatgpt.tokens.use.direct` scope 检查）、`api.openai.com` 上游与模型列表（`BuildModelsRequest` 不再实现，两种类型都用 manifest 默认模型）、SIWC 的刷新分支、凭证字段 `client_id` / `ext_agent_host_id` / `scopes`、`externalServices` 里的 `api.openai.com`。上文 §2.2、§4 中 SIWC 的路由与凭证、§6–§9 中 SIWC 的行为、§12 中 SIWC 的模型核实都只是历史记录。`classify.go` 里 SIWC 的错误码规则由同期另一项改动删除。
- 不留兼容代码：已有的 `chatgpt_siwc` 账号必须在升级前手动删除；升级后插件不认识该类型（`ValidateCredentials` 回 `account_type unsupported`，上游与刷新回 `FailedPrecondition` / `InvalidArgument`）。

### 14.4 测试

- `oauth_test.go`：start 的授权地址参数与 KV 会话（verifier 对得上 challenge、绑定用户 1）、exchange 成功（换令牌表单、凭证与设备码相同）、会话用后即删、只粘查询串、state 不符 / access_denied / 其他错误 / 无 code / 空、错误后会话保留与拒绝后删除、令牌端点拒绝 / 无 access_token / 连不上；他人会话。`upstream_test.go`、`refresh_test.go`、`resetcredits_test.go`：`codex_oauth` 与 `codex_device` 的上游请求、刷新、重置次数相同；无模型列表。`manifest_test.go`：两个类型、标签、`codex_oauth` 的额度头、路由、插槽与行操作覆盖两种类型、`externalServices`。UI：`CodexAuthForm.spec.ts`（Sign in with Codex 流程、编辑与重新授权）、`flow.spec.ts`、`messages.spec.ts`。
