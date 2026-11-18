> 临时文件，由文档包（W13）并入 CONTRACTS 后删除。W5（运行时）写给 W6（网关）、W7（用量）、W9（注册表与安装）和集成者。

# 转发 V2：核心端口交接说明（W5）

依据 `docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md` 和协调方在实现期间下达的三条更正（请求体上限是网关设置 `max_request_body_mb`；中继请求体的界限是 max(交给插件的请求体, 上限) + 1 MiB；取回的主机规则加上“本账号自己的上游地址”）。端口的权威说明是 `ports_execution.go`、`ports_plugin.go`、`ports_billing.go`、`ports_tasks.go` 里的注释，本文是索引加待办。

W5 的提交只让 `internal/core` 自己编译。`internal/plugin/grpcruntime`、`internal/plugin/rollout` 的代码已改完，但它们 import `internal/plugin/registry`，而 `registry.go` 还 import 已经编译不过的 `sdk/upstream` 和 `plugin/declarative`（W9 的文件，W5 不许改）。见第 5 节。

## 1. 端口一览

### 1.1 执行（`ports_execution.go`）

```go
type RequestBodyLimit func() int64                      // 网关的请求体上限，字节；nil 或 <=0 取默认
const DefaultMaxRequestBodyBytes = 128 << 20
func MaxRequestBodyBytes(limit RequestBodyLimit) int64

const ExecutionHTTPMaxRequests = 8
const ExecutionHTTPMaxResponseBytes = 8 << 20
type ExecutionHTTP func(context.Context, *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error)

type ExecutionClient struct {
    SegmentTimeout time.Duration                        // 平台调用超时；<=0 取 2 秒
    Check func(context.Context, *pluginv1.UpstreamRequest) error
    Relay func(context.Context, *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error)
    Fetch ExecutionHTTP
}
type ExecutionResult struct {
    Response *pluginv1.ExecuteResponse                  // 没有错误时非 nil
    Relay    *pluginv1.UpstreamRequest                  // 不等待的中继，且插件返回了 host_usage 时才有
}
type ExecutePlugin interface {
    Execute(ctx context.Context, in *pluginv1.ExecuteRequest, client ExecutionClient) (ExecutionResult, error)
}
const PluginUnavailableSegmentTimeout = "segment_timeout" // ErrPluginUnavailable 的 Details["reason"]
const PluginUnavailableInflightBytes  = "inflight_bytes"

type MonitorPlugin interface {
    Monitor(ctx context.Context, in *pluginv1.PollRequest, fetch ExecutionHTTP) (*pluginv1.ReconcileResult, error)
}

const ExecutionStateIntent, ExecutionStateObserved, ExecutionStateCommitted, ExecutionStateUnreported
const ExecutionOpUsage = "usage"; const ExecutionOpWatch = "watch"
type ExecutionRecords interface {
    BeginExecution(context.Context, ExecutionIntent) error
    ObserveExecution(ctx context.Context, id string, rec *UsageRecord, observation []byte) error
    CommitExecution(context.Context, ExecutionCommit) error
    CloseExecutionUnreported(context.Context, ExecutionUnreported) error
}
type ExecutionIntent struct { TaskSubmit bool; Record *UsageRecord; ID, RequestID, PluginKey string; AccountID int64; Attempt int }
type ExecutionCommit struct { ID, Operation, Digest string; Record *UsageRecord; Task *TaskRegistration }
type ExecutionUnreported struct { ID, Reason string }
func FactDigest(m proto.Message) string                 // 确定性编码的 SHA-256；nil（host_usage）是空串的摘要
```

### 1.2 其余改动

- `PlatformPlugin`（`ports_plugin.go`）少了 `BuildUpstreamRequest`、`ExtractUsage`、`ParseTaskSubmission`、`BuildReconcileRequest`、`ParseReconcileResponse`。`Execute`、`Monitor` 不在这个接口里，仍然靠类型断言取（`binding.Client.(core.ExecutePlugin)`、`.(core.MonitorPlugin)`）：这样别的包里的 `PlatformPlugin` 假实现不用动。运行时的客户端三者都实现。
- 删除：`DeclaredUpstream`、`PollPlugin`、`ExecutionCallbacks`、`CommitExecution` 返回的回执、`AsyncTasks.RegisterTask`（任务只经 `CommitExecution` 的 `ExecutionCommit.Task` 登记）。
- `ExecutionHTTP` 从 `ports_plugin.go` 搬到 `ports_execution.go`，类型没变。
- `ports_billing.go` 新增 `UsageExtractUnreported = "unreported"`、`UsageErrorUnreported = "usage_unreported"`；`UsageExtractPlugin`、`UsageExtractFallback` 的注释改成返回值的说法。
- `AccountTypeBinding.Client` 的注释写明永不为 nil（没有无进程的类型了）。

