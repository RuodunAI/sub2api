> **已被 [DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md](DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md) 取代，仅作推导记录。** 被推翻的结论：适用范围“只有 codex、volcengine，普通转发路径不改”（现在所有平台插件都走 `Execute`）；A1 旧插件加载但不执行、A8 允许切回旧版本、整节 7“硬切的迁移、执行办法与回退”（用户定为不考虑兼容，旧包加载时直接拒绝）；A2 的 `platform.execute.v2` / `platform.monitor.v2`（不引入 v2，`platform.execute.v1` 删除）；A3 不升 Host API 版本（升到 10，插件协议升到 2）；“旧的 `Poll` 这次不动”（`Poll` 与两阶段对账接口删除）；“非执行路径的 `usageSource: plugin` 事后提取、WebSocket 不改”（前者删除，后者并入同一条路径）；固定 15 秒的插件收尾时限（并入平台调用超时）。

# 执行型插件的用量只走返回值：删除上报回调，核心不兜底（设计稿）

2026-11-18，第二版。分支 `feat/usage-return`（从本地 develop `123282faa` 开出）。**本文只有设计，没有实现代码。** 第一版（提交 `6aad8d939`）按“回调与返回值并存”写；用户随后改了决定，本版整篇重写。第 9 节列出已定的和仍需确认的点。

文中“执行型插件”指声明能力 `platform.execute.v1` 的插件，目前只有 codex 和 volcengine（`plugins/codex/manifest.json:19`、`plugins/volcengine/manifest.json:27`）。“上报回调”指插件在 `Execute` / `Monitor` 调用期间反过来调核心的三个 RPC：`RecordUsage`、`ReserveAndWatch`、`ReportTaskProgress`。“核心版本 V”指带本设计的第一个核心版本，预计 0.1.305（0.1.304 已被未发布的云厂商一节占用，`CHANGELOG.md:11-18`）。

用户的决定（本文围绕它们设计）：

1. **只有一种方式**（2026-11-18 改定，原话“统一改成调用完成之后直接返回，这样就不用那个上报机制了”）：事实只放在核心这次调用的返回值里。三个上报回调删除，不并存。
2. 返回值里没有事实时，核心不自己兜底计费。
3. 迁移用**硬切**：核心 V 直接删掉回调，插件随核心一起升级，不保留一个版本的双路径。
4. 异步任务（提交时预扣、完成时结算）用同一个模型；任务完成仍由核心发起的 `Monitor` 得知，插件不自己起定时器。
5. 预扣：用户问“核心直接把请求发给插件，让插件来预扣费吧”。本文第 6 节给出落法：插件**返回估算**，钱由核心扣。

姊妹文档在分支 `feat/plugin-forwarding`（核心提供 HTTP 客户端，选择加入的厂商插件变成执行式）。两篇用同一套说法：响应由核心中继、用量由核心按规则读的插件，`Execute` 只需返回 `host_usage`。

## 1. 范围

网关请求在选定账号、改写请求体之后分叉（`server/internal/gateway/dispatch.go:620-639`）：`canExecute` 为真时走 `executeAttempt`，由插件的 `Execute` 驱动；否则核心调 `BuildUpstreamRequest` 再 `forwardBuilt(..., nil)`。

本文只改第一条路径和任务监控的 `Monitor`。第二条路径上没有“上报”这一步：只有清单的插件（openai、gemini、relay、azure_openai）没有进程，`declarative.Client` 不实现 `core.ExecutePlugin`（`server/internal/plugin/declarative/declarative.go:25-31`）；只实现 `BuildUpstreamRequest` 的插件（anthropic、claude-oauth、vertex、bedrock）也走这里。核心在 `forward()` 里按清单的 `usage` 规则读用量，`submit()` 交给结算（`gateway/forward.go:95-146`、`gateway/pipeline.go:825-876`）。这是它们唯一的用量机制，不是决定 2 要删的兜底，不改。

同样不改：非执行路径上 `usageSource: "plugin"` 的端点（核心在 handler 返回后调 `ExtractUsage`，失败记 `usage_extract=fallback`，`gateway/usageplugin.go:159-194`）；WebSocket；`ForwardUpstream`（它是插件请核心代发上游请求，不是上报，`sdk/pluginsdk/execute.go:40-52`）；`ExecuteHTTP`（监控里的查询）。

## 2. 现状

### 2.1 同步请求

1. `executeAttempt` 生成执行 ID，带 `Forward`、`Record`、`Watch` 三个回调调用 `Execute`，总时限 6 小时加 `relay_timeout_seconds`（`gateway/execute.go:78-92`）。
2. 插件调 `ForwardUpstream`：核心写 `plugin_executions` 一行（状态 `intent`），发上游请求并中继响应（`execute.go:181-190`、`usage/executions.go:22-28`）。
3. 上游成功后：SSE 逐块发给客户端，非 SSE 响应先写进暂存、不发（`execute.go:241-249`）。`persistObservation` 把核心读到的用量和诊断摘要落库，状态 `observed`（`execute.go:268-280`、`usage/executions.go:30-48`）。这一步不计费。
4. 插件调 `RecordUsage`，核心的 `record()`（`execute.go:302-331`）：带报告时要求端点由插件计量（`:306-308`）；不带报告表示采用核心读数（`:320-322`）。然后 `CommitExecution` 在一个事务里插使用记录、扣费、把执行行改为 `committed` 并记摘要（`usage/executions.go:50-116`）。
5. `Execute` 返回，已记录则发布暂存的响应（`execute.go:133-150`）。

时限：`ForwardUpstream` 结束后启动 15 秒的收尾计时（`execute.go:91`、`plugin/grpcruntime/execute.go:94-105`），到点取消调用。回调只有在上游观察完成之后才被接受（`grpcruntime/execute.go:87-89`）。

### 2.2 核心的兜底（两处，都只针对同步请求）

