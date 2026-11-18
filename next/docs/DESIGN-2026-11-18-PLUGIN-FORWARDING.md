> **已被 [DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md](DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md) 取代，仅作推导记录。** 被推翻的结论：`requestBody: "full"` 是选择加入、默认仍是字段加补丁（决定 10；现在完整请求体是唯一模式，补丁模式删除）；新权限 `gateway.body`（决定 9、11；不设权限，审查页加固定说明）；SDK 的 `http.RoundTripper` 适配器与按厂商使用官方 SDK 客户端（决定 2、8 和第 6 节；不用厂商 SDK 客户端，只用 AWS 签名器、eventstream 和 golang-jwt）；完整请求体上限是核心常量 64 MiB（决定 7；改为系统设置，现行默认 128 MB）；bedrock 留在普通转发路径、执行路径的代价照付（决定 5；只有一条路径并重新设计）；只有清单的插件不用动（没有进程的插件取消，声明式引擎删除）；方案一、方案二作为并存选项（只保留核心提供客户端）；Host API 升 10 的理由与混合版本、回退的安排（按不考虑兼容重写）；“用量可以回调或返回值上报、先到的生效”（只有返回值）。

# 转发下放到插件：核心提供 HTTP 客户端（主方案）与两个对比方案

2026-11-18。分支 `feat/plugin-forwarding`（从本地 develop `123282faa` 开出）。**本文只有设计，没有实现代码。**

用户的方向："通用转发逻辑从核心搬到插件。核心收到客户端调用后交给插件；插件自己改写请求、自己发给上游（可以用厂商的官方 SDK 客户端）、上报用量；核心按规则计费。不要来核心转换。" 用户随后把模型说得更准确：插件默认不能联网（沙箱不变）；核心向插件提供一个应用层的 HTTP 客户端，插件拿到完整请求体、自己构造上游请求、**调用核心的客户端**发出去；真正的 HTTP 交换由核心完成，账号代理、传输设置、SSRF、允许的主机、运营方的请求头设置、日志都由核心套上。用户还问：公共逻辑做成插件链接的 SDK 库，还是每个插件各自实现。

本文写三个方案：

- **方案三「核心提供 HTTP 客户端」**（用户的模型，主方案，第 3 节）。
- **方案一「插件发送，流量经核心的出口隧道」**（对比，第 4 节）：插件用自己的 HTTP 客户端经隧道发，TLS 在插件与上游之间。
- **方案二「插件构造，核心发送」**（对比，第 5 节）：插件把构造好的一个请求作为返回值交回，核心发送。

编号沿用讨论时的叫法，所以主方案是"三"。

用量怎么上报由并行设计稿 `next/docs/DESIGN-2026-11-18-EXECUTE-USAGE-RETURN.md`（分支 `feat/usage-return`）负责，本文称"用量设计稿"，沿用它的术语：回调上报或返回值上报（含明确的 `host_usage`）、"没有上报"、先到的生效。

"现状"的每一条都带 `文件:行号`，路径相对 `next/`，行号对应 develop `123282faa`。厂商 SDK 的行号对应第 6 节写明的版本。

## 0. 结论

1. **方案三不是新概念，是现有宿主调用的推广。** 核心今天已经替插件发请求：`ForwardUpstream`（执行型插件，核心发送并把流中继给客户端）、`SendAccountRequest` 和 `ExecuteHTTP`（核心发送并把有界的回答交还插件）、续期、额度、模型列表、账号测试。缺的是三样：完整请求体交给插件；`ForwardUpstream` 接受插件给出的最终请求体；一个让厂商 SDK 跑在核心客户端上的 `http.RoundTripper` 适配器。
2. **方案三保住了今天的两项保证**：核心看得到明文请求，所以系统设置仍由核心强制；核心直接从上游读回答，所以计费证据不变。方案一做不到这两点（TLS 端到端）。沙箱不动，插件仍不联网。
3. **载体用现有的插件执行（`platform.execute.v1`）**，不另开一条路径。方案二就是方案三里"只发一个请求、不看回答"的特例，两者共用同一个消息改动：构造结果里加一个最终请求体字段。
4. **官方 SDK 在方案三下的可用程度**：非流式调用四家都能直接跑在适配器上；流式调用，OpenAI 和 Anthropic 的 SDK 可以用"SDK 构造并签名、适配器截下请求交给核心转发"的方式，流由核心直接中继给客户端；AWS 的类型化流式接口和 Google 的 `genai` 不适合，用签名器和鉴权库即可。要让 SDK 自己读流式回答，需要新增流式 RPC，而且回答要再经插件回到核心，本文放在后面的阶段。
5. **库还是各写各的**：库，但在方案三下它很薄（适配器、完整请求体辅助、签名辅助）。因为强制都在核心，插件各写各的也绕不过系统设置，这是方案三相对方案一最大的好处。
6. **分阶段**：第 1 阶段做完整请求体、最终请求体、核心侧准入、SDK 适配器，bedrock 迁移并自己签名，核心删掉 `signing`（它还没发布，现在删没有兼容负担）。后面的阶段只列提纲。
7. 减弱的保证只有一项：完整提示词进入选择加入的插件的内存（第 10 节）。需要用户回答的 11 个决定在第 15 节。

## 1. 现状

### 1.1 热路径上核心做的事

一次网关请求在核心里的顺序：`serve`（`server/internal/gateway/pipeline.go:190`）→ `run`（`pipeline.go:232`）→ `dispatch`（`dispatch.go:42`）→ `attempt`（`dispatch.go:538`）→ `forwardBuilt`（`dispatch.go:744`）→ `forward`（`forward.go:49`）。

| # | 职责 | 现在在哪里 |
|---|---|---|
| 1 | 客户端连接、API Key 鉴权 | `pipeline.go:234-246` |
| 2 | 读整个请求体进内存，上限默认 128 MiB；校验是一段 JSON | `pipeline.go:386-416`、`settings.go:27`、`pipeline.go:433` |
| 3 | 取模型、判断流式；钩子后再取一次 | `pipeline.go:257`、`pipeline.go:293` |
| 4 | Key 限额与并发 | `pipeline.go:265-278` |
| 5 | 审核与拦截钩子（请求级、尝试级），钩子可以改请求体 | `hooks.go:421`、`hooks.go:430`、`dispatch.go:133` |
| 6 | 价格、余额、预扣 | `pipeline.go:310`、`precharge.go:35` |
| 7 | 路由规划：哪些账号类型能服务这个端点（原生、经转换、本地计数） | `routing.go:95-150` |
| 8 | 调度选号、粘性会话、排队 | `dispatch.go:231-303`、`sticky.go:329` |
| 9 | 账号并发槽位、RPM/TPM 准入 | `dispatch.go:496-516`、`dispatch.go:521-535` |
| 10 | 协议转换（请求）：选号时就转换，用来判断有损策略 | `convert_policy.go:88`、`dispatch.go:573-579` |
| 11 | 账号模型映射，写回请求体 | `dispatch.go:583-591` |
| 12 | 请求体里引用的其他模型的映射 | `dispatch.go:594` |
| 13 | 强制流式（`streamOnly`）：插件前写一次、插件 patch 后再写一次 | `dispatch.go:601-607`、`dispatch.go:775-780`、`streamonly.go:39-44` |
| 14 | 请求体改写三层：声明的过滤字段与 `paramOverride` → 内置 `claude_max_tokens` → 账号 `param_override`（以 `header_override` 为初始请求头状态） | `param_override.go:82-93`、`param_override.go:68`、`param_override.go:95`、`param_override.go:107-119` |
| 15 | 取插件声明的请求字段，调 `BuildUpstreamRequest`（插件看不到整个请求体） | `dispatch.go:616-628` |
| 16 | 应用插件返回的 `BodyPatch`；校验 patch 没有破坏模型引用和用量规则 | `dispatch.go:755`、`hooks.go:614`、`dispatch.go:714-737` |
| 17 | 声明式上游的 URL 重新渲染 | `dispatch.go:760`、`param_override.go:126` |
| 18 | SSRF 检查（`netguard.CheckURL`） | `dispatch.go:764`、`ssrf.go:18-20`、`server/internal/netguard/netguard.go:49` |
| 19 | 按账号取代理的 HTTP 客户端；传输层参数（连接池、TLS 握手 15 秒、拨号时拒绝私网地址） | `dispatch.go:784`、`server/internal/proxy/proxy.go:289`、`proxy.go:177-193` |
| 20 | 请求头分层，后面的盖前面的：声明的头 → 声明的 paramOverride 头 → 透传（账号 `header_passthrough`、`header_override` 的透传键、粘性规则，只补没有的）→ `header_override` 与 `param_override` 的设值和删除 → 插件的头 → 声明的鉴权头 → 默认 Content-Type | `dispatch.go:818-833`、`dispatch.go:668-686`、`dispatch.go:693-707` |
| 21 | 请求签名（SigV4），在所有改写和请求头之后 | `dispatch.go:834`、`upstream_ext.go:74-93` |
| 22 | 模型可用性守卫 | `dispatch.go:837-851` |
| 23 | 发送：不跟随重定向、无总超时；等响应头的超时、整体超时；记录"请求是否已写出" | `dispatch.go:852-880` |
| 24 | 客户端断开的处理：上游请求不挂在客户端 context 上，由 `upstreamDrain` 决定立即取消还是读完再计费 | `dispatch.go:798-801`、`disconnect.go:66-197` |
| 25 | 流式空闲超时（默认 180 秒） | `dispatch.go:900-902`、`environment.go:45-56` |
| 26 | 额度响应头观测 | `dispatch.go:909`、`dispatch.go:976-983` |
| 27 | 上游错误：读错误体（上限 64 KiB，前 4 KiB 给插件分类）、`ClassifyError`、应用分类（冷却、禁用、自动禁用规则） | `dispatch.go:911-924`、`dispatch.go:990-991`、`dispatch.go:998-1089`、`dispatch.go:1096-1129` |
| 28 | 响应流格式转成 SSE（AWS eventstream） | `dispatch.go:925`、`upstream_ext.go:104-119` |
| 29 | 把标错内容类型的流识别成 SSE | `dispatch.go:926-928`、`forward.go:180-202` |
| 30 | 强制流式时把 SSE 收成非流式 JSON | `dispatch.go:929-933`、`streamonly.go:225` |
| 31 | 中继：SSE 逐行透传并在事件边界 flush；JSON 分 32 KiB 块透传；经转换的路由逐事件转换；Gemini 的 JSON 数组流 | `forward.go:353-495`、`forward.go:272-317`、`forward.go:322-344`、`forward.go:519-521` |
| 32 | 写客户端：每次写 60 秒超时；客户端走后吞掉输出继续读 | `client_write.go:13-55`、`disconnect.go:202-239` |
| 33 | 用量提取：按声明式规则在线读流或读 JSON（上限 32 MiB）；端点声明 `usage.source: plugin` 时截取片段事后问插件 | `forward.go:52-56`、`forward.go:499-513`、`server/internal/usagerules/usagerules.go:242-331`、`usageplugin.go:159`、`usageplugin.go:222` |
| 34 | 断开计费："最终用量事件已送达客户端"的判定依赖用量规则的 `Final()` | `forward.go:468-470`、`forward.go:76-90`、`usagerules.go:315` |
| 35 | 非流式回答在记账提交前先缓冲（内存 1 MiB，总 64 MiB，超出落临时文件） | `spool.go:14-31`、`execute.go:242-244` |
| 36 | 故障策略：重试还是换号、等待、次数上限 | `failure_policy_runtime.go:241-271`、`dispatch.go:148`、`dispatch.go:86` |
| 37 | 本地 `count_tokens`（不发上游） | `dispatch.go:568-570`、`count_local.go:35-50` |
| 38 | WebSocket：核心握手上游、逐轮中继、每 30 秒双向 ping | `websocket.go:77`、`websocket.go:688`、`websocket.go:42-60`、`websocket.go:354` |
| 39 | 使用记录、调度轨迹、尝试链路、观测事件 | `pipeline.go:825`、`sched_trace.go:57`、`attempt_trail.go:34`、`accountstats.go:12` |
| 40 | 热路径之外由核心代发的请求：账号测试、拉模型列表、凭证续期、额度查询 | `server/internal/account/testreq.go:211-246`、`account/models.go:186`、`account/refresh.go:304`、`account/quota.go:282` |

HTTP 路径上核心**没有** SSE 保活 ping：`server/internal/gateway` 里除 WebSocket（`websocket.go:42-60`）外找不到 ping 或 keep-alive 的实现。任务说明把它列为核心现有职责，与代码不符（第 12 节第 6 条）。

### 1.2 声明式引擎

