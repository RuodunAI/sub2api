# 插件开发：平台插件

本文写给开发平台插件（声明账号类型、为网关请求服务的插件）的人：一次网关请求怎样经过插件、插件怎么写和怎么测，以及控制台“系统设置”和账号编辑器里的每一项设置在插件的请求上由谁生效、插件要配合什么。规则本身以 [CONTRACTS.md](CONTRACTS.md) 为准（转发的契约在 §112），本文只做说明、索引和对照；两者不一致时以 CONTRACTS 为准，并把本文改对。插件的打包、签名、权限、迁移、界面写法见 CONTRACTS §2、§23.4、§26、§36 和 [ARCHITECTURE.md](ARCHITECTURE.md) §5；设计的来龙去脉见 [DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md](DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md)。

**总规则（2026-11-17 用户定）：每个平台插件都必须遵守全部系统设置，用户在控制台改的设置对任何插件的账号都要生效。** 新增系统设置或插件能力时，要同时核对 HTTP 与 WebSocket 两种传输并更新第 5 节的表。

## 1. 一条请求路径

每个平台插件都是有进程的代码插件（`runtime: "grpc"`），都实现 `PlatformService.Execute`。没有只有清单、没有进程的插件，也没有第二条请求路径：核心里不替插件构造上游请求。按旧契约构建的插件包（插件协议 1、`runtime: "none"`、清单里有 `accountTypes[].upstream`、`requestFields`、`request.maxBodyBytes`，或声明了 `platform.execute.v1`、`platform.poll.v1`）在上传、安装和加载时以同一条信息被拒绝（CONTRACTS §112.10）。

一次尝试（一个账号一次，换号后从头再来）：

| 步 | 谁 | 做什么 |
|---|---|---|
| 1 | 核心 | 鉴权、限额、请求级钩子、价格、余额、预扣（可调插件的 `EstimateUsage`）、调度选号、并发与限流 |
| 2 | 核心 | 协议转换（`internal/gateway/convert`）、账号模型映射、引用模型映射、强制流式、`claude_max_tokens`、账号参数覆盖的请求体部分；模型可用性预检 |
| 3 | 核心 → 插件 | `Execute`：元信息、账号（含凭证）、**完整请求体**（上游协议，核心的改写都已做完）、白名单内的客户端请求头、尝试序号，以及两个标志 `extract_usage`、`task_submission` |
| 4 | 插件 | 构造最终的上游请求：方法、URL、请求头、请求体字节；需要时自己签名。可以先用 `Fetch` 发辅助请求（换令牌、上传、预检） |
| 5 | 插件 → 核心 | `Relay`（不等待）或 `RelayAndWait`（等待），把请求交给核心的 HTTP 客户端 |
| 6 | 插件 → 核心 | `Execute` 返回事实：`host_usage`、`usage`、`task` 或 `classification` |
| 7 | 核心 | 发送（账号代理、SSRF、请求头分层、超时、断开处理）、中继、读用量、结算；上游出错时调 `ClassifyError` 并按故障策略裁决 |

两种形式由请求上的两个标志决定：

| 端点 | 标志 | 插件调用 | 返回 | 核心何时发请求 |
|---|---|---|---|---|
| 用量由核心按清单规则读（绝大多数） | 都不置 | `Relay` | `host_usage` | `Execute` **返回之后**。插件在返回前出任何问题（报错、崩溃），请求都没发出，核心按插件不可用换号，账号不受罚。流式回答期间插件不在调用里，插件重启不影响在途的流 |
| 端点声明 `usageSource: "plugin"` | `extract_usage` | `RelayAndWait` | `usage`；读不出时 `host_usage`（核心记 `usage_extract=fallback`）；上游出错时 `classification` | 中继调用里立即发送；调用返回观察（2xx）或错误事实 |
| 任务提交（`endpoint.task.action: "submit"`） | `task_submission` | `RelayAndWait` | `task`；上游出错时 `classification` | 同上 |

- 等待形式下什么都没返回（或 `Execute` 报错、插件崩溃，或返回了 `host_usage` 而核心的读数完全为空）：同步请求记一条零费用的“未上报”记录（`error_type=usage_unreported`，预扣释放，ERROR 日志），已缓冲的非流式回答不发出、回 503，已开始的流异常结束；任务提交没有 `task` 时任务不登记、客户端报错、预扣释放（任务失败的路径，不是“未上报”）。等待期间插件崩溃或放弃调用，核心切断上游、这个请求不计费。核心不替插件补记用量。不等待形式下插件返回 `host_usage` 而核心的读数完全为空时，客户端已经拿到回答，记录同样是零费用的“未上报”（CONTRACTS §112.2）。
- WebSocket（`platform.websocket.v1`）：会话的每一轮消息调一次 `Execute`，请求体是这一轮的消息。插件照常 `Relay` + `host_usage`，给出握手地址（`ws(s)://` 或 `http(s)://`）、握手头和最终消息；核心在会话还没有上游连接时用第一轮的 URL 和握手头建连，以后每轮只发消息。WebSocket 端点没有等待形式，不能给 `response_stream`（CONTRACTS §83.6、[OPENAI-RESPONSES-WEBSOCKET.md](OPENAI-RESPONSES-WEBSOCKET.md)）。
- 插件只描述请求、不开套接字：上游请求、辅助请求都经核心的客户端发出，核心套上账号的代理、SSRF 规则、超时和运营方的请求头设置。插件不重试、不换号，一次执行最多一个中继。
- `Execute` 不计时：插件在 `Execute` 里自己的工作没有时限，等待形式的交换也没有总时限（所有者决定“超时、限制都去掉”，CONTRACTS §112.16）。挂住的插件由客户端离开、上游超时（管理员配置）或它的进程结束收场，期间这次尝试占着账号和 API Key 的槽位，所以插件要自己保证每一步都会结束、把 `ctx` 传到底。平台调用超时（网关设置 `platform_call_timeout_ms`）只管不等待形式下的 `ClassifyError` 和读断开策略（CONTRACTS §111.4）。