- **请求内**（`gateway/execute.go:115-132`）：`Execute` 返回后观察已落库而没有记录时，核心自己调 `x.record(ctx, &RecordUsageRequest{})`，按核心读数计费（`:125-127`）。不看 `Execute` 是否报错，所以插件崩溃、超时也在内。同一块里还有“插件来过回调但提交失败时重试同一份事实”（`:120-124`），那不是兜底。
- **后台恢复**（`usage/executions.go:118-154`，结算重试任务每轮调用，`usage/settler.go:1100`）：`observed` 超过 1 分钟的同步执行行按核心读数提交。

`RecordUsage(nil)` 不是兜底，是插件的明确表态。`ExecuteDefault` 每次都会调一次（`sdk/pluginsdk/execute.go:141-155`）。

### 2.3 任务提交与监控

- 提交：插件调 `ReserveAndWatch`，核心的 `watch()` 校验并改写任务 ID，`CommitExecution` 在一个事务里写使用记录、预扣和监控登记，成功后才把提交响应发给客户端（`gateway/execute.go:332-390`、`usage/executions.go:92-97`）。没有兜底：插件没登记时进入 `taskFailure`，客户端收到错误（`execute.go:153-155`、`gateway/tasks.go:107-116`）。
- 监控：核心认领到期任务（租约 3 分钟，`usage/reconcile.go:46`、`usage/task_reconcile.go:30-52`），调 `Monitor`。插件用 `ExecuteHTTP` 查一次，再调 `ReportTaskProgress`；核心在 `outcomeTx` 的一个事务里结算或重排并写回执（`usage/monitor.go:68-123`、`:32-66`）。没有兜底：`Monitor` 返回而没有已提交的上报时报错（`grpcruntime/monitor.go:84-86`），按一次查询失败处理（`usage/poll_failures.go:35-57`）。
- 旧能力 `platform.poll.v1` 的 `Poll` 本来就把 `ReconcileResult` 放在返回值里（`platform.proto:101`、`grpcruntime/poll.go:117-141`、`usage/reconcile.go:421-444`），核心对返回的结果走同一套结算函数（`usage/task_reconcile.go:113-130`）。

### 2.4 幂等

摘要是报告的确定性序列化的 SHA-256（`gateway/execute.go:294-301`）。`CommitExecution` 锁住执行行：已 `committed` 且操作名、摘要相同则当作成功；不同则 `ErrConflict`，不覆盖（`usage/executions.go:71-76`）。监控回执按“任务 ID:认领 token”加摘要判重，规则相同（`usage/monitor.go:47-54`、`:73-78`）。

### 2.5 两个插件今天怎么用

codex 和 volcengine 的 `Execute` 都直接是 `ExecuteDefault`（`plugins/codex/internal/codex/upstream.go:259-261`、`plugins/volcengine/internal/volcengine/execute.go:12-14`）。volcengine 的 `Monitor` 是 `Poll` 加 `ReportTaskProgress`（`execute.go:18-25`）。两处都把回执丢掉了（`sdk/pluginsdk/execute.go:137`、`:153`，volcengine `execute.go:23`）。仓库里没有任何插件在请求中途上报，也没有插件读回执的内容。

## 3. 方案：一种机制

### 3.1 `Execute` 和 `Monitor` 的返回值

| 调用 | 返回 | 核心怎么处理 |
|---|---|---|
| `Execute`，上游出错 | `classification` | 不变（`gateway/execute.go:96-107`） |
| `Execute`，同步成功 | `usage`（插件解析的用量） | 校验、定价、提交，再发布响应 |
| | `host_usage`（明确采用核心读数） | 按核心读数提交，再发布响应 |
| `Execute`，任务提交 | `task`（上游任务 ID、初始快照、可选的预估） | 一个事务登记任务、扣预估、建监控，再发布提交响应 |
| `Execute` | 什么都不填 | 未上报（第 4 节），不计费 |
| `Monitor` | `result`（进行中或终态） | 一个事务里重排，或结算、退款并结束监控 |
| `Monitor` | 不填 | 按一次查询失败处理（与今天相同） |

核心在调用**返回之后**处理这些事实，用的还是今天的校验、定价和事务代码，只是入口从回调改成返回值。

### 3.2 proto

`sdk/proto/sub2api/plugin/v1/platform.proto`：

```protobuf
message ExecuteResponse {
  ClassifyErrorResponse classification = 1;
  // What the plugin states about a successful upstream exchange. At most one.
  // None of them: the execution is unreported and is not billed.
  oneof report {
    UsageReport usage = 2;
    HostUsage host_usage = 3;   // use the usage the host collected by the declared rules
    TaskSubmission task = 4;
  }
}
message HostUsage {}

message MonitorResponse {
  ReconcileResult result = 1;
}
```

“采用核心读数”必须是显式标记，才能和“什么都没填”分开。`UsageReport`、`TaskSubmission`、`ReconcileResult` 原样复用。

`host.proto` 删除：三个 RPC（`:49`、`:52`、`:55`，连同 `:45-47`、`:50-54` 的说明里相应的话）和四个消息 `RecordUsageRequest`、`ReserveAndWatchRequest`、`ReportTaskProgressRequest`、`ExecutionReceipt`（`:349-368`）。`ForwardUpstream`、`ExecuteHTTP` 及其消息保留。

### 3.3 能力 ID 改为 v2

`platform.execute.v1`、`platform.monitor.v1` 的语义是“用回调上报”。返回值语义用新 ID `platform.execute.v2`、`platform.monitor.v2`。原因是硬切需要一个核心能识别的标记：已发布的旧包 `hostCompat` 写的是 `<0.2.0`（`plugins/codex/manifest.json:14`、`plugins/volcengine/manifest.json:21`），事后改不了，核心 V 光看 `hostCompat` 分不出新旧包。详见第 7 节。

### 3.4 幂等（决定 3 剩下的部分）