## 2. 运行时保证的不变量

1. **不等待的中继在 `Execute` 返回前什么都不发。** 插件调 `RelayUpstream{wait:false}` 时运行时只做自己的检查，再调 `client.Check`，然后把请求挂在这次执行上、立即回空响应。`Execute` 返回后，只有 `Response` 带 `host_usage` 时这份请求才出现在 `ExecutionResult.Relay` 里。插件报错、超时、崩溃、返回别的种类：`Relay` 为 nil，什么都不发，不计费，换号且不罚账号。
2. **一次执行最多一个中继。** 第二次是 `FAILED_PRECONDITION`。被运行时或 `Check` 拒绝的请求是 `INVALID_ARGUMENT`，不算已中继，插件可以改了再交。
3. **等待的中继只在 `extract_usage` 或 `task_submission` 为真且 `client.Relay` 非 nil 时放行**，否则 `FAILED_PRECONDITION`，`Relay` 不被调用。运行时先调 `Check` 再调 `Relay`，`Relay` 的结果必须恰好有 `Observation` 或 `Error` 之一（否则插件得到 `INTERNAL`）。
4. **取消与崩溃。**
   - `Execute` 的 `ctx` 管整个调用；取消它会取消插件的调用和正在进行的宿主调用。客户端断开要不要传到这个 `ctx` 由网关决定（今天用 `WithoutCancel` 加 6 小时上限）。
   - 传给 `Check`、`Relay`、`Fetch` 的 `ctx` 在两种情况下结束：插件放弃这次宿主调用（进程被杀、崩溃、排空），或 `Execute`/`Monitor` 调用结束。**等待形式下插件崩溃：`Relay` 的 `ctx` 结束，网关必须据此切断上游；随后 `Execute` 返回 `ErrPluginUnavailable`，这次执行按未上报处理、不计费。**（所有者已定。）
   - `Execute` 返回之前运行时会等正在进行的宿主调用结束；返回之后 `Check`、`Relay`、`Fetch` 都不会再被调用，令牌作废（`PERMISSION_DENIED`）。
   - 不等待形式下，`Execute` 返回之后的交换与插件无关：插件崩溃、重启、换代都不影响它。
