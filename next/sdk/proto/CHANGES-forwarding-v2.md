> 临时文件：W1（契约）给后续工作包的迁移说明。文档包（W13）把内容并入 CONTRACTS 后删除本文件。

# 转发 V2 契约变更（W1）

依据 `docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md`，加上所有者的四条更正：请求体上限只有一个网关系统设置；不定义 UDP 和 TCP；不留任何兼容字段；没有进程的插件和清单 `upstream` 声明不存在。

每行格式：变了什么 → 改用什么。Go 符号都在 `sdk/gen/pluginv1`（下文省略包名）。

## 1. 版本常量

| 变化 | 改用 |
|---|---|
| `sdk/protocol.ProtocolVersion` 1 → 2（go-plugin 握手版本与 `GetInfoResponse.protocol_version`） | 旧二进制在握手时被 go-plugin 拒绝；运行时（W5）把这个错误换成 `protocol.OutdatedContractMessage` |
| `runtime-contract.HostAPIVersion` 9 → 10 | — |
| `sdk/protocol.HostAPIVersion` 不再是字面量，定义为 `runtimecontract.HostAPIVersion` | 两者恒等，不需要再写相等性测试（`server/internal/app/managed_test.go:287` 的断言可删） |
| 新增 `protocol.OutdatedContractMessage(pluginKey, pluginVersion, hostVersion string) string` | 旧契约包的唯一拒绝信息（设计 9.2）。握手版本不符、清单检查出 `check.CodeOutdatedContract` 时，上传、安装、加载都返回它 |
| 新增 `check.CodeOutdatedContract = "outdated_contract"`、`check.OutdatedContract(err) bool` | 核心（W9）判断“这个清单是旧契约的”，然后用上面的信息代替字段错误 |

## 2. PlatformService

### 删除的 RPC

| RPC | 改用 |
|---|---|
| `BuildUpstreamRequest` | `Execute`：插件拿完整请求体，自己构造最终请求，经 `HostService.RelayUpstream` 交给核心 |
| `ExtractUsage` | 等待形式的 `RelayUpstream` 返回观察，插件在 `Execute` 的返回值里给 `usage`。SDK 的 `UsageExtractor` 变成 `ExecuteDefault` 进程内调用的接口（W3） |
| `ParseTaskSubmission` | 同上，`Execute` 返回 `task`。SDK 的 `TaskSubmissionParser` 同样变成进程内接口 |
| `Poll` | `Monitor`（结果在 `MonitorResponse.result`） |
| `BuildReconcileRequest`、`ParseReconcileResponse` | `Monitor` 里用 `HostService.ExecuteHTTP` 查一次上游 |

生成代码里随之消失：`PlatformServiceClient`/`PlatformServiceServer` 的上述六个方法、`UnimplementedPlatformServiceServer` 的对应桩、`_PlatformService_<名>_Handler`。

### 删除的消息

| 消息 | 改用 |
|---|---|
| `BuildUpstreamRequestRequest` | `ExecuteRequest`（字段直接在它上面） |
| `BuildUpstreamRequestResponse`（含 `patches`、`signing`、`base`） | `UpstreamRequest`（host.proto）。没有补丁：`body` 是最终字节 |
| `BuildReconcileRequestRequest`、`BuildReconcileRequestResponse`、`ParseReconcileResponseRequest` | `ExecutionHTTPRequest` / `ExecutionHTTPResponse` |
| `RequestSigning`（common.proto） | 插件用 `sdk/reqsign.Sign` 自己签，把被签名的头放进 `UpstreamRequest.headers` |

### 改动的消息

| 消息 | 变化 |
|---|---|
| `ExecuteRequest` | 字段 1 `request` 删除并 `reserved`（`GetRequest()` 没了）。现在是：`execution_token = 2`、`meta = 3`、`account = 4`、`body = 5`（`bytes`，完整请求体；WebSocket 端点是这一轮的消息）、`inbound_headers = 6`、`attempt = 7`、`extract_usage = 8`、`task_submission = 9` |
| `ExecuteResponse` | 新增 `oneof report { UsageReport usage = 2; HostUsage host_usage = 3; TaskSubmission task = 4; }`；`classification = 1` 保留。Go：`ExecuteResponse_Usage`、`ExecuteResponse_HostUsage`、`ExecuteResponse_Task`、`GetUsage()`、`GetHostUsage()`、`GetTask()`、`GetReport()` |
| 新增 `HostUsage {}` | 显式的“采用核心读数”，取代 `RecordUsage(report=nil)` |
| `MonitorResponse` | 新增 `ReconcileResult result = 1`，取代 `ReportTaskProgress` |
| `PollRequest` | 结构不变，只作 `Monitor` 的入参（名字按设计 11.1 保留） |
| `ResolveModelRequest` | 字段 2 `fields` 删除并 `reserved`（`GetFields()` 没了）：它的来源是平台级 `requestFields`，那个清单字段已删。仓库里唯一的实现（volcengine）不读它 |
| `ExtractUsageRequest` | 结构不变。不再是某个 RPC 的入参，是 `RelayUpstreamResponse.observation` 的类型 |
| `ClassifyErrorRequest`、`ClassifyErrorResponse`、`UsageReport`、`TaskSubmission`、`Reservation`、`ReconcileEntry`、`ReconcileResult`、`EstimateUsageRequest` | 不变 |