`sdk/upstream` 是纯计算的引擎：把账号类型的 `upstream` 声明和预设合并，算出校验结果、上游请求、测试请求、模型列表请求和错误分类（`sdk/upstream/upstream.go:1-12`）。核心用 `server/internal/plugin/declarative` 把它包成一个 `core.PlatformPlugin`，在核心进程里代替插件进程（`declarative.go:26-58`、`declarative.go:78`、`declarative.go:142`、`declarative.go:158`）。所以 openai、gemini、relay、azure-openai 四个插件的 `manifest.json` 里 `runtime` 是 `none`，没有进程（`docs/PLUGIN-DEVELOPMENT.md:13`、`:66-67`）。

### 1.3 插件沙箱与网络

- 插件由隐藏子命令 `sub2api plugin-exec` 启动，先加限制再 `execve`（`server/internal/plugin/sandbox/exec.go:1-7`）。
- 严格网络模式下 seccomp 只允许 `AF_UNIX` 的 `socket()`，插件自己开不了 TCP/UDP（`sandbox/seccomp_linux.go:29-33`）。
- 所有对外连接走 `EgressService.Dial`：每个连接一条双向流，帧最大 32 KiB（`sdk/proto/sub2api/plugin/v1/host.proto:690-711`、`server/internal/plugin/egress/egress.go:35`）。SDK 替换 `http.DefaultTransport` 和 `net.DefaultResolver`（`sdk/pluginsdk/egress/egress.go:1-9`、`:34-36`）。
- 核心侧每个连接的检查顺序：数据库例外 → `net` 授权 → 域名策略 → 解析出的地址必须全部是公网地址且不是核心服务地址；检查过的地址就是拨号的地址（`egress.go:420-465`）。策略模式是 `allow_all` 或 `allowlist`，域名来自批准的 `net` 授权范围（`server/internal/core/ports_plugin_infra.go:106-115`）。
- **隧道今天不支持按请求选代理**：`DialOpen` 只有 `network` 和 `address` 两个字段（`host.proto:706-709`），核心直接拨目标地址（`egress.go:270-285`）。账号代理只存在于核心自己的 `proxy.Service.HTTPClient`（`proxy.go:289`）。
- TLS 在插件和目标之间端到端，核心只看到域名、端口和流量（`docs/ARCHITECTURE.md:1237`）。
- 资源限制：`GOMEMLIMIT` 设为上限的 90%，`GOMAXPROCS` 设为 `ceil(cpu)`（`sandbox/launcher.go:93-94`、`:169`）；每 5 秒采样，连续 3 次超过上限 110% 就重启插件（`launcher.go:21-27`、`server/internal/plugin/grpcruntime/instance.go:526-530`）；`oom_score_adj` 1000、nice +10（`sandbox/exec.go:26-30`）。bedrock 和 codex 申请的是 64 MiB、0.1 CPU、64 个文件描述符（`plugins/bedrock/manifest.json:289-294`、`plugins/codex/manifest.json:172`）。
- 调用并发按类别分开：热路径 64、控制台 16、后台 8、执行 1024（`grpcruntime/runtime.go:112-142`）。

### 1.4 契约规则

`sdk/proto/sub2api/plugin/v1/common.proto:7-12`：

- 除 `EgressService.Dial` 外每个 RPC 都是一元调用；
- 网关热路径上请求体从不整个发给插件：插件声明要读的字段，用 `BodyPatch` 指令回答。

理由写在同一处：让契约对将来的 JS 运行时仍然可用。相应地，`upstream_ext.go:6-9` 说"只有核心能签名，因为签名覆盖请求体，而插件从来看不到整个请求体"；SDK 的 `ExecuteDefault` 注释说"它从不打开套接字"（`sdk/pluginsdk/execute.go:99-102`）；`docs/PLUGIN-DEVELOPMENT.md:76` 要求插件"不要自己连上游"。

gRPC 单条消息默认上限 4 MiB，插件可以用 `WithMaxRecvMsgSize` 调大（`sdk/pluginsdk/serve.go:80-84`）。

### 1.5 最接近的先例：插件执行

`platform.execute.v1`（codex、volcengine 在用，`docs/PLUGIN-DEVELOPMENT.md:14`）：

- 核心调 `Execute`，时限 6 小时加整体超时（`execute.go:89-92`）；
- 插件在 `Execute` 里回调 `ForwardUpstream`，核心用同一段 `forwardBuilt` 发送和中继（`execute.go:169-190`、`sdk/pluginsdk/execute.go:40-52`）；
- 插件只拿到有界的用量观测，不是整个流（`host.proto:339-347`）；
- 插件 `RecordUsage`，核心提交；回调以执行令牌鉴权，令牌绑定进程、账号和这次调用（`grpcruntime/execute.go:38-76`、`host.proto:45-49`）；
- 核心在 `forwardBuilt` 里自己记下"请求已发出"（`dispatch.go:872-874`），插件崩溃后据此判断能不能换号（`execute.go:151-166`）。

它证明了"核心选号、插件主导一次尝试、核心裁决重试"的分工可行。它没有做的两件事正是本设计要做的：请求体给插件、插件自己发。


### 1.6 出口隧道的细节（方案一要用）

- SDK 一侧：`egress.Install` 克隆 `http.DefaultTransport`，把 `Proxy` 置空、`DialContext` 换成隧道拨号，注释写明"宿主按名字拨号，环境里的代理不生效"；`net.DefaultResolver` 改成连 `dns.sub2api:53`（`sdk/pluginsdk/egress/egress.go:23-24`、`:34-70`）。
- 什么时候安装：默认只在宿主设置了严格网络环境变量时安装（`sdk/pluginsdk/serve.go:42-45`、`:358-366`、`:386`）。非 Linux 上严格网络和 seccomp 都关闭（`server/internal/config/config.go:253-255`），插件直接开套接字，不经隧道。
- 宿主一侧（`server/internal/plugin/egress/egress.go:208-316`）：读 `open` 帧 → 校验地址 → 取这个**插件**的出口策略 → `dns.sub2api:53` 走进程内的 DNS 应答（`:246-258`）→ 其他地址经 `check`（`:430-465`）→ 用 `opts.Dial` 直接拨解析出的地址（`:270-285`）→ 双向转发并记日志。
- **隧道里没有账号的概念**：`ServerFor(pluginKey, policy)` 只按插件键区分（`egress.go:187`），`DialOpen` 只有网络和地址（`sdk/proto/sub2api/plugin/v1/host.proto:706-709`）。**也不套任何代理**：账号代理只在 `proxy.Service.HTTPClient` 里（`server/internal/proxy/proxy.go:289`），隧道不调用它。
- 隧道服务只给有 `net` 授权或自带数据库的插件注册（`server/internal/plugin/grpcruntime/instance.go:452-456`），与是否严格模式无关。
- `net` 授权的风险级别是 high（`sdk/manifest/manifest.go:1166`）。清单的 `externalServices` 是审查时展示的主机列表，也是 `SendAccountRequest` 允许访问的主机范围（`sdk/manifest/manifest.go:58`、`host.proto:233-243`）。

### 1.7 gRPC 消息大小（三个方案都要用）

- 核心调用插件：go-plugin 的客户端把单次调用的收、发上限都设成 `math.MaxInt32`（`github.com/hashicorp/go-plugin@v1.8.0/grpc_client.go:40-41`；版本见 `server/go.mod:25`）。
- 插件的 gRPC 服务端：默认只收 4 MiB，插件用 `WithMaxRecvMsgSize` 调大（`sdk/pluginsdk/serve.go:80-84`、`:110-113`）；发送没有另设上限。
- 插件回调核心（`HostService`、`EgressService`）：核心侧的服务端是 `grpc.NewServer(opts...)`，没有调大接收上限（`server/internal/plugin/grpcruntime/instance.go:459`），所以插件发给核心的单条消息默认最多 4 MiB。
- 网关请求体上限默认 128 MiB，环境变量 `max_request_body_mb` 可以调高（`server/internal/gateway/settings.go:27`、`pipeline.go:386-394`）。
- 网关只接受 JSON 请求体：非空的请求体必须是合法 UTF-8 的一段 JSON，否则 400（`pipeline.go:433-451`）。所以 multipart、二进制上传今天进不了网关流水线，本设计不涉及它们（第 12 节第 7 条）。
- 先例：审核钩子已经把整个顶层字段（`messages|@this` 等）发给审核插件，授权 `gateway.hook` 风险 high，插件自己按字节数做并发准入（默认 128 MiB）来防内存超限（`docs/CONTRACTS.md:4188`、`:4176`）。
- `BuildUpstreamRequest` 的时限是 `platform_call_timeout_ms`，默认 2000，范围 100 到 30000（`settings.go:70`、`:78-79`、`dispatch.go:626`）；它占用"热路径"并发类，每个插件实例 64（`grpcruntime/runtime.go:116-118`）。

### 1.8 发布状态

`signing`、`response_stream` 和 bedrock 插件还没有发布：`CHANGELOG.md` 把它们放在"未发布"一节，bedrock 0.1.0 要求核心 0.1.304（`CHANGELOG.md:9-22`、`plugins/bedrock/manifest.json:12-21`）；已有的核心 tag 到 `core-v0.1.303`；`plugins/released.json` 里没有 bedrock。也就是说现在改这两个扩展点没有线上兼容负担。（`CHANGELOG.md:13` 写"分支 `feat/cloud-providers`，未合并"，实际已合并进 develop，提交 `9a503d974`，这句过时了。）

## 2. 三个方案共同的部分：完整请求体交给插件

### 2.1 选择加入

清单里账号类型的平台条目（`AccountPlatform`，`sdk/manifest/manifest.go:555-565`）加一个字段：

```json
{ "platform": "anthropic", "requestBody": "full" }
```

- 不写或 `"fields"`：今天的行为，`requestFields` 加 `BodyPatch`。只有清单的插件和简单插件不受影响。
- `"full"`：核心把改写后的完整请求体交给插件。
- 清单检查拒绝的组合：`requestBody: "full"` 与声明式上游（`upstream`）同时出现；用在 WebSocket 端点上（握手没有请求体，逐轮消息走另一套补丁，`websocket.go:821-855`）。
- 需要新的宿主权限，见第 10 节。

### 2.2 交出之前核心做什么

现有顺序（`dispatch.go:538-628`）不变，全部发生在调用插件之前：

| 步骤 | 位置 | 说明 |
|---|---|---|
| 协议转换（客户端协议 → 上游协议） | `dispatch.go:571-579`，转换发生在选号时（`convert_policy.go:88`） | 插件拿到的永远是**上游协议**的请求体 |
| 账号模型映射写回请求体 | `dispatch.go:583-591` | |
| 引用的其他模型映射 | `dispatch.go:594` | |
| 强制流式 | `dispatch.go:601-607` | 插件之前写好 `stream: true` |
| 声明的过滤字段与 `paramOverride`、内置 `claude_max_tokens`、账号 `param_override` | `dispatch.go:612`、`param_override.go:82-93` | 请求头覆盖是参数覆盖的初始请求头状态，同一次运行算出请求头的决定（`param_override.go:107-119`） |
| 取 `requestFields`、组装调用 | `dispatch.go:616-619` | `full` 模式下 `fields` 照常给，另带整个请求体 |

每次尝试都从头走一遍（`attempt` 按账号调用，带尝试序号，`dispatch.go:146`、`:619`），所以换号后请求体按新账号重新改写、重新交给插件。

**插件交回或发出的字节是最终的**。核心之后不再改请求体：今天插件之后的那一次强制流式（`dispatch.go:775-780`）在 `full` 模式下不执行，保持流式标志是插件的责任（请求元数据里有 `stream`，`common.proto:116`）。

这一点与"不要来核心转换"有出入：转换和请求体改写仍在核心里。理由是它们产生的请求头决定、有损策略判断、计费用的上游模型名都是核心要用的，而且这样系统设置里请求体那一半仍由核心强制（第 12 节第 2 条，决定 3）。

### 2.3 大小、拷贝、时限、内存

- **通道**：一元调用就够，不需要流式。核心发给插件没有上限问题（1.7 第一条）；插件一侧 SDK 在插件声明 `full` 时自动把接收上限调到"完整请求体上限"加余量。
- **完整请求体上限**：核心加一个常量（建议 64 MiB），超过它的请求不交给 `full` 模式的账号类型，这次尝试以中性原因换号（不冷却账号），都不行时回 413。理由：请求体上限可以被环境变量调到任意大（`pipeline.go:386-394`），而插件内存有上限。
- **拷贝**（N MiB 的请求体，一次尝试）：核心序列化 1 次、插件反序列化 1 次；方案二、方案三还要插件序列化、核心反序列化各 1 次。加上插件改写请求体自己的拷贝。插件进程里的瞬时峰值约 2N（方案一）到 4N（方案二、方案三）。
- **耗时估算**（没有测量，按内存拷贝每毫秒约 1 MiB 的量级估）：1 MiB 请求体增加 1 到 4 毫秒，20 MiB 增加几十毫秒，64 MiB 增加一两百毫秒。默认 2 秒的调用时限对上限内的请求体够用，机器繁忙时大请求体会接近它（未验证事项 1）。
- **内存**：bedrock 申请 64 MiB（`plugins/bedrock/manifest.json:289-294`），看门狗在连续 15 秒超过 110% 时重启插件（`sandbox/launcher.go:21-27`）。几个十来 MiB 的请求同时到达就会超。两项对策：迁移的插件提高 `memoryMB`；核心按插件实例做"在途完整请求体字节数"的准入（预算取插件内存上限的一个比例，超出时排队到调用时限、再不行就中性换号），做法照搬审核插件的 `max_inflight_mb`，但放在核心一侧，因为核心在发送前就知道大小。
- **并发类**：方案二的 `BuildUpstreamRequest` 仍占热路径类（64）；大请求体让单次调用变慢，准入预算同时起到保护这个并发类的作用。