回调没有了，“回调与返回值谁先到”不再存在。保留的只有重复结果的规则，存储层已经是这样（2.4 节）：

- 同一次执行、同一次监控认领，相同摘要的结果再次提交：不做任何事。
- 不同摘要：拒绝，记 ERROR 日志（两边的摘要），已提交的内容不变。

重复只会来自核心自己的重试（3.5），或监控认领过期后别的节点再查一次（认领 token 不同，由任务行的状态和行锁保证只结算一次，`usage/reconcile.go:629-639`）。

### 3.5 提交失败由核心重试

今天提交失败时插件从回调拿到错误，核心在收尾块里用同一份事实再试一次（`gateway/execute.go:120-124`）。回调删除后插件不再参与：

- 同步与任务提交：核心拿着返回的事实，在现有的 15 秒收尾时间内重试提交（同一执行 ID、同一摘要，重复提交是无操作）。仍然失败：
  - 同步：进入“记录未确认”分支（`execute.go:151-162`），非流式 503、流式连接异常结束。执行行停在 `observed`，1 分钟后被扫描标记为未上报（第 4 节），**不计费**。插件其实返回了事实，是核心没存住；内存里的事实随请求结束丢失。
  - 任务提交：`taskFailure`，客户端收到 `task execution could not be confirmed`，任务不登记，预扣释放。上游的任务已经创建，成为没人管的任务；上游的原始应答留在 `task_submission_receipts`（`execute.go:233-237`）可供人工处理。这与今天提交失败时的结果相同。
- 监控：事务失败时 `Monitor` 这一轮按查询失败处理（`usage/task_reconcile.go:109-112`）；如果连重排也写不进去，认领的租约 3 分钟后过期，任务被重新认领，核心再调一次 `Monitor`，插件再查一次上游，得到同样的结果。重试靠的是任务行的租约和 `core.usage.reconcile` 这个集群定时任务（`usage/reconcile.go:115-127`，`Scope: core.TaskCluster`，由 River 承载）的下一轮扫描，不是把上次的结果存下来重放。

## 4. 没有上报时（决定 2，语义已由用户确认）

**算上报**：返回值里有 `usage`、`host_usage`、`task`、`result` 之一，并且通过校验。`host_usage` 是插件的表态，不是兜底。

**算没有上报**（同步请求，观察已落库）：`Execute` 成功返回但没填；`Execute` 报错、超时或插件崩溃；填的东西不合法（种类与端点不符、端点不由插件计量却给了 `usage`、任务 ID 校验不过）。

处理：

| 项 | 处理 |
|---|---|
| 预扣 | 立即释放（写下面的免费记录时在同一事务里释放，`usage/settler.go:477-488`） |
| 使用记录 | 一条零费用记录：`success=false`，`error_type=usage_unreported`，token 清零，`billing_status=free`，`anomalies.usage_extract="unreported"` |
| 客户端 | 非流式 503，暂存的响应丢弃；流式内容已发出，连接异常结束（现有分支 `gateway/execute.go:151-162`） |
| 账号与插件 | 账号不受罚；每次写 ERROR 日志（请求 ID、执行 ID、插件、账号、原因） |
| 观察行 | `plugin_executions` 改为 `unreported`，核心读数与诊断摘要保留 7 天，不计费 |
| 后台恢复 | 不再按核心读数提交，改为把停在 `observed` 超过 1 分钟的行标记为未上报 |

任务提交没填 `task`、监控没填 `result`：与今天没调回调时相同（2.3 节），不需要新规则。

SDK 在插件解析用量失败时返回 `host_usage`（今天是 `RecordUsage(nil)`，`sdk/pluginsdk/execute.go:142-151`），核心照旧记 `usage_extract=fallback`。这一条已确认保留。

**钱的后果**：上游成功而没有上报的请求不计费。发生在：自己写 `Execute` 的插件漏填；插件在上游应答后、返回前崩溃或被终止；收尾超过 15 秒；总时限到期；核心在观察落库与提交之间崩溃；数据库故障使提交全部失败（成批）。用 `ExecuteDefault` 的插件不会漏填。

缓解（已确认）：a ERROR 日志；b 控制台的未上报筛选，加“按核心读数补记”的人工操作（权限 `usage:settle`，执行行还在的 7 天内可用，可放第二期）；c 按插件统计最近 10 分钟的未上报数，达到固定阈值 20 时经 `core.Notifier` 发通知。不做自动禁用插件。

## 5. 删掉回调失去了什么

一次说清，供用户确认接受：

**(a) 请求中途不能上报。** 实际没有损失：今天回调也只在上游观察完成之后才被接受（`grpcruntime/execute.go:87-89`、`gateway/execute.go:303`），codex 和 volcengine 都只在最后调一次。真正变化的是一个很窄的窗口：今天插件调完 `RecordUsage` 之后再崩溃，这笔已经记上（`execute.go:133` 只看是否已记录）；以后插件必须活到 `Execute` 返回，否则不计费。监控同理：今天 `ReportTaskProgress` 提交后插件崩溃不影响结果，以后这一轮作废，下一轮重查。

**(b) 插件拿不到“已存住”的回执。**

- 没有插件用到回执的内容（2.5 节）。失去的是能力：插件不能在 `Execute` 内得知公开任务 ID，也不能做“提交成功之后再做一步”的事。需要这种逻辑的插件以后只能放到下一次核心调用里。
- 暂存响应的发布顺序不变：核心在 `Execute` 返回后先提交、再发布（现在发布也在 `Execute` 返回之后，`execute.go:133-148`）。
- `ReserveAndWatch` 的原子性在核心一侧原样保留：使用记录、预扣、监控登记仍在 `CommitExecution` 的一个事务里。SDK 注释里“丢了 ACK 可以重试同一份内容”（`sdk/pluginsdk/execute.go:72`）不再需要，因为没有 ACK；重试改由核心做（3.5）。
- 监控的结算事务由返回的结果驱动，失败后靠租约重查（3.5）。

