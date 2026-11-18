> 临时文件：W3（SDK）给十个插件改写包的迁移说明。文档包（W13）把内容并入 CONTRACTS 和 PLUGIN-DEVELOPMENT 后删除本文件。

# 转发 V2：平台插件怎么改（W3）

依据 `docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md` 和 `sdk/proto/CHANGES-forwarding-v2.md`。下文的符号不写包名时都在 `sdk/pluginsdk`。

一句话：`Platform` 接口现在包含 `Execute`。插件拿到完整请求体，自己构造最终的上游请求，调 `Relay`（或 `RelayAndWait`）交给核心，然后在 `Execute` 的返回值里说明用量归谁。没有回调，没有补丁，插件不开套接字、不设大小上限、不重试。

## 1. 旧接口 → 新接口

| 旧 | 新 |
|---|---|
| `Platform.BuildUpstreamRequest(ctx, *BuildUpstreamRequestRequest) (*BuildUpstreamRequestResponse, error)` | `UpstreamBuilder.BuildUpstream(ctx, *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error)`，再让 `Execute` 调 `ExecuteDefault(ctx, p, in)`。`Platform` 现在是 `Executor` + `ErrorClassifier` + `ValidateCredentials` + `BuildTestRequest` |
| `in.GetFields()["x"]`（声明的请求字段） | `in.GetBody()`：完整请求体，上游协议，核心的改写都已做完。用 `gjson` 读、SDK 的 `jsonedit` 改（见第 3 节“改请求体”）。不要整份 `json.Unmarshal` 再 `Marshal`（会重排键、丢重复键、多拷贝一遍），也不要连串调用 `sjson.SetBytes` / `DeleteBytes`（每改一处整份拷贝一次，大请求体过不了一致性第 10 条） |
| `in.GetMeta()`、`in.GetAccount()`、`in.GetInboundHeaders()`、`in.GetAttempt()` | 同名，直接在 `ExecuteRequest` 上（原来在 `in.GetRequest()` 里的那一层没有了） |
| `BuildUpstreamRequestResponse.Patches`、`SetPatch`、`DeletePatch` | 没有补丁。把改完的字节放进 `Upstream.Body(...)`，那就是最终发出的字节 |
| `BuildUpstreamRequestResponse.Headers`（一个 map） | 两个：`Upstream.Header`（权威头）和 `Upstream.DefaultHeader`（默认头），见第 2 节 |
| `BuildUpstreamRequestResponse.Signing` | 插件自己签：对自己构造的 `*http.Request` 调 `reqsign.Sign`，再 `NewUpstream(req.Method, req.URL.String()).Headers(reqsign.SignedHeaders(req, scheme))`：只有签名写入或覆盖的头是权威头，其余照常作默认头（见第 3 节的 bedrock 例子） |
| `BuildUpstreamRequestResponse.ResponseStream`、`UpstreamModel` | `Upstream.ResponseStream(...)`、`Upstream.Model(...)` |
| `RequestModel(*BuildUpstreamRequestRequest)` | `RequestModel(*pluginv1.ExecuteRequest)`：请求体里的 `model`，没有则 `meta.model`。新增 `RequestStream(in)`：`meta.stream` 或请求体的 `"stream": true` 任一为真就是流式（核心可能把强制流式写进请求体；请求体的 `false` 不推翻 `meta.stream`） |
| `ForwardUpstream(ctx, built)` | `Relay(ctx, request) error`（不等待）或 `RelayAndWait(ctx, request) (*pluginv1.RelayUpstreamResponse, error)`（等待） |
| `ForwardUpstreamResponse.ExtractUsage` / `.TaskSubmission` | `ExecuteRequest.GetExtractUsage()` / `.GetTaskSubmission()`：发请求之前就知道 |
| `RecordUsage(ctx, nil)` | `return ReportHostUsage(), nil` |
| `RecordUsage(ctx, report)` | `return ReportUsage(report), nil` |
| `ReserveAndWatch(ctx, task)` | `return ReportTask(task), nil` |
| 上游出错时返回 `&ExecuteResponse{Classification: c}` | `return ReportClassification(c), nil` |
| `ExecutionReceipt`、任务 ID | 没有：插件拿不到回执 |
| `UsageExtractor`、`TaskSubmissionParser` | 接口签名不变，但不再对应 RPC：`ExecuteDefault` 在插件进程里调它们 |
| `TaskMonitor.Monitor(ctx, *PollRequest) error` + `ReportTaskProgress` | `Monitor(ctx, *PollRequest) (*pluginv1.ReconcileResult, error)`：结果就是返回值。返回 `nil, nil` 算一次查询失败 |
| `Poller.Poll` | 函数体改名成 `Monitor` |
| `Reconciler`（`BuildReconcileRequest` / `ParseReconcileResponse`） | 删除。在 `Monitor` 里用 `Fetch` 查一次 |
| `ExecuteHTTP(ctx, req)` | `Fetch(ctx, req)`；现在 `Execute` 里也能用（换令牌、上传、预检），每次调用最多 `FetchLimit`（8）次，回答最多 `FetchResponseLimit`（8 MiB） |
| `PollFailure`、`TaskNotFound`、`ClassifyPollResponse` | 不变 |
| `WithMaxRecvMsgSize(n)` | 删除。每个插件的接收上限固定为 gRPC 的最大值，请求体上限由核心按系统设置检查 |
| 清单能力 `platform.execute.v1`、`platform.poll.v1` | 删掉。`platform.adapter.v1` 就表示实现了 `Execute`；任务插件声明 `platform.adapter.v1` + `platform.monitor.v1` + `platform.tasks.v1` |
| `Capabilities(p)` 按 `Executor`、`Poller`、`TaskSubmissionParser` 推断 | 只推断 `platform.adapter.v1`（`Platform`）和 `platform.monitor.v1`（`TaskMonitor`）。`platform.tasks.v1`、`platform.websocket.v1` 在清单声明时上报，并要求插件是 `Platform`（tasks 还要求 `TaskMonitor` 和 `platform.monitor.v1`） |
| 假宿主 `FakeHost.ForwardUpstreamFunc`、`RecordUsageFunc`、`ReserveAndWatchFunc`、`ReportTaskProgressFunc` | `FakeHost.RelayFunc`、`ExecuteHTTPFunc`，加 `Harness.Execute` / `Harness.Monitor`，见第 6 节 |

