# 插件开发：系统设置适配

本文写给开发平台插件（声明账号类型、为网关请求服务的插件）的人：控制台“系统设置”和账号编辑器里的每一项设置，在插件的请求上由谁生效、插件要配合什么。规则本身以 [CONTRACTS.md](CONTRACTS.md) 为准，本文只做索引和对照；两者不一致时以 CONTRACTS 为准，并把本文改对。插件的打包、签名、权限、迁移、界面写法见 CONTRACTS §2、§23.4、§26、§36 和 [ARCHITECTURE.md](ARCHITECTURE.md) §5。

**总规则（2026-11-17 用户定）：每个平台插件都必须遵守全部系统设置，用户在控制台改的设置对任何插件的账号都要生效。** 新增系统设置或插件执行能力时，要同时核对下面三条请求路径并更新本文的表。

## 1. 三条请求路径

网关请求在核心里走同一条流水线：鉴权、价格、余额与预扣、审核钩子、调度与准入、请求体改写，然后才按选中的账号分叉（`server/internal/gateway/dispatch.go` 的 `attempt`）：

| 路径 | 用在哪里 | 谁发上游请求、谁记用量 |
|---|---|---|
| 普通转发 | 只有清单的插件（openai、gemini、relay、azure_openai）和实现了 `BuildUpstreamRequest` 的 gRPC 插件（anthropic、claude-oauth、vertex、bedrock） | 核心按插件给的请求（或声明式上游）发出，核心中继响应、按 `usage` 规则记用量 |
| 插件执行（`platform.execute.v1`） | codex、volcengine | 插件的 `Execute` 调 `ForwardUpstream`，上游请求仍由核心的同一段代码（`forwardBuilt` → `forward`）发出和中继；用量由插件在 `Execute` 里 `RecordUsage`（SDK 的 `ExecuteDefault` 已做好），异步任务用 `ReserveAndWatch` |
| WebSocket（`platform.websocket.v1`） | openai、codex 的 `/v1/responses` WebSocket 模式 | 核心建连并逐轮中继，每轮按规则记用量（CONTRACTS §83.6、[OPENAI-RESPONSES-WEBSOCKET.md](OPENAI-RESPONSES-WEBSOCKET.md)） |

因为分叉发生在改写之后、发送之前，大部分设置对三条路径天然一致；插件执行路径曾经有两处没做到（下表标“0.1.301 起”）。

## 2. 设置对照表

“核心统一”表示插件什么都不用做；“插件配合”写明插件要实现或声明什么。表里说的都是网关请求上的生效方式；插件在请求之外（定时任务、事件处理、自己的 HTTP 路由）要按这些设置行事时，用第 5 节的接口读取当前值，不要让运营方在插件设置里再填一遍。

### 2.1 计费（系统设置 → 计费）

| 设置 | 归属 | 普通 / 执行 / WebSocket | 说明与位置 |
|---|---|---|---|
| 模型没有配置价格时、最低余额、高额费用警告 | 核心统一 | 一致 / 一致 / 每轮检查 | `pipeline.go` `prepareBilling` |
| 请求预扣 Token 数、输出预留 | 核心统一；**插件配合**：可实现 `EstimateUsage` 给出更准的估算，返回 `Unimplemented` 时核心用本地 tokenizer（`task.submit` 端点必须实现） | 一致 / 一致 / 每轮 | `precharge.go`，CONTRACTS §25.8 |
| 客户端中途断开时（读完再计费 / 按预扣计费 / 不计费） | 核心统一 | 一致 / 一致（**0.1.301 起**，此前插件执行一律按预扣） / 不续读，按预扣（契约规定） | `disconnect.go`；CONTRACTS §25.8。终止用量事件送达客户端之后才断开的请求按成功计费（0.1.301 起，三种策略都一样）；插件放弃 `ForwardUpstream`（被终止、超时）记为上游中断，不算客户端断开 |
| 用量来源 | **插件配合**：在端点或账号类型上声明 `usage` 规则；规则表达不了时声明 `usage.source: "plugin"` 并实现 `ExtractUsage` | 一致 / 在 `Execute` 内记录 / WebSocket 不支持 `source: plugin`（清单检查拒绝） | `usageplugin.go`、`sdk/manifest/check/websocket.go` |

流式用量规则的写法会影响断开判定：**输出 token 数要映射在一个指名的终止事件上**（例如 Anthropic 的 `message_delta`、Responses 的 `response.completed`），核心据此判断“最终用量已经送达客户端”（`usagerules.Acc.Final`）。映射在任意事件（`"event": ""`）上的规则不会被当成终止，只能以读到流结束为准。

### 2.2 网关设置与账号的请求改写