### 2.4 契约文字怎么改

`common.proto:11-12` 现在写"网关热路径上请求体从不整个发给插件"。改为：

> 默认不整个发给插件：插件声明要读的字段，用 `BodyPatch` 回答。账号类型在清单里声明 `requestBody: "full"` 并获得 `gateway.body` 授权时，宿主把改写后的完整请求体交给它，大小不超过宿主的完整请求体上限。

"除 `EgressService.Dial` 外每个 RPC 都是一元调用"在方案二下不用改；方案一要加一条流式通道，改法见 4.2。

## 3. 方案三（主方案）：核心提供 HTTP 客户端

```
客户端 ─► 核心：鉴权…选号、占槽、协议转换、请求体改写
            │ Execute（完整请求体、执行令牌）
            ▼
          插件：自己构造上游请求（可以用厂商 SDK 构造、签名）
            │ 调用核心的客户端（宿主调用，带执行令牌）
            ▼
          核心：允许的主机 → SSRF → 请求头分层 → 账号代理 → 发送
            ├─ 方式 (i)：核心把回答中继给客户端，按规则读用量，只把有界的观察交还插件
            └─ 方式 (ii)：核心把回答交还插件（多步流程、需要看回答内容时）
```

插件不开套接字。TLS 在核心与上游之间，核心看得到明文的请求和回答。

### 3.1 已经有什么，各自的限制

| 已有的宿主调用 | 做什么 | 限制 | 位置 |
|---|---|---|---|
| `ForwardUpstream` | 执行型插件把构造结果交给核心，核心用 `forwardBuilt` 发送，流直接中继给客户端，插件只得到有界的用量观察或错误事实 | 只有执行型插件；只收 `BuildUpstreamRequestResponse`（请求体留在核心，插件只能给补丁）；**一次执行只能调一次** | `sdk/pluginsdk/execute.go:40-52`、`server/internal/gateway/execute.go:169-206`、`host.proto:334-347`、`grpcruntime/execute.go:81-85` |
| `SendAccountRequest` | 核心经账号的代理替插件发一个请求，回答交还插件 | 请求体最多 64 KiB，URL 最多 8 KiB，回答截到 64 KiB；主机必须在清单 `externalServices` 之内；不跟随重定向；需要 `accounts.credentials` 授权；每次调用审计 | `sdk/pluginsdk/hostcore.go:91-102`、`host.proto:233-250`、`grpcruntime/host.go:47-49`、`:632-661` |
| `ExecuteHTTP` | 任务轮询期间的一次有界观察，凭执行令牌，核心套账号代理、限制和 SSRF | 一个令牌只许一次请求；只在轮询和监控里可用 | `host.proto:226-231`、`:370-385` |
| 续期、额度、模型列表、账号测试 | 插件描述请求，核心发送，插件解析回答 | 各自的成对 RPC，不在热路径上 | `server/internal/account/refresh.go:304`、`account/quota.go:282`、`account/models.go:186`、`account/testreq.go:211-246` |

所以"核心替插件做 HTTP 交换，套上账号代理和 SSRF"今天已经有四种形态。方案三把它们补成一个通用的客户端。

### 3.2 载体：复用插件执行

方案三实际上是"选择加入的平台插件都按执行型插件的方式工作"。载体就用 `platform.execute.v1`（1.5），不另造一条路径：`Execute` 调用、执行令牌、6 小时时限、执行类并发、`ForwardUpstream`、用量上报的回调和返回值都是现成的。

走执行路径相对普通转发路径多出的东西要说清楚：

- 每个请求在 `plugin_executions` 里多写一行并更新两次（开始、观察、提交，`gateway/execute.go:181-186`、`:268-280`、`:324-329`）；
- 非流式回答在记账提交之前先缓冲，上限 64 MiB（`execute.go:241-249`、`spool.go:14-15`）；
- **插件放弃或崩溃会切断它正在进行的 `ForwardUpstream`**，流记为上游中断（`dispatch.go:789-805`、`execute.go:47-50`）。普通转发路径上流在核心手里，插件崩溃不影响它。这是 codex、volcengine 今天已有的行为；
- 平台插件与账号类型插件不是同一个、且端点由插件解析用量或是任务提交时，不走执行路径（`execute.go:59-66`）。

只发一个请求、不需要看回答的插件不必付这些代价：它们可以留在普通转发路径上，在 `BuildUpstreamRequest` 的返回值里给出最终请求体，这就是方案二（第 5 节）。两者用同一个消息字段（3.3 的 b）。

### 3.3 要加什么

**(a) 完整请求体交给插件。** 第 2 节。执行路径上它在 `ExecuteRequest.request` 里（`platform.proto:197-200`）。

**(b) `ForwardUpstream` 接受插件给出的最终请求体。** `BuildUpstreamRequestResponse` 加 `optional bytes body = 8`：设了就取代宿主的请求体，此时 `patches` 必须为空，否则这次尝试按插件不可用失败、不发请求。`ForwardUpstreamRequest` 里就是这个消息（`host.proto:334-337`），`BuildUpstreamRequest` 的返回值也是它，所以一处改动同时服务方案三和方案二。插件发给核心的单条消息默认上限 4 MiB（1.7），核心侧的服务端要把接收上限调到"完整请求体上限"加余量。

**(c) 方式 (ii)：回答交还插件。**

- 第 1 阶段：有界、非流式。把 `ExecuteHTTP` 的适用范围扩到 `Execute` 期间（同一个消息 `ExecutionHTTPRequest`/`ExecutionHTTPResponse`），请求体上限用完整请求体上限，回答上限建议 8 MiB，超出时带 `truncated`。一次执行里可以调多次，次数有上限（建议 8）。
- 流式变体（插件自己读上游的流）：需要一条新的流式 RPC（核心到插件的回答分帧），`common.proto` 的规则要改成"除 `EgressService.Dial` 和这一条外都是一元调用"。**而且只有它还不够**：插件读了流之后，内容要到客户端还得再有一条"插件写给客户端"的通道，回答的路径变成 上游 → 核心 → 插件 → 核心 → 客户端。本文不放在第 1 阶段，是否要做是决定 4。

**(d) 一次执行里发多个请求。**

- 方式 (ii) 的请求可以有多个；方式 (i) 的转发最多一个（现有约束，`grpcruntime/execute.go:81-85`）。
- **计费的回答是被转发的那一个**：核心从它读用量。插件按用量设计稿回答 `host_usage`（采用核心读数）或自己上报。
- 方式 (ii) 的请求如果在上游也产生费用，核心不读它们的用量，由插件在上报里合并进去。这部分是插件说了算的数字，但核心看到了这些请求的地址、大小和状态码，记在这次执行的轨迹里，可以对账。
- 方式 (ii) 的请求失败时，事实（状态码、错误体、传输错误、请求是否已写出）是核心自己看到的。插件在 `ExecuteResponse.classification` 里给分类（今天这个字段只用于 `ForwardUpstream` 的错误，`platform.proto:202-206`），核心照旧按故障策略裁决。

**(e) SDK 里的 `http.RoundTripper` 适配器**（暂名 `sdk/pluginsdk/hosthttp`），让厂商的官方 SDK 客户端不改代码就跑在核心的客户端上。两种工作方式：

- **往返方式**：`RoundTrip` 把请求交给方式 (ii)，把核心交还的回答包成 `http.Response`。适用于非流式调用。流式调用需要边读边到的响应体，要等 (c) 的流式变体。
- **截取转发方式**：SDK 只用来构造和签名。适配器截下 SDK 构造好的 `http.Request`，交给 `ForwardUpstream`（方式 (i)），核心发送并把流直接中继给客户端；适配器给 SDK 一个合成的回答（状态码取自核心返回的观察，响应体为空或错误体前缀）。这样流不经过插件，响应一侧与今天相同。

  这种方式是否可行取决于 SDK 会不会去解析那个合成的回答：按原始模式调用的 SDK 不解析，可行；类型化的流式接口会去解码响应体，要伪造一个合法的空流，不值得。逐家结论在 6.6。

**(f) 签名搬到插件。** 插件有了最终请求体字节，可以自己签。核心的 `signing` 指令在 bedrock 迁移后退役，分析与方案二相同，见 5.3。`response_stream`（eventstream 转 SSE）：方式 (i) 下留在核心，因为是核心在中继；方式 (ii) 的流式变体下才会移到插件。

### 3.4 运营方设置的顺序

| 时机 | 核心做什么 | 位置 |
|---|---|---|
| 交出请求体之前 | 协议转换、模型映射、强制流式、过滤字段、声明的 `paramOverride`、`claude_max_tokens`、账号 `param_override` 的请求体部分 | 2.2 |
| 经方式 (i) 发送时 | 请求头分层照旧：透传（只补没有的）→ `header_override` 与 `param_override` 的设值和删除 → 插件给的头 → 没有 Content-Type 时补默认值；SSRF、账号代理、三个超时、断开处理 | `dispatch.go:764`、`:784`、`:824-833`、`:861-871` |
| 经方式 (ii) 发送时 | 允许的主机（清单 `externalServices`，或该账号设置里的上游主机）、SSRF、账号代理、超时、大小上限、不跟随重定向、记日志。**不套**请求头透传和覆盖：这些设置定义的是"客户端请求转给上游时"的行为，辅助请求（换令牌、上传、预检）不是那个请求 | 沿用 `SendAccountRequest` 的规则，`grpcruntime/host.go:632-661` |
| 发送之后 | 请求体不再改。今天插件之后的第二次强制流式（`dispatch.go:775-780`）在插件给了最终请求体时不执行 | |

**插件自己签名时的冲突**：SigV4 实现只签 `host`、`x-amz-date`，以及存在时的 `content-type`、`x-amz-security-token`（`sdk/reqsign/sigv4.go:10-13`、`:106-112`），加请求体的哈希。插件的头在透传和覆盖之后写入（`dispatch.go:829`），所以运营方的设置改不掉这四个头；其余的头不在签名范围内，核心照常加减。逐项核对在 5.3，结论相同。

**插件和库不该做的事**：不自己开套接字；不重试（重试和换号是核心按故障策略决定的）；不改已经交给核心的请求体；不把运营方透传或覆盖的头再覆盖回去（除了它必须掌握的凭证头和被签名的头）；辅助请求不带客户端的请求头。

### 3.5 计费

按用量设计稿，选择加入的插件成为执行型插件，适用它的规则：回调或返回值上报，没有上报就是零成本记录并释放预扣，先到的生效。方式 (i) 下核心照旧按声明式规则从上游回答里读用量，插件回答 `host_usage`（SDK 的 `ExecuteDefault` 默认就是这样）即可，读数仍是核心直接从上游看到的，证据不变。留在普通转发路径上的插件（方案二的形态）计费完全不变，用量设计稿不涉及它们。

### 3.6 1.1 表里的每项职责在三个方案下的去向

"核心"表示留在核心、行为不变。编号是 1.1 表的行号。