5. **同一次执行的宿主调用是串行的。** 一个还没结束时来第二个：`FAILED_PRECONDITION`。
6. **分段计时（设计 8.4）。** `SegmentTimeout` 管插件自己的每一段：`Execute` 开始到第一次宿主调用、两次宿主调用之间、最后一次宿主调用返回到 `Execute` 返回。宿主调用内部的时间不计。超时：`ErrPluginUnavailable`，`Details["reason"] == "segment_timeout"`。`Execute` 没有总时限，总时限由调用方的 `ctx` 给。`Monitor` 仍是整个调用 30 秒，`ClassifyError` 仍是 2 秒（调用方的 `ctx` 更短时取更短的）。
7. **请求体大小。** 中继的请求体不超过 max(这次 `ExecuteRequest.body` 的长度, 请求体上限) + 1 MiB；取回的请求体不超过请求体上限；超出 `INVALID_ARGUMENT`。上限每次检查时现读，保存后立即生效。宿主服务端的接收上限是 `MaxInt32`；除 `RelayUpstream`、`ExecuteHTTP` 外的宿主调用仍然只收 4 MiB（拦截器，保持 `sdk/protocol` 里 KV 等上限所依赖的数字）。
8. **取回。** 每次调用最多 8 个（第 9 个 `RESOURCE_EXHAUSTED`，`Fetch` 不被调用）；主机必须是清单 `externalServices` 之一或其子域，**或者是这次调用的账号自己的上游地址的主机**（账号类型 `upstreamFields` 声明的字段，从账号的 settings 和 credentials 里读，只比较主机名、不比较端口，不含子域），否则 `PERMISSION_DENIED`；回答超过 8 MiB 由运行时截到 8 MiB 并置 `truncated`。**SSRF 不在运行时做**：账号地址是运营方填的，可能指向私网，`Fetch` 的实现必须照旧过 `netguard`（按账号所在范围的“允许私网”）并走账号代理。被运行时拒绝的取回不计入 8 个。
9. **在途字节准入。** 每个插件实例“已交给 `Execute`、尚未返回”的请求体字节不超过该进程内存上限的三分之一；放不下的排队，最长等 `SegmentTimeout`，之后 `ErrPluginUnavailable`，`reason == "inflight_bytes"`。没有其他在途请求时，不论多大都放行（准入是并发预算，不是第二个大小上限）。进程没有内存上限时不做准入。
10. **内存下限。** 声明 `platform.adapter.v1` 的插件，进程内存上限至少是 64 MB 加 3 倍请求体上限（清单或运营方的值更大时用更大的），再受 `SUB2API_PLUGIN_MAX_MEMORY_MB` 封顶。清单没写内存上限的保持不限。上限变了之后 `LimitsStale()` 为真，现有的对账循环会平滑替换实例。
11. **`Monitor`。** 返回 `(*ReconcileResult, error)`：没有结果是错误；终态（`PENDING`、`POLL_FAILED` 以外）而这次调用里没有一次完整、成功、未截断的取回，也是错误。调用方把任何错误都当作一次查询失败。`Monitor` 里不能中继。
12. **`ExecuteRequest` 不被修改、请求体不被复制。** 运行时浅拷贝一份填 `execution_token`。
13. **旧契约的包。** 清单检查报 `outdated_contract`、握手协议版本不符、`GetInfo` 报的协议版本不符，`Runtime.Load` 都返回同一个错误：文字恰好是 `protocol.OutdatedContractMessage(key, version, hostVersion)`，`errors.Is(err, grpcruntime.ErrOutdatedContract)` 为真。前一种情况进程不会被启动。

## 3. W6（网关）

### 3.1 要做的

- 每次尝试调 `rt.binding.Client.(core.ExecutePlugin).Execute(ectx, &pluginv1.ExecuteRequest{Meta, Account, Body, InboundHeaders, Attempt, ExtractUsage, TaskSubmission}, client)`。`ExecutionToken` 不用填。
- 装配 `core.ExecutionClient`：
  - `SegmentTimeout`：`c.gw.platformTimeout()`。
  - `Check`：URL 合法、SSRF 名字检查、请求头合法、`response_stream` 认识（WebSocket 轮次上给了就拒绝）、守卫（设计 4.5：模型引用一致、强制流式的路由上流式标志仍为真、用量规则可计量、URL 与请求体里的模型与核心算的一致）。返回 `*core.Error` 时插件看到的是它的 `Message`。运行时已经拒绝了：nil 请求、空 URL、超界的请求体、`content-length`/`accept-encoding`/`connection`/`transfer-encoding`。
  - `Relay`：只给等待形式用（今天 `forwardCallback` 的内容）：`BeginExecution` → 发送并中继 → `ObserveExecution` → 返回 `Observation` 或 `Error`。WebSocket 轮次传 nil。**`ctx` 结束时切断上游。**
  - `Fetch`：经账号代理、过 SSRF、不套透传和覆盖、不跟随重定向、`in.Options` 只能收紧超时；回答最多读 `core.ExecutionHTTPMaxResponseBytes + 1` 字节并置 `truncated`；传输失败用 `TransportError` 加 `Status 0` 回答，不返回错误。
- `Execute` 返回之后：
  - `err != nil` 且自己的 `Relay` 没被调用过：请求没发出，按插件不可用换号（`ErrPluginUnavailable` 的两个 `reason` 可以写进尝试链路）。
  - `err != nil` 且 `Relay` 被调用过：等待形式失败。观察已落库就 `CloseExecutionUnreported`，然后走“用量记录未确认”的分支（非流式 503，流式连接异常结束）；任务提交走 `taskFailure`。
  - `res.Relay != nil`：现在发送（今天 `forwardBuilt` 第 764 行以后的部分），`forward()` 读用量，`submit()` 结算，不写执行记录。上游出错时调 `ClassifyError`。
  - `res.Relay == nil` 且 `Relay` 没被调用过：插件没有中继。有 `Response.Classification`（取回失败后插件给的）就按它裁决，否则按插件不可用换号。
  - 等待形式成功：按 `Response.GetReport()` 的种类取事实。`usage`（只在端点由本插件解析用量时合法）或 `host_usage` → `CommitExecution{Operation: ExecutionOpUsage, Digest: core.FactDigest(report 或 nil)}`；`task`（只在任务提交端点合法）→ `ExecutionOpWatch`，`Digest: core.FactDigest(task)`。提交失败在 15 秒收尾时限内用同一份 `ExecutionCommit` 重试（幂等）。没有合法事实、或一直提交不上：`CloseExecutionUnreported`。等待形式上游出错：用 `Response.Classification`，没有就用默认分类。
  - 需要网关自己定的两种组合：带标志的执行上插件却用了不等待的中继并返回 `host_usage`（运行时照常给出 `res.Relay`；任务提交端点应当拒绝，插件解析用量的端点可以按 `usage_extract=fallback` 发送）；插件既没中继也返回了 `host_usage`。