| 设置 | 归属 | 普通 / 执行 / WebSocket | 说明与位置 |
|---|---|---|---|
| 账号参数覆盖（请求体）、Claude 默认 max_tokens 内置覆盖、过滤字段 | 核心统一，在插件之前执行 | 一致 / 一致（插件按改写后的请求体构建）/ 握手和每轮各一次 | `rewriteBody`；CONTRACTS §84、§84.1。插件的 patch 排在覆盖之后，可以再改覆盖的结果 |
| 账号参数覆盖（请求头）、请求头覆盖 `header_override`、请求头透传 | 核心统一 | 一致 / 一致 / 覆盖头只用于握手；透传头（含请求头覆盖的透传键）在握手上不生效（有意如此，`websocket.go` `dial`）；请求头覆盖与首条消息的改写分开解析，改写失败时握手仍带它，已存的值编译不过时换号 | `forwardBuilt`、`accountHeaders`；CONTRACTS §84、§108。请求头覆盖是参数覆盖的初始请求头状态；插件的头排在两者之后 |
| 上游只接受流式（`streamOnly`） | **插件配合**：在账号类型路由上声明 | 一致 / 一致 / 不适用 | `streamonly.go`，CONTRACTS §83 |
| 请求体上限 | 核心统一 | 一致 / 一致 / 作为单条消息上限 | `readBody` |
| 上游整体超时、流式空闲超时（环境变量） | 核心统一 | 一致 / 一致（`Execute` 总时限 6 小时 + 整体超时）/ 握手固定 30 秒，每轮受流式空闲超时约束 | `forwardBuilt`、`newIdleBody` |

### 2.3 调度、限流与保护

| 设置 | 归属 | 普通 / 执行 / WebSocket | 说明与位置 |
|---|---|---|---|
| 优先级、权重、调度策略、均衡负载 | 核心统一；插件可选实现 `RankAccounts` | 一致 / 一致 / 首轮调度，后续轮复用连接 | `dispatch.go` |
| 粘性会话 | 核心统一（规则按协议匹配，内置规则不设模型和 UA） | 一致 / 一致 / 首轮 | `stampSticky`；CONTRACTS §15.5 |
| 最大并发、RPM、TPM、会话数限制、Key 限额、用量限制 | 核心统一 | 一致 / 一致 / 每轮重新准入和占槽 | `acquireAccount`、`admitRate`、`admitKey` |
| CPU 保护 | 核心之前，由节点网关处理 | 与路径无关 | `next/gateway` |
| 提示词审核 | 审核插件以钩子实现，平台插件不用做什么 | 一致 / 一致 / 每轮 | `runHooks`、`runAttemptHooks` |

### 2.4 故障策略与订阅额度

| 设置 | 归属 | 普通 / 执行 / WebSocket | 说明与位置 |
|---|---|---|---|
| 故障策略、冷却、自动禁用 | 核心统一；**插件配合**：实现 `ClassifyError`，故障策略下核心只采用其中的额度观测和限时禁用 | 一致 / 一致（**0.1.301 起**，此前故障策略下插件执行的 HTTP 错误不调用插件分类，codex 的“禁用到重置时刻”失效）/ 握手和流内错误事件都分类 | `applyClassification`、`execute.go` `forwardCallback`；CONTRACTS §69 |
| 订阅额度窗口（5 小时、周） | **插件配合**：在账号类型上声明 `quota.headers` | 一致 / 一致 / 握手头和带头的错误事件 | `observeQuota`；CONTRACTS §44、§105 |

## 3. 现有平台插件

| 插件 | 路径 | 插件配合的部分 |
|---|---|---|
| anthropic、claude-oauth | 普通转发（gRPC，`BuildUpstreamRequest`） | claude-oauth 声明额度头 |
| openai、gemini、relay | 普通转发（只有清单）；openai 另有 WebSocket | 无，全部由清单和核心完成 |
| azure_openai（目录 `plugins/azure-openai`，市场） | 普通转发（只有清单），不提供 WebSocket | 无；清单里覆盖鉴权头（`api-key`）、路由和类型自己的 `passHeaders`，错误分类用预设的 `openai` 策略（[UPSTREAM-DECLARATIVE.md](UPSTREAM-DECLARATIVE.md) §6.1） |
| vertex（市场） | 普通转发（gRPC，`BuildUpstreamRequest`） | `ClassifyError`；凭证续期 `BuildRefreshRequest` / `ParseRefreshResponse`（CONTRACTS §48），第一枚令牌靠保存后续期（§48.3 第 5 条）；插件不联网 |
| bedrock（市场，核心 0.1.304 起；只服务 anthropic 平台的 messages，OpenAI 协议的请求由核心转换后走同一条路） | 普通转发（gRPC，`BuildUpstreamRequest`） | `ClassifyError`；构造结果带 `signing`（AK/SK、临时凭证）和 `response_stream`（流式 InvokeModel），见第 6 节；插件不联网，只自己签账号测试请求 |
| codex | 插件执行 + WebSocket | `ClassifyError`、额度头、`streamOnly`；`session_id` 由插件按 `prompt_cache_key` 派生（[PLUGIN-CODEX.md](PLUGIN-CODEX.md) §16） |
| volcengine | 插件执行（含异步任务） | `ClassifyError`、`EstimateUsage`（视频任务）、`ReserveAndWatch` |