| 职责（1.1 的行号） | 方案三 | 方案二 | 方案一 |
|---|---|---|---|
| 客户端连接、鉴权、读请求体、模型与流式判断、Key 限额、钩子、价格与预扣（1 至 6） | 核心 | 核心 | 核心 |
| 路由规划、调度、粘性、并发槽位与限流（7 至 9） | 核心 | 核心 | 核心 |
| 协议转换（10） | 核心，交出请求体之前；响应在中继里 | 同左 | 同左（做法 A） |
| 模型映射、强制流式的第一次、请求体改写三层（11 至 14） | 核心，交出请求体之前 | 同左 | 同左 |
| `requestFields`、`BodyPatch`、补丁应用（15、16） | 选择加入的类型不再用补丁；守卫保留 | 同左 | 同左 |
| 声明式 URL 渲染（17） | 核心（只有清单的插件不变） | 同左 | 同左 |
| SSRF（18） | 核心，发送时 | 核心，发送时 | 核心，换到隧道拨号时 |
| 账号代理（19） | 核心的传输层 | 核心的传输层 | 核心，隧道里按令牌选代理（新代码） |
| 连接池、TLS（19） | 核心 | 核心 | **插件** |
| 请求头分层（20） | 核心 | 核心 | 核心算出决定，**库**落实 |
| 请求签名（21） | **插件**；核心的 `signing` 删除 | **插件**；同左 | **插件** |
| 模型可用性守卫（22） | 核心 | 核心 | 核心（调用前） |
| 发送、三个超时、"请求是否已写出"（23） | 核心 | 核心 | 发送和"已写出"在**插件**；超时核心兜底 |
| 客户端断开的裁决、空闲超时（24、25） | 核心 | 核心 | 核心（取消的是回传流） |
| 额度响应头观测、错误体读取、应用分类（26、27） | 核心 | 核心 | 核心（事实由插件经回传通道交来） |
| 错误分类本身（27） | 插件（现有 `ClassifyError`，执行路径在 `Execute` 内） | 插件（现有 `ClassifyError`） | 插件 |
| 响应流格式转 SSE、SSE 嗅探、强制流式收集（28 至 30） | 核心 | 核心 | 核心，或插件自己解码后回传 |
| 中继、写客户端（31、32） | 核心 | 核心 | 核心（做法 A） |
| 用量读取、断开计费判定、非流式缓冲（33 至 35） | 核心 | 核心 | 核心读插件交回的字节 |
| 故障策略、重试、换号（36） | 核心 | 核心 | 核心 |
| 本地 `count_tokens`（37） | 核心 | 核心 | 核心 |
| WebSocket（38） | 不在范围内，不变 | 同左 | 同左 |
| 记录、轨迹、观测（39） | 核心，另记方式 (ii) 的请求 | 核心 | 核心，另记带令牌的隧道连接 |
| 账号测试、模型列表、续期、额度查询的发送（40） | 核心，不变；将来可以并入方式 (ii) | 核心，不变 | 核心，不变 |

方案三和方案二下，从核心搬到插件的只有"厂商特有的请求构造"和签名；方案三另外让插件能编排多个请求。"通用转发"里的传输、中继、用量、断开、故障策略都留在核心。这比用户最初说的"把通用转发逻辑搬到插件"搬得少，原因是用户随后明确了核心提供客户端、插件不联网；差别记在第 12 节第 2 条。

## 4. 方案一（对比）：插件发送，流量经核心隧道

```
客户端 ─► 核心：鉴权…选号、占槽、转换与请求体改写
            │ Execute（完整请求体 + 核心算好的请求头决定 + 执行令牌）
            ▼
          插件：构造请求 → 厂商 SDK 或 HTTP 客户端发送
            │ 出口隧道（拨号带执行令牌）
            ▼
          核心隧道：按令牌找到账号 → 套账号代理 → SSRF 检查 → 拨号，TLS 在插件与上游之间
            ▼ 上游
          插件收到回答 ─► 回传通道（流式）─► 核心：中继、转换、读用量、断开处理 ─► 客户端
```

入口复用插件执行（`platform.execute.v1`，1.5）：核心调 `Execute`，今天插件在里面回调 `ForwardUpstream` 让核心发；方案一里插件自己发，再把回答交给核心。执行令牌、6 小时时限、执行类并发 1024、用量上报的三个回调都已经有了。

### 4.1 网络：隧道要补什么

隧道今天不知道账号、不套代理（1.6）。设计：

- `DialOpen` 增加可选的执行令牌。令牌就是现有的执行令牌（绑定进程、账号、这次调用，调用结束失效，`grpcruntime/execute.go:38-76`）。
- 带令牌的拨号，核心一侧：
  1. 由令牌得到账号，取账号的代理；
  2. 目标主机必须在插件清单的 `externalServices` 之内（或其子域），与 `SendAccountRequest` 的规则相同；账号可以自填上游地址的类型，另外允许等于该账号设置里的上游主机；
  3. SSRF：与网关相同的规则。直连时检查解析出的地址（隧道已有，`egress.go:457-463`）；经代理时检查名字（与 `ssrf.go:15-17` 相同），由代理解析；"允许私网上游"用网关那个开关；
  4. 有代理：核心连到代理，HTTP 代理发 CONNECT，socks5 走握手，之后转发字节。`proxy.Service` 今天只给出 `*http.Client`（`proxy.go:289`），要加一个"经某个代理拨一条 TCP 连接"的方法；
  5. 记出口日志时带上账号。
- 不带令牌的拨号：行为不变（插件的后台任务、数据库）。
- SDK 库提供带令牌的 `http.RoundTripper`，令牌从 `Execute` 的 context 里取。厂商 SDK 通过注入 `*http.Client` 使用它（第 6 节）。
- **非 Linux、非严格模式**：插件能直接开套接字，默认传输层也不走隧道（1.6）。库的 RoundTripper 不看严格模式，总是经隧道拨号，所以用库的插件在开发模式下也走账号代理；不用库而直接开套接字的插件会绕过代理和 SSRF 检查。生产环境只有 Linux 严格模式，内核会拒绝直接开套接字（`sandbox/seccomp_linux.go:29-33`）。
- **TLS 端到端在插件与上游之间**（`docs/ARCHITECTURE.md:1237`）。后果：这条路径上核心看不到也改不了上游请求和原始回答，只看到域名、端口、字节数；请求头是否按设置发出，核心无法核对；TLS 的 CPU 落在插件进程。
- `net` 授权：发送插件必须有。带令牌的连接按上面第 2 条限制主机，所以 `net` 的域名范围不必为了转发放宽到 `*`。

### 4.2 回答怎么回到客户端

契约今天除 `EgressService.Dial` 外没有流式 RPC。两种通道：

| | 新增一条流式 RPC（建议） | 复用 `Dial` |
|---|---|---|
| 做法 | `HostService` 上加 `RelayUpstream`：插件到核心的字节流，第一帧带执行令牌、状态码和响应头，其后是数据帧，最后是结束帧（正常结束或传输错误） | 插件 `Dial` 一个保留地址（像 `dns.sub2api:53` 那样在核心进程内应答，`egress.go:246-258`），往里写一个 HTTP/1.1 响应 |
| 契约 | `common.proto` 的规则改成"除 `EgressService.Dial` 和 `HostService.RelayUpstream` 外都是一元调用" | 规则文字不用改 |
| 评价 | 含义明确，帧里能带"事件边界""请求已写出"等信息 | 不改 proto，但把一个网络拨号接口挪作他用，令牌、结束原因都要塞进字节流里 |

**回传什么：两种做法**

- **做法 A（建议）：回传上游的原始回答**（状态码、响应头、未加工的响应体字节）。核心把它包成一个 `http.Response`，交给今天的 `execution.forward` → `forward()`（`execute.go:224-266`、`forward.go:49`）。于是下面这些全部不变：SSE 逐事件 flush、协议转换成客户端协议、Gemini 数组流、强制流式的收集、按声明式规则读用量、`response_stream` 的 eventstream 解码（插件也可以自己解码后回传 SSE）、非流式回答在记账提交前缓冲、断开计费、空闲超时。
- **做法 B：回传已经是客户端协议的字节，插件自己读用量并上报**。这是"通用转发全部搬出核心"的终态，需要把转换包（13366 行）、用量规则包（1989 行）搬进 SDK，核心不再读流。本文不把它作为近期目标（第 8 节）。

以下按做法 A：

- **flush 延迟**：每个数据帧到达即交给 `forward()` 的读取循环，循环按事件边界 flush（`forward.go:463-465`），与今天一样；多出的是帧的封装和一次进程间传输。
- **背压**：客户端慢 → 核心不读流 → gRPC 流控让插件的写阻塞 → 插件不再读上游。
- **客户端断开**：今天的 `upstreamDrain` 取消的是上游请求的 context（`dispatch.go:798-801`）。方案一里它取消的是回传流和 `Execute` 调用，库据此取消上游请求。"读完再计费"策略下不取消，核心继续读流并丢弃输出，机制不变。
- **保活 ping**：HTTP 路径上核心今天没有（1.1 末尾）。如果要加，放在核心写客户端的地方，对三个方案都一样，不属于本设计。
- **空闲超时、整体超时、等响应头超时**：都在核心。空闲超时包在回传流的读取上（今天是 `newIdleBody`，`environment.go:58-68`）；到点取消调用。库在插件一侧可以再设一层，不是必须。
- **核心还能观察什么**：做法 A 下核心读到的就是上游的回答，所以用量照旧按声明式规则读，断开计费照旧。这与"核心不兜底"不冲突：按用量设计稿，声明式读数对非执行型插件是首要机制；对执行型插件，插件用 `host_usage`（或 `RecordUsage(nil)`）明确表示"采用核心读数"，或者自己上报。要说明的是：这份读数不再是独立证据，因为字节是插件交来的，核心没有从上游直接看到（第 7 节）。

### 4.3 运营方的设置，逐项

| 设置 | 谁执行 | 说明 |
|---|---|---|
| 模型映射、过滤字段、声明的 `paramOverride`、`claude_max_tokens`、账号 `param_override` 的请求体部分、强制流式 | 核心，交出请求体之前 | 2.2 |
| 协议转换 | 核心（请求在交出之前，响应在做法 A 的中继里） | |
| 请求头透传（账号 `header_passthrough`、`header_override` 的透传键、粘性规则） | **核心算出结果，库落实** | 核心按今天的规则把要透传的头和值算好（`dispatch.go:668-686`），放进调用里 |
| `header_override` 与 `param_override` 的请求头设值和删除 | **核心算出结果，库落实** | 结果是一组"设为某值、删除某头"（`core.HeaderOverride`），今天在 `dispatch.go:828` 应用 |
| 请求头的层次顺序、禁止覆盖的头、凭证头最后写 | **库** | 顺序照 `dispatch.go:818-833`：先透传（只补没有的），再覆盖的设值和删除，再插件自己的头，凭证最后 |
| 去掉厂商 SDK 自带的标识头、关重试 | **库** | 第 6 节 |
| 请求体上限、三个超时、并发、限流、粘性、故障策略、计费 | 核心 | 不变 |

请求体那一半仍由核心强制。请求头这一半核心算好、库落实，核心看不到发出的请求，无法核对。这是"核心强制"降为"库保证加一致性测试"，范围限于请求头。

### 4.4 故障策略

每次 `Execute` 是一次尝试，核心照旧裁决（`dispatch.go:146-148`、`failure_policy_runtime.go:241-271`）。

- 上游回了 HTTP 错误：插件把状态码、响应头、错误体经回传通道交给核心，核心按今天的路径分类（`dispatch.go:911-924`，执行路径的 `execution.classify`，`execute.go:207-222`）。故障策略下 HTTP 错误仍取插件分类里的额度观测和限时禁用（`dispatch.go:1039-1047`）。
- 传输错误（没连上、超时、连接被重置）：插件在结束帧里报告错误文本和"请求是否已写出"。故障策略下传输错误完全由策略决定（`dispatch.go:1001-1003`、`execute.go:191-198`）。"请求是否已写出"今天由核心的 `httptrace` 得到（`dispatch.go:875-883`），以后由库的 RoundTripper 得到并上报；隧道也能佐证：带令牌的连接上有没有出过字节。
- 厂商 SDK 的自动重试必须关掉，否则一次尝试打上游多次，槽位、RPM、故障策略的次数都不对。

### 4.5 崩溃与资源

- 今天插件崩溃只丢决策调用，在途的流在核心手里继续。方案一里**插件崩溃、内存超限重启、升级换代都会切断该插件所有在途的流**；已经给客户端写过字节的请求不能换号。排空时限只有 30 秒（`grpcruntime/runtime.go:105`），长流跨不过去，要延长到整体超时或接受切断。
- 崩溃时核心怎么判断能不能换号：隧道上这个令牌的连接没有出过字节，可以安全换号；出过字节但没收到响应头，按"请求可能已到上游"处理；已给客户端写过字节，记为上游中断。
- 内存：请求体在插件里常驻到尝试结束（2.3）。CPU：`GOMAXPROCS` 是 `ceil(cpu)`（`sandbox/launcher.go:169`），申请 0.1 的插件只有 1 个 P，TLS 加解密全在它上面；nice +10（`sandbox/exec.go:26`）。发送插件要申请更高的 `cpu` 和 `memoryMB`。
- 连接池：今天每个代理一个连接池，全部账号类型共用（`proxy.go:177-193`）。方案一里连接池在各插件进程里，库要按"目标主机 + 账号代理"分池，不同代理的账号不能共用连接。
- 每 10 分钟重启超过 5 次，插件在该节点被标记不可用（`grpcruntime/runtime.go:98-104`），以后等于该节点上这个厂商的全部账号不可用。

### 4.6 计费

按用量设计稿：执行型插件用回调或返回值上报，`host_usage` 表示采用核心读数，没有上报就是零成本记录并释放预扣，先到的生效。做法 A 下 `host_usage` 的读数来自核心对回传流的读取，现有代码不用改。

## 5. 方案二（对比）：插件构造，核心发送