## 2. 写一个平台插件

### 2.1 接口

SDK 是 `sdk/pluginsdk`。`pluginsdk.Platform` 包含 `Execute`（`Executor`）、`ClassifyError`、`ValidateCredentials`、`BuildTestRequest`。大多数插件只写构造请求的部分：实现 `UpstreamBuilder`，让 `Execute` 是 `ExecuteDefault`：

```go
func (p *Plugin) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	return pluginsdk.ExecuteDefault(ctx, p, in)
}

func (p *Plugin) BuildUpstream(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	cfg, err := spec.FromAccount(in.GetAccount()) // apikey.Spec
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return pluginsdk.NewUpstream(http.MethodPost, cfg.BaseURL+"/v1/messages").
		Header("x-api-key", cfg.APIKey).                                           // authoritative: the credential
		DefaultHeader("anthropic-version", "2023-06-01").                          // default: the operator may override
		DefaultHeader("anthropic-beta", in.GetInboundHeaders()["anthropic-beta"]). // an empty value writes nothing
		Body(in.GetBody()).
		Model(pluginsdk.RequestModel(in)).
		Build()
}
```

`ExecuteDefault` 按两个标志选形式（第 1 节的表）：普通端点 `Relay` 后返回 `ReportHostUsage()`；`extract_usage` 时 `RelayAndWait` 后调插件的 `UsageExtractor`（没有实现、出错或返回空时回落到 `host_usage`）；`task_submission` 时 `RelayAndWait` 后调 `TaskSubmissionParser`（没有实现时在构造之前就报错）；等待形式的上游错误用插件的 `ClassifyError` 分类。`BuildUpstream` 返回 `pluginsdk.Classified(c)` 时（例如 `Fetch` 被上游拒绝），`ExecuteDefault` 回 `ReportClassification(c)`，核心按分类裁决而不是当成插件故障。

自己写 `Execute` 也可以，返回值用 `ReportHostUsage`、`ReportUsage`、`ReportTask`、`ReportClassification`。SDK 先拦住明显的错误并说明原因：第二次中继、没有标志却等待、`Monitor` 里中继、`host_usage` 却没有中继过、`usage` / `task` 却没有等待过、大写的头名和传输层的头。中继请求的方法和请求头由 `ValidateUpstreamRequest` 按宿主同一套规则（`httpguts`）判断，SDK 放行的宿主也放行。

可选接口：`UsageExtractor`、`TaskSubmissionParser`、`TaskMonitor`（`Monitor`）、`ModelLister`（`BuildModelsRequest`）、`ModelResolver`、`QuotaReader`、`CredentialRefresher`、`ModelAvailabilityChecker`、`UsageEstimator`（`EstimateUsage`；见 `interfaces.go`、`estimate.go`）。能力由 `Serve` 推断：`Platform` 就是 `platform.adapter.v1`，`TaskMonitor` 就是 `platform.monitor.v1`；`platform.tasks.v1`、`platform.websocket.v1` 在清单里声明，任务插件三个都要声明。

### 2.2 构造请求：`NewUpstream`

`pluginsdk.NewUpstream(method, url)` 返回构造器，`Build()` 得到 `*pluginv1.UpstreamRequest`，核心会拒绝的东西在 `Build` 时就报错（`INVALID_ARGUMENT`）：

- `method` 为空是 POST；URL 是绝对地址，路径写成线上的样子（每段已百分号编码），核心发送前会再解析一次。
- 头名一律小写（构造器替你转）；`content-length`、`accept-encoding`、`connection`、`transfer-encoding` 归传输层，不许出现；空值不写入；同名时权威头生效。
- `Body(b)` 是最终发出的字节，核心不再改；`Model(m)` 是记进使用记录的上游模型（类型声明了模型可用性时，核心核对它与自己算的上游模型一致，不一致按插件不可用换号）；`ResponseStream(f)` 见 2.5；`ResponseHeaderTimeout(d)`、`TotalTimeout(d)` 只能比账号的生效值更短。代理、TLS、重定向、重试不归插件。
- `pluginsdk.RequestModel(in)`：请求体的 `model`，没有则 `meta.model`（都已经过账号的模型映射）。`pluginsdk.RequestStream(in)`：`meta.stream` 或请求体的 `"stream": true` 任一为真。
- 请求体上限（网关设置 `max_request_body_mb`，CONTRACTS §111）只管网关读客户端的请求。核心交给插件的请求体可能因为自己的改写比它大，插件交回的中继请求体、取回的请求体也都不检查大小（CONTRACTS §112.16）。插件不设自己的大小上限。

核心不检查插件交回的请求体（不再比对模型、流式标志），这是插件的责任，由一致性套件（第 4 节）检查：模型引用与交入的一致，交入时流式为真交回仍为真，合法 JSON，没有重复键。

### 2.3 两类请求头

| | 权威头 `Header` / `Headers` / `HeaderMap` | 默认头 `DefaultHeader` / `DefaultHeaders` / `DefaultHeaderMap` |
|---|---|---|
| 写入时机 | 运营方的透传和覆盖**之后**，运营方改不掉 | 运营方设置**之前** |
| 运营方能做什么 | 不能改 | 客户端头透传**只补没有的**（不替换默认头）；请求头覆盖（`header_override`）和参数覆盖的请求头部分可以设值或删除 |

核心交给插件的客户端请求头（`inbound_headers`）已经按清单过滤过：账号类型在这个平台上写了 `passHeaders` 就用它，写成 `[]` 就一个都不交，没写才用平台的列表（`AccountPlatform.ClientHeaders`）。交来的头插件要作为默认头转发（一致性第 4 条），不想转发任何客户端头的类型在清单里写 `"passHeaders": []`（claude_oauth、codex 就这样）。