**(c) `Execute` 内的先后。** 今天插件可以先记录、再做别的事、再返回；以后一切都在返回之后提交。对延迟没有不利影响：账号并发槽位在这次尝试返回后释放（`gateway/dispatch.go:146`），今天也是在 `Execute` 返回之后；预扣在提交时结清，时刻不变；每个请求少一次 RPC 往返。唯一要求是插件在 `ForwardUpstream` 之后不要做慢操作，15 秒收尾时限照旧。

## 6. 预扣

### 6.1 今天怎么做（全程）

1. **请求前，调度之前**（`gateway/pipeline.go:608-627`）：先查 API Key 额度和余额（`CheckBalance`），再调 `c.precharge`（`gateway/precharge.go:35-144`）。模型没有价格且策略为免费时跳过（`:36-39`）。
2. **估算从哪来**：
   - 声明平台的插件有进程时，核心调 `PlatformService.EstimateUsage`（`precharge.go:49-62`），传元信息、端点声明的请求字段（`usageRequestFields`，不是整个请求体）、截断的提示文本和设置里的最低预扣 token 数。超时用热路径超时。
   - 插件返回 `Unimplemented`：非任务端点改用核心本地的 tokenizer 数提示（`precharge.go:63-76`）；任务提交端点直接报错，请求失败（`:66-68`、`:79-81`）。SDK 对没实现 `UsageEstimator` 的插件默认回 `Unimplemented`（`sdk/pluginsdk/estimate.go:30-37`）。
   - 没有插件进程的平台：核心本地数（`precharge.go:82-88`）。
   - 目前只有 volcengine 实现了它，并且只对视频提交协议给出估算，其他协议回 `Unimplemented`（`plugins/volcengine/internal/volcengine/video_billing.go:15-24`）。codex 没有实现，用核心的本地估算。
3. **金额**：输入取估算值（不低于设置 `pre_consume_tokens`，默认 500），输出取“估算的输出”与设置 `output_reserve_tokens`（默认 4000）的较大者（`precharge.go:100-116`、`billing/settings.go:59`、`:63`），加上估算的计量值，按这次请求的价格和分组倍率算出金额（`billing/precharge.go:36-44`）。估算里不允许带 `Reservation`（`precharge.go:90-92`）。
4. **扣**：`billing.Precharge` 用一条语句锁余额行、写账本（kind `usage`，幂等键 `precharge:{request_id}`）、写 `request_precharges`、改余额（`billing/precharge.go:251-280`）。余额减去金额不高于最低余额时不写，返回 `insufficient_balance`，客户端收到 402（`billing/precharge.go:262`、`gateway/pipeline.go:619-626`、`core/errors.go:69`）。
5. **结清**：使用记录提交时，结算语句在同一事务里退回这笔预扣并扣真实费用（`billing/settle.go:60-90`）；记录是免费的就只退回（`usage/settler.go:477-488`）；没有任何记录时 24 小时后由清扫退回（`migrations/0025_request_precharges.sql:6`、`billing/precharge.go:299-306`）。
6. **任务**：提交前的预扣同上（估算必须来自插件）。上游接受任务后，`ReserveAndWatch` 带的 `usage.reserve` 是插件的第二次、更准的预估；核心登记任务时在同一事务里按它定价、结清请求前的预扣并扣这笔预估（`usage/settler.go:489-501`、`:716-730`），使用记录状态为 `reserved`。任务完成时 `Monitor` 的结果驱动多退少补或全额退款（`usage/reconcile.go:606-`、`:698-`）。

### 6.2 插件不碰账本

价格、余额和预扣、扣款、释放、结算四条语句都归核心（AGENTS.md“数据库”一节：整条手写 SQL 只允许账本的 4 条 CTE；`PLUGIN-EXECUTES-CORE-RECORDS.md` §4“永远不交给插件：定价、金额、倍率、直接写账本”）。`UsageReport` 没有金额字段（`platform.proto:470-476`）。所以“让插件来预扣费”落成：**插件返回估算，核心据此扣住钱。**

### 6.3 返回值模型下的预扣

| 阶段 | 做法 | 相对今天 |
|---|---|---|
| (a) 上游请求之前 | 核心调插件的 `EstimateUsage`，插件返回估算的用量；核心定价、预扣，余额不足照常返回 402。插件没实现时用核心的默认估算 | **没有变化**。`EstimateUsage` 本来就是一次带返回值的调用，已经符合统一模型。唯一可能变的是传给插件的内容：姊妹文档若让核心把完整请求体交给插件，这里可以一并改为传完整请求体，由那篇文档决定 |
| (b) 任务提交 | `Execute` 返回的 `task` 里带更准的预估（`usage.reserve`）；核心登记任务时原子地把请求前的预扣换成这笔预估 | 事实相同，入口从 `ReserveAndWatch` 改成返回值 |
| (c) 结清 | 同步请求按返回的 `usage` / `host_usage` 结清；任务按 `Monitor` 返回的终态多退少补或退款；未上报则释放预扣 | 入口改成返回值；未上报的释放是第 4 节的新规则 |

由此产生两个待确认点（第 9 节 N4、N5）：是否要求每个执行型插件都实现估算；没实现时的默认估算是什么。

## 7. 硬切：迁移、执行办法与回退

### 7.1 用户接受的风险

核心 V 不再提供三个回调，也不保留过渡期。代价是：从某个节点换成核心 V，到该节点上的插件换成新版本，这段时间里旧版本的 codex、volcengine 不能按执行路径工作；仓库外如果有调用这三个回调的插件，必须重新编译并发新版本后才能在核心 V 上执行；回退核心前要先把插件换回旧版本。换来的是核心和 SDK 里只有一条路径，符合 AGENTS.md 规则 2。