```
客户端 ─► 核心：鉴权…选号、占槽、转换与请求体改写
            │ BuildUpstreamRequest（完整请求体）
            ▼
          插件：构造方法、URL、请求头、最终请求体字节（需要时自己签名）
            │ 返回
            ▼
          核心：SSRF 检查 → 请求头分层 → 账号代理 → 发送 → 中继、转换、读用量、断开处理 ─► 客户端
```

插件不开套接字；响应一侧完全不变。

### 5.1 契约

- `BuildUpstreamRequestRequest` 加 `bytes body = 7`：`full` 模式下是改写后的完整请求体，否则不设。
- `BuildUpstreamRequestResponse` 加 `optional bytes body = 8`：设了就取代宿主的请求体；此时 `patches` 必须为空，否则这次尝试按插件不可用失败、不发请求（与无法应用的补丁同样处理，`dispatch.go:755-758`）。
- 执行型插件经 `ForwardUpstream` 走同一段 `forwardBuilt`（`execute.go:190`），`ForwardUpstreamRequest` 里就是这个消息（`host.proto:334-337`），所以同样生效；但插件发给核心的消息默认上限 4 MiB（1.7 第三条），核心侧的服务端要调大接收上限。
- Host API 版本从 9 升到 10（`runtime-contract/contract.go:14`、`sdk/protocol/protocol.go:19`）。
- `common.proto` 的文字按 2.4 改。

### 5.2 构造之后核心还做什么

`forwardBuilt`（`dispatch.go:744-941`）在 `full` 模式下：

| 步骤 | 现在 | `full` 模式 |
|---|---|---|
| 应用补丁 | `dispatch.go:755` | 跳过，用插件的请求体 |
| 第二次强制流式 | `dispatch.go:775-780` | **跳过** |
| 补丁守卫（模型引用一致、用量规则能计量） | `dispatch.go:781`、`:714-737` | 保留，拿交出的请求体与交回的请求体比 |
| SSRF、账号代理、发送、超时、断开、中继、用量 | `dispatch.go:764`、`:784`、`:798-940` | 不变 |
| 请求头分层 | `dispatch.go:824-833` | 不变 |
| 模型可用性比对 | `dispatch.go:840-851` | 不变 |

请求体：核心之后什么都不改。

请求头：核心在插件的头**之前**应用透传和覆盖，插件的头盖在上面（`dispatch.go:827-829`）。在插件之后核心只做一件事：没有 Content-Type 时补 `application/json`（`dispatch.go:831-833`）。传输层再加 Content-Length、Accept-Encoding，没有 User-Agent 时加 Go 默认的；`content-length`、`accept-encoding`、`connection`、`transfer-encoding` 即使插件给了也由传输层决定（`dispatch.go:657`）；插件给的 `host` 写进 `req.Host`（`dispatch.go:655-656`）。

### 5.3 签名搬到插件，核心的 `signing` 可以删

插件拿到最终请求体，就能自己签。`sdk/reqsign` 本来就在 SDK 里、对 `*http.Request` 签名（`sdk/reqsign/reqsign.go:73`），bedrock 今天已经用它给账号测试请求自己签（`plugins/bedrock/internal/bedrock/upstream.go:101-120`）。

签名之后还会不会被核心改坏，逐项核对：

- SigV4 实现签的请求头只有 `host`、`x-amz-date`，以及存在时的 `content-type`、`x-amz-security-token`（`sdk/reqsign/sigv4.go:10-13`、`:106-112`），再加请求体的哈希。
- 请求体：之后不变（5.2）。
- 这四个头：插件在返回的 `headers` 里给出它们和 `Authorization`。插件的头在透传和覆盖之后写入（`dispatch.go:829`），所以运营方的覆盖改不掉它们。`host` 在覆盖的禁止名单里（`sdk/paramoverride/headers.go:22`）；`content-type`、`x-amz-date` 不在固定名单里，靠的是"插件的头后写"这个顺序。
- 插件**没给**而被透传或覆盖加进来的头不在签名范围内，不影响签名。一个例外要注意：账号没有会话令牌、而运营方透传或设置了 `x-amz-security-token` 时，上游会收到一个没签名的令牌头，上游怎么处理没有验证（未验证事项 4）。
- Content-Type：插件必须返回它签名时用的那个值，否则核心补的默认值不在签名里。
- Content-Length、Accept-Encoding、User-Agent：不在签名范围内。
- 主机与端口、查询串：今天签名器把默认端口去掉、把查询串重写成规范形式并写回请求（`sigv4.go:17-22`、`:47-53`）。以后这是插件的事：返回的 URL 不带默认端口、查询串已是规范形式，`host` 头与签名用的一致。用 `reqsign.Sign` 对自己构造的 `http.Request` 签完，再把它的 URL、Host、请求头抄进返回值即可。
- URL 往返：核心会重新解析插件给的 URL 再发（`dispatch.go:764`、`:814`）。路径里 `%3A` 这类转义经一次解析再输出后是否逐字节不变，要用黄金样例测试确认（未验证事项 3）。
- 时间：`x-amz-date` 在构造时生成，构造到发送之间只有本机的几步；每次尝试重新构造，排队等待发生在尝试之前。

可以删的：`signUpstreamRequest` 与 `checkUpstreamExt` 的签名部分（`upstream_ext.go:55-93`）、`unsupportedOnWebSocket` 的签名部分（`:124-129`）、proto 的 `RequestSigning` 消息和 `signing` 字段（`common.proto:28-52`、`platform.proto` 字段 6）、`dispatch.go:834`。`sdk/reqsign` 保留为插件用的库。

按 `AGENTS.md` 开发规则 2 评估：bedrock 是唯一使用者，而且还没发布（1.8）。**如果在核心 0.1.304 发布之前完成，同一次改动里直接删干净，不需要任何兼容守卫**；`platform.proto` 的字段 6 标为保留。如果 0.1.304 先带着 `signing` 发布了，就要留一个守卫（带了 `signing` 的构造结果让这次尝试失败并提示升级插件），因为忽略这个字段的核心会把请求不签名就发出去（`docs/PLUGIN-DEVELOPMENT.md:120`）。这是决定 6。

**`response_stream` 留在核心**：响应一侧不变，eventstream 转 SSE 仍是核心做（`upstream_ext.go:104-119`）。

### 5.4 bedrock 的已知限制消失

今天 bedrock 只能按固定路径打补丁，够不到工具定义里的 `custom`、`cache_control` 里的 `scope` 和 `ttl`，Claude Code 的请求会被上游 400；`output_format` 不替客户端转换；beta 名单只能看请求头和声明的字段（`docs/PLUGIN-CLOUD-PROVIDERS.md:235-242`、`upstream.go:31-44`）。拿到完整请求体后，这些都可以在插件里处理，不再要求运营方手写参数覆盖。

### 5.5 计费

不变。核心照旧从上游回答里按声明式规则读用量（`forward.go:38-48`）。用量设计稿只涉及执行型插件，与方案二无关。

## 6. 厂商官方 Go SDK 逐个评估

用户的意图是每个厂商插件尽量用该厂商的官方 Go SDK。下面的结论来自阅读 SDK 源码（2026-11-18 从 Go 模块代理取得的版本），**没有编译运行过**；标"未验证"的是没有在源码里确认的。6.1 到 6.4 按"SDK 客户端自己发送"来评估五个问题：(1) 能否把请求体按原始字节发出、拿回原始响应或流；(2) 能否注入自定义 HTTP 客户端；(3) SDK 自己加了什么需要去掉；(4) 流式返回的是什么；(5) 只有类型化接口时丢什么。6.6 再按三个方案分别说怎么用。

注入自定义 HTTP 客户端这一条对三个方案都是关键：方案三把核心的客户端包成 `http.RoundTripper` 注入，方案一把经隧道拨号的传输层注入。

### 6.1 OpenAI：`github.com/openai/openai-go/v3` v3.102.0（Azure OpenAI 同一个模块的 `azure` 包）

| 问题 | 结论 |
|---|---|
| 原始字节 | **可以**。`Client.Post(ctx, path, params, res, opts...)`（`client.go:183-199`）：`params` 传 `[]byte` 或 `io.Reader` 时原样作请求体（`internal/requestconfig/requestconfig.go:246-253`）；`res` 传 `**http.Response` 时拿到未读的原始响应（`requestconfig.go:738-740`、`:779-788`）。也可以用 `option.WithRequestBody`、`option.WithResponseInto`（`option/requestoption.go:261-287`） |
| 自定义 HTTP 客户端 | `option.WithHTTPClient(*http.Client)`（`requestoption.go:61-88`）；另有 `option.WithMiddleware`（`:102-107`） |
| 要去掉的 | 默认重试 2 次（`requestconfig.go:286`），用 `option.WithMaxRetries(0)`；`User-Agent: OpenAI/Go <版本>`（`:38`）、`X-Stainless-*` 六个头（`:103-108`）、`X-Stainless-Retry-Count`、`X-Stainless-Timeout`（`:276-277`），用 `option.WithHeaderDel` 或中间件删；`Accept` 默认 `application/json`（`:275`），流式要改；状态码 ≥ 400 时 SDK 把错误体**不设上限地**读完再返回 `*apierror.Error`（`:748-765`），原始字节仍可从其 `Response` 取回。环境变量默认值（API Key 等）是否在 `Post` 路径生效：未验证 |
| 流式 | 原始路径拿到的是未解码的 `http.Response.Body`，就是上游的 SSE 字节，不用重新编码。类型化路径返回 `ssestream.Stream[T]`（`packages/ssestream/ssestream.go:175`），是解码后的事件 |
| 类型化接口的损失 | 请求经 Go 结构体重新序列化，SDK 不认识的新字段丢失；流要重新编码成 SSE。**不要用类型化接口** |
| Azure | `azure.WithEndpoint`、`azure.WithAPIKey`、`azure.WithTokenCredential`（`azure/azure.go:76`、`:251`、`:170`）。它怎样把模型名改写成部署路径、对原始请求体是否成立：未验证 |

**结论：可以用 SDK 客户端，只用原始模式**（`Post` + 字节 + `**http.Response`）。SDK 实际贡献的是基础 URL 拼接、鉴权头和 Azure 的端点规则。

### 6.2 Anthropic：`github.com/anthropics/anthropic-sdk-go` v1.84.0

| 问题 | 结论 |
|---|---|
| 原始字节 | **可以**，与 openai-go 同一套生成器：`Client.Post`（`client.go:259-273`），`option.WithRequestBody`、`option.WithResponseInto`（`option/requestoption.go:341-352`） |
| 自定义 HTTP 客户端 | `option.WithHTTPClient`（`requestoption.go:174`）、`option.WithMiddleware`（`:203`） |
| 要去掉的 | 默认重试 2 次（`internal/requestconfig/requestconfig.go:175`）；`User-Agent: Anthropic/Go <版本>`（`:28`）、`X-Stainless-*`（`:70-75`、`:165-166`）；**默认写 `anthropic-version: 2023-06-01`**（`:170`），客户端透传的版本头必须盖过它；客户端构造时会从环境变量和配置文件自动找凭证（`client.go:70-105`），要加 `option.WithoutEnvironmentDefaults()`（`requestoption.go:508`）。OpenTelemetry 选项存在（`option/opentelemetry.go`），默认是否关闭：未验证 |
| 流式 | 原始路径是上游的 SSE 字节 |
| 类型化接口的损失 | 同 OpenAI；另外类型化的非流式调用会按 `max_tokens` 计算超时并可能拒绝请求（`message.go:66`、`client.go:299`），原始路径不经过它 |
| Bedrock 上的 Claude | `bedrock` 子包是一个中间件：用 gjson/sjson 直接改原始请求体（补 `anthropic_version`、把模型移进 URL，`bedrock/bedrock.go:302-336`），SigV4 签名，并把 AWS eventstream 翻译成 SSE 字节（`bedrock.go:68-176`）。**不重新类型化请求体**。它做的正是今天核心的 `signing` 和 `response_stream` 两个扩展点做的事 |
| Vertex 上的 Claude | `vertex` 子包同样是改原始请求体的中间件（`vertex/vertex.go:147-191`），凭证用 Google 官方库 |

**结论：可以用 SDK 客户端，只用原始模式。** Bedrock 和 Vertex 上的 Claude 可以用它的 `bedrock`、`vertex` 子包。

### 6.3 Google：`google.golang.org/genai` v1.78.0（Gemini API 与 Vertex AI）