- 跨插件规则（设计 4.3）：别的插件的账号类型不能服务“插件解析用量”或任务提交的端点，事后提取的代码删除。
- `bodyLimit` 由 W8 改；`requestFields` 相关的删除见下表。

### 3.2 现在的编译错误和改法

| 位置 | 错误 | 改法 |
|---|---|---|
| `execute.go:28`、`:78` | `pluginv1.BuildUpstreamRequestRequest` | `gatewayExecution` 不再保存“构造请求的入参”；需要的元信息直接取自 `c` |
| `execute.go:34`、`:169`、`:200`、`:220`、`:251` | `pluginv1.ForwardUpstreamRequest`、`ForwardUpstreamResponse` | `forwardCallback` 改成 `ExecutionClient.Relay` 的实现，入参 `*pluginv1.UpstreamRequest`，返回 `*pluginv1.RelayUpstreamResponse{Observation | Error}`。原来放在响应上的 `ExtractUsage`、`TaskSubmission` 标志没有了，改放在 `ExecuteRequest` 上 |
| `execute.go:71` | `manifest.CapPlatformExecute` | 删 `canExecute`：没有第二条路径 |
| `execute.go:91` | `core.ExecutionCallbacks` | `core.ExecutionClient`（3.1） |
| `execute.go:92` | `ExecuteRequest` 没有 `Request` 字段 | 字段直接写在 `ExecuteRequest` 上 |
| `execute.go:97` | `out.GetClassification` | `out` 现在是 `core.ExecutionResult`，用 `res.Response.GetClassification()` |
| `execute.go:121`、`:325`、`:372` | `CommitExecution` 只返回一个值 | 去掉回执；任务 ID 就是自己传进去的 `Task.PublicID` |
| `execute.go:126` | `pluginv1.RecordUsageRequest` | 兜底整段删除（没有兜底）；改成 `CloseExecutionUnreported` |
| `execute.go:302`、`:332` | `RecordUsageRequest`、`ReserveAndWatchRequest`、`ExecutionReceipt` | `record`、`watch` 改成从返回值取：入参分别是 `*pluginv1.UsageReport`（`host_usage` 时 nil）和 `*pluginv1.TaskSubmission`，只返回 `error` |
| `dispatch.go:619`、`:627`、`:643`、`:693`、`:744` | `BuildUpstreamRequest*`、`core.DeclaredUpstream` | 设计 11.4：分叉、`BuildUpstreamRequest` 调用、`splitHeaders`、`writeDeclaredAuth` 删除；`forwardBuilt` 的入参换成 `*pluginv1.UpstreamRequest`，请求头按“默认头 → 透传 → 覆盖 → 权威头”分层 |
| `param_override.go:70`、`:126`、`:127` | `core.DeclaredUpstream`、`BuildUpstreamRequest*` | 删 `prepareDeclared`、`declaredTarget` |
| `upstream_ext.go:55`、`:74`、`:124` | `BuildUpstreamRequestResponse` | 签名部分删除；`response_stream` 从 `UpstreamRequest.ResponseStream` 取 |
| `websocket.go:733`、`:735`、`:829`、`:878` | `BuildUpstreamRequest*` | 每轮调 `Execute`（`Relay: nil`），用 `res.Relay` 的 URL、握手头、消息；`turnPatches`、`applyTurnPatches`、`handshakePatches` 删除 |
| `pipeline.go:388`、`:389` | `Request.MaxBodyBytes` | W8 的 `bodyLimit`（`c.gw.MaxRequestBodyBytes()`） |
| `pipeline.go:554`、`:561`、`routing.go:216`、`:235` | `RequestFields`、`ResolveModelRequest.Fields` | 删 `requestFieldValues` 和路由上的 `requestFields`（`usageRequestFields` 留） |
| `tasks.go:143`、`:172` | `ParseTaskSubmission`、`Tasks.RegisterTask` | 事后登记的路径整段删除；任务只经 `CommitExecution` 登记 |
| `usageplugin.go:183` | `ExtractUsage` | 事后提取的路径删除 |
| 测试 | `execute_test.go`、`declared_test.go`、`patch_guard_test.go`、`upstream_ext_test.go`、`env_test.go` 的假插件等 | 设计 14.2；`declared_test.go` 还 import `sdk/upstream`，随 W4 删除 |