不变的：`ValidateCredentials`、`ClassifyError`、`BuildTestRequest`、`ModelLister`、`ModelResolver`、`QuotaReader`、`CredentialRefresher`、`ModelAvailabilityChecker`、`UsageEstimator`、`SendAccountRequest`，以及 `apikey`、`classify`、`accountform`、`egress`、`providers/*` 各包（W3 没有改它们一行）。

## 2. 两种请求头

| | 权威头 `Header` / `Headers` | 默认头 `DefaultHeader` / `DefaultHeaders` |
|---|---|---|
| 写入时机 | 运营方的透传和覆盖**之后**，运营方改不掉 | 运营方设置**之前**，透传的客户端头或覆盖可以替换、删除它 |
| 放什么 | 只放鉴权头（`authorization`、`x-api-key`、`api-key` ……）和被签名的头（签名时用的 `content-type` 也算） | 其余全部：`anthropic-version`、`anthropic-beta`、`accept`、`user-agent`、厂商的可选头 |

规则（核心会拒绝，SDK 在 `Build` 和 `Relay` 时先拒绝并说明原因）：

- 两个 map 的键都是小写。`Upstream` 的方法替你转小写（`Header`、`Headers(http.Header)`、`HeaderMap(map[string]string)`，默认头同样有 `DefaultHeader`、`DefaultHeaders`、`DefaultHeaderMap`）；手写 `pluginv1.UpstreamRequest` 时自己保证。`Build` 的校验错误带 gRPC 码 `INVALID_ARGUMENT`。
- `content-length`、`accept-encoding`、`connection`、`transfer-encoding` 归传输层，不许出现。
- 同名时权威头生效。空值不写入（方便写可选头）。
- 原来声明式类型的“声明的头”对应默认头，鉴权头对应权威头；原来代码插件的全部头都在运营方设置之上，改写时把非鉴权、非签名的头挪到默认头。

## 3. 普通端点：不等待，返回 host_usage

```go
type Plugin struct{}

// Execute is the whole request path. ExecuteDefault picks the relay form
// from the two flags of the request and fills the report.
func (p *Plugin) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	return pluginsdk.ExecuteDefault(ctx, p, in)
}

func (p *Plugin) BuildUpstream(_ context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	cfg, err := spec.FromAccount(in.GetAccount()) // apikey.Spec, as before
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// The body is final unless the vendor needs a change: edit it with
	// jsonedit, one pass whatever the number of edits.
	body, err := jsonedit.Edit(in.GetBody(), func(d *jsonedit.Doc) { d.Delete("metadata.internal") })
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return pluginsdk.NewUpstream(http.MethodPost, cfg.BaseURL+"/v1/messages").
		Header("x-api-key", cfg.APIKey).                                        // authoritative: the credential
		DefaultHeader("anthropic-version", "2023-06-01").                       // default: the operator may override
		DefaultHeader("anthropic-beta", in.GetInboundHeaders()["anthropic-beta"]). // empty value writes nothing
		DefaultHeader("content-type", "application/json").
		Body(body).
		Model(pluginsdk.RequestModel(in)).
		Build()
}
```