| 问题 | 结论 |
|---|---|
| 原始字节 | **不可以**。公开接口只有类型化的 `Models.GenerateContent(ctx, model, []*Content, *GenerateContentConfig)` 和 `GenerateContentStream`（`models.go:5490`、`:5532`）。内部的 `sendRequest` 收的是 `map[string]any` 和未导出的 `*apiClient`（`api_client.go:91-96`），包外调不了。`HTTPOptions.ExtraBody` 只能往类型化请求体里合并额外字段（`types.go:2176`） |
| 自定义 HTTP 客户端 | `ClientConfig.HTTPClient`（`client.go:141`） |
| 要去掉的 | 总是追加 `user-agent` 和 `x-goog-api-client` 的版本值（`api_client.go:313-318`）；没有配置 `RetryOptions` 时不重试（`common.go:503-505`）；未给凭证时回落到应用默认凭证（`client.go:132-135`）和环境变量里的 API Key（`client.go:184-193`） |
| 流式 | `iter.Seq2[*GenerateContentResponse, error]`，是解码后的结构体。发给客户端要重新序列化成 SSE 或 JSON 数组流。响应上有 `HTTPResponse.Body` 字段（`types.go:3397-3401`），流式时是否填充：未验证 |
| 类型化接口的损失 | 客户端 JSON 要先反序列化成 SDK 类型：SDK 版本不认识的字段丢失；响应重新序列化后字段顺序、省略规则、数字格式与上游不同；上游新增的响应字段在 SDK 升级前到不了客户端。对网关来说每个上游新功能都要等 SDK 发版再重发插件 |

**结论：不适合做透传。** gemini、vertex 插件用原始请求，只用官方库做鉴权（Vertex 的访问令牌用 `golang.org/x/oauth2/google` 或 `cloud.google.com/go/auth`；Gemini API 是一个请求头）。坚持用 `genai` 客户端的代价就是上面"损失"一行（决定 8）。

### 6.4 AWS Bedrock：`github.com/aws/aws-sdk-go-v2/service/bedrockruntime` v1.63.13

| 问题 | 结论 |
|---|---|
| 原始字节 | `InvokeModel` 的输入 `Body []byte`、输出 `Body []byte`（`api_op_InvokeModel.go:95`、`:181`），请求体原样。非流式回答由 SDK 整个读进内存后返回。`Converse` 系列是类型化的，不适合透传 |
| 自定义 HTTP 客户端 | `Options.HTTPClient`（`options.go:137`） |
| 要去掉的 | `Options.Retryer`、`RetryMaxAttempts`（`options.go:92-115`）要设成只试一次；默认重试次数、SDK 自己加的 `User-Agent` 和 `amz-sdk-*` 头：未在本模块源码里核对（在 `aws-sdk-go-v2` 主模块里） |
| 请求头分层 | 签名覆盖部分请求头，签名之后再改会让签名失效。客户端透传头和请求头覆盖必须经 `Options.APIOptions` 的中间件在签名之前写入（`options.go:28`）；具体挂在哪一步：未验证 |
| 流式 | `InvokeModelWithResponseStream` 返回事件通道（`api_op_InvokeModelWithResponseStream.go:330`），每个 chunk 是 `PayloadPart.Bytes`（`types/types.go:6437-6441`），内容是上游模型的原始事件 JSON。发给客户端要自己加 `event:`/`data:` 封成 SSE，成本是每事件一次小拷贝；异常事件是类型化的，要自己映射成错误事件 |

**结论（SDK 客户端自己发送时）：`InvokeModel` 和 `InvokeModelWithResponseStream` 可以用 SDK 客户端**（请求体是原始字节），流式事件要重新封成 SSE。不经 SDK 客户端的做法是原始请求加官方签名器和 eventstream 解码器：分支 `feat/official-libs`（提交 `a972296a5`）已经把 `sdk/reqsign`、`sdk/awseventstream` 改成基于官方库，尚未合并。只服务 Claude 的 bedrock 插件还有第三个选择：`anthropic-sdk-go/bedrock`（6.2）。三个方案下各自怎么选见 6.6。

### 6.5 厂商 SDK 覆盖什么、不覆盖什么

| 厂商 SDK 覆盖 | 不覆盖，三个方案下都要由核心或共享库提供 |
|---|---|
| 基础 URL 和路径 | 请求体怎么到插件、回答怎么到客户端 |
| 鉴权（API Key 头、SigV4、Google 令牌、Azure 凭证） | 运营方设置：请求头覆盖、参数覆盖、透传、过滤字段、强制流式、模型映射 |
| 发送 | 客户端协议与上游协议不同时的协议转换 |
| 流的解码（eventstream → 事件） | 用量读取或上报、错误分类 |
| | 超时、客户端断开、账号代理、SSRF |
| | 去掉 SDK 自带的重试、标识头和遥测 |

用户提供的参考（new-api，只作行为参考，未抄代码；以下为主控核对的结论，我只复核了标注的两处）：new-api 的 OpenAI、Anthropic、Gemini 流式路径都不用官方 SDK，而是原始 HTTP 加逐行读取，同协议时原样转发上游的事件内容；它唯一经 SDK 读流的是 Bedrock（`bedrockruntime` 的事件通道），而这条路径恰恰缺了通用行为。我复核到的：请求头覆盖在该路径上被解析并写进一个本地的 `http.Header`（`new-api/relay/channel/aws/relay-aws.go:130-136`），这个对象之后只用来提取 `anthropic-beta`（`relay-aws.go:160`、`dto.go:35-45`），发给 AWS 的请求由 SDK 自己构造（`relay-aws.go:285`），所以覆盖的请求头到不了线上。教训是：**让插件把 SDK 的流直接接到客户端，通用行为就会一条条漏掉**；必须由核心（方案三、方案二）或共享库（方案一）包住 SDK。

### 6.6 三个方案下各家 SDK 怎么用

| SDK | 方案三：往返方式（非流式） | 方案三：截取转发方式（流式由核心中继） | 方案一（SDK 自己发，含流式） | 方案二（只构造） |
|---|---|---|---|---|
| openai-go、Azure | 可用：`option.WithHTTPClient` 接适配器，`option.WithMaxRetries(0)` | **可行**：原始模式的 `Post` 把回答原样交给调用方，不解析合成的回答。SDK 贡献的是 URL、鉴权头、Azure 端点规则 | 可用，原始模式；流是上游的 SSE 字节，原样转发 | 同"截取"：能用，但 SDK 的贡献很少，直接拼 URL 和鉴权头更简单 |
| anthropic-sdk-go（含 `bedrock`、`vertex` 子包） | 可用 | **可行，而且有实际价值**：`bedrock` 子包的中间件改写请求体、把模型放进 URL、做 SigV4 签名（6.2），截下来的就是可以直接发的请求；eventstream 的解码由核心的 `response_stream` 做 | 可用，原始模式 | 同"截取" |
| `bedrockruntime` | 可用：`InvokeModel` 本来就整个缓冲 | **不值得**：`InvokeModelWithResponseStream` 会解码响应体，要伪造合法的事件流。改用官方签名器（`aws-sdk-go-v2/aws/signer/v4`，独立于客户端）自己构造请求 | 可用；事件要重新封成 SSE | 只用签名器 |
| `genai` | 可用，但请求和回答都经过类型化，有损（6.3） | 不可行：只有类型化接口 | 有损 | 不适用；用原始 HTTP 加官方鉴权库 |

哪些 SDK 让"SDK 发送、插件读原始响应体、原样转发事件内容"成为可能（主控问的做法 a）：openai-go 和 anthropic-sdk-go（原始模式拿到未解码的 `http.Response`）；`bedrockruntime` 给的是解码后的事件（内容字节原样，只是要重新封帧）；`genai` 只有解码后的结构体，只能重新编码（做法 b）。这只在方案一、或方案三的流式变体下有意义。

各家的结论：

- **OpenAI、Azure OpenAI**：方案三下用截取转发方式或直接拼请求都行。SDK 的价值主要在 Azure 的端点规则和凭证，建议 Azure 用 SDK、OpenAI 直连拼请求。
- **Anthropic，Bedrock 和 Vertex 上的 Claude**：用 anthropic-sdk-go 的截取转发方式，`bedrock`、`vertex` 子包省掉自己写请求体改造和签名。
- **Bedrock（非 Claude 模型，将来）**：官方签名器加自己构造请求。
- **Gemini、Vertex 上的 Gemini**：原始 HTTP 加官方鉴权库。`genai` 客户端不适合透传。

## 7. 三个方案对比

| 维度 | 方案三：核心提供 HTTP 客户端 | 方案一：插件发送，经隧道 | 方案二：插件构造，核心发送 |
|---|---|---|---|
| 官方 SDK | 非流式整个客户端可用；流式用"截取转发"（OpenAI、Anthropic 可行）；SDK 自己读流要等流式变体 | 整个客户端可用，含流式（原始模式）；Google 的除外 | 只能用签名器、鉴权库，或"截取"后作为返回值 |
| 账号代理 | 核心现有的传输层，不变 | 隧道要新增"按令牌选账号、经代理拨号" | 不变 |
| 系统设置的强制 | 核心强制，与今天相同 | 请求体一半核心强制；请求头一半核心算、库落实，核心无法核对 | 核心强制，与今天相同 |
| 计费证据 | 方式 (i)：核心直接从上游读，不变。方式 (ii) 的辅助请求：插件上报，核心有请求轨迹 | 读数来自插件交回的字节，不是核心直接从上游看到的 | 不变 |
| 沙箱 | 不变，插件不联网 | 不放宽，但发送插件必须有 `net`；TLS 在插件里 | 不变 |
| 流式路径 | 方式 (i)：上游 → 核心 → 客户端，与今天相同。流式变体：上游 → 核心 → 插件 → 核心 → 客户端 | 上游 → 隧道（核心）→ 插件 → 回传流 → 核心 → 客户端 | 上游 → 核心 → 客户端 |
| 多步上游（先换令牌、先上传再生成） | **支持**：一次执行里多个方式 (ii) 的请求 | 支持 | 不支持，只有一个请求 |
| 插件崩溃的影响 | 切断该插件正在进行的转发（执行路径今天就是这样，3.2）；不影响其他插件 | 切断该插件全部在途的流，连接池和 TLS 状态也在插件里 | 只影响正在构造的请求，在途的流不受影响 |
| 契约改动 | 清单 `requestBody`；构造结果加 `body`；`ExecuteHTTP` 扩到执行期间；改 `common.proto` 一条规则；删 `RequestSigning` | 清单 `requestBody`；`DialOpen` 加令牌；新增流式 RPC；`Execute` 带请求头决定；改 `common.proto` 两条规则 | 清单 `requestBody`；构造请求和结果各加 `body`；改 `common.proto` 一条规则；删 `RequestSigning` |
| 核心改动 | `forwardBuilt` 几处分支；核心侧接收上限；方式 (ii) 的发送与允许主机；在途字节准入 | 隧道、代理拨号、回传流、崩溃时的换号判断、在途字节准入 | `forwardBuilt` 几处分支；在途字节准入 |
| 每请求的额外开销 | 请求体 2 次序列化往返；执行行的数据库写入（走执行路径时）；响应无额外开销 | 请求体 1 次往返；每个响应帧 2 次封帧；TLS 在低优先级进程里 | 请求体 2 次序列化往返；响应无额外开销 |
| 做完能删的 | `signing` 一套；选择加入的类型不再用 `requestFields`、`BodyPatch`。全部平台插件迁完后可以删默认的字段加补丁模式（决定 10） | 全部迁完后可删核心的发送路径；中继与用量读取仍留在核心（做法 A） | `signing` 一套 |
| 工作量 | 中 | 大 | 小 |

**能否并存**：方案三和方案二本来就是同一套改动的两个入口（执行路径、普通转发路径），自然并存。方案一可以作为第三种模式并存（同一个账号类型上多一个"插件发送"的能力声明），但没有必要同时维护，除非出现方案三满足不了的需求。

**方案二是垫脚石还是死路**：是方案三的子集，不是死路。对方案一也是垫脚石：它的全部内容（选择加入、交出之前的改写顺序、大小上限与准入、新权限）是方案一的前半段。

**什么需求只有方案一能满足**：厂商 SDK 必须自己掌握连接的场景，例如 SDK 用了 HTTP 之外的传输（gRPC、WebSocket 握手之外的自有协议）、双向流、或者 SDK 不接受注入的 HTTP 客户端。本文评估的四家都接受注入（6.1 到 6.4），目前没有这样的需求。

**建议：方案三。** 它是用户说的模型；相对方案一，它不需要动隧道和沙箱，系统设置和计费证据不降级，流式路径不绕圈；相对方案二，它支持多步流程和官方 SDK。方案一留作将来出现上面那类需求时的选项，第 4 节保留了它完整的做法。

## 8. 用户的问题：库，还是各插件自己实现

**建议用库（`sdk/pluginsdk` 里的几个包），不让每个插件各写。**

要放进库的东西，按方案三：

- `hosthttp`：`http.RoundTripper` 适配器（往返方式、截取转发方式）；
- 完整请求体的辅助：读取、按路径改写、交回最终请求体；
- 签名辅助：`sdk/reqsign` 已有，补"签完把 URL、Host、请求头抄进构造结果"的函数；
- 去掉厂商 SDK 自带的重试、标识头、环境变量凭证的一组默认选项；
- `ExecuteDefault` 的对应版本（构造 → 转发 → 回答 `host_usage`）；
- `pluginsdktest` 里的一致性用例。