### 7.2 为什么必须由核心识别旧包

旧版本插件（codex 0.1.11、volcengine 0.12.8）的 `hostCompat` 包含核心 V，光靠它核心 V 会照常加载并调用它们的 `Execute`。旧 SDK 在上游应答之后调 `RecordUsage`，核心 V 回 `Unimplemented`，`ExecuteDefault` 返回错误（`sdk/pluginsdk/execute.go:153-155`），于是**每个请求都成为未上报**：流式内容白送，非流式 503，任务提交全部失败。这种逐请求的静默失败必须挡住。

办法是 3.3 的能力 ID：核心 V 只对声明 `platform.execute.v2` 的插件调 `Execute`，只对 `platform.monitor.v2` 调 `Monitor`。旧包声明的是 v1，核心 V 不会调它们的这两个方法，也就不会走到回调。

### 7.3 执行办法

| 时机 | 规则 |
|---|---|
| 上传、安装新包 | 清单里声明 `platform.execute.v1` 或 `platform.monitor.v1` 的包被拒绝，错误信息写明“此版本使用已删除的上报回调，需要为核心 V 及以上重新构建” |
| 新包的 `hostCompat` | codex、volcengine 新版本下限提到 V。旧核心不认识 v2 能力（`sdk/manifest/check/validate.go:452-453`），也会因 `hostCompat` 拒绝加载（`plugin/grpcruntime/runtime.go:205-213`） |
| 已安装的旧包在核心 V 上 | 见下，两种做法，待确认（N1） |
| 控制台 | 插件列表对旧包显示“不兼容：需要升级”，并说明当前的服务方式 |

已安装的旧包在核心 V 上怎么办：

- **做法甲，拒绝加载**。最直接，但有一个代码上的障碍：托管升级的节点就绪检查要求每个已启用的插件都已加载到批准的版本（`app/managed_checks.go:241-251`，由 `app/phases.go:638-642` 接入）。内置插件的升级又要等所有节点都换成新核心之后才开始（CONTRACTS §37）。旧 codex 加载不了，新核心节点就不就绪，升级计划会卡住或回滚。要用这个做法，必须同时改就绪检查（把“因能力已删除而未加载”算作就绪）。在内置升级完成前，codex、volcengine 的账号不可调度，请求得到“没有可用账号”，不会错账。
- **做法乙，加载但不执行（建议）**。核心 V 把 v1 当作“认识但已停用”的能力：插件照常加载，`canExecute` 为假，请求走普通转发路径（核心调它的 `BuildUpstreamRequest`、自己中继、按规则读用量）；volcengine 的任务提交走普通路径的任务登记（`gateway/tasks.go:118-180`），监控走它同时声明的 `platform.poll.v1`（`usage/reconcile.go:401-417`）。就绪检查不用改，窗口内请求不失败、照常计费，核心里没有任何回调代码。保留的只有“v1 这两个 ID 在清单里仍被认识”这一处常量，删除时机：所有部署上已安装的插件都不再声明 v1 之后的下一个核心版本（启动时对仍声明 v1 的已启用插件记 WARN，便于判断）。这条降级路径对旧 codex、旧 volcengine 是否完全正确**需要在发布前实测**（第 12 节）。

### 7.4 发布顺序

volcengine 在发布包里是内置插件，默认只安装不启用（`deploy/docker/build-go.sh:155-166`），线上 ovh 集群上是启用的（`deploy/gateway/ovh/README.md:11`）。协调方消息里说它是市场安装的，与仓库不符；两种情况都写：

1. 构建核心 V 的发布包，内置集合里是 codex、volcengine 的新版本（v2 能力、`hostCompat >= V`）。
2. 升级核心。没有迁移，`rolling-v1` 和 `primary-first-v1` 都可以。期间各节点状态：
   - 旧核心节点：旧插件，回调照常。
   - 新核心节点：旧插件。做法乙下走普通路径；做法甲下不可用。
   - 共享数据库没有不兼容：新状态 `unreported` 旧核心不会读写（旧的恢复扫描只找 `observed`）。旧节点提交的任务可以被新节点监控（经 `Poll`），反之亦然。
3. 所有节点都是核心 V 之后，内置同步把**已启用**的 codex、volcengine 升到新版本（CONTRACTS §37，约十几秒内开始）。各节点独立切换版本（§36），先切的节点先用返回值路径。
4. 没启用的内置插件保持旧版本、不启用（§37“被禁用的保持禁用”）。以后启用时必须先升级到新版本；这一步的具体行为没有核对（第 12 节）。
5. 从市场安装、版本高于包内的插件不会被内置同步触碰（§37）：管理员在核心升级后从市场升级到新版本。市场索引要在核心发布的同时上架新版本。
6. 仓库外的执行型插件：作者用新 SDK 重新编译、改清单、发新版本。在此之前按 7.3 处理。

### 7.5 回退

- 只回退插件：核心 V 上不能安装 v1 的新包，但切回一个**已经批准过**的旧版本要允许（否则无法为回退核心做准备）；切回后按 7.3 的做法服务。
- 回退核心到 V-1：旧核心不认识 v2、`hostCompat` 也不包含它，新版本插件加载不了，就绪检查不过。顺序必须是**先把 codex、volcengine 切回旧版本，再回退核心**。旧核心不会自动降级内置插件（§37）。
- `unreported` 状态的执行行对旧核心无副作用，7 天后清理。

### 7.6 Host API 版本

第一版的结论是不升。删除 Host RPC 之后重新评估：