发送时自下而上：插件的默认头 → 透传（账号 `header_passthrough`、`header_override` 的透传键、粘性规则，只补没有的）→ `header_override` 与 `param_override` 的设值和删除 → 插件的权威头 → 没有 Content-Type 时补 `application/json`（CONTRACTS §86.5）。WebSocket 握手同样分层，但不套透传。

放哪一类：

- 鉴权头（`authorization`、`x-api-key`、`api-key` ……）和签名写入或覆盖的头放权威头；签名时用的 `content-type` 也算。
- 原来是声明式的五个类型（openai、gemini、relay、azure_openai、anthropic 的 `apikey`）改写后**只有鉴权头**是权威头，其余都是默认头，和以前的行为一样（`standard` 库就这样做）。
- 已经发布过的代码插件（claude_oauth、codex、volcengine）的头以前全都在运营方设置之上，改写后**保持全部权威**，运营方的透传和覆盖照旧改不了它们。没有发布过的 vertex、bedrock 按第一条写：只有凭证头（bedrock 的签名类型是签名头）是权威头，`content-type`、`accept` 是默认头。新插件按第一条写。
- 一致性套件第 4 条检查的是“声明的集合”：权威头的名字必须在插件声明的 `Credentials`（鉴权与签名头）与 `Authoritative`（其余要保留为权威的头，每个写理由）的并集里，`Credentials` 的头不能出现在默认头里。

签名的插件不要写 `Headers(req.Header)`（会把请求上每个头都变成权威头），用 `reqsign.SignedHeaders(req, scheme)` 只取签名写入或覆盖的那几个。

### 2.4 改请求体：`jsonedit`

所有插件用同一个工具 `sdk/pluginsdk/jsonedit`，不要各写一份，也不要整份 `json.Unmarshal` 再 `Marshal`（重排键、丢重复键、多拷贝一遍）或连串调用 `sjson.SetBytes` / `DeleteBytes`（每改一处整份拷贝一次，大请求体过不了一致性第 10 条）。打开请求体、记下改写、一次拼出结果：

```go
out, err := jsonedit.Edit(in.GetBody(), func(d *jsonedit.Doc) {
	for _, f := range []string{"metadata", "service_tier"} {
		d.Delete(f) // every occurrence; a missing field is no edit
	}
	d.Set("stream_options.include_usage", []byte("true")) // in place, or added at the end of its object
	d.Default("instructions", []byte(`""`))               // only where nothing is
	jsonedit.EachElement(d.Root().Get("messages"), func(m gjson.Result) {
		d.Object(m.Get("cache_control")).Delete("scope") // members of a nested object
		if role := m.Get("role"); role.Str == "system" {
			d.Replace(role, []byte(`"developer"`)) // a value read from the document
		}
	})
})
if err != nil {
	return nil, status.Error(codes.InvalidArgument, err.Error())
}
```

- 没有动到的字节原样保留（未知字段、键顺序、空白、数字写法、`\u` 转义）；什么都没改时返回原切片，不拷贝；改了就只分配一个最终大小的切片。
- 非法 JSON：`Parse` / `Edit` 返回 `jsonedit.ErrInvalid`；交给改写的值也必须是合法 JSON。
- 重复键：`Delete`、`DeleteFunc` 删掉每一个匹配的成员；`Set` 路径上的某个键在所在对象里出现两次是 `jsonedit.ErrDuplicateKey`，整批不出结果。改写之外的重复键不管（核心本来就以 400 拒绝带重复键的请求体）。
- 改写的字节重叠（删了一个成员又改它里面的值）是 `jsonedit.ErrConflict`。
- 同一个成员上的几次改写按调用顺序生效：先 `Set` 后 `Delete` 结果是删掉，先 `Delete` 后 `Set` 结果是设上。
- 路径是点分隔的普通键，没有下标和通配：数组元素先读（`EachElement`）再按值改（`Replace`、`Object`）。

### 2.5 签名与响应流格式

插件自己签名：对自己构造的 `*http.Request` 调 `sdk/reqsign` 的 `Sign`，再把签过的 URL 和签名头交给核心：

```go
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
req.Header.Set("Content-Type", "application/json")
if err := reqsign.Sign(req, body, directive, now); err != nil {
	return nil, err
}
return pluginsdk.NewUpstream(req.Method, req.URL.String()). // the URL as signed
	Headers(reqsign.SignedHeaders(req, directive.Scheme)).     // only what the signature wrote or covers
	DefaultHeader("accept", "application/vnd.amazon.eventstream").
	Body(body).ResponseStream(awseventstream.FormatChunk).Build()
```

- 请求体在签名之后不再改；签名头是权威头，运营方的覆盖改不了；`host` 在覆盖的禁止名单里。每次尝试重新构造、重新签，时间从可替换的时钟取（一致性第 6、7 条）。
- `aws-sigv4` 返回的权威头：`Authorization`、`X-Amz-Date`、有会话令牌时的 `X-Amz-Security-Token`、被签名的 `Content-Type`。签名由 AWS 官方的 `aws/signer/v4` 完成，包只决定签哪些头。
- `response_stream`：回答不是 SSE 或 JSON 时说明格式，核心解码后照常中继和计量。现有 `aws-eventstream-chunk`（`sdk/awseventstream.FormatChunk`）：内容类型是 `application/vnd.amazon.eventstream` 的 2xx 回答，chunk 事件解成 SSE，异常帧变成 Anthropic 形状的 `error` 事件；别的内容类型原样不动。不认识的名字、WebSocket 轮次上给了名字，核心都拒绝这次中继（按插件不可用换号）。只在这一次请求的回答确实是该格式时设置（bedrock 只在流式 InvokeModel 上设）。
- 账号测试请求（`BuildTestRequest`）核心原样发送，插件同样自己签（bedrock 的 `sign`）。
- 要支持新的签名方案或新的事件形状，在 `sdk/reqsign` 或 `sdk/awseventstream` 里加一个名字（响应流格式还要在 `server/internal/gateway/upstream_ext.go` 的表里登记），不在网关里写厂商判断。