两条理由：

1. 每个选择加入的插件都要做这几件一样的事，其中"关掉 SDK 重试""不带 SDK 的标识头""签名后不被改坏"做错了不会报错，只会在线上表现为重复请求、被上游识别或签名失败。一份实现配一套测试，比 N 份可靠。
2. 库的版本分叉问题在方案三下很轻：安全边界和系统设置都在核心里强制，插件用旧库最多是自己的请求构造有缺陷，影响不到别的账号和设置。所以不需要"核心检查 SDK 版本下限"这类强制手段，控制台显示 SDK 版本即可（`GetInfoResponse.sdk_version` 已经上报，`sdk/proto/sub2api/plugin/v1/plugin.proto:32`、`sdk/pluginsdk/serve.go:24-25`；核心现在没有读它）。

对照：方案一下库要落实请求头分层和禁止覆盖的头，核心看不到结果，库的版本就成了安全问题，需要版本下限和强制的一致性测试。这是方案一的额外代价。

"每个插件各自实现"在方案三下不是不行（核心兜住了强制），只是重复。在方案一下不建议。

**只有清单的插件**（openai、gemini、relay、azure_openai）：方案三、方案二下都不用动，默认仍是字段加补丁，由核心进程内的声明式引擎服务（1.2）。只有在决定 10 选择"最终删除默认模式"时才需要给它们找承载方式，届时的选项是随核心发布一个通用的声明式执行器二进制、每个插件一个进程。

## 9. 性能与风险

能从代码估算的给数字，其余标"未测量"。

**请求体拷贝与耗时**：见 2.3。方案三走执行路径时请求体去一次（`Execute`）、回一次（`ForwardUpstream`），与方案二相同。

**响应**：方式 (i) 和方案二没有额外开销。方案三的流式变体和方案一，每个响应帧多两次封帧和进程间传输，未测量。

**执行路径的数据库写入**：每个请求多一行 `plugin_executions` 的插入和两次更新（3.2）。codex、volcengine 今天已经在付；把 anthropic 这类流量大的插件迁到执行路径之前要先量这部分（未验证事项 2）。只发一个请求的插件用普通转发路径的形态可以避开。

**插件内存与 CPU**：

- 内存：2.3。迁移的插件按"热路径并发 × 典型请求体 × 4"估算并提高 `memoryMB`，核心侧的在途字节准入兜底。
- CPU：`GOMAXPROCS` 是 `ceil(cpu)`（`sandbox/launcher.go:169`），申请 0.1 的插件只有 1 个 P。方案三、方案二下插件只做 JSON 改写和签名，压力不大；方案一下 TLS 也在这个进程里，必须提高。
- 请求体改写在插件里的写法：整份解析成通用结构再序列化会放大内存（审核插件的经验是数倍，`docs/CONTRACTS.md:4176`）。库的辅助函数按路径改写，不整份解析。

**崩溃的影响范围**：

| | 今天 | 方案三 | 方案二 | 方案一 |
|---|---|---|---|---|
| 普通转发插件崩溃 | 在途的流继续 | 走执行路径的：正在转发的流被切断 | 在途的流继续 | 全部在途的流被切断 |
| 内存超限重启 | 少见（插件不持有请求体） | 可能性上升，靠准入和提高上限控制 | 同左 | 更高（请求体常驻到尝试结束，外加 TLS 缓冲） |
| 插件升级 | 旧实例排空 30 秒（`grpcruntime/runtime.go:105`） | 走执行路径的长流跨不过 30 秒（codex 今天已有此问题） | 不受影响 | 长流全部受影响 |

**其他**：

- 厂商 SDK 进插件的 `go.mod`，二进制变大，第三方依赖的漏洞要逐个插件跟进。AWS SDK、Google 鉴权库依赖树较大。
- 方式 (ii) 让插件能经账号代理访问清单里声明的主机并读回答。它与今天的 `SendAccountRequest` 是同一类能力，只是上限变大、次数变多，审计要跟上（第 10 节）。
- 截取转发方式依赖"SDK 的原始模式不解析回答"这个行为。SDK 升级改了这一点，表现为插件侧报错而不是悄悄发错请求，可以由插件自己的测试发现。

## 10. 安全

**不变的**：沙箱（seccomp、rlimit、看门狗）；插件不联网；账号凭证的授权方式；SSRF、账号代理、系统设置由核心强制；核心看到明文请求和回答。

**新增的暴露**：

1. 完整的提示词进入选择加入的插件进程的内存。今天平台插件只看到声明的请求字段（`common.proto:11-12`）。审核插件已经在拿整个顶层字段（1.7），所以这不是全新的暴露面，但对平台插件是新的。
2. 方式 (ii) 的回答也进入插件内存（辅助请求的回答，不是给客户端的回答）。
3. 方式 (i) 下给客户端的回答**不**进入插件，插件仍然只拿到有界的用量观察（`host.proto:339-341`）。

没有 `net` 授权的插件拿到提示词也发不出去：它唯一的对外通道是核心的客户端，主机受清单 `externalServices` 限制，每个请求有记录。这是方案三相对方案一的一个实质区别。

**权限**：新增宿主权限，建议名 `gateway.body`，风险级别 **high**（与 `gateway.hook`、`net` 同级，`sdk/manifest/manifest.go:1147-1182`）。

- 含义："读取本插件账号的网关请求的完整请求体"。清单里声明了 `requestBody: "full"` 而没有申请它，清单检查拒绝。
- 安装和升级的确认页单独列出，文案写明"能看到经它的账号发出的全部提示词内容"。已安装的插件升级后新申请它，需要管理员重新批准（与 `gateway.hook` 范围变化时的做法相同，`docs/CONTRACTS.md:4188`）。
- 同时持有 `gateway.body` 和 `net` 的插件，确认页给出组合警告：它既能读提示词又能自己联网。
- 不定为 critical 的理由：critical 留给能直接动钱、动凭证、动用户数据的权限；`accounts.credentials`（critical）插件本来就要有。是否定为 critical 是决定 9。

**方式 (ii) 的约束**：凭执行令牌，只在 `Execute` 期间有效；主机限于清单 `externalServices`（或该账号设置里的上游主机）；不跟随重定向；次数和大小有上限；每次记录账号、方法、主机和路径、状态码、字节数，不记请求头和内容（与 `SendAccountRequest` 的审计相同，`host.proto:240-242`）。

## 11. 迁移计划

发布顺序始终是先核心、后插件。插件用清单的 `hostCompat` 下限要求对应的核心版本。

### 第 1 阶段：完整请求体、最终请求体、适配器，bedrock 迁移

bedrock 先行的理由：它是 `signing` 的唯一使用者；它有明确的已知限制要靠完整请求体解决（5.4）；它是市场插件，还没发布（1.8），出问题影响面最小。

内容：

- **契约**：清单 `requestBody`；宿主权限 `gateway.body`；`BuildUpstreamRequestRequest.body`、`BuildUpstreamRequestResponse.body`；`ExecuteHTTP` 在 `Execute` 期间可用；`common.proto` 的规则按 2.4 改写；删除 `RequestSigning` 和 `signing` 字段（字段号标为保留）；Host API 版本从 9 升到 10（`runtime-contract/contract.go:14`、`sdk/protocol/protocol.go:19`）。
- **核心**：`attempt` 在 `full` 模式下带上请求体；`forwardBuilt` 用插件的请求体、跳过补丁和第二次强制流式、保留守卫；删除 `signUpstreamRequest`；核心侧服务端接收上限；完整请求体上限与在途字节准入；方式 (ii) 的发送（复用 `SendAccountRequest` 的主机检查和代理发送）。
- **SDK**：`hosthttp` 适配器；完整请求体辅助；签名辅助；`pluginsdktest` 的假宿主支持新字段和方式 (ii)。
- **插件**：bedrock 新版本，声明 `requestBody: "full"`，申请 `gateway.body`，自己改造请求体（含深度清洗）并签名；提高 `memoryMB`；`hostCompat` 下限提到带本设计的核心版本。bedrock 用执行路径还是普通转发路径的形态是决定 5。
- **文档**：CONTRACTS 新增一节并修改 §109；`docs/ARCHITECTURE.md` §3、§5.3；`docs/PLUGIN-DEVELOPMENT.md` 第 4 节（"插件看不到整个请求体"的说法，`:152`）和第 6 节（`signing` 一行删除）；`docs/PLUGIN-CLOUD-PROVIDERS.md` 的已知限制；`CHANGELOG.md`。
- **`signing` 的删除**：bedrock 和 `signing` 都还没发布（1.8）。本阶段在核心 0.1.304 发布之前合入，就在同一组提交里删干净，不需要兼容守卫，符合 `AGENTS.md` 开发规则 2。如果 0.1.304 已经带着 `signing` 发布，就改为保留一个守卫（构造结果带 `signing` 时这次尝试失败并提示升级插件），在下一个核心版本删除（决定 6）。

**退出条件**：单元测试和 e2e 全绿（第 13 节的验收）；bedrock 在模拟上游上通过 SigV4 校验的流式和非流式请求；1、8、32 MiB 请求体的延迟和插件内存数字写回本文；Claude Code 形态的请求体（带 `tools[].custom`、`cache_control.scope`）不再被模拟上游拒绝。

**混合版本**：

| 情形 | 行为 |
|---|---|
| 新核心、没有选择加入的插件 | 完全不变 |
| 新核心、旧 bedrock 0.1.0（带 `signing`） | 0.1.0 没有发布过，线上不存在这个组合；开发环境里旧包的 `hostCompat` 仍满足，核心删了 `signing` 后会把请求不签名发出。所以删除的同一版本里把 bedrock 0.1.0 的包从内置和市场索引里替换掉，并在 CHANGELOG 写明 |
| 旧核心、新 bedrock | `hostCompat` 下限拒绝安装 |
| 多节点滚动升级 | 新旧核心节点并存时，插件新版本只能装在新核心节点上；插件按节点独立切换（CONTRACTS §36），未切换的节点上该插件的账号走旧行为。bedrock 没有旧版本在线上，所以实际是"升完核心再装" |

**回退**：卸载或停用 bedrock 新版本（它没有旧版本可回）。核心回退按发布手册；本阶段没有数据库迁移。

### 第 2 阶段（提纲）：其他平台插件按需迁移

vertex、anthropic、claude-oauth 等按是否有收益决定：需要深度改造请求体、需要多步流程、想用官方 SDK 的迁；只是拼 URL 和鉴权头的不必迁。每个插件一个版本。迁到执行路径之前先量 3.2 的数据库写入成本。

### 第 3 阶段（提纲）：流式变体，按需

如果出现"插件必须自己读上游的流"的需求（决定 4），加流式 RPC 和"插件写给客户端"的通道，修改 `common.proto` 的一元规则。

### 第 4 阶段（提纲）：是否删除默认的字段加补丁模式

见决定 10。删的话，只有清单的插件需要一个随核心发布的声明式执行器；不删的话，两种模式长期并存，各有使用者，不属于规则 2 说的过渡代码。

## 12. 与决定的冲突与待定

1. **`common.proto:11-12`"请求体从不整个发给插件"与方向冲突**，要按 2.4 改写。它的理由（为将来的 JS 运行时保留可行性）不受影响：仍然是一元调用，只是消息变大。"除 `EgressService.Dial` 外都是一元调用"在第 1 阶段不动。
2. **"不要来核心转换"与本设计不完全一致**。三个方案里协议转换和请求体的运营方改写都留在核心、在交出请求体之前完成（2.2）。厂商特有的改造（Bedrock 的信封、深度清洗、签名）搬到插件。如果用户的意思是协议转换（OpenAI 协议 ↔ Anthropic 协议）也要搬到插件，那需要把转换包搬进 SDK，并解决有损策略在选号时就要判断的问题（`convert_policy.go:88`），这是比本文大得多的另一项工作（决定 3）。
3. **`docs/PLUGIN-DEVELOPMENT.md:76`"不要自己连上游"、`sdk/pluginsdk/execute.go:99-102`"从不打开套接字"** 在方案三、方案二下继续成立，在方案一下要反过来。
4. **`upstream_ext.go:6-9`"只有核心能签名，因为插件看不到整个请求体"** 在三个方案下都不再成立，随 `signing` 一起删除。
5. **`AGENTS.md` 开发规则 2**：第 1 阶段没有过渡代码，前提是在 0.1.304 发布之前合入（第 11 节）。默认模式与 `full` 模式并存不是过渡，是两种各有使用者的模式；是否最终合一是决定 10。
6. **任务说明把"保活 ping"列为核心在 HTTP 热路径上的现有职责，代码里没有**（1.1 末尾）。只有 WebSocket 有。要加的话放在核心写客户端的地方，是独立的新功能。
7. **任务说明要求考虑 multipart、二进制请求体**。网关今天只接受 JSON 请求体（1.7），这类请求进不了流水线，所以本文没有为它们设计；`bytes` 字段本身可以承载任意内容，将来放开时不需要再改契约。
8. **用量设计稿的范围要跟着变**：它写的是"执行型插件目前只有 codex 和 volcengine"。方案三让更多插件成为执行型，它的"没有上报就是零成本记录"会适用到这些插件上。两稿实施时要确认：SDK 的默认执行流程对迁移过来的插件默认回答 `host_usage`，否则迁移会让它们变成不计费。
9. **走执行路径时，平台插件与账号类型插件不是同一个的情况不走执行**（`execute.go:59-66`）。bedrock 服务的是 anthropic 平台，平台插件是 anthropic，不是 bedrock。这条限制只针对"端点由插件解析用量或是任务提交"，anthropic 的 messages 端点是否属于这种情况我没有逐个端点核对（未验证事项 5）；如果属于，bedrock 只能用普通转发路径的形态（方案二），这会直接决定决定 5 的答案。
10. **`CHANGELOG.md:13`** 说 `feat/cloud-providers` 未合并，实际已合并（1.8）。发布时要改。
11. **分支 `feat/official-libs`**（未合并）把 `sdk/reqsign`、`sdk/awseventstream` 改成基于官方 AWS 库，而 `docs/PLUGIN-DEVELOPMENT.md:122` 仍写"只用标准库"。bedrock 迁移用哪一版要先协调，不需要用户决定。