- 仓库的硬性规则只有两处常量必须相等（`runtime-contract/contract.go:14`、`sdk/protocol/protocol.go:19`，说明在 `CONTRACTS.md:2570`）。SDK 自 0.1.290 起不检查这个版本（§101.1），它现在只被网关外壳用来判断两个核心版本能否滚动并存（`gateway/internal/control/store.go:454`）。升了就不能用 `rolling-v1`（§91.3）。
- 按字面，删除 Host RPC 是不兼容的接口变化，升版本名正言顺。
- 但升它保护不了任何东西：这三个 RPC 只有拿着核心在 `Execute` / `Monitor` 调用里签发的 token 才能调（`grpcruntime/execute.go:63-76`、`grpcruntime/monitor.go:15-28`），核心 V 不再对 v1 插件发起这两种调用，旧插件根本走不到被删的 RPC；新旧核心并存期间也没有数据不兼容（7.4）。

建议**仍不升**（9），兼容性由能力 ID 和 `hostCompat` 保证。如果用户更看重“接口删了版本号就该变”，升到 10 的代价是这次发布只能走停服式升级（约十秒）。列为 N2。

## 8. 改动清单

### 8.1 删除

| 位置 | 删除的内容 |
|---|---|
| `sdk/proto/.../host.proto` | `RecordUsage`、`ReserveAndWatch`、`ReportTaskProgress` 三个 RPC（`:49-55`）；`RecordUsageRequest`、`ReserveAndWatchRequest`、`ReportTaskProgressRequest`、`ExecutionReceipt`（`:349-368`） |
| `sdk/proto/.../platform.proto` | `:16-18`、`:22-23` 注释里关于回调的说法 |
| `sdk/gen/pluginv1` | 重新生成 |
| `sdk/pluginsdk/execute.go` | 函数 `RecordUsage`（`:54-67`）、`ReserveAndWatch`（`:69-82`）、`ReportTaskProgress`（`:84-97`）；`ExecuteDefault` 里的两处调用（`:137`、`:153`）；`TaskMonitor` 的旧签名（`:21-27`） |
| `sdk/pluginsdk/pluginsdktest` | `FakeHost` 的三个桩和字段（`execution.go:16-33`、`fakehost.go:72` 一带） |
| `sdk/pluginsdk/doc.go:47-58`、`interfaces.go:59`、`:79` | 注释 |
| `server/internal/core/ports_execution.go` | `ExecutionCallbacks.Record`、`.Watch`（`:16-17`）；`MonitorPlugin.Monitor` 的上报函数参数（`:25`）；`CommitExecution` 返回的 `ExecutionReceipt`（`:34`，改为直接返回任务 ID） |
| `server/internal/gateway/execute.go` | 回调装配里的 `Record`、`Watch`（`:91`）；兜底调用（`:125-127`）；`record`、`watch` 的回调签名（改为直接接收报告与任务） |
| `server/internal/plugin/grpcruntime/execute.go` | `hostServer.RecordUsage`（`:131-154`）、`ReserveAndWatch`（`:155-178`）；`online()` 里只为这两个回调存在的分支（`:87-89`） |
| `server/internal/plugin/grpcruntime/monitor.go` | `hostServer.ReportTaskProgress`（`:14-66`） |
| `server/internal/plugin/grpcruntime/poll.go` | 监控上报用的状态 `reportSealed`、`report`、`reportDone`、`reportBusy`、`reported`（`:24`、`:31-33`、`:66`、`:72-74`、`:89`） |
| `server/internal/usage/monitor.go` | `report` 闭包作为回调的形式（`:92-117`）；`e.reported` 的回调含义 |
| `server/internal/usage/executions.go` | 后台恢复按核心读数提交（`:148`） |
| `plugins/volcengine/internal/volcengine/execute.go` | `Monitor` 里的 `ReportTaskProgress`（`:23`） |
| 测试 | `sdk/pluginsdk/execute_test.go`、`gateway/execute_test.go`、`grpcruntime/execution_audit_test.go`、`grpcruntime/monitor_test.go`、`usage/executions_test.go`、`plugins/codex/internal/codex/upstream_test.go:327`、`plugins/volcengine/internal/volcengine/execute_test.go`、`e2e/ac24_async_video_test.go` 里对回调的使用，改成断言返回值 |
| 文档 | 见 P9 |

### 8.2 新增与改写

- **SDK**：`ExecuteDefault` 返回带报告的 `ExecuteResponse`（任务返回 `task`；插件解析出用量返回 `usage`；其余返回 `host_usage`）。`TaskMonitor` 改为 `Monitor(ctx, *PollRequest) (*ReconcileResult, error)`，`platformServer.Monitor` 把结果放进 `MonitorResponse`。能力推断、清单检查改用 v2（`serve.go:187-192`、`task.go:13-18`）；声明 v1 的清单在 `sub2api-plugin` 检查时报错。
- **网关**（`gateway/execute.go`）：`Execute` 返回、上游错误分支之后，从 `out` 取报告，调 `record` / `watch`，提交失败在收尾时限内重试；没有合法报告时把执行关闭为未上报，再进入现有的“记录未确认”分支。`canExecute` 判断 v2。
- **用量**（`usage/executions.go`、`core/ports_execution.go`）：`ExecutionRecords` 加“关闭为未上报”：一个事务里锁执行行、插免费记录（其中释放预扣）、状态改 `unreported`、保留 `record` 与 `observation`。恢复扫描改为调它。新常量 `core.UsageExtractUnreported`。
- **监控**：`core.MonitorPlugin.Monitor` 返回 `*pluginv1.MonitorResponse`。运行时对终态结果检查这次调用有完整、成功的 HTTP 观察（今天在回调入口，`grpcruntime/monitor.go:37-40`；改到返回处，与 `Poll` 相同，`grpcruntime/poll.go:126-131`）。`askMonitor` 用返回的结果走现有的 `applyMonitorResult`，回执照写。
- **清单**（`sdk/manifest/manifest.go:81-82`、`check/validate.go:38-45`、`:459`）：新增 v2 两个 ID；v1 的处理按 N1。
- **插件**：codex、volcengine 清单改 v2、`hostCompat >= V`、升版本；volcengine 的 `Monitor` 直接返回 `Poll` 的结果。
- **控制台**：使用记录详情与筛选显示未上报；插件列表显示旧包不兼容。
- **存储**：不需要迁移。`plugin_executions.state`、`operation` 是没有 CHECK 的 `text`（`migrations/0019_plugin_executions.sql:8`、`:11`）；`usage_logs.anomalies` 是 jsonb。汇总的“异常行数”自动包含 `unreported`（`usage/rollup.go:63-71`）。
- **设置**：没有新设置项。