发生的事：`ExecuteDefault` 调 `BuildUpstream`，`Relay`（核心只校验、立即返回），返回 `ReportHostUsage()`；核心在 `Execute` 返回**之后**才发请求、中继、按清单规则读用量。上游出错时核心另外调 `ClassifyError`。`BuildUpstream` 返回错误时什么都没发出，核心换号。

自己写 `Execute`（不用 `ExecuteDefault`）也可以，等价于：

```go
if err := pluginsdk.Relay(ctx, request); err != nil {
	return nil, err
}
return pluginsdk.ReportHostUsage(), nil
```

签名的插件（bedrock）：

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

不要写 `Headers(req.Header)`：那会把请求上的**每个**头（转发的客户端头、`accept`、厂商的可选头）都变成权威头，运营方的透传和覆盖再也改不了它们。`reqsign.SignedHeaders` 按签名方案返回该当权威头的那几个（aws-sigv4：`Authorization`、`X-Amz-Date`、有会话令牌时的 `X-Amz-Security-Token`、被签名的 `Content-Type`），插件不用自己维护这张表。

URL 的路径要写成线上的样子（每段已经百分号编码），核心发送前会再解析一次。

### 改请求体：`jsonedit`

所有插件用同一个工具 `sdk/pluginsdk/jsonedit`，不要各写一份。打开请求体、记下改写、一次拼出结果：

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

- 没有动到的字节原样保留（未知字段、键顺序、空白、数字写法、`\u` 转义）；什么都没改时返回原切片，不拷贝；改了就只分配一个最终大小的切片（8 MiB 请求体实测约 1.2 倍）。
- 非法 JSON：`Parse` / `Edit` 返回 `jsonedit.ErrInvalid`；交给改写的值也必须是合法 JSON。
- 重复键：`Delete`、`DeleteFunc` 删掉每一个匹配的成员；`Set` 要改的键（路径上的每一级）在所在对象里出现两次就是 `jsonedit.ErrDuplicateKey`，整批不出结果。改写之外的重复键不管。核心本来就拒绝带重复键的请求体，这条规则只保证改写不去猜。
- 改写的字节重叠（删了一个成员又改它里面的值）是 `jsonedit.ErrConflict`。
- 路径是点分隔的普通键，没有下标和通配：数组元素先读（`EachElement`）再按值改（`Replace`、`Object`）。

辅助请求（换令牌等）在 `BuildUpstream` 里用 `Fetch`；上游拒绝时想让核心按分类处理而不是当成插件故障，返回 `pluginsdk.Classified(c)`，`ExecuteDefault` 会把它变成 `ReportClassification(c)`。

可选的超时只能收紧：`Upstream.ResponseHeaderTimeout(d)`、`Upstream.TotalTimeout(d)`；`Fetch` 的请求里是 `Options`。代理、TLS、重定向、重试不归插件。

WebSocket 端点：每一轮消息一次 `Execute`，`in.GetBody()` 是这一轮的消息，`in.GetMeta()` 的传输是 websocket。照常 `Relay` + `ReportHostUsage()`：URL 是握手地址（`ws(s)://` 或 `http(s)://`），头是握手头，`Body` 是最终消息。不能给 `ResponseStream`，不能用等待形式。

## 4. 插件自己解析用量的端点：等待，返回 usage

端点声明了 `usageSource: "plugin"` 时核心把 `ExecuteRequest.extract_usage` 置真。用 `ExecuteDefault` 的插件只要再实现 `UsageExtractor`：

```go
func (p *Plugin) ExtractUsage(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.UsageReport, error) {
	n := gjson.GetBytes(in.GetBody(), "usage.total_tokens")
	if !n.Exists() {
		return nil, errors.New("no usage in the response") // -> host_usage fallback
	}
	return &pluginv1.UsageReport{Tokens: &pluginv1.UsageTokens{OutputTokens: n.Int()}}, nil
}
```

`ExecuteDefault` 做的事，手写等价于：

```go
observed, err := pluginsdk.RelayAndWait(ctx, request) // the host sends and relays now
if err != nil {
	return nil, err
}
if failed := observed.GetError(); failed != nil { // non-2xx or transport error
	c, _ := p.ClassifyError(ctx, failed)
	return pluginsdk.ReportClassification(c), nil
}
report, err := p.ExtractUsage(ctx, observed.GetObservation())
if err != nil || report == nil {
	return pluginsdk.ReportHostUsage(), nil // documented fallback, recorded by the host as usage_extract=fallback
}
return pluginsdk.ReportUsage(report), nil
```