## 13. 第 1 阶段任务拆分

| # | 任务 | 文件范围 | 验收 | 依赖 |
|---|---|---|---|---|
| W1 | 契约：清单 `requestBody`、宿主权限 `gateway.body`、两个 `body` 字段、删除 `RequestSigning`、改写 `common.proto` 的规则、Host API 10 | `sdk/proto/sub2api/plugin/v1/{common,platform,host}.proto`、`sdk/gen`、`sdk/manifest/manifest.go`、`sdk/manifest/check`、`sdk/protocol/protocol.go`、`runtime-contract/contract.go` | `buf generate` 后无未提交差异；两个 `HostAPIVersion` 相等的现有测试通过；清单检查：`full` 缺权限、`full` 配声明式上游、`full` 用于 WebSocket 端点各有一条拒绝用例 | 决定 6、9 |
| W2 | 核心热路径：`full` 模式带请求体调用；`forwardBuilt` 用插件请求体、`body` 与 `patches` 同时出现时失败、跳过第二次强制流式、保留守卫；删除签名钩子 | `server/internal/gateway/dispatch.go`、`upstream_ext.go`、`routing.go`、`execute.go` | 新测试：交给插件的是转换、映射、覆盖之后的请求体；插件请求体原样发出（逐字节比对模拟上游收到的）；`upstream_ext_test.go` 里签名相关用例删除、响应流格式用例保留；换号后第二次尝试拿到按新账号改写的请求体 | W1 |
| W3 | 大小与准入：完整请求体上限、按插件实例的在途字节准入、超限时中性换号；核心侧服务端接收上限；SDK 在 `full` 时调大插件接收上限 | `server/internal/gateway`（新文件）、`server/internal/plugin/grpcruntime/instance.go`、`runtime.go`、`sdk/pluginsdk/serve.go` | 新测试：超过上限的请求不调用插件、账号不冷却；并发大请求体时在途字节不超过预算；32 MiB 请求体往返成功 | W1 |
| W4 | 方式 (ii)：`ExecuteHTTP` 在 `Execute` 期间可用，主机限制、账号代理、次数与大小上限、审计 | `server/internal/plugin/grpcruntime/host.go`、`execute.go`、`poll.go` | 新测试：清单外的主机被拒；经账号代理发出；第 9 次调用被拒；回答超限带 `truncated`；令牌过期后被拒。现有 `host_account_request_test.go`、`poll_test.go` 不变 | W1 |
| W5 | SDK：`hosthttp` 适配器（往返、截取转发）、完整请求体辅助、签名辅助、去掉厂商 SDK 默认行为的选项、假宿主支持 | `sdk/pluginsdk/hosthttp`（新）、`sdk/pluginsdk/request.go`、`sdk/reqsign`、`sdk/pluginsdk/pluginsdktest` | 单测：标准库 `http.Client` 经适配器往返；截取转发方式下交给宿主的请求与 `http.Request` 逐项一致、合成回答的状态码来自观察；签名辅助的输出通过 `reqsign` 现有黄金样例 | W1 |
| W6 | 安装审查与控制台：新权限的风险级别和文案、与 `net` 的组合警告、插件详情显示 SDK 版本 | `server/internal/plugin/install/review.go`、`server/internal/plugin/api`、`web/src`、`docs/plugin-permissions` | 审查接口测试含新权限；`npm run typecheck`、`npm test`、`npm run lint` 通过 | W1 |
| W7 | bedrock 新版本 | `plugins/bedrock`（`manifest.json`、`internal/bedrock`） | 插件测试：请求体改造（含 `tools[].custom`、`cache_control.scope`/`ttl` 的清洗、beta 合并）、自签名通过模拟上游的 SigV4 校验、URL 往返逐字节不变；`TestReleasedPluginsBumped` 通过 | W2、W5、决定 5、8 |
| W8 | 文档 | `docs/CONTRACTS.md`、`docs/ARCHITECTURE.md`、`docs/PLUGIN-DEVELOPMENT.md`、`docs/PLUGIN-CLOUD-PROVIDERS.md`、`CHANGELOG.md` | 与代码在同一组提交 | W1 至 W7 |
| W9 | e2e 与测量 | `next/e2e`、`server/internal/gateway` 的基准 | bedrock 流式、非流式、三种断开策略、故障策略换号、代理账号；1、8、32 MiB 请求体的延迟与插件内存写回第 9 节 | W2、W3、W7 |

W2、W3、W4、W5、W6 在 W1 之后可以并行；W7 在 W2、W5 之后；W9 最后。数据库测试要确认真的跑了，没有成片跳过。

## 14. 界面与存储

| 项 | 位置 | 默认 |
|---|---|---|
| `requestBody` | 插件 `manifest.json` 的 `accountTypes[].platforms[]` | `"fields"`（今天的行为） |
| 宿主权限 `gateway.body` | 插件 `manifest.json` 的 `hostPermissions`；批准记录在现有授权存储 | 不授予则不能声明 `full` |
| 完整请求体上限 | 核心常量，不是设置项 | 64 MiB（决定 7） |
| 在途字节预算 | 按插件内存上限的比例计算，不是设置项 | 内存上限的四分之一（决定 7） |
| 方式 (ii) 的上限 | 核心常量 | 每次执行 8 个请求，回答 8 MiB |
| 控制台 | 安装确认页列出新权限；插件详情页显示 SDK 版本、哪些账号类型用了完整请求体 | |
| 插件资源 | `manifest.json` 的 `resources`，迁移的插件提高 `memoryMB` | 数字待第 1 阶段测量 |
| 系统设置、数据库迁移 | 不新增 | 新权限是否需要改授权存储的结构未验证（未验证事项 6） |

账号编辑器里的请求头覆盖、参数覆盖、透传等界面和存储都不变。

## 15. 需要用户决定的事项

每条可以用一两个字回答。

| # | 决定 | 建议 | 另一选择的代价 |
|---|---|---|---|
| 1 | 采用哪个方案 | **方案三**（核心提供 HTTP 客户端），方案二作为它的单请求形态一并实现 | 方案一：要改隧道和加流式通道，请求头设置和计费证据降级，插件崩溃切断全部在途的流；换来的是官方 SDK 连流式都原样可用 |
| 2 | 公共逻辑：SDK 库，还是各插件自己实现 | **库**（方案三下很薄） | 各自实现：方案三下核心兜住了强制，只是每个插件重复写适配器和签名；方案一下不可接受 |
| 3 | 协议转换和运营方的请求体改写留在核心、在交出请求体之前完成，是否符合"不要来核心转换" | **留在核心**；搬到插件的是厂商特有的改造 | 协议转换也搬到插件：要把转换包（13366 行）搬进 SDK，重新设计有损策略的判断时机，每个插件各带一份转换代码 |
| 4 | 是否做"插件自己读上游的流"（方式 (ii) 的流式变体） | **现在不做**；截取转发方式已经让 OpenAI、Anthropic 的 SDK 能用于流式请求 | 现在做：新增流式 RPC 和"插件写给客户端"的通道，流式路径多绕一圈，插件进入响应路径后计费证据和断开处理要重新设计 |
| 5 | bedrock 第 1 阶段用哪种形态：执行路径（`Execute` 里调核心的客户端），还是普通转发路径（在构造结果里交回最终请求体） | **普通转发路径的形态**：bedrock 只发一个请求，这样没有执行行的数据库写入，插件崩溃不切流；适配器和方式 (ii) 仍然实现，由单元测试和 codex、volcengine 的现有路径覆盖 | 执行路径：更贴近"插件调用核心的客户端"的说法，并且在第 1 阶段就用真实插件走通适配器；代价是每请求多三次数据库写入、崩溃切断正在转发的流，另外受第 12 节第 9 条的限制，可能根本走不了 |
| 6 | `signing` 的删除时机 | **在核心 0.1.304 发布之前随第 1 阶段一起删**，不留守卫 | 0.1.304 先带着 `signing` 发布：之后删除要留一个版本的守卫，并处理已装出去的 bedrock 0.1.0 |
| 7 | 完整请求体上限和在途字节预算 | **64 MiB；预算为插件内存上限的四分之一** | 不设上限：一个大请求就能让插件被看门狗重启。上限更小：大的多模态请求走不了选择加入的账号类型 |
| 8 | 厂商 SDK 是否按厂商区别对待 | **是**：Anthropic 系（含 Bedrock、Vertex 上的 Claude）用 anthropic-sdk-go 的截取转发；Azure 用 openai-go；OpenAI 直连和 Gemini 用原始请求加官方鉴权库；AWS 非 Claude 模型用官方签名器 | 一律要求用 SDK 客户端：Google 只能走类型化接口，请求里的新字段和回答的原样格式会丢；`bedrockruntime` 的流式接口要伪造事件流 |
| 9 | `gateway.body` 的风险级别 | **high** | critical：每次安装和升级的确认更重，官方插件也一样；好处是第三方插件拿提示词的门槛更高 |
| 10 | 默认的字段加补丁模式最终是否删除，全部平台插件都改成完整请求体 | **不删**：只有清单的插件和简单插件用它更省，也不把提示词交出去 | 删：契约只剩一种模式，但只有清单的四个插件要改由一个随核心发布的执行器进程承载，所有平台插件都要申请 `gateway.body` |
| 11 | 第三方市场插件是否可以申请 `gateway.body` | **可以**，靠 high 级别的确认和"没有 `net` 就发不出去"的约束；同时持有 `net` 时给组合警告 | 只允许官方插件：第三方的云厂商插件做不了深度改造和自签名 |

## 16. 未验证事项

1. 完整请求体往返增加的延迟和插件内存峰值没有测量，2.3 的数字是按内存拷贝速度估的。
2. 执行路径每请求的数据库写入在高流量插件上的成本没有测量。
3. 插件给出的 URL 经核心重新解析再发出后是否逐字节不变（路径里的 `%3A`、`%2F`，查询串顺序）没有测试过。
4. 账号没有会话令牌而运营方透传或设置了 `x-amz-security-token` 时上游的行为。
5. anthropic 平台的 messages 端点是否属于"由插件解析用量"，从而让 bedrock 不能走执行路径（第 12 节第 9 条）。
6. 新宿主权限是否需要改授权存储的结构或迁移。
7. 厂商 SDK 的结论全部来自读源码，没有编译运行。逐条标了"未验证"的有：openai-go 的环境变量默认值在 `Post` 路径是否生效；Azure 包对原始请求体的模型到部署的改写；anthropic-sdk-go 的 OpenTelemetry 默认是否关闭；`genai` 流式响应是否填充 `HTTPResponse.Body`；`bedrockruntime` 的默认重试次数、SDK 自加的请求头、在签名前插入请求头的中间件位置；`aws-sdk-go-v2/aws/signer/v4` 的函数签名（只确认了包存在）。
8. 截取转发方式下，anthropic-sdk-go 的 `bedrock` 中间件产出的请求经核心的请求头分层后签名是否仍然有效，只做了纸面分析（5.3）。
9. 模型引用守卫和用量规则守卫（`dispatch.go:714-737`）对插件大幅改造过的请求体是否会误报。bedrock 今天用补丁删掉 `model`、`stream` 能通过，更大的改造没有试过。
10. 方案一的全部性能数字（每帧开销、TLS 在插件进程里的 CPU）没有测量。
11. new-api 的行为除 6.5 标注的两处外，是主控提供的结论，我没有逐条复核。