不用厂商的 SDK 客户端（也不做 `RoundTripper` 适配）：插件和核心里只用 AWS 的签名与 event stream 库和 `golang-jwt`；OpenAI、Anthropic 的官方 SDK 只允许用作 e2e 测试的客户端。

### 2.6 取回：`Fetch`

`pluginsdk.Fetch(ctx, &pluginv1.ExecutionHTTPRequest{...})` 发一个非流式的请求，回答整份交还插件。用于 `Execute` 里的辅助请求和 `Monitor` 里的任务查询。

- 主机：清单 `externalServices` 之一或其子域，**或者是这次调用的账号自己的上游地址的主机**（账号类型 `upstreamFields` 声明的字段，从账号的设置和凭证里读，只比主机名，不含子域）。别的主机 `PERMISSION_DENIED`。规则是 `manifest.Manifest.FetchHostAllowed`，核心和假宿主共用。
- 核心经账号代理、过 SSRF（账号地址指向私网时按账号所在范围的“允许私网”判断）、不跟随重定向、不重试；**不套**运营方的透传和覆盖（它们描述的是客户端的请求）；拒绝 `Host`、hop-by-hop 和代理认证头。超时：`Execute` 里是账号的响应头超时和 `relay_timeout_seconds`（没设就没有整体超时），`Monitor` 里 20 秒；`Options` 只能收紧。
- 不限次数、不限大小：一次调用取回多少次都行，回答整份交还，核心不置 `truncated`（读不完整是传输错误）；URL、请求头、请求体也不限大小（CONTRACTS §112.16；`FetchLimit`、`FetchResponseLimit` 已删除）。没有了次数上限，循环取回的插件要自己保证会停。传输失败不是错误：回答的 `status` 为 0、`transport_error` 有值（文字固定，原因只进核心日志）。
- 不需要 `net` 授权。取回在主请求之前失败时可以安全换号：返回 `Classified(c)`（或自己 `ReportClassification(c)`）让核心按分类裁决。
- 插件自己的对外连接（授权流程换令牌、Webhook、SMTP 等，不是转发也不是取回）走出口隧道，需要 `net` 授权，还要通过管理员在“系统设置 → 插件联网”里的域名设置（默认放行，可以屏蔽某些域名，或改为只允许列出的域名，CONTRACTS §113）。被拒的拨号返回错误、在插件的外部访问日志里写明原因；插件要把这类错误当成配置问题报告，不要重试。取回和 `Relay` 不受这项设置影响。

### 2.7 插件自己解析用量的端点

清单端点声明 `usageSource: "plugin"`（规则表达不了用量时）。用 `ExecuteDefault` 的插件只要再实现 `UsageExtractor`：

```go
func (p *Plugin) ExtractUsage(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.UsageReport, error) {
	n := gjson.GetBytes(in.GetBody(), "usage.total_tokens")
	if !n.Exists() {
		return nil, errors.New("no usage in the response") // -> host_usage fallback
	}
	return &pluginv1.UsageReport{Tokens: &pluginv1.UsageTokens{OutputTokens: n.Int()}}, nil
}
```

观察：非流式是整个回答体，流式是 `usageStreamEvents` 列出的每一条事件，默认都不限大小；端点自己声明了 `usageMaxBytes`（不小于 1 KiB）时，非流式超过它就为空、流式超过它就停止收集并置 `truncated`。`usageRequestFields` 声明的请求字段整个交给插件（`fields_omitted` 不再填）。回答本身已经由核心中继给客户端。只有声明端点的插件自己的账号类型能服务这类端点：别的插件的账号类型声明了这样的平台，注册表绑定时把那个平台从它的平台里去掉（CONTRACTS §112.8）。WebSocket 端点不能声明 `usageSource: "plugin"`。

### 2.8 任务：提交与 `Monitor`

任务插件声明 `platform.adapter.v1`、`platform.monitor.v1`、`platform.tasks.v1`，实现 `TaskSubmissionParser` 和 `TaskMonitor`：

```go
func (p *Plugin) ParseTaskSubmission(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.TaskSubmission, error) {
	id := gjson.GetBytes(in.GetBody(), "id").String()
	if id == "" {
		return nil, status.Error(codes.Internal, "upstream accepted the task without an id")
	}
	return &pluginv1.TaskSubmission{UpstreamRefId: id, SnapshotJson: string(in.GetBody()), NextCheckAfterSec: 5}, nil
}

func (p *Plugin) Monitor(ctx context.Context, in *pluginv1.PollRequest) (*pluginv1.ReconcileResult, error) {
	answer, err := pluginsdk.Fetch(ctx, &pluginv1.ExecutionHTTPRequest{
		Url:     baseURL + "/tasks/" + in.GetEntry().GetRefId(),
		Headers: map[string]string{"authorization": "Bearer " + key(in.GetAccount())},
	})
	if err != nil {
		return nil, err
	}
	if early := pluginsdk.ClassifyPollResponse(answer.GetStatus(), answer.GetTransportError(), answer.GetTruncated()); early != nil {
		return early, nil // transport error, 404/410, non-2xx
	}
	return parseTaskState(answer.GetBody()), nil // PENDING or a terminal state
}
```