要点：

- 等待形式只允许用在 `extract_usage` 或 `task_submission` 为真的执行上，否则 SDK 直接报 `FAILED_PRECONDITION`。
- 等待之后必须返回 `usage`、`host_usage`、`task` 或 `classification` 之一。什么都不返回（或 `Execute` 报错）就是“未上报”：不计费，核心不兜底。
- 观察是有界的：非流式是整个回答体（超过 `usageMaxBytes` 时为空），流式只有 `usageStreamEvents` 列出的事件。回答本身已经由核心中继给客户端。

## 5. 任务提交与监控

提交端点（`endpoint.task.action: "submit"`）核心置 `task_submission`。实现 `TaskSubmissionParser` 和 `TaskMonitor`：

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
		return early, nil // transport error, truncated, 404/410, non-2xx
	}
	return parseTaskState(answer.GetBody()), nil // PENDING or a terminal state
}
```

- `ExecuteDefault` 在任务提交上：`RelayAndWait` → `ParseTaskSubmission` → `ReportTask(task)`。没有 `TaskSubmissionParser` 时在构造和发送**之前**就报错。解析失败时返回错误（任务不登记，客户端报错），没有 host_usage 兜底。
- `Monitor` 不能中继，只能 `Fetch`；`Fetch` 的主机必须在清单 `externalServices` 内。终态只在这次调用里有一次完整、成功的 `Fetch` 时才被核心接受，所以截断的回答要当成查询失败（`ClassifyPollResponse` 已经这样做）。
- 清单：`platform.adapter.v1`、`platform.monitor.v1`、`platform.tasks.v1` 三个都要声明，否则 `Serve` 启动时报错。

## 6. 用假宿主做单元测试

```go
func TestExecute(t *testing.T) {
	h := pluginsdktest.Start(t, New(), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestJSON)}})

	ex := h.Execute(ctx, &pluginv1.ExecuteRequest{
		Meta:           &pluginv1.RequestMeta{Protocol: "anthropic.messages", Model: "claude-x", Stream: true},
		Account:        &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-test"}`},
		Body:           []byte(`{"model":"claude-x","stream":true}`),
		InboundHeaders: map[string]string{"anthropic-beta": "x"},
	})
	relay := ex.Relayed(t) // fails unless the call succeeded and relayed exactly once
	if relay.Wait || !proto.Equal(ex.Response, pluginsdk.ReportHostUsage()) {
		t.Fatalf("form or report: %v %v", relay.Wait, ex.Response)
	}
	// relay.Request is the *pluginv1.UpstreamRequest the host accepted.
}
```

- `Harness.Execute(ctx, in)` / `Harness.Monitor(ctx, in)`：像核心一样发执行令牌、调用、调用返回后作废令牌；返回 `*Execution`，不会让测试失败，所以也能断言错误。
- `Execution`：`Response` / `MonitorResponse` / `Err`；`Relays()`（被接受的中继，含 `Wait`）；`RelayErrors()`（核心回给中继的错误）；`Fetches()`；`Relayed(t)`（断言恰好一次中继）；`Token()`。
- 编排上游：
  - `h.Host.RelayFunc`：等待的中继返回什么。`pluginsdktest.Observed(200, body)` 是 2xx 的观察（要流式事件、响应头或 `Truncated` 就在返回的 `Observation` 上设；`Meta` 和不带凭证的 `Account` 由假宿主从请求补上）；`pluginsdktest.UpstreamFailed(429, body, "")` 或 `UpstreamFailed(0, nil, "connection reset")` 是失败的交换。返回 `INVALID_ARGUMENT` 的错误表示核心拒绝了请求（不算已中继），其他错误表示中继调用失败。不设时不等待的中继照常被接受，等待的中继回 `UNIMPLEMENTED`。
  - `h.Host.ExecuteHTTPFunc`：`Fetch` 的回答。超过 8 MiB 的回答体被截断并置 `Truncated`；也可以自己设 `Truncated`、`TransportError`。
  - `h.Host.ExternalServices`：设了就只允许这些主机（及子域）；`h.Host.MaxBodyBytes`：请求体上限。
  - `h.Host.ExpireExecutions()`：在 `RelayFunc` / `ExecuteHTTPFunc` 里调用，模拟核心中途放弃这次调用（之后的中继和取回都是 `PERMISSION_DENIED`）。
- 假宿主自己执行核心的检查，与 SDK 是否检查过无关：令牌必须是在途调用的；一次执行一个中继；没有标志不许等待；`Monitor` 不许中继；请求头小写且不含传输层的头；取回最多 8 次。
- 旧测试里直接 `h.Platform.Execute(...)` / `h.Platform.Monitor(...)` 并自带令牌的写法不再可用（假宿主不认识那个令牌），改成 `h.Execute` / `h.Monitor`。
- 只想测构造、不想起进程时，直接调 `p.BuildUpstream(ctx, in)` 比较返回的 `*pluginv1.UpstreamRequest` 即可；它是纯函数时不需要假宿主。

### 接黄金样例（`sdk/pluginsdk/standard/goldentest`，W2/W4）

`goldentest.Subject.Execute` 的签名是 `func(ctx, *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error)`，字段与 `pluginv1.ExecuteRequest` / `pluginv1.UpstreamRequest` 一一对应，适配只有两次字段拷贝：

```go
func goldenExecute(p pluginsdk.UpstreamBuilder) func(context.Context, *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
	return func(ctx context.Context, in *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
		built, err := p.BuildUpstream(ctx, &pluginv1.ExecuteRequest{
			Account: in.Account.Proto(), Meta: in.Meta.Proto(),
			InboundHeaders: in.ClientHeaders, Body: []byte(in.Body),
		})
		if err != nil {
			return nil, err
		}
		return &goldentest.UpstreamRequest{
			Method: built.GetMethod(), URL: built.GetUrl(),
			Headers: built.GetHeaders(), DefaultHeaders: built.GetDefaultHeaders(),
			Body: string(built.GetBody()), UpstreamModel: built.GetUpstreamModel(),
		}, nil
	}
}
```

想连同“恰好一次中继、不等待、返回 host_usage”一起验，就把 `p.BuildUpstream(...)` 换成 `h.Execute(ctx, req)`，取 `ex.Relayed(t).Request`（`ex.Err` 原样返回）。注意黄金样例的 `method` 是写明的：`NewUpstream` 的第一个参数传 `http.MethodPost`，不要留空。`Subject` 的其余四项（`Test`、`Models`、`Validate`、`Classify`）直接是插件的同名方法。

## 7. 一致性套件会检查什么（设计 14.1，套件在 W12）

每个平台插件在自己的测试里调用套件。现在就按这些写：

1. 每个声明的上游协议、流式与非流式，`Execute` 都能完成，并且**恰好一次**中继。
2. 普通端点：不等待 + `host_usage`。`extract_usage` 的端点：等待 + `usage`。任务提交：等待 + `task`。什么都不返回不过。
3. 交回的请求体：模型引用与交入的一致；交入时 `stream` 为真，交回时仍为真；合法 JSON；没有重复键（用 `jsonedit` 改就满足后两条）。
4. 请求头：鉴权头和被签名的头在权威头，其余在默认头；不带 `content-length`、`transfer-encoding`、`connection`、`accept-encoding`；不把没在 `passHeaders` 里的客户端头带出去。
5. 不重试：中继或取回失败时不发第二次中继，返回错误或 `classification`。
6. 确定性：同样的输入（固定时钟）产生同样的请求。时间从可替换的时钟取，不要在构造里直接 `time.Now()`；不要遍历 map 拼字符串。
7. 换号：`attempt > 0` 时从头构造，不复用上一次的结果（不要按请求缓存）。
8. 不开套接字：整个套件期间默认传输一拨号就失败。不要用 `http.DefaultClient`、厂商 SDK 客户端；对外只有 `Relay` / `RelayAndWait` / `Fetch`。
9. 取消：context 取消后在平台调用超时内返回。把 `ctx` 传给 `Fetch`、`Relay`，不要用 `context.Background()`。
10. 大请求体：请求体取上限时能完成，插件一侧的额外分配不超过请求体的 4 倍。用 `jsonedit` 一次改完，不要整份解析，也不要连串调用 sjson。
11. 声明了 `platform.websocket.v1` 的：一轮消息的 `Execute` 给出握手地址、握手头和最终消息。
12. `ValidateCredentials`、`ClassifyError`、`BuildTestRequest`、`BuildModelsRequest` 对标准输入有回答。
13. 签名的插件（bedrock）：交回的请求能通过模拟上游的 SigV4 校验，URL 解析再输出后逐字节不变。

SDK 已经替你拦住的（返回带原因的错误，不用等套件）：第二次中继；没有标志却等待；`Monitor` 里中继；大写的头名和传输层的头；`host_usage` 却没有中继过；`usage` / `task` 却没有等待过。