故障策略：未上报的尝试以 `attemptReturn` 结束，不会换号重放（`gateway/failure_policy_runtime.go:318-321`）；它是否会触发管理员的 503 规则而冷却账号，实现时要排除（第 12 节）。

## 9. 决定一览

### 9.1 已定

| 原编号 | 内容 | 结果 |
|---|---|---|
| 1 | 没有上报时非流式响应 | 不发，503 |
| 2 | 没有上报时的使用记录 | 写零费用记录，立即释放预扣 |
| 3 | 后台恢复按核心读数补记 | 删除，改为标记未上报 |
| 4 | `Execute` 报错、超时、崩溃且没有上报 | 不计费 |
| 5 | SDK 在插件解析失败时采用核心读数 | 保留，表现为显式的 `host_usage` |
| 6 | `ExecuteDefault` 用返回值 | 现在是唯一方式 |
| 7 | volcengine 的 `Monitor` | **必须**改成返回值（原建议是不改，被新决定推翻） |
| 8 | 缓解措施 | a 日志、b 列表与人工补记、c 通知：要；d 自动禁用：不要 |
| 9 | 返回的报告不合法 | 按没有上报处理 |
| 11 | 旧的 `Poll`（`platform.poll.v1`） | 这次不动 |
| — | 迁移方式 | 硬切 |

### 9.2 仍需确认

| # | 问题 | 建议 |
|---|---|---|
| N1 | 已安装的旧版本插件在核心 V 上：拒绝加载（甲），还是加载但不执行、走普通转发（乙）？ | 乙。甲会让节点就绪检查不过、升级计划卡住，除非同时改就绪检查 |
| N2 | Host API 版本（原第 10 条）：删了三个 Host RPC，升不升？ | 不升，仍是 9；升了这次只能停服式发布 |
| N3 | 用新能力 ID `platform.execute.v2`、`platform.monitor.v2` 区分新旧包？ | 用。没有它核心分不出旧包 |
| N4 | 是否要求每个执行型插件都实现 `EstimateUsage`？ | 不要求。维持现状：只有任务提交端点必须实现 |
| N5 | 插件没实现估算时的默认估算 | 维持现状：核心本地数提示，输入不低于 `pre_consume_tokens`，输出取 `output_reserve_tokens` |
| N6 | 提交失败时核心是否把返回的事实落库以便跨请求重试？ | 不落库。只在 15 秒收尾时间内重试，失败即未上报，与今天的保证相同 |
| N7 | 接受“插件在 `Execute` 内拿不到公开任务 ID 和提交结果”这一能力损失？ | 接受。现有插件都没用到 |
| N8 | 核心 V 上是否允许切回已批准过的旧版本插件（为回退核心做准备）？ | 允许，只拒绝新上传的 v1 包 |

### 9.3 与任务说明的出入

- volcengine 是内置插件（默认只安装不启用），不是市场安装（7.4）。
- 监控提交失败后的重试：`core.usage.reconcile` 是 River 承载的集群任务，但重试的方式是租约过期后下一轮重新认领、重新查询上游，不是 River 重放上次的结果（3.5）。
- 总时限是 6 小时加 `relay_timeout_seconds`（`gateway/execute.go:89`）。
- CONTRACTS §31.3 写“内置 anthropic/openai/gemini/relay 0.2.0 使用 Execute”，与代码不符，实现时改对。

## 10. 任务拆分

| 包 | 内容 | 主要文件 | 验收 | 依赖 |
|---|---|---|---|---|
| P1 | proto、能力 ID、生成代码 | `sdk/proto/.../platform.proto`、`host.proto`、`sdk/gen/pluginv1`、`sdk/manifest/manifest.go`、`sdk/manifest/check/validate.go` | 三个 RPC 与四个消息不再出现在生成代码里；清单检查认识 v2，对 v1 的行为符合 N1 | 无 |
| P2 | 用量：未上报关闭、恢复扫描、`CommitExecution` 返回值 | `core/ports_execution.go`、`core/ports_billing.go`、`usage/executions.go` 及测试 | 关闭后免费记录一条、预扣已释放、执行行 `unreported` 且保留读数；重复关闭无副作用；`observed` 超 1 分钟的行被标记而不计费 | P1 |
| P3 | 网关：从返回值提交、删回调与兜底 | `gateway/execute.go`、`execute_test.go`、其他实现 `ExecutionRecords` 的测试桩（`streamonly_test.go`、`accountstats_test.go`） | 第 11 节矩阵的同步与任务行 | P1、P2 |
| P4 | 运行时：删回调服务端、`Monitor` 返回值 | `plugin/grpcruntime/execute.go`、`monitor.go`、`poll.go` 及测试 | 旧 RPC 不存在；终态返回值缺完整 HTTP 观察被拒绝 | P1 |
| P5 | 监控结算走返回值 | `usage/monitor.go`、`usage/executions_test.go` | 第 11 节矩阵的监控行 | P4 |
| P6 | SDK | `sdk/pluginsdk/execute.go`、`interfaces.go`、`serve.go`、`task.go`、`doc.go`、`execute_test.go`、`pluginsdktest/*` | `ExecuteDefault` 三种返回各有测试；包内不再有三个回调函数 | P1 |
| P7 | 插件 | `plugins/codex`、`plugins/volcengine` 的清单、`volcengine/execute.go`、两边测试、`e2e/ac24_async_video_test.go` | `TestReleasedPluginsBumped` 通过；任务提交断言返回的 `task`；监控断言返回的 `result` | P6 |
| P8 | 旧包的处理与提示 | 按 N1：`gateway/execute.go` 的 `canExecute`、`usage/reconcile.go:401-417`、安装校验（`plugin/pkg/validate.go`、`plugin/install`）、控制台插件列表 | 旧 codex、旧 volcengine 包在核心 V 上的表现符合 N1；上传 v1 包被拒绝且信息清楚；切回已批准的旧版本可行 | P1、P3 |
| P9 | 文档 | `docs/CONTRACTS.md`（§31 重写，新增一节，目前下一节是 §112）、`PLUGIN-DEVELOPMENT.md`（路径表、“用量来源”行、自查清单）、`PLUGIN-EXECUTES-CORE-RECORDS.md` 开头、`ARCHITECTURE.md:1211`、`PLUGIN-VOLCENGINE-ARK.md:240`、`:263`、`CHANGELOG.md` | 与代码同一组提交；文中不再把三个回调写成现行接口 | 各包 |
| P10 | 控制台与缓解措施 | `web/src/views/usage/*`、`usage/api.go`、`usage/settler.go`（通知） | 详情显示未上报；补记后记录为已计费、账本有一笔扣款、重复操作无副作用；达到阈值发一条通知 | P2 |