- 提交：`ExecuteDefault` 做 `RelayAndWait` → `ParseTaskSubmission` → `ReportTask(task)`。解析失败时返回错误：任务不登记，客户端报错，预扣释放。`task.usage.reserve` 是更准的第二次预估，核心登记任务时原子地替换请求前的预扣。插件拿不到任务 ID 和回执。
- 观察：核心认领到期任务后调 `Monitor`（租约、调度、原账号都归核心）。`Monitor` 只能 `Fetch`、不能中继；结果在返回值里，返回 `nil` 或报错算一次查询失败。终态只在这次调用里有一次完整、成功的 `Fetch`（拿到 HTTP 状态、没有传输错误）时才被接受。结果不限大小。`PollFailure`、`TaskNotFound` 构造对应的结果。

### 2.9 不要做的事

- 不开套接字、不用 `http.DefaultClient` 或厂商 SDK 客户端；对外只有 `Relay`、`RelayAndWait`、`Fetch`。
- 不重试、不换号、不按请求缓存构造结果（`attempt > 0` 时从头构造）；按账号缓存令牌可以。
- 构造里不直接 `time.Now()`、不遍历 map 拼字符串（确定性）；把 `ctx` 传给 `Fetch`、`Relay`，不要用 `context.Background()`（取消）。
- 不在插件里实现限流、并发、粘性、价格、断开计费、自动禁用这类核心统一处理的逻辑；要让账号禁用到某个时刻，实现 `ClassifyError`。
- 不另设请求体大小上限，不另设插件设置让运营方重填系统设置（用第 6 节的接口读）。

## 3. `standard` 库与厂商预设

“账号 = API Key + 基础地址，请求 = 基础地址上按上游协议选的路由，Key 在请求头或查询串里”这一类账号类型，用 `sdk/pluginsdk/standard` 实现全部平台接口：openai、gemini、relay、azure_openai 四个插件和 anthropic 的 `apikey` 类型都是这样写的。它是链接进插件自己进程的 Go 库，不是运行器，也没有清单字段：插件用 Go 写一个 `standard.Type`（通常从 `sdk/pluginsdk/providers/{openai,anthropic,gemini}` 的预设 `APIKey(id)` 出发再改几处），交给 `standard.New`，得到的 `*standard.Platform` 实现 `Platform`、`UpstreamBuilder`、`ModelLister`，`Execute` 就是 `ExecuteDefault`。接口、构造步骤和三个风格的完整例子见 [`sdk/pluginsdk/standard/README.md`](../sdk/pluginsdk/standard/README.md)。

旧的清单 `upstream` 声明（已删除）的每一项在库里的对应：

| 原清单写法 | `standard.Type` |
|---|---|
| `preset` | 从 `openai.APIKey`、`anthropic.APIKey`、`gemini.APIKey` 出发 |
| `baseURL`（`default`、`required`、`stripSuffixes`）、`credentials.keyValidator`、`credentials.texts` | `Spec`（`apikey.Spec`：`DefaultBaseURL`、`RequireBaseURL`、`StripSuffixes`、`KeyValidator`、`Texts`） |
| `auth`（header / none） | `Auth{Header, Prefix}`；`Header` 空 = 不带凭证。鉴权头是唯一的权威头。查询串里的 Key（`Auth.Query`）已删除：没有使用者 |
| `headers` 的 `set` / `default` / `append` | `Headers []Header`：`standard.Set`、`standard.Default`、`standard.AppendTo`；值是 `standard.Fixed`、`standard.Field(账号字段)` 或自己的函数，空串不写 |
| 模板 `{credentials.<k>}` | `standard.Field("k")`（先凭证后设置）；`Request.APIKey`、`Request.BaseURL` |
| `routes`（`path`、`transport`、`model`、路由 `headers`、`patches`） | `Routes map[string]Route`：`Path`（函数，路径已转义）、`WebSocket`、`Model{FromMeta, TrimPrefix, Required, Missing}`、`Headers`、`Edit func(*jsonedit.Doc, *Request)`（在删掉 `FilterFields` 的同一次 `jsonedit` 里接着改）。HTTP 路由一律 POST、WebSocket 路由一律 GET（`Route.Method` 已删除） |
| `defaultProtocol` | `DefaultProtocol` |
| `filterFields` | `FilterFields`（每个请求体删掉的路径）；预设不带，要过滤的类型自己写 |
| `paramOverride`（清单里的固定改写） | 没有对应：写进 `Edit`。三个预设和五份清单里都没有用过它 |
| `classify` | `Classify`（`classify.Policy`） |
| `test`、`models` | `Test`、`Models`（nil 回 `UNIMPLEMENTED`） |
| 类型的 `passHeaders` | 没有对应：只在清单里写。核心按清单过滤后才交给插件，`standard` 把交来的每个客户端头（小写、去空白、空值不写）都作为默认头写出（`Type.PassHeaders`、`PlatformPassHeaders` 已删除） |

两个例子：

- **azure_openai**（`plugins/azure-openai/internal/azureopenai`）：两个类型都从 openai 预设出发，改了这几处：`Spec` 去掉默认地址、必填、去掉 `/openai/v1` 和 `/openai` 后缀、不校验 Key 前缀；`Auth{Header: "api-key"}`；路由逐条给出（v1 类型 `/openai/v1/...`，经典类型 `/openai/deployments/<部署名>/...?api-version=<设置>`，模型必填）；`Models = nil`；自己写过滤字段 `service_tier`、`safety_identifier`（预设不带）。客户端请求头的名单在清单里：openai 平台的列表去掉 `openai-organization`、`openai-project`。两个类型是因为路由不能按账号设置分支。验收断言在插件自己的 `platform_test.go`、`manifest_test.go`（SDK 里原来那份抄写的测试已删除）。
- **openai** 的组织与项目：`standard.Set("openai-organization", standard.Field("organization"))`、`project` 同样。账号没填时值为空、不写，客户端经透传带来的头保持原样；填了就以默认头覆盖客户端的（运营方的请求头覆盖仍可改它）。