## 4. 新插件自查

- 用量规则：流式规则把输出数映射在指名的终止事件上；非流式规则映射响应体。
- 走插件执行时用 SDK 的 `ExecuteDefault`，或在自己的 `Execute` 里同样经 `ForwardUpstream` 发请求、经 `RecordUsage` 记用量，不要自己连上游（插件的对外连接本来也要经核心转发）。
- 需要账号被自动禁用到某个时刻时实现 `ClassifyError`，不要在插件里自己改账号状态。
- 不要在插件里实现限流、并发、粘性、价格、断开计费这类核心已经统一处理的逻辑。
- 后台逻辑需要系统设置时用 `pluginsdk.Settings` 在用到的地方读（第 5 节），不要在 `Init` 里读一次存起来，也不要另设一份插件设置。
- 改了上表涉及的行为，同步本文、CONTRACTS 对应章节，并在三条路径上补测试（参考 `gateway/disconnect_test.go` 的 `TestClientDisconnectExecutedRequest`、`execute_test.go`）。

## 5. 读取系统设置与变更通知（核心 0.1.301 起）

插件可以只读地读取下表的系统设置分组，并在管理员保存后立即得知（CONTRACTS §107）：

| 分组 | SDK 类型 | 典型用途 |
|---|---|---|
| `billing` | `protocol.BillingSettings` | 自己结算或预估费用的插件按同样的最低余额、缺价策略、断开计费策略处理 |
| `gateway` | `protocol.GatewaySettings` | 自己调上游的后台任务参照重试次数、超时、媒体下载上限 |
| `scheduling` | `protocol.SchedulingSettings` | 排序插件（`RankAccounts`）参照均衡负载的指标 |
| `sticky` | `protocol.StickySettings` | 粘性会话关闭时放弃排队的绑定清理（dynweight 0.5.11） |
| `auto_disable`、`failure_policy` | `protocol.AutoDisableSettings`、`protocol.FailurePolicySettings` | 判断核心会不会自动禁用、冷却账号，避免插件重复处理；故障策略的开关按 `schema_version` 解读 |
| `environment` | `protocol.EnvironmentSettings` | 全局层的环境变量；分组层、账号层用 `GetEnvironment` |

```go
sticky, err := pluginsdk.Settings[protocol.StickySettings](ctx, p.host)
if err != nil {
	// 从未读成功过且宿主读不到：按插件自己的安全默认继续
}
if !sticky.Enabled { ... }
```

- 不用授权，不用在清单里声明能力；只读，不能改设置。
- SDK 在进程里缓存每个分组，核心在任一节点保存设置后会通知每个节点上的插件进程（`PluginService.SettingsChanged`，SDK 已实现），缓存随即作废；另外最多缓存 10 秒。缓存失效时并发调用只读宿主一次；读取失败时返回上次读成功的值，2 秒内不再读宿主，从未读成功过才返回错误（宿主的读取失败一律是 `UNAVAILABLE`）。所以在用到的地方直接调用即可，热路径（如 `RankAccounts`）也一样：命中时不访问宿主，只解码一次 JSON。
- 值与核心实际使用的一致（默认值、范围收敛都已算好），字段说明见 CONTRACTS §107.2。故障策略只给开关和 `schema_version`，规则不开放。
- 需要在变更后做一次性的工作（重算、通知）时，另外订阅 `settings.changed` 事件（`app.events.v1`，payload `{"sections": [...]}`），每个插件由一个节点接收，至少投递一次（按事件 id 幂等）。
- 用到这些接口的插件把清单的 `hostCompat` 下限提到 `>=0.1.301`。
- 测试：`pluginsdktest.FakeHost.SetSettings(v)` 设置分组（没设置的分组回 `UNAVAILABLE`），`Harness.SettingsChanged(sections...)` 模拟变更通知；读取失败后 SDK 2 秒内不再读宿主，测试里补设分组后要调一次 `Harness.SettingsChanged`。

## 6. 请求签名与响应流格式（`BuildUpstreamRequestResponse.signing`、`.response_stream`；CONTRACTS §109）

上游要对整个请求体签名，或者流式响应不是 SSE 时，插件不用自己发请求，在 `BuildUpstreamRequest` 的结果里说明，由核心执行。三条路径上的情况：