P1 先做；P2、P4、P6 并行；P3 等 P2；P5 等 P4；P7 等 P6；P8 等 P3；P10 可以第二期。

## 11. 测试方案

网关测试用现有的假执行插件（`gateway/execute_test.go:23-93`），改成按开关返回不同内容。每行检查：客户端收到什么、`usage_logs` 行数与费用、账本、`request_precharges`、`plugin_executions` 状态。流式与非流式各一遍。

| `Execute` 的结果 | 期望 |
|---|---|
| 返回 `usage` | 按报告计费；非流式在提交之后才发布 |
| 返回 `host_usage` | 按核心读数计费 |
| 返回 `host_usage`，端点由插件计量 | 按核心读数计费，记 `usage_extract=fallback` |
| 返回 `task` | 任务登记、预估已扣、请求前的预扣已结清、提交响应里是公开 ID |
| 什么都不返回 | 未上报：免费记录、预扣释放、执行行 `unreported`、非流式 503、流式异常结束 |
| 上游应答后 `Execute` 返回错误（含超时、进程退出） | 同上 |
| 返回的种类不对（任务端点给 `usage`，或反过来；端点不由插件计量却给 `usage`） | 未上报，日志写明原因 |
| 任务端点什么都不返回 | 任务不登记，客户端报错，预扣释放 |
| 上游出错，返回 `classification` | 不变：按分类换号或返回 |
| 提交第一次失败、重试成功 | 计费一次，响应发布 |
| 提交一直失败 | 同步：503，1 分钟后执行行被标记未上报；任务：客户端报错，上游应答留在回执表 |
| 同一执行重复提交相同事实 | 无操作，只有一条记录 |
| 同一执行提交不同事实 | 拒绝，日志带两个摘要，记录不变 |

监控：返回进行中（重排）；返回成功（多退少补）；返回失败（全额退款）；不返回结果（记一次查询失败）；返回终态但没有完整的 HTTP 观察（拒绝）；结算事务失败后租约过期被重新认领、第二次结果相同时只结算一次；过期认领的结果不能覆盖别的节点已提交的结果。

预扣：余额不足在上游请求之前返回 402；插件没实现估算时用默认估算；任务端点的插件没实现估算时请求失败（现有行为，补回归测试）。

迁移：旧版本的 codex 0.1.11、volcengine 0.12.8 包装到核心 V 上，表现符合 N1；v1 包上传被拒绝；新包装到 V-1 上被拒绝。

端到端（ec2pool 测试实例）：codex 流式、非流式各一次，核对 token、费用与升级前同类请求一致，执行行 `committed`。数据库测试要确认真的跑了，没有成片的 SKIP。

## 12. 未验证事项

- 做法乙的降级路径（旧 codex、旧 volcengine 在核心 V 上走普通转发、普通任务登记和 `Poll`）只读了代码入口，没有运行过。这是发布前必须实测的一项。
- 做法甲下就绪检查的实际表现（升级计划是卡住还是回滚）只读了 `app/managed_checks.go:241-251`。
- 没启用的内置插件（volcengine 在未启用它的部署上）日后启用时，核心会不会先把它升到包内版本，没有核对 `plugin/install/builtin.go`。
- 核心 V 上切回已批准的旧版本插件，现有的升级流程是否允许“降版本”，没有核对。
- 仓库外是否有调用三个回调的插件，代码里查不到。
- 未上报的失败在启用故障策略时是否触发管理员的 503 规则（`gateway/failure_policy_runtime.go:249-254`）。
- 人工补记复用 `settleReconciled` 的程度。
- volcengine 视频任务没有真实账号可测，只能靠模拟上游、插件测试和 `e2e/ac24`。
- 通知由核心自己发出没有先例（只有测试发送用到来源 `core`，`notify/http.go:112`），渠道与收件人怎么取要补设计。
- `ExecuteResponse` 加 `oneof`、`host.proto` 删 RPC 后生成代码的差异没有跑过 `buf generate`。

## 13. 不在本次范围

- 非执行路径上 `usageSource: "plugin"` 的兜底；WebSocket 的用量。
- 删除 `platform.poll.v1` 与两阶段 `BuildReconcileRequest` / `ParseReconcileResponse`。
- 上游已发出但观察没能落库的情况（`gateway/execute.go:151-162` 中 `x.sent && !x.observed`）：今天既没有使用记录也没有观察，预扣等 24 小时过期。另记一笔。
- `EstimateUsage` 的入参是否改为完整请求体：由 `feat/plugin-forwarding` 的文档决定。