## 4. W7（用量）

### 4.1 要做的

- `CommitExecution(ctx, core.ExecutionCommit) error`：语义不变，只是不再返回回执。幂等规则：同一执行、相同 `Operation` 和 `Digest` 再来一次返回 nil；不同的返回 `core.ErrConflict`、不改任何东西；执行必须处于 `observed`；已经是 `unreported` 的返回 `core.ErrConflict`。
- 新增 `CloseExecutionUnreported(ctx, core.ExecutionUnreported) error`：一个事务里锁执行行；由行里存的 `record` 生成免费记录（`Success=false`、`ErrorType=core.UsageErrorUnreported`、token 与计量清零、计费状态免费、`UsageExtract=core.UsageExtractUnreported`），插入时照现有逻辑释放预扣；状态改 `unreported`，`record` 与 `observation` 保留；写 ERROR 日志（请求 ID、执行 ID、插件、账号、`Reason`）。已经是 `unreported`：nil。`committed`：`core.ErrConflict`。`intent`（没有观察）：返回错误、什么都不写，预扣由到期清扫释放。
- 恢复扫描（`recoverExecutions`）：停在 `observed` 超过 1 分钟的行不再按核心读数提交，改成调上面的关闭。
- 按插件统计 10 分钟内的未上报数、达到 20 发通知（设计第 6 节）在用量模块内部做，不需要新端口。
- 任务观察只调 `Monitor`：`result, err := client.(core.MonitorPlugin).Monitor(ctx, &pluginv1.PollRequest{Entry, Account}, fetch)`；`err != nil` 按一次查询失败；拿到结果后走现有的 `applyMonitorResult`，摘要用 `core.FactDigest(result)`。`fetch` 的实现见 3.1 的 `Fetch` 一条和第 2 节第 8 条：不要再检查“只许一次”，SSRF 照旧（`netguard.CheckURL`），回答上限改成 `core.ExecutionHTTPMaxResponseBytes`，尊重 `in.Options`。
- `RegisterTask` 已不在 `core.AsyncTasks` 里：导出的方法可以删，`registerTask` 留给 `CommitExecution`。

### 4.2 现在的编译错误和改法

| 位置 | 错误 | 改法 |
|---|---|---|
| `executions.go:50`、`:54` | `pluginv1.ExecutionReceipt` | `CommitExecution` 只返回 `error` |
| `executions.go:164` | `*Service` 没有实现 `core.ExecutionRecords` | 加 `CloseExecutionUnreported` |
| `reconcile.go:412`、`:422` | `manifest.CapPlatformPoll`、`core.PollPlugin` | 删 `pluginObservationContract` 的分支和 `askPoll` |
| `reconcile.go:450`、`:464`、`:520` | `BuildReconcileRequest`、`ParseReconcileResponse` 及其消息 | 删 `buildAndAsk` 和两阶段的发送代码 |
| `monitor.go:92`、`:116` | `ReportTaskProgressRequest`、`ExecutionReceipt` | 上报闭包的内容移到 `Monitor` 返回之后 |
| `monitor.go:118` | `Monitor` 返回两个值、只收三个参数 | 见 4.1 |
| 测试 | `reconcile_test.go`、`poll_test.go`、`poll_failures_test.go`、`executions_test.go`、`precharge_test.go`、`pricing_db_test.go`、`task_reconcile_test.go`、`tasks_test.go` 里的 `ParseReconcileResponseRequest`、`BuildReconcileRequest*`、`BuildUpstreamRequest*`、`CapPlatformPoll`、`ReportTaskProgressRequest` | 假插件改成实现 `core.MonitorPlugin`；用量草稿第 11 节的矩阵 |