`extract_usage`、`task_submission` 两个标志原来在 `ForwardUpstreamResponse` 上，现在在 `ExecuteRequest` 上：插件在发请求之前就知道该用哪种形式。

## 3. HostService

### 删除

| RPC / 消息 | 改用 |
|---|---|
| `ForwardUpstream`、`ForwardUpstreamRequest`、`ForwardUpstreamResponse` | `RelayUpstream`、`RelayUpstreamRequest`、`RelayUpstreamResponse` |
| `RecordUsage`、`RecordUsageRequest` | `ExecuteResponse.usage` 或 `.host_usage` |
| `ReserveAndWatch`、`ReserveAndWatchRequest` | `ExecuteResponse.task` |
| `ReportTaskProgress`、`ReportTaskProgressRequest` | `MonitorResponse.result` |
| `ExecutionReceipt` | 没有替代：插件拿不到任务 ID 和回执（D9） |

### 新增

```protobuf
rpc RelayUpstream(RelayUpstreamRequest) returns (RelayUpstreamResponse);

message UpstreamRequest {
  string method = 1;                        // 默认 POST
  string url = 2;                           // 绝对地址；WebSocket 轮次是握手地址
  map<string, string> headers = 3;          // 权威头：写在运营方的透传与覆盖之后
  map<string, string> default_headers = 4;  // 默认头：写在运营方设置之前
  bytes body = 5;                           // 最终字节；WebSocket 轮次是这一轮的消息
  string upstream_model = 6;
  string response_stream = 7;               // 如 "aws-eventstream-chunk"
  UpstreamOptions options = 8;
}
message UpstreamOptions {                   // 只能收紧；0 或不小于生效值时不起作用
  int32 response_header_timeout_ms = 1;
  int32 total_timeout_ms = 2;
}
message RelayUpstreamRequest {
  string execution_token = 1;
  UpstreamRequest request = 2;
  bool wait = 3;
}
message RelayUpstreamResponse {             // 不等待时为空；等待时恰好一个
  ExtractUsageRequest observation = 1;      // 2xx
  ClassifyErrorRequest error = 2;           // 非 2xx 或传输错误
}
```

三种形式：

| 形式 | 调用 | 约定 |
|---|---|---|
| 中继，不等待 | `RelayUpstream{wait: false}` | 核心只做本地校验并立即返回空响应；请求在 `Execute` 带 `host_usage` 返回之后才发出 |
| 中继，等待 | `RelayUpstream{wait: true}` | 核心立即发送并中继，结束后返回 `observation` 或 `error`。只允许用在 `extract_usage` 或 `task_submission` 为真的执行上，否则 `FAILED_PRECONDITION` |
| 取回 | `ExecuteHTTP` | RPC 名不变（设计 11.7）。适用范围扩到 `Execute` 期间；主机必须在清单 `externalServices` 内；每次执行最多 8 个请求，回答最多 8 MiB |

一次执行最多一个中继。被核心拒绝的请求返回 `INVALID_ARGUMENT`，不算已中继。

`ExecutionHTTPRequest` 新增 `UpstreamOptions options = 6`（`GetOptions()`）。`ExecutionHTTPResponse`、`SendAccountRequest` 不变。

请求头约定：两个 map 的键都是小写；`content-length`、`accept-encoding`、`connection`、`transfer-encoding` 不许出现；同名时权威头生效。权威头里只放鉴权头和被签名的头（签名时包括 Content-Type）。

## 4. 其余 proto

| 位置 | 变化 |
|---|---|
| `common.proto` 契约规则 | “请求体从不整个发给插件”改为：平台插件在 `Execute` 里拿到完整请求体；网关钩子仍然只看声明的字段、用 `BodyPatch` 回答 |
| `BodyPatch` | 保留，只有 `hook.proto` 用 |
| `plugin.proto` | `protocol_version` 注释改为 2；`SettingsChanged` 注释里“旧 SDK 回 UNIMPLEMENTED、核心忽略”删掉。核心侧的忽略逻辑由 W5 核对后删除 |
| `host.proto` 注释里的列表符号 `*` 改成 `-` | 只为让 `buf generate` 的输出与提交的生成代码逐字节相同，不用再手工还原 |

没有为 TCP、UDP 定义任何东西：`EgressService.Dial`、`DialOpen` 未动。

## 5. 清单（`sdk/manifest`）