**顺序变化（与改写前不同的一处）**：过滤字段和路由改写现在在插件里，排在核心的改写（`claude_max_tokens`、账号参数覆盖）**之后**；以前声明式类型的过滤字段在核心里、排在账号参数覆盖之前。运营方在账号的参数覆盖里写了一个被过滤的字段时，以前发给上游，现在被插件删掉。目前只有 azure_openai 的两个类型写了过滤字段（`service_tier`、`safety_identifier`），所以只在 azure_openai 上可见（CONTRACTS §112.14）。

## 4. 测试

**假宿主**（`sdk/pluginsdk/pluginsdktest`）：

```go
h := pluginsdktest.Start(t, New(), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestJSON)}})
ex := h.Execute(ctx, &pluginv1.ExecuteRequest{
	Meta:    &pluginv1.RequestMeta{Protocol: "anthropic.messages", Model: "claude-x", Stream: true},
	Account: &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-test"}`},
	Body:    []byte(`{"model":"claude-x","stream":true}`),
})
relay := ex.Relayed(t) // exactly one relay, the call succeeded
if relay.Wait || !proto.Equal(ex.Response, pluginsdk.ReportHostUsage()) { ... }
```

- `Harness.Execute` / `Harness.Monitor` 像核心一样发执行令牌、调用、调用返回后作废令牌；返回 `*Execution`（`Response`、`MonitorResponse`、`Err`、`Relays()`、`RelayErrors()`、`Fetches()`、`Relayed(t)`）。
- `h.Host.RelayFunc` 编排等待形式的回答（`pluginsdktest.Observed(200, body)`、`pluginsdktest.UpstreamFailed(429, body, "")`）；`h.Host.ExecuteHTTPFunc` 编排取回；`h.Host.Manifest` 是宿主知道的清单（`Start` 时取 `WithManifest`），取回能到哪些主机按它和这次调用的账号决定（`manifest.FetchHostAllowed`），没有清单时只有 `h.Host.ExternalServices` 放行的主机；`h.Host.ExpireExecutions()` 模拟核心中途放弃调用。假宿主自己执行核心的检查（令牌、一个中继、没有标志不许等待、`Monitor` 不许中继、方法和头名按 `httpguts`、HTTP 协议上不许 ws(s) 地址、`Monitor` 的终态要有一次完整成功的取回）；和宿主一样不限请求体、不数取回、不截回答。不经 `Harness` 时用 `h.Host.Begin(in)` / `BeginMonitor(in)` 发令牌。仍与宿主不同的地方写在 `FakeHost` 的注释里。
- 只测构造时直接调 `p.BuildUpstream(ctx, in)` 比较结果。

**一致性套件**（`sdk/pluginsdk/pluginsdktest/conformance`）：平台插件在自己的测试里调一次 `conformance.Run(t, plugin, cases...)`，逐条检查 11 条规则：恰好一次中继、形式与返回种类、交回的请求体、请求头（声明的集合）、不重试、确定性、换号、不开套接字、取消、大请求体（额外分配不超过请求体的 4 倍）、WebSocket。输入表、例外的写法和套件证明不了的事见 [它的 README](../sdk/pluginsdk/pluginsdktest/conformance/README.md)。十个平台插件都调用它（`Plugin.Manifest` 必填，交给插件的客户端请求头从清单读）。每个 HTTP 协议要在核心会发的每种形式下都有用例，形式由套件从协议的端点读出，插件不能声明自己不服务某种形式；WebSocket 协议不列在 `Protocols` 里，它的轮次只归第 11 条。只有 bedrock 声明了例外，都是 InvokeModel 本身的性质：流式用例的 `03_body`（InvokeModel 按 URL `invoke-with-response-stream` 选流式，请求体里带 `stream` 会被拒绝）和全部用例的 `04_headers`（客户端的 `anthropic-beta` 放进请求体的 `anthropic_beta`，Bedrock 不收这个请求头）。

**黄金样例**（`sdk/pluginsdk/standard/goldentest`）：原声明式引擎的 1498 条录制样例（openai 441、anthropic 343、gemini 371、relay 343）转换成不依赖引擎的 `execute`、`test`、`models`、`validate`、`classify` 样例并冻结，用 `standardtest.Run(t, plugin, p)` 回放（它像核心一样先按文件的 `pass_headers` 过滤样例里故意多给的客户端请求头）。用 `standard` 写的插件都要全部通过。结构和例外见 [它的 README](../sdk/pluginsdk/standard/goldentest/README.md)。

## 5. 设置对照表

“核心统一”表示插件什么都不用做；“插件配合”写明插件要实现或声明什么。表里说的都是网关请求上的生效方式；插件在请求之外（定时任务、事件处理、自己的 HTTP 路由）要按这些设置行事时，用第 6 节的接口读取当前值，不要让运营方在插件设置里再填一遍。“等待形式”指第 1 节表里的后两行。

### 5.1 计费（系统设置 → 计费）

| 设置 | 归属 | HTTP / WebSocket | 说明与位置 |
|---|---|---|---|
| 模型没有配置价格时、最低余额、高额费用警告 | 核心统一 | 一致 / 每轮检查 | `pipeline.go` `prepareBilling` |
| 请求预扣 Token 数、输出预留 | 核心统一；**插件配合**：可实现 `EstimateUsage` 给出更准的估算，返回 `Unimplemented` 时核心用本地 tokenizer（`task.submit` 端点必须实现） | 一致 / 每轮 | `precharge.go`，CONTRACTS §25.8 |
| 客户端中途断开时（读完再计费 / 按预扣计费 / 不计费） | 核心统一 | 一致（等待形式同样） / 不续读，按预扣（契约规定） | `disconnect.go`；CONTRACTS §25.8。终止用量事件送达客户端之后才断开的请求按成功计费（三种策略都一样）；等待形式下插件放弃中继调用（被终止、进程退出）记为上游中断，不算客户端断开 |
| 用量来源 | **插件配合**：在端点或账号类型上声明 `usage` 规则；规则表达不了时声明 `usageSource: "plugin"` 并实现 `UsageExtractor`（第 2.7 节） | 一致 / WebSocket 不支持 `usageSource: "plugin"`（清单检查拒绝） | `forward.go`、`waited.go`、`sdk/manifest/check/websocket.go` |

流式用量规则的写法会影响断开判定：**输出 token 数要映射在一个指名的终止事件上**（例如 Anthropic 的 `message_delta`、Responses 的 `response.completed`），核心据此判断“最终用量已经送达客户端”（`usagerules.Acc.Final`）。映射在任意事件（`"event": ""`）上的规则不会被当成终止，只能以读到流结束为准。

### 5.2 网关设置与账号的请求改写

| 设置 | 归属 | HTTP / WebSocket | 说明与位置 |
|---|---|---|---|
| 协议转换、账号参数覆盖（请求体）、Claude 默认 max_tokens 内置覆盖、强制流式 | 核心统一，在交给插件**之前**执行；插件拿到的是改写完的请求体 | 一致 / 每轮 | `upstreamBody`、`rewriteBody`；CONTRACTS §84、§84.1、§88。插件在其后的改写（过滤字段、厂商清洗）可以改掉覆盖的结果，见第 3 节的顺序变化 |
| 账号参数覆盖（请求头）、请求头覆盖 `header_override`、请求头透传 | 核心统一；**插件配合**：按 2.3 把头分成权威头和默认头 | 默认头可被覆盖设值或删除，透传只补没有的；权威头不受影响 / 握手只套覆盖，透传不生效（有意如此，`websocket.go` `dial`）；请求头覆盖与首条消息的改写分开解析，改写失败时握手仍带它，已存的值编译不过时换号 | `upstreamRequest`；CONTRACTS §84、§86.5、§108 |
| 上游只接受流式（`streamOnly`） | **插件配合**：在账号类型路由上声明；核心在交给插件前把流式写进请求体，插件交回时保持 | 一致 / 不适用 | `streamonly.go`，CONTRACTS §83 |
| 请求体上限（网关设置 `max_request_body_mb`，默认 128 MB） | 核心统一，是唯一的请求大小上限，只管网关读客户端的请求；插件和清单都不能另设，核心与插件之间也不由它推出任何上限 | 一致 / 作为单条消息上限 | `bodyLimit`、`readBody`；CONTRACTS §111 |
| 平台调用超时（网关设置 `platform_call_timeout_ms`） | 核心统一，只管不等待形式的 `ClassifyError` 和读断开策略；`Execute` 不计时 | 一致 / 每轮的 `Execute` 同样不计时 | `gateway/settings.go` `platformTimeout`；CONTRACTS §111.4 |
| 上游整体超时、响应头超时、流式空闲超时（环境变量） | 核心统一；插件只能经 `UpstreamOptions` 收紧；`Fetch` 在 `Execute` 里用同样的响应头超时和整体超时 | 一致（`Execute` 没有另外的总时限）/ 握手没有固定时限（随客户端离开结束），每轮受流式空闲超时约束，不读的一方由心跳 ping 结束 | `upstreamTimeouts`、`newIdleBody`；CONTRACTS §112.16 |

### 5.3 调度、限流与保护

| 设置 | 归属 | HTTP / WebSocket | 说明与位置 |
|---|---|---|---|
| 优先级、权重、调度策略、均衡负载 | 核心统一；插件可选实现 `RankAccounts` | 一致 / 首轮调度，后续轮复用连接 | `dispatch.go` |
| 账号的协议绑定：本平台协议（`protocols`）、模型的协议限定与显式协议转换（`model_protocols`） | 核心统一，在调度时过滤账号，早于转换和插件的 `Execute`；插件什么都不用做，清单里也没有对应的声明 | 一致 / 首轮调度时判断；后续轮换成该账号在这个协议下不提供的模型返回 400，要新建连接 | `sched_trace.go` `ineligibleReason`；CONTRACTS §110。账号能配哪些协议来自账号类型的 `platforms[].endpoints`：声明过的平台的协议是本平台协议（默认开放），其他平台的协议要经转换、默认关闭 |
| 粘性会话 | 核心统一（规则按协议匹配，内置规则不设模型和 UA） | 一致 / 首轮 | `stampSticky`；CONTRACTS §15.5 |
| 最大并发、RPM、TPM、会话数限制、Key 限额、用量限制 | 核心统一；`Fetch` 算在同一次执行里，不另占 | 一致 / 每轮重新准入和占槽 | `acquireAccount`、`admitRate`、`admitKey` |
| 账号代理、SSRF | 核心统一：中继和取回都经账号代理，插件不能选也不能关 | 一致 / 握手经账号代理 | `send`、`fetch.go`、`ssrf.go` |
| CPU 保护 | 核心之前，由节点网关处理 | 与传输无关 | `next/gateway` |
| 提示词审核 | 审核插件以钩子实现，平台插件不用做什么 | 一致 / 每轮 | `runHooks`、`runAttemptHooks` |

### 5.4 故障策略与订阅额度

| 设置 | 归属 | HTTP / WebSocket | 说明与位置 |
|---|---|---|---|
| 故障策略、冷却、自动禁用 | 核心统一；**插件配合**：实现 `ClassifyError`，故障策略下核心只采用其中的额度观测和限时禁用；传输错误完全由策略决定 | 不等待形式由核心调 `ClassifyError`；等待形式由插件在返回值里给 `classification` / 握手和流内错误事件都分类 | `applyClassification`、`waited.go`；CONTRACTS §69 |
| 订阅额度窗口（5 小时、周） | **插件配合**：在账号类型上声明 `quota.headers` | 一致 / 握手头和带头的错误事件 | `observeQuota`；CONTRACTS §44、§105 |

## 6. 读取系统设置与变更通知（核心 0.1.301 起）

插件可以只读地读取下表的系统设置分组，并在管理员保存后立即得知（CONTRACTS §107）：

| 分组 | SDK 类型 | 典型用途 |
|---|---|---|
| `billing` | `protocol.BillingSettings` | 自己结算或预估费用的插件按同样的最低余额、缺价策略、断开计费策略处理 |
| `gateway` | `protocol.GatewaySettings` | 自己调上游的后台任务参照重试次数、超时、媒体下载上限、请求体上限 |
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
- 测试：`pluginsdktest.FakeHost.SetSettings(v)` 设置分组（没设置的分组回 `UNAVAILABLE`），`Harness.SettingsChanged(sections...)` 模拟变更通知；读取失败后 SDK 2 秒内不再读宿主，测试里补设分组后要调一次 `Harness.SettingsChanged`。

## 7. 现有平台插件

全部是 gRPC 代码插件，清单 `hostCompat` 下限 `>=0.1.304`。

| 插件 | 怎么构造请求 | 插件配合的部分 |
|---|---|---|
| openai、gemini、relay | `standard` 库加预设；openai 另有 WebSocket（`openai.responses_ws`） | 全部在库和预设里；鉴权头之外都是默认头 |
| anthropic | 在已有进程（模型目录路由）里嵌入 `standard` 的 `apikey` 类型 | 同上 |
| azure_openai（目录 `plugins/azure-openai`，市场） | `standard` 库，两个类型（v1 与按部署） | 同上；错误分类用 openai 预设的策略 |
| claude_oauth | 自己的 `BuildUpstream`；`messages` 发到 `/v1/messages`，`count_tokens` 发到 `/v1/messages/count_tokens`，两者都带 OAuth 的 `anthropic-beta` | `ClassifyError` 用 Anthropic 的策略（`providers/anthropic.Policy`，读 `error.type`），前面加一条：401 按“OAuth 令牌过期或无效”禁用；额度头；全部头保持权威；两个类型 `passHeaders: []`，不转发任何客户端头（与已发布的 0.2.5 相同） |
| vertex（市场） | 自己的 `BuildUpstream`；把核心交来的 `user-agent`、`x-goog-api-client` 作为默认头转发；没有 `externalServices`（不取回） | `ClassifyError`；凭证续期 `BuildRefreshRequest` / `ParseRefreshResponse`（CONTRACTS §48），第一枚令牌靠保存后续期（§48.3 第 5 条）；服务账号断言用 `golang-jwt/jwt/v5` 签名，密钥文件由插件自己解析（§109.6） |
| bedrock（市场；只服务 anthropic 平台的 messages，OpenAI 协议的请求由核心转换后走同一条路；带 CONTRACTS §110 的核心上这是跨平台转换，要在账号上为模型开启） | 自己的 `BuildUpstream`：把 Messages 请求装进 InvokeModel 信封，清洗 Bedrock 不接受的字段（顶层的 `model`、`stream`、`metadata`、`service_tier`、`inference_geo`、`speed`、`provider`，以及 `tools[].custom`、`cache_control.scope` 等），AK/SK 与临时凭证类型自己做 SigV4 签名，流式请求给 `response_stream`；没有 `externalServices` | `ClassifyError`；账号测试请求同样自己签；一致性套件的两处例外见第 4 节 |
| codex | 自己的 `BuildUpstream`，HTTP 与 WebSocket | `ClassifyError`、额度头、`streamOnly`；`session_id` 由插件按 `prompt_cache_key` 派生（[PLUGIN-CODEX.md](PLUGIN-CODEX.md) §16）；全部头保持权威；两个类型 `passHeaders: []`，插件自己写出它发的每个头 |
| volcengine | `ExecuteDefault`：普通端点不等待；两个视频提交端点是任务提交，等待并返回 `task`；任务观察用 `Monitor`（任务查询由核心从自己的快照回答，不调 `Execute`）。anthropic 平台只服务 `messages`（`endpoints: ["messages"]`），`count_tokens` 是核心的本地估算 | `ClassifyError`、`EstimateUsage`（视频任务）、`TaskSubmissionParser`、`TaskMonitor` |

## 8. 新插件自查

- 实现 `UpstreamBuilder` 并让 `Execute` 是 `ExecuteDefault`；API Key 加基础地址的类型直接用 `standard` 库。
- 权威头只放鉴权头和签名头，其余放默认头；改请求体只用 `jsonedit`。
- 用量规则：流式规则把输出数映射在指名的终止事件上；非流式规则映射响应体。规则表达不了时声明 `usageSource: "plugin"` 并实现 `UsageExtractor`。
- 账号类型声明的平台决定什么算“本平台协议”：只声明一个平台的类型，其他平台的客户端协议默认不调度到它的账号，管理员要在账号上按模型开启转换（CONTRACTS §110）。上游本身兼容多种协议的类型可以把这些平台都声明出来（volcengine 声明了 openai、anthropic、volcengine）。
- 辅助请求只用 `Fetch`，主机写进清单 `externalServices`（账号自己的上游地址不用写）。
- 测试里调一致性套件；用 `standard` 写的插件另跑黄金样例。
- 新增第一方插件时，把插件键加进 `server/internal/plugin/install/reserved.go` 的 `reservedKeys`（`TestReservedCoverRepo` 会检查）。
- 改了第 5 节表涉及的行为，同步本文和 CONTRACTS 对应章节，并在 HTTP 与 WebSocket 上补测试（参考 `gateway/disconnect_test.go`、`execute_test.go`）。

## 9. SDK 里的账号表单辅助

自己实现 `ValidateCredentials` 的插件可以用包 `sdk/pluginsdk/accountform`，不用各写一份（vertex、bedrock 在用）。给字段是自己一套的账号类型用（云凭证、密钥文件）；`api_key` + `base_url` 的表单仍由 `apikey.Spec` 整个实现。

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