## 5. W9（注册表、安装、审查）

**先做这一条，其余的包都等着它：`registry/registry.go` 现在编译不过，`grpcruntime`、`rollout`、`install` 和 `app` 因此都编译不了。**

| 位置 | 错误 | 改法 |
|---|---|---|
| `registry/registry.go:21`、`:24` | import `sdk/upstream`（它依赖的 `manifest.Upstream` 已删）和 `plugin/declarative` | 删这两个 import |
| `registry/registry.go:371` | `at.Upstream` | 条件改成 `if pf == nil { continue }` |
| `registry/registry.go:386-407`、`:546` | `bindDeclared` 及其调用 | 删除 |
| `plugin/declarative/**` | 整个目录 | 删除 |
| `registry/registry_test.go:32`、`:69`、`:81`、`:662-692` | 假插件的 `BuildUpstreamRequest`、`BuildReconcileRequest`、`ParseReconcileResponse`；声明式类型的用例、`core.DeclaredUpstream` | 删这三个方法和声明式用例 |
| `pkg/pkgtest/pkgtest.go:133` | `Platform.RequestFields` | 删字段 |
| `pkg/pkgtest/pkgtest.go:188-196`、`:222` | `manifest.RuntimeNone`、`AccountType.Upstream`、`manifest.Upstream` | `Declared` 夹具换成有进程的；`Files` 里去掉 `RuntimeNone` 的判断 |
| `api/ops.go:442` | `manifest.RuntimeNone` | 删这个分支 |

上面前三行就是 W5 本地验证时用的全部补丁（`go build -overlay`，没有提交）：按这三行改完，`go build ./internal/plugin/grpcruntime/... ./internal/plugin/rollout/...` 即通过，`install` 也能编译。

还要做的：

- 注册表绑定时的跨插件规则（设计 4.3）。
- 上传、安装时的拒绝信息：清单检查的错误满足 `check.OutdatedContract(err)` 时，回 `protocol.OutdatedContractMessage(key, version, hostVersion)`，与 `grpcruntime.Runtime.Load` 给的逐字相同。`Runtime.Load` 自己已经做了这个检查（读包里的 `manifest.json` 原文，所以已删字段也能查出）。
- 加载失败的原因：`rollout` 把 `Load` 的错误文字原样记进节点状态，插件列表显示它即可。
- **取回 `rollout/staged_grants_test.go`**：文件头有构建标签 `fv2_parked`。它用 `pkgtest.Declared`（`runtime none` 夹具）建包、用 `Runtime.LoadStatic` 读授权，两者都没有了。换成有进程的夹具（`registrytest.TestPlugin(t)` 作为包里的二进制）和 `Runtime.Load` 之后去掉标签。它覆盖的是“批准中的版本的授权不影响正在运行的版本”（3 个测试），不要删。

## 6. 集成者

- **请求体上限的接线**：`grpcruntime.Options.RequestBodyLimit`（类型 `core.RequestBodyLimit`，`func() int64`，字节）。W8 提供的是 `(*gateway.Gateway).MaxRequestBodyBytes(ctx) int64`。在 `app/phases.go:291` 的 `grpcruntime.Options` 里加：

  ```go
  RequestBodyLimit: func() int64 { return b.gw.MaxRequestBodyBytes(context.Background()) },
  ```

  用闭包是因为 `b.gw` 在运行时之后才创建（`:312`）；这个函数只在加载插件和处理请求时才被调用。不接线时取默认的 128 MiB。
- `internal/account/account_test.go:74`、`:89`、`:93`：假插件上三个用到已删消息的方法（`BuildUpstreamRequest`、`BuildReconcileRequest`、`ParseReconcileResponse`），删掉即可。这个包不属于 W6、W7、W9。
- `internal/observation/hub_test.go`、`internal/plugin/egress/*_test.go` import `sdk/pluginsdk`，等 W3。
- `internal/app` 要等 `gateway`、`usage`、`registry` 都编译之后才能看到自己的错误；W5 的改动在它那里只有上面的接线一处（它没有用过 `LoadStatic`）。
- `sdk/protocol/kv.go` 里“宿主一次请求最多收 4 MiB（代理服务端的 gRPC 默认接收上限）”的注释现在由运行时的拦截器保证，数字没变。