| 字段 | 普通转发 / 插件执行 / WebSocket | 插件要配合什么 |
|---|---|---|
| `signing`（`RequestSigning`，方案名如 `aws-sigv4`） | 一致 / 一致（`ForwardUpstream` 经同一段 `forwardBuilt`）/ **不支持**：握手带了它就按插件不可用换号 | `url` 的路径按线上的样子编码好（`:` → `%3A`、段内 `/` → `%2F`），签名器只做二次转义；查询串会被核心排序重编码后再发；主机的默认端口会被去掉。每次尝试核心都重新签。密钥只放在这个字段里，不要写日志 |
| `response_stream`（格式名如 `aws-eventstream-chunk`） | 一致 / 一致 / **不支持**，同上 | 只在这一次请求的回答确实是该格式时设置（bedrock 只在流式 InvokeModel 上设）；回答的内容类型不是该格式时核心不处理。转成 SSE 之后按平台的流式用量规则计量，所以事件要是该平台规则认识的形状 |

- 核心不认识的方案名或格式名：这次尝试在发送前失败（插件不可用，换号），没有请求发出。
- 更早的核心会忽略这两个字段：请求不签名就发出去，二进制流原样转给客户端。用到它们的插件必须在 `hostCompat` 里要求带 §109 的核心（bedrock 是 `>=0.1.304`）。
- 账号测试请求（`BuildTestRequest`）核心原样发送、不签名：请求体是插件自己写的，插件调 `sdk/reqsign` 的 `Sign` 自己签（bedrock 的 `sign`）。
- 两个库都在 SDK 里、只用标准库：`sdk/reqsign`（签名，方案表）、`sdk/awseventstream`（`Decoder` 只管分帧；`NewChunkSSEReader` 是 chunk 事件到 SSE 的适配器；`AppendMessage` 给测试和模拟上游编码）。要支持新的方案或新的事件形状，在库里加一个名字，再在 `server/internal/gateway/upstream_ext.go` 的表里登记，不在网关里写厂商判断。
- 新增第一方插件时，把插件键加进 `server/internal/plugin/install/reserved.go` 的 `reservedKeys`（`TestReservedCoverRepo` 会检查）。

## 7. SDK 里的账号表单与请求体补丁辅助（2026-11-17）

自己实现 `ValidateCredentials`、`BuildUpstreamRequest` 的 gRPC 插件可以用下面两组辅助，不用各写一份（vertex、bedrock 在用）。

**账号表单：包 `sdk/pluginsdk/accountform`**。给字段是自己一套的账号类型用（云凭证、密钥文件）；`api_key` + `base_url` 的表单仍由 `apikey.Spec` 整个实现。

| 导出 | 用途 |
|---|---|
| `Object`（`map[string]any`），方法 `Str(k)`、`JSON()` | 解码后的 JSON 对象。数字保持原样（`json.Number`），重新编码不改动它们（令牌过期时间、大整数）。`Str` 返回去掉首尾空白的字符串值，没有或不是字符串时为空 |
| `Decode(raw) (Object, error)` | 解码一个 JSON 对象；空白和 `null` 得到空对象，对象后面还有内容报错 |
| `FieldError(field, code, msg)` | 一条字段错误（`*pluginv1.FieldError`） |
| `Invalid(e)` | 用一条字段错误拒绝表单的 `ValidateCredentials` 回答 |
| `DecodeForm(in) (*Form, *ValidateCredentialsResponse)`，`Form{Creds, Settings}` | 解出 `ValidateCredentials` 请求的凭证和设置。有一个不是 JSON 对象时第二个返回值是拒绝的回答，直接返回它 |
| `(*Form).Normalized()` | 接受表单的回答。校验过程可以直接改写 `Creds`、`Settings`（填默认值、归一化）；回答里只带被改过的那个对象的 JSON，没改的留空，核心保留提交的内容 |
| `DecodeAccount(acc) (creds, settings Object, err error)` | 解出已存账号的凭证和设置（构造请求时用）；不是 JSON 对象时回 `INVALID_ARGUMENT` |

```go
form, refused := accountform.DecodeForm(in)
if refused != nil {
	return refused, nil
}
if bad := check(form.Creds, form.Settings); bad != nil { // 插件自己的校验，可以顺手填默认值
	return accountform.Invalid(bad), nil
}
return form.Normalized(), nil
```

**请求体补丁：`pluginsdk.SetPatch(path, value)`、`pluginsdk.DeletePatch(path)`**（`sdk/pluginsdk/request.go`）。构造 `BuildUpstreamRequestResponse.Patches` 的元素（`*pluginv1.BodyPatch`）：`SetPatch` 把值编码成 JSON 写到路径上，编码不了时写 `null`；`DeletePatch` 删除路径，路径不存在时什么都不变。插件看不到整个请求体，只能按固定路径打补丁（要读的字段在清单的 `requestFields` 里声明）。