| 删除 | 改用 |
|---|---|
| `manifest.RuntimeNone`；`runtime: "none"` | 每个插件都是 `runtime: "grpc"`。清单检查对 `"none"` 报 `outdated_contract` |
| `manifest.Upstream`、`UpstreamBaseURL`、`UpstreamCredentials`、`UpstreamKeyValidator`、`UpstreamTexts`、`UpstreamAuth`、`HeaderWrite`、`UpstreamRoute`、`UpstreamPatch`、`RouteModel`、`UpstreamTest`、`UpstreamModels`，常量 `UpstreamAuthHeader`/`Query`/`None`、`PatchWhenStream`、`ModelFromRequest`、`ModelFromMeta`；`AccountType.Upstream`；`(*Upstream).Layer` | 没有替代的清单字段。行为写成插件代码，公共部分在 `sdk/pluginsdk/standard`（W4）。清单里出现 `accountTypes[].upstream` 报 `outdated_contract` |
| `manifest.CapPlatformExecute`、`manifest.CapPlatformPoll` | `CapPlatformAdapter` 的含义就是“实现 `Execute`”；任务观察只有 `CapPlatformMonitor`。清单里声明这两个 ID 报 `outdated_contract` |
| `Platform.RequestFields`、`AccountPlatform.RequestFields`；`sdk/platforms/{anthropic,openai}.json` 里的 `requestFields` | `ExecuteRequest.body`。清单里出现 `requestFields` 报 `outdated_contract` |
| `EndpointRequest.MaxBodyBytes` | 网关的请求体上限（一个系统设置）。清单里出现 `request.maxBodyBytes` 报 `outdated_contract` |
| `check` 包：`runtimeNone`、`upstream` 校验、`needsCredentials`、`noProcessCapabilities` | 声明了账号类型就必须有 `accounts.credentials`（原来声明式类型可以没有） |

| 保留或移动 | 说明 |
|---|---|
| `manifest.RuntimeGRPC`、`TransportHTTP`、`TransportWebSocket` | 从 `upstream.go` 搬到 `manifest.go` |
| `usageSource: "plugin"`（`UsageSourcePlugin`、`Endpoint.PluginUsage()`）、`usageStreamEvents`、`usageMaxBytes`、`usageRequestFields`、`endpoint.task` | 不变。`ExecuteRequest.extract_usage` 来自 `PluginUsage()`，`task_submission` 来自 `TaskSubmit()`。设计没有引入新的清单字段 |
| `CapPlatformAdapter`、`CapPlatformMonitor`、`CapPlatformTasks`、`CapPlatformWebSocket` | ID 不变 |

| 新增 | 说明 |
|---|---|
| `AccountType.AddressFields()` 只认 `upstreamFields` | 原来声明了 `upstream` 的类型自动得到 `["base_url"]`。openai、gemini、relay、azure_openai、anthropic 的 `apikey` 类型改写时要显式写 `"upstreamFields": ["base_url"]`，否则落回旧规则（`CustomUpstream()` 的结果会变） |
| 检查 `foreign_facts`（`accountTypes[i].platforms[j].platform`） | 设计 4.3 的跨插件规则：账号类型不能服务别的插件的平台上由那个插件解析用量或提交任务的端点。只在被引用的平台可见时生效（已安装，或 `ValidateOptions.OtherPlatforms`）；注册表绑定时的同一条规则归 W6 |
| 检查 `outdated_contract` | 见上。被删的字段从包里的 `manifest.json` 原文读（类型里已经没有它们），所以只有传了 `files["manifest.json"]` 的调用方会报这四个字段；`runtime` 和能力 ID 不依赖原文 |

## 6. 这次改动之后不能编译的包

W1 通过的：`sdk/gen/...`、`sdk/protocol`、`sdk/manifest/...`、`sdk/platforms`（不含子目录 `upstream`）、`runtime-contract` 整个模块。`gateway`、`e2e`、`deploy/mock-upstream` 三个模块 `go vet ./...` 仍然通过。

直接报错的（`go vet ./...` 逐模块扫出来的），按设计第 13 节由各包修复：

| 包 | 原因 | 归属 |
|---|---|---|
| `sdk/platforms/upstream`（连带 `sdk/upstream`） | `manifest.Upstream` 等类型已删 | W4 删除 |
| `sdk/pluginsdk`（连带它的全部子包） | 已删的 RPC、消息、能力常量 | W3 |
| `server/internal/core`（连带核心里几乎所有包） | `ports_plugin.go`、`ports_execution.go` 引用已删的消息 | W5 |
| `server/internal/plugin/pkg/pkgtest` | `runtime none` 夹具 | W9 |
| `tools/sub2api-plugin` | `manifest.RuntimeNone`（`build.go:59`、`dev.go`） | W11 |
| `plugins/*` 全部十六个模块 | 都依赖 `sdk/pluginsdk`；openai、gemini、relay、azure-openai、anthropic 另外依赖 `sdk/platforms/upstream` | W10、W11 |

## 7. 重新生成

```sh
cd next/sdk
buf generate          # buf 1.47.2，protoc-gen-go v1.31.0，protoc-gen-go-grpc v1.2.0
git status --short gen  # 应当没有差异
```
