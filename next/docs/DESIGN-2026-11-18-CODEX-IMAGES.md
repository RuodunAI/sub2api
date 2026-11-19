# Codex 账号服务 OpenAI 生图接口：`/v1/images/*` 经核心转换成 Responses 生图

2026-11-18，所有者问：“现在我们的 codex 账号里面，我觉得是可以使用 response 协议，然后带上生图工具，就能调用服务端生成图片了，是吗？如果是的话呢，那我们的 Codex 它就要兼容 OpenAI 的生图协议去转成 response 生图，你确定一下呢？”

本文只做可行性核实与设计，没有写产品代码。基线是 `feat/upstream-plugins`（含 §118 的生图端点）；Responses 里生图工具的附加用量（§118.10、§120）在 `feat/up-p10`，本文依赖它。

## 0. 结论

1. **能生图**。Codex 账号（设备码登录，上游 `chatgpt.com/backend-api/codex/responses`）发带 `image_generation` 工具的 Responses 请求能出图：2026-11-18 在测试实例上两轮共 7 次出图请求全部成功（第 2 节），编辑图片（输入图片 + `action: "edit"`）也可以。之前文档里“Sign in with ChatGPT 账号经生图工具被上游拒绝”（§118.5 的表）说的是已删除的 `chatgpt_siwc` 类型（上游 `api.openai.com`），不适用于现在的两种 codex 账号；§118.5 的那一行要改。
2. **转换可行，推荐方案 (a)**：在核心转换层加两对同平台转换 `openai.images → openai.responses`、`openai.images_edit → openai.responses`，账号类型在清单里**显式声明**要经适配服务这两个端点（codex 两种类型声明，别的类型不受影响）。请求侧把生图参数放进 `image_generation` 工具、强制调用；应答侧把 `image_generation_call` 的结果和 `tool_usage.image_gen` 转回生图接口的 JSON 或事件流。计费：生图部分是主用量，按客户端请求的生图模型计价；对话部分（Codex 后端约 2300 输入 token 的固定开销）作为附加用量按驱动对话的模型计价。
3. **这一版的范围**：generations 的非流式与流式、edits（JSON 形式）的非流式与流式都做。不做：`n > 1`（400）、`response_format: "url"`（400）、edits 的 multipart（§118.1 既有限制）、Codex 后端的专用生图接口（未实测，第 7 节）。
4. **已知限制**：Codex 后端不按工具参数里的 `size` 出图（由驱动模型决定，回显恒为 `auto`），实测在 `instructions` 里写明尺寸后按要求出图，转换器因此把尺寸等写进一句固定的 `instructions`；没有 `partial_image` 事件（请求了也不给）；每次多约 2300 个对话输入 token；上游回显的生图模型是 `gpt-image-2-codex`，没有公开单价；工具参数里写什么生图模型（包括不存在的名字）都按 `gpt-image-2-codex` 出图，`quality` 恒为 `low`（写 `high`、在 `instructions` 里要求 `high` 都不变）。

## 1. 现状

### 1.1 代码（`feat/upstream-plugins`）

- **端点**：`sdk/platforms/openai.json` 有 `images_generations`（`POST /v1/images/generations`，协议 `openai.images`）、`images_edits`（`openai.images_edit`），只收 JSON，`modelPath: model`、`streamPath: stream`，应答 `stream: sse`、`nonStream: json`，用量规则读 `usage.*`（§118.1、118.2）。
- **谁服务**：openai `apikey`（不写端点子集）和 New API、Sub2API 中转插件的类型；codex `codex_device`、`codex_oauth` 的子集是 `responses`、`responses_websocket`，不是候选（§118.5，`gateway/images_test.go` `TestOpenAIImages`）。
- **转换层**（`server/internal/gateway/convert`）：`Converter` 接口是 `From/To/Grade/Prepare/Response/NewStream`（`convert.go`），`Prepare` 返回绑定到这次请求的副本和上游请求体，`Response` 转非流式应答，`NewStream` 逐事件转流。登记在 `builtins.go`：独立实现的 `chatResponses`（`openai.chat → openai.responses`，§83.2）加 `builtinPairs` 里经 IR 的跨平台对；没有直连的跨平台对经 `openai.responses` 两跳组合（`compose`，同平台之间不组合）。
- **路由**（`gateway/routing.go` `planRoutes`）：类型在入站平台的子集里有该端点 → 原生；否则 `addConvertedRoute(b, x, samePlatform)`：类型声明了入站平台时只在本平台子集里找能转到的端点（§83.3 同平台适配），没声明时找其他平台（跨平台转换）。转换路由的 `usage` 取**上游端点**的规则（`usagerules.For(ap, &ep, &pf, y)`，`y` 是上游协议），`modelPath`、`streamPath` 也是上游端点的。
- **协议绑定**（§110）：同平台适配算“本平台协议”，默认对所有账号、所有模型开放，可以在账号的 `protocols` 里关；跨平台转换默认关闭，要在 `model_protocols` 里按模型开。所以“只服务 `openai.responses` 的 codex 账号服务 `openai.chat`”今天就是默认开着的同平台适配，不需要开转换；`openai.images → openai.responses` 若照现有规则登记，也会是默认开着的同平台适配。
- **问题**：§83.3 的同平台适配对所有类型自动生效。只要核心登记了 `openai.images → openai.responses`，`azure_v1`、volcengine `apikey`/`relay`（子集含 `responses` 不含生图）也会自动成为生图候选——volcengine 正是 §118.5 刻意排除的（方舟的 Responses 没有这个工具，请求会在上游失败并触发故障策略）。所以要加一个“类型显式声明”的开关（3.1）。
- **模型**：`upstreamBody`（`gateway/dispatch.go`）在转换之后把账号映射后的模型写到路由的 `modelPath`（上游 Responses 的顶层 `model`）。生图转换后顶层 `model` 必须是对话模型，生图模型在 `tools[0].model`，照现在的写法映射后的生图模型会覆盖掉对话模型——要让转换器告诉网关“客户端模型在转换后的请求体里的位置”（3.4）。
- **只流式**：codex 声明 `streamOnly: ["openai.responses"]`，非流式客户端由网关强制流式、把终态事件的 `response` 聚合成 JSON，并按 `output_index` 把 `response.output_item.done` 的项补回 `output`（Codex 后端终态 `output` 是 `[]`，§83.1、§120.5）。转换路由的非流式请求可以选只流式的目标（`addConvertedRoute` 的条件里有 `ap.StreamsOnly`），chat 适配就是这样工作的。
- **codex 插件**（0.2.0）`BuildUpstream`：`store=false`、字符串 `input` 变成一条 user 消息、`system` 改 `developer`、缺 `instructions` 时补 `""`、删除上游不认的字段（含 `user`、`safety_identifier`），上报的上游模型是请求体的 `model`（`pluginsdk.RequestModel`）。插件不注入系统提示词。

### 1.2 实测（测试实例，经网关发 `/v1/responses`，codex 插件 0.1.11，账号都是 `codex_device`）

**之前一轮（所有者转述）**：`gpt-6-luna` + `{"type":"image_generation","output_format":"png","quality":"low","size":"1024x1024"}`、强制工具、流式：出图 1254x1254 PNG（base64 约 1.28 MB），`usage` 2304/52，`tool_usage.image_gen` 输入 39 / 输出 515；非流式、最朴素的工具同样出图。

**这一轮（2026-11-18，临时 Key，用完已删）**，每次都是 `model: gpt-6-luna`、`stream: true`、`tool_choice: {"type":"image_generation"}`、字符串或数组 `input`、无害的简单提示词：

| # | 目的 | 请求里的工具参数 / 其它 | 结果 | 图片 | `usage` 对话 in/out | `tool_usage.image_gen` in(文字/图片)/out(图片) |
|---|---|---|---|---|---|---|
| 1 | 转换后的最小形状；jpeg、尺寸、partial | `output_format: jpeg`、`quality: low`、`size: 1024x1536`、`partial_images: 1`；无 `instructions` | 200，账号 8 | 出图 1 张，jpeg，项里 `size: "1536x1024"`（要的是竖图，给了横图），`quality: "low"` | 2235 / 69 | 56 (56/0) / 343 (343) |
| 2 | 工具参数 `n` | `n: 2` | **400** `unknown_parameter`，`Unknown parameter: 'tools[0].n'.`（未出图，账号 7 未冷却） | — | — | — |
| 3 | 工具 `model`、webp | `model: gpt-image-2`、`output_format: webp`、`size: 1024x1024`、`quality: low` | 200，账号 8 | 出图，webp 1254x1254（要的 1024x1024） | 2294 / 54 | 41 (41/0) / 515 (515) |
| 4 | 编辑 | `input` 为一条消息：`input_text` + `input_image`（64x64 纯红 PNG，136 字节，data URL）；`action: edit`、`partial_images: 2`、`size: 1024x1024` | 200，账号 4 | 出图，png 1254x1254，项里 `action: "edit"` | 2306 / 56 | 67 (51/16) / 515 (515) |
| 5 | 顶层 `model` 用生图模型 | `model: gpt-image-2` | **403** `model_price_not_configured`（实例上没给这个模型录价，网关在调度前拒绝，没发往上游） | — | — | — |
| 6 | 用 `instructions` 指定尺寸 | `instructions` 一句话写明 “size 1024x1536 (portrait) and quality low”；工具 `size: 1024x1536` | 200，账号 6 | 出图，png **1024x1536**，项里 `size: "1024x1536"` | 2279 / 77 | 未记（这个实例的旧版本不存附加用量） |
| 7 | 一次要两张 | 提示词要求“两张，每次调用一张”，工具只有格式和画质 | 200，账号 4 | **出图 2 张**（两个 `image_generation_call`，`output_index` 0、1），都是 1254x1254，另有一段文字回复 | 4340 / 167 | 未记 |

共 5 次实际出图（1、3、4、6、7）。事件与形状：

- 事件类型计数（单张）：`response.created`、`response.in_progress`、`response.output_item.added`×2、`response.image_generation_call.in_progress`、`.generating`、`.completed` 各 1、`response.output_item.done`×2、`response.content_part.added`/`done`、`response.output_text.done`（有时还有 `.delta`）、`response.metadata`、`response.completed`。两张图时 `image_generation_call.*` 各 2 次、`output_item.*` 各 3 次。
- **没有 `partial_image` 事件**：请求 1（`partial_images: 1`）、4（`partial_images: 2`）都没有。
- 图片之后总有一个 `phase: "final_answer"` 的 assistant 消息，文字为空串，或 “Done.”，或一句说明（请求 7）。
- `response.created` 与 `response.completed` 的 `tools` 回显恒为 `{"type":"image_generation","background":"auto","model":"gpt-image-2-codex","moderation":"auto","n":1,"output_compression":100,"output_format":<请求的格式>,"quality":"auto","size":"auto"}`：`output_format` 按请求生效（jpeg、webp、png 都对），`model` 被改写，`size`、`quality` 回显为 `auto`。`image_generation_call` 项本身带 `action`、`background`（`opaque`）、`output_format`、`quality`、`size`、`revised_prompt`、`result`。
- `size` 不按工具参数生效（请求 1、3、4），在 `instructions` 里写明后生效（请求 6）：尺寸是驱动模型发起工具调用时自己定的。`quality` 每次都是 `low`：第二轮写 `high`（工具参数，以及工具参数加 `instructions`）仍是 `low`，图片输出 token 只随尺寸变（1536x1024 为 343，1254x1254 为 515），所以 Codex 后端不接受画质参数。
- `tool_usage.image_gen` 的形状与生图接口的 `usage` 一样：`{input_tokens, input_tokens_details{image_tokens, text_tokens}, output_tokens, output_tokens_details{image_tokens, text_tokens}, total_tokens}`。
- 对话部分每次约 2235–2306 输入、54–77 输出；要两张图时翻倍（4340 / 167，第二次工具调用把第一次的结果带进了上下文）。这个实例的价格下对话部分每次记 0.000256–0.000266 美元（两张时 0.000518）。
- 终态 `output` 为 `[]`，图片只在 `response.output_item.done` 里（与之前一致）。

### 1.2.1 第二轮实测：生图模型名（2026-11-18，临时 Key，用完已删）

测试实例上先录了生图模型价格、给 7 个 `codex_device` 账号的模型列表追加了生图模型（所有者要求保留）。每次 `model: gpt-6-luna`、`output_format: png`、`quality: low`（第 7、8 次除外）、强制工具、流式：

| # | 请求里的生图模型 | 结果 / 账号 | 回显 | 图 | 对话 in/out | image_gen in/out |
|---|---|---|---|---|---|---|
| 1 | 顶层 `model: gpt-image-2` | **400** `model_not_found`：“The 'gpt-image-2' model is not supported when using Codex with a ChatGPT account.” / #2 | — | 无 | — | — |
| 2 | 工具 `model: gpt-image-0`（不存在） | 200 / #5 | `gpt-image-2-codex` | png 1254x1254 | 2317/74 | 61/515 |
| 3 | `gpt-image-2.5-sunburst` | 200 / #7 | `gpt-image-2-codex` | png 1536x1024 | 2239/73 | 60/343 |
| 4 | `gpt-image-2.6-flare` | 200 / #5 | `gpt-image-2-codex` | png 1536x1024 | 2233/67 | 54/343 |
| 5 | `gpt-image-1.5` | 200 / #7 | `gpt-image-2-codex` | png 1536x1024 | 2238/72 | 59/343 |
| 6 | `gpt-image-1-mini` | 200 / #2 | `gpt-image-2-codex` | png 1536x1024 | 2231/65 | 52/343 |
| 7 | 不写模型，`quality: high` | 200 / #4 | `gpt-image-2-codex`，图项 `quality: low` | png 1254x1254 | 2313/70 | 57/515 |
| 8 | 同 7，另加 `instructions` 要求 quality high、size 1024x1024 | 200 / #7 | 同上，图项 `quality: low` | png 1254x1254（没有按 1024x1024） | 2358/83 | 70/515 |

- 结论：Codex 后端的生图模型只有一个。工具参数里的 `model` 不校验、不生效，写什么都改写成 `gpt-image-2-codex`；顶层 `model` 写生图模型被拒（顶层只认对话模型）。`quality` 恒为 `low`。
- `instructions` 指定尺寸：竖图 1024x1536 生效（第一轮请求 6），方图 1024x1024 不生效（给了 1254x1254）。可能只认几种宽高比，方图固定 1254x1254。
- 都没有 `partial_image` 事件。事件计数与第一轮单张时相同。

### 1.3 公开资料（2026-11-19 抓取，`developers.openai.com`；`github.com/openai/codex` HEAD）

- Responses 的 `image_generation` 工具参数：`action`（`generate`/`edit`/`auto`）、`background`、`complexity`、`frame_count`、`frame_duration_ms`（gif）、`input_fidelity`（gpt-image-2 不要传）、`input_image_mask`（`{file_id | image_url}`）、`model`、`moderation`、`output_compression`、`output_format`（`png`/`webp`/`jpeg`/`gif`/`svg`）、`partial_images`（0–3）、`quality`、`size`、`svg`。**没有 `n`**。强制调用写 `tool_choice: {"type": "image_generation"}`。一次调用一张图，多张靠模型多次调用（官方示例取 `[0]`；`tool_usage.image_gen` 是各次调用之和）。
- 编辑：`input` 里放 `{"type":"input_image","image_url":"data:…"}` 或 `{"type":"input_image","file_id":"…"}`；“强制 `edit` 而上下文里没有图片会报错”。
- 流式事件 `response.image_generation_call.partial_image`：`partial_image_b64`、`partial_image_index`（0 起）、`output_index`、`item_id`，可选 `background`、`output_format`、`quality`、`size`。`image_generation_call` 项失败时带 `error{code, message, moderation_details?}`。
- 生图接口：generations 参数 `model`（必填）、`prompt`、`background`、`complexity`、`frame_*`、`moderation`、`n`（1–10）、`output_compression`、`output_format`、`partial_images`、`quality`、`response_format`（GPT 图片模型“不支持，总是 base64”）、`safety_identifier`、`size`、`stream`、`style`、`svg`、`user`；edits 的 JSON 形式另有 `images: [{image_url | file_id}]`（1–16 张）、`mask: {image_url | file_id}`、`markup`、`input_fidelity`。应答 `{created, background, data:[{b64_json}], output_format, quality, size, usage{…}}`；流式 `image_generation.partial_image` / `image_generation.completed`（edits 为 `image_edit.*`），字段 `b64_json`、`background`、`created_at`、`output_format`、`quality`、`size`、`partial_image_index`（只在 partial）、`usage`（只在 completed）。审核拦截的错误 `{"error":{"type":"image_generation_user_error","code":"moderation_blocked","moderation_details":{…}}}`。
- 官方 Codex 客户端：2026-07-09（commit `a7c72aee8b`）之前在主请求里带 `{"type":"image_generation","output_format":"png"}`，`tool_choice: "auto"`；之后改为客户端函数工具 `image_gen.imagegen`，由客户端另发 `POST {base}/images/generations`、`/images/edits`（`base` = `https://chatgpt.com/backend-api/codex`），见第 7 节。

## 2. 方案比较

| | 做法 | 可行性 | 改动面 | 对别的类型 |
|---|---|---|---|---|
| (a) **推荐** | 核心转换层加 `openai.images`/`openai.images_edit` → `openai.responses`，账号类型显式声明经适配服务 | 实测覆盖了请求形状、出图、编辑、用量 | 核心：一个转换器、路由读声明、转换路由的用量规则与模型位置、应答侧错误；清单两个字段；codex 插件清单与一处上报 | 只对声明的类型生效 |
| (b) | codex 插件声明原生服务生图端点，`BuildUpstream` 里改写成 Responses | 请求能改，**应答不能**：转发 V2 下插件交回请求、核心把上游应答原样流回客户端，插件没有改应答的途径（`responseStream` 只是帧格式解码）；客户端会收到 Responses 事件流 | 要在核心加“按插件声明转应答”，等于把 (a) 的应答侧做一遍，还多一条特殊路径 | 无 |
| (c) | 走 Codex 后端专用接口 `/backend-api/codex/images/generations`、`/images/edits` | **未实测**（这次不允许绕过网关直连）。形状接近生图接口（第 7 节），若可用则 codex 插件原生声明这两个端点即可，核心零改动，也没有对话开销 | 只改 codex 插件 | 无 |

(c) 若可用是最省的，但它是 Codex 客户端私有的接口：只有非流式；请求头（`x-codex-image-turn-id` 等）、能否接受 sup2api 的如实标识、`n`/`size` 的取值、额度是否另算都不知道。建议另开一项**先实测 (c)**（需要所有者允许在测试实例部署一个带该路由的 codex 构建，或允许直连一次）；本文按 (a) 设计，两者不冲突：类型原生声明了端点就走原生，原生优先（§83.3）。

## 3. 设计

### 3.1 账号类型显式声明适配（清单）

`manifest.AccountPlatform` 加 `adapted`：

```json
"platforms": [{
  "platform": "openai",
  "endpoints": ["responses", "responses_websocket"],
  "adapted": {
    "chat_completions":   {},
    "images_generations": {"model": "gpt-6-luna"},
    "images_edits":       {"model": "gpt-6-luna"}
  },
  "passHeaders": [],
  "streamOnly": ["openai.responses"]
}]
```

- 键是本平台的端点 id，值是给转换器的选项（目前只有 `model`：生图转换里驱动工具调用的对话模型，见 3.3）。
- **同平台适配只对列出的端点生效**（§83.3 改写）：`planRoutes` 里 `addConvertedRoute(b, x, samePlatform=true)` 只在 `ap.Adapted` 含客户端端点 id 时调用。账号模块的 `servedEndpoints`、`typeProtocols`、`served_protocols` 用同一判断。chat 适配今天对所有类型自动生效，改成显式后 codex 要列 `chat_completions`；仓库里别的插件都原生服务 chat，不受影响（第三方插件若依赖隐式 chat 适配要补声明，不做兼容）。
- 清单校验（`sdk/manifest/check`）：键必须是该平台的端点、不在 `endpoints` 子集里、不是 WebSocket 或任务端点；值的 `model` 若写了是完整模型 ID。核心绑定注册表时再查一次核心确实有“该端点协议 → 子集内某端点协议”的转换器，没有则拒绝绑定该类型（与清单引用不存在的协议同一处理）。
- 跨平台转换不变（仍按 §110 在账号上按模型开）。

### 3.2 请求转换（`openai.images` / `openai.images_edit` → `openai.responses`）

新转换器 `convert/openai_images_responses.go`，一个类型两份实例（generations、edits），`Grade` 为 `GradeGood`，不经 IR（与 `chatResponses` 一样独立实现）。

| 生图接口 | Responses | 说明 |
|---|---|---|
| `model` | `tools[0].model` | 客户端的生图模型；账号映射后的值也写在这里（3.4） |
| — | `model` | 驱动模型：`adapted` 选项的 `model`（3.3） |
| `prompt` | `input[0]`：`{"type":"message","role":"user","content":[{"type":"input_text","text":prompt}]}` | 编辑时同一条消息里接着放图片 |
| `images[i].image_url` / `images[i].file_id`（edits） | `input[0].content[1+i]`：`{"type":"input_image","image_url":…}` / `{"type":"input_image","file_id":…}` | 原样搬；`file_id` 能不能被 Codex 后端认得是上游的事 |
| `mask`（edits，`{image_url | file_id}`） | `tools[0].input_image_mask` | 原样搬 |
| — | `tools[0].type: "image_generation"`、`tool_choice: {"type":"image_generation"}` | 强制调用 |
| — | `tools[0].action`：generations 写 `"generate"`，edits 写 `"edit"` | |
| `background`、`quality`、`size`、`output_format`、`output_compression`、`moderation`、`partial_images`、`input_fidelity`、`complexity`、`svg`、`frame_count`、`frame_duration_ms` 以及其它未列出的成员 | `tools[0]` 的同名成员 | 原样搬，值不检查（上游认不认由上游答，如 `tools[0].n`） |
| `size`、`quality`、`background` 有值时 | `instructions`：一句固定的英文，例 `Generate the image with the image_generation tool using size 1024x1536, quality low.` | 实测 Codex 后端只有这样才按尺寸出图（1.2 的请求 1 与 6）；对官方 Responses 是多余但无害的同义说明。没有这三个参数时不写 `instructions` |
| `stream` | `stream` | codex 是只流式类型，网关照常强制 `true`（§83.1） |
| `n` | — | 缺省或 `1` 照常；`> 1` **拒绝**（`RequestError` → 400 `unsupported_conversion`），见第 5 节决定 |
| `response_format` | — | 缺省或 `"b64_json"` 照常；`"url"` 拒绝（400）：上游只回 base64，网关不存图 |
| `user`、`safety_identifier`、`style` | — | 丢弃，记 `LossOther`（`user`、`safety_identifier` 即使搬过去 codex 插件也会删） |

- 固定写 `store: false`？不写：codex 插件已经写，别的类型以后声明时由它们的插件决定。
- 转换器不加 `parallel_tool_calls`、`reasoning` 等字段：上游的默认值就是实测的条件。
- 损失：只有上面三个丢弃的字段，类别 `other`，`strict` 策略的账号会拒绝带这些字段的请求（§88.4，既有规则）。

### 3.3 对话模型从哪来

- 转换后的顶层 `model` 必须是上游认的对话模型；它是网关为了调用工具而用的，客户端看不到、也不选。
- 来源：账号类型在 `adapted` 选项里声明（codex 写 `gpt-6-luna`：实测用的就是它）。插件发版时随 `defaultModels` 一起维护。
- 转换器怎么拿到：`convert.Options` 加 `AdaptModel string`。`convertOptions` 是一次请求共享的，而这个值随账号类型不同，所以 `prepareRoute` 在调用 `Prepare` 前按路由的 `ap.Adapted[客户端端点].Model` 填进一份拷贝（转换结果本就按路由缓存）。没有声明 `model` 的适配端点对生图转换器是配置错误：`Prepare` 返回非 `RequestError` 的错误，这个类型不服务这次请求（`refuseConversion` 按 WARN 记）。
- 账号上要换驱动模型（例如某个账号的套餐没有 `gpt-6-luna`）：用账号的参数覆盖（§84），条件写 `tools.0.type == image_generation`，改 `model`。不另加界面。
- 驱动模型**不经**分组白名单、账号模型列表、协议绑定（它不是客户端模型）。它的价格只在记附加用量时用（3.6）。

### 3.4 模型与价格：三个模型各自的角色

| 模型 | 例 | 调度：分组白名单、账号 `models`、`model_protocols` | 账号模型映射 | 价格 | 使用记录 |
|---|---|---|---|---|---|
| 客户端请求的生图模型 | `gpt-image-2` | **检查**（与所有请求一样按映射前的客户端模型） | 映射到的值写进 `tools[0].model` | **主用量**按它计价；调度前按 `missing_price_policy` 准入（`reject` 下没价 403） | `model` |
| 上游回显的生图模型 | `gpt-image-2-codex` | 不检查 | — | 不用 | `upstream_model`（取自应答 `tools[0].model`） |
| 驱动对话的模型 | `gpt-6-luna` | 不检查 | 不映射 | **附加用量** `conversation` 按它计价，记录时取价（§120.3、120.4） | `billing_detail.additional[].model` |

- **模型位置**：`Converter` 加一个可选接口 `ModelLocator { ModelPath() string }`。生图转换器返回 `tools.0.model`；`addRoute` 在 `conv` 实现了它时用它作路由的 `modelPath`（代替上游端点的 `model`），`upstreamBody` 的映射因此写到工具里，顶层的驱动模型不被覆盖。chat 等现有转换器不实现它，行为不变。
- **上报的上游模型**：codex 插件今天用 `pluginsdk.RequestModel`（读请求体顶层 `model`）上报上游模型，生图转换后那是驱动模型。插件在 `meta.client_protocol` 为 `openai.images`、`openai.images_edit` 时不上报（`Model("")`），网关就按用量规则读到的 `model`（应答回显 `response.tools.0.model`）写 `upstream_model`（`forward.go` 已有的取法）。
- 管理员要做的：给 codex 账号的模型列表加上 `gpt-image-2`（账号模型列表为空的不用；codex 清单的 `defaultModels` 加上它，新账号预填）；给 `gpt-image-2` 录 §118.6 的表达式价格；驱动模型一般已有价格。建议在 codex 账号的“模型的协议限定”里把 `gpt-image-2` 限定到 `openai.images`、`openai.images_edit`，免得 `/v1/responses` 用这个名字打到 codex（上游会报模型不存在）。

### 3.5 用量规则：转换路由按客户端端点声明的规则读

现在转换路由的用量取上游端点（`openai.responses`）的规则，那样主用量就是对话的 2235/69，却按客户端模型 `gpt-image-2` 的价格算——错。改为：

- `manifest.Endpoint` 加 `convertedUsage`：`{上游协议: UsageRules}`，表示“这个端点经转换由该上游协议服务时，按这份规则从上游应答读用量”。`addRoute` 在 `conv != nil` 且客户端端点为该上游协议声明了规则时用它，否则照旧（chat 等不声明，不变）。规则仍然读**上游**字节（`forward` 的约定不变），只是规则由客户端端点给。
- `openai.json` 给 `images_generations`、`images_edits` 各加一份（两份相同）：

```json
"convertedUsage": {"openai.responses": {
  "semantics": "inclusive",
  "sse": [{"event": "response.completed", "map": {
      "model": "response.tools.0.model",
      "input_tokens": "response.tool_usage.image_gen.input_tokens",
      "output_tokens": "response.tool_usage.image_gen.output_tokens",
      "cache_read_tokens": "response.tool_usage.image_gen.input_tokens_details.cached_tokens",
      "image_input_tokens": "response.tool_usage.image_gen.input_tokens_details.image_tokens",
      "image_output_tokens": "response.tool_usage.image_gen.output_tokens_details.image_tokens"}}],
  "json": {"map": {
      "model": "tools.0.model",
      "input_tokens": "tool_usage.image_gen.input_tokens",
      "output_tokens": "tool_usage.image_gen.output_tokens",
      "cache_read_tokens": "tool_usage.image_gen.input_tokens_details.cached_tokens",
      "image_input_tokens": "tool_usage.image_gen.input_tokens_details.image_tokens",
      "image_output_tokens": "tool_usage.image_gen.output_tokens_details.image_tokens"}},
  "additional": [{"name": "conversation", "object": true,
      "jsonPath": "usage", "ssePath": "response.usage",
      "modelSources": [{"from": "sse", "modelPath": "response.model"},
                       {"from": "json", "modelPath": "model"},
                       {"from": "request", "modelPath": "model"}],
      "semantics": "inclusive",
      "map": {"input_tokens": "input_tokens", "output_tokens": "output_tokens",
              "cache_read_tokens": "input_tokens_details.cached_tokens"}}]
}}
```

- 主用量 = `tool_usage.image_gen`，与 §118.2 的含义相同（图片 token 是输入、输出的一部分）。非流式客户端经强制流式聚合后按 `json` 规则读（§120.5）。
- 对话部分是 §120.2 的对象形式附加用量，种类名 `conversation`。附加用量对象形式的 `map` 现在是否接受 `cache_read_tokens` 要在实现时核对（`check/additional_usage.go`）；不接受就只映射输入输出。
- 这份规则替代上游 Responses 规则里的 `image_generation` 附加用量（`feat/up-p10`）：转换路由用这份，就不会把生图部分既当主用量又当附加用量计两次。清单检查“同一路径不能被两条规则计两次”照常适用于这份规则内部。
- `usage.json` 的 `model` 路径 `tools.0.model` 能通过路径检查（`check/usage.go` 的 sjson 探测）。

### 3.6 计费

- 一条记录：`protocol = openai.images`（或 `openai.images_edit`）、`upstream_protocol = openai.responses`、`conversion` 的 hops 为 `[openai.images, openai.responses]`；`model = gpt-image-2`，`upstream_model = gpt-image-2-codex`；token 列是生图部分；`billing_detail.additional[]` 有一项 `conversation`（驱动模型）；`total_cost` 是两者之和（§120.3）。
- 驱动模型没有价格时按 §120.4：`reject` 下对话部分不计、记录标 `additional_unpriced`；`free` 下不计。生图部分照常计。
- 预扣只按客户端模型估算（§120.5 不变），对话部分在结算时按实际扣。
- **算例**（`gpt-image-2` 价 `p*5 + cr*1.25 + img*8 + img_o*30 + c*30`；驱动模型价**假设**为 `p*1.25 + cr*0.125 + c*10`，以管理员录的为准；单位美元/百万 token，结果微美元）：
  - 1.2 的请求 1（生成）：生图部分输入 56（全是文字）、输出 343（全是图片）→ `p = 56`、`img_o = 343`、`c = 0` → 56×5 + 343×30 = 280 + 10 290 = **10 570**；对话部分 2235 / 69 → 2235×1.25 + 69×10 = 2 793.75 + 690 = **3 483.75**；合计 14 053.75 微美元 ≈ 0.0141 美元，其中对话占约 25%。
  - 请求 4（编辑）：生图部分输入 67（文字 51、图片 16）、输出 515 → 51×5 + 16×8 + 515×30 = 255 + 128 + 15 450 = **15 833**；对话 2306 / 56 → 2 882.5 + 560 = **3 442.5**；合计 19 275.5。
- 对客户端：应答的 `usage` 只是生图部分（3.7），扣费里多出的对话部分在使用记录的计费明细里看得到。

### 3.7 应答转换

**非流式**（客户端 `stream` 缺省或 false；codex 上游是强制流式聚合出的 `response` 对象，别的上游是普通 JSON）：

```json
{"created": <response.created_at>,
 "background": <第一张图的 background>, "output_format": …, "quality": …, "size": …,
 "data": [{"b64_json": <output[i].result>, "revised_prompt": <output[i].revised_prompt>}],
 "usage": <tool_usage.image_gen 原样>}
```

- `data` 按 `output` 顺序取每个 `type == "image_generation_call"` 且有 `result` 的项（一般一张；上游给两张就是两项，第 5 节）。
- `background`、`output_format`、`quality`、`size` 取第一张图项里的值（上游实际的，不是请求的）。
- `usage` 原样搬 `tool_usage.image_gen`（形状与生图接口相同）；没有时不写。
- assistant 的 `message` 输出项（空串、“Done.”或说明文字）不进应答，记一条应答损失 `response.output[i]`（`LossOther`）。

**流式**（客户端 `stream: true`）：

| 上游事件 | 客户端事件（edits 把 `image_generation.` 换成 `image_edit.`） |
|---|---|
| `response.image_generation_call.partial_image` | `image_generation.partial_image`：`{type, b64_json: partial_image_b64, partial_image_index, background, output_format, quality, size, created_at}`（Codex 后端实测不发，规则照官方写） |
| `response.output_item.done`，项为 `image_generation_call` 且有 `result` | 暂存（`usage` 要等终态） |
| `response.completed`（或 `response.incomplete`） | 每张暂存的图一个 `image_generation.completed`：`{type, b64_json, background, output_format, quality, size, created_at}`，最后一个带 `usage`（`tool_usage.image_gen`） |
| `response.failed`、`error` 事件、`status == "failed"` 的生图项 | `error` 事件（3.8） |
| 其它（`created`、`in_progress`、文字增量、`metadata` 等） | 不输出 |

- `created_at` 取 `response.created` 事件里的 `response.created_at`。
- 流以最后一个 `completed` 或 `error` 结束，不发 `[DONE]`（生图接口没有）。

### 3.8 错误映射

- **上游 HTTP 错误**（4xx/5xx，OpenAI 错误形状）：两个端点的 `errorFormat` 都是 `openai`，照现有路径原样交给客户端、按故障策略处理，不经转换器。实测的 `tools[0].n` 400 就是这样回来的（但转换器已经在本地拒绝 `n > 1`，不会走到上游）。
- **流里的失败**（`response.failed`、带 `status` 的 `error` 事件）：非流式时已由强制流式聚合处理（§83.1：502 或事件里的状态，交插件分类、可冷却）；流式时转换成 `{"type":"error","error":<上游的 error 对象>}`。
- **生图项失败**（`image_generation_call.status == "failed"`，带 `error{code, message, moderation_details}`，如 `moderation_blocked`）：非流式返回 **400**，正文 `{"error":{"type":"image_generation_user_error","code":<上游 code>,"message":…,"moderation_details":…}}`（上游给什么搬什么）；流式发同一对象的 `error` 事件。Codex 后端审核拦截的真实形状没有测（不发违规提示词），按官方文档写，实现后用 mock 覆盖，以后见到真实样本再核对。
- **没有出图**（终态里没有任何带 `result` 的生图项，例如驱动模型只回了文字）：非流式返回 **502** `{"error":{"type":"server_error","code":"no_image_generated","message":<assistant 的文字，没有则固定一句>}}`；流式发同一对象的 `error` 事件。上游已经返回成功，按 §25 的“已到达上游”计：用量照读、照扣（对话部分和可能为 0 的生图部分），不换号。
- 实现：转换器的 `Response` 现在只能返回字节（状态码沿用上游的 200）。加 `convert.ClientError{Status int, Body []byte}`：`forwardJSONConverted` 遇到它按其状态码写正文，记录 `status_code` 与 `error_type`（`invalid_request` / `upstream_error`）；不算转换失败（不是 `convertError`）。流式里错误本来就是一条事件，不需要它。

### 3.9 只流式账号、客户端断开

- 非流式客户端：网关强制流式、聚合终态、补回 `output`（§83.1、§120.5 已有），再交给转换器的 `Response`。转换器只看到一个完整的 Responses JSON，与普通 OpenAI 上游的非流式应答相同。
- 流式客户端：转换器按 3.7 逐事件输出；图片在终态前暂存，所以客户端会在 `response.completed` 时一次收到 `completed` 事件（Codex 后端本来也是一次给全图）。
- 客户端断开：§112 的既有规则（为计费读完应答，在既有时限内），不加新规则。读到终态才有生图用量；读不到时生图部分没有用量，对话部分按 §25.8 的既有规则。

### 3.10 协议绑定：管理员要做什么、默认开不开

- 声明了 `adapted` 的端点对该类型是**本平台协议**（`via: adapted`），`protocols` 为 `null`（默认）的 codex 账号升级后**自动开放** `openai.images`、`openai.images_edit`（§110.2 的既定行为）。决定：默认开（第 5 节）。
- 但实际能被调度还要过模型白名单与价格：账号模型列表里没有 `gpt-image-2`（codex 账号一般预填了对话模型），或 `gpt-image-2` 没有价格（`reject`），请求就不会到 codex。所以管理员要做的三件事是：账号模型列表加 `gpt-image-2`、给它录价、（建议）在“模型的协议限定”里把它限定到两个生图协议。
- 不想让某个 codex 账号出图：在“本平台协议”里取消这两个协议（`protocols` 指定）。
- 控制台：“本平台协议”卡里适配的协议已经显示“适配 → responses”（§110.9），账号详情的“可服务的端点”按 `served_protocols` 显示，都不用改；`GET /account-types` 的 `endpoints` 会多出两项 `native: false`。

## 4. 契约改动、测试、任务拆分

### 4.1 契约改动清单

- **CONTRACTS**：新增一节（暂编 §121“codex 账号经适配服务 OpenAI 生图”）写 3.1–3.10；§83.3 改为“同平台适配只对 `adapted` 列出的端点”；§110.1 的“本平台协议”定义里同平台适配加这个前提；§118.5 的表改 codex 一行（经适配服务、旧说法“被上游拒绝”删掉）、§118.9 去掉相应的“以后”；§118.10 注明经生图接口转换的请求用 `convertedUsage`、不再有 `image_generation` 附加项；§88.1 的转换对列表。
- **`sdk/manifest`**：`AccountPlatform.Adapted map[string]AdaptOptions`（`AdaptOptions{Model string}`）、`Endpoint.ConvertedUsage map[string]UsageRules`；`check` 的两组校验（3.1、3.5）。
- **`sdk/platforms/openai.json`**：两个生图端点加 `convertedUsage`。
- **CONVERT-MATRIX**：§0 的转换对表加两行（同平台、`good`）；§9.9 逐对要点加生图一对（3.2、3.7 的表）；§5.1 写 `adapted`；§7 使用记录说明 hops。
- **codex 插件**：清单 `adapted`（三项）、`defaultModels` 加 `gpt-image-2`、描述里写“经核心转换提供 /v1/images/generations、/v1/images/edits”；`BuildUpstream` 在生图客户端协议下不上报上游模型；版本 0.2.0 → 0.3.0，`hostCompat` 提到带本功能的核心；`plugins/released.json`。
- 不改 proto、Host API、迁移。

### 4.2 测试计划

- **转换器单元与黄金样例**（`convert/testdata`）：generations / edits 的请求映射（每个字段、`instructions` 有无、`n: 1` 照常、`n: 2` 与 `response_format: url` 拒绝、`user` 等记损失、未知成员搬进工具）；非流式应答（一张、两张、无 `usage`、只有文字 → `ClientError` 502、生图项失败 → 400）；流式（单张、两张、带 partial、`response.failed`、`error` 事件、无终态时 `Flush` 不输出 completed）。黄金样例用这次实测的形状（`result` 换成短串），覆盖测试（`golden_coverage_test.go`）登记两对。
- **清单与注册表**：`adapted` 的每个错误码；没有转换器的适配端点拒绝绑定；`convertedUsage` 的键与规则校验。
- **网关**（`gateway/images_test.go` 扩充，用假插件与 mock 上游）：声明 `adapted` 的只流式类型服务非流式与流式生图（上游收到的请求体、`tools[0].model` 是映射后的模型、顶层是驱动模型）；未声明的类型（azure 式、volcengine 式子集）不是候选；原生类型优先；chat 适配改显式后 codex 仍服务 chat，未声明的类型不再适配；记录的 `protocol`/`upstream_protocol`/`model`/`upstream_model`/token 列/附加项/金额；驱动模型无价（`reject`、`free`）；`ClientError` 的状态码与记录；参数覆盖改驱动模型。
- **账号模块**：`served_protocols` 与 `/account-types` 的 `endpoints` 跟随 `adapted`（`TestServedProtocolsMatchScheduling` 照常对拍）。
- **codex 插件**：执行测试覆盖生图客户端协议下不上报模型、请求体改写照旧；合规套件不变。
- **e2e**：mock 上游的 Codex Responses 生图应答（流式事件序列取实测），一个用例走 `/v1/images/generations` 非流式与流式、一个走 `/v1/images/edits`。
- **实测**：在测试实例上部署后，用临时 Key 各发一次 generations（非流式、流式）和 edits，核对出图、记录与金额（部署前需所有者允许）。

### 4.3 任务拆分

| 包 | 内容 | 主要文件 | 依赖 |
|---|---|---|---|
| P1 契约与清单 | `Adapted`、`ConvertedUsage` 类型与校验；`openai.json` 的 `convertedUsage`；CONTRACTS、CONVERT-MATRIX 文字 | `sdk/manifest/manifest.go`、`sdk/manifest/check/*`、`sdk/platforms/openai.json`、`next/docs/*` | `feat/up-p10` 合入（对象形式附加用量） |
| P2 转换器 | 生图两对、`ModelLocator`、`Options.AdaptModel`、`ClientError`；黄金样例 | `server/internal/gateway/convert/openai_images_responses.go`、`builtins.go`、`convert.go`、`testdata/*` | 无（可与 P1 并行，用本地类型先写） |
| P3 网关与账号模块 | `planRoutes` 读 `adapted`；转换路由的用量规则与 `modelPath`；`prepareRoute` 的每路由选项；`forwardJSONConverted` 的 `ClientError`；注册表绑定检查；`servedEndpoints`/`typeProtocols` | `gateway/routing.go`、`convert_policy.go`、`forward.go`、`plugin/registry*`、`account/protocols.go`、`account/types.go` | P1、P2 |
| P4 codex 插件 | 清单三项与默认模型、描述、版本；`BuildUpstream` 不上报模型；测试 | `plugins/codex/manifest.json`、`internal/codex/upstream.go`、`manifest_test.go`、`plugins/released.json` | P1 |
| P5 测试与验收 | 网关测试、e2e 用例与 mock、测试实例验证 | `gateway/images_test.go`、`next/e2e/*` | P3、P4 |

## 5. 替所有者做的决定

1. **走核心转换（方案 a），不走插件改写（b）**：转发 V2 下插件改不了应答，(b) 最终也要在核心做应答转换。
2. **同平台适配改为账号类型显式声明（`adapted`），chat 适配一并改**：否则登记生图转换后 azure、volcengine 会自动成为生图候选并在上游失败；一条规则比“chat 自动、生图要声明”两条好；不留隐式适配的兼容。
3. **对话模型由账号类型在清单里声明（codex 为 `gpt-6-luna`），账号层面用参数覆盖改**：它是上游特有的知识，客户端不该看到；不为它加界面。
4. **主用量是生图部分、按客户端请求的生图模型计价**：客户端调的是生图接口、选的是生图模型，白名单与价格准入都按它；上游回显的 `gpt-image-2-codex` 只记在 `upstream_model`。
5. **对话部分照实计费，作为附加用量按驱动模型计价**：上游报了这些 token，就照上游的数计（“上游返回什么就是什么”）；没有价格时按 §120.4 不计并标记。
6. **转换路由的用量规则由客户端端点声明（`convertedUsage`）**：生图接口怎么计费是生图端点的语义，不该由上游 Responses 端点的规则决定；不声明的转换（chat 等）不变。
7. **`n > 1` 拒绝（400）**：工具没有 `n`，靠提示词让模型多次调用不保证张数（请求 7 虽然给了两张），且对话开销翻倍；与 chat 转换拒绝 `n > 1` 一致。
8. **`size`、`quality`、`background` 同时写进工具参数和一句 `instructions`**：实测 Codex 后端只按 `instructions` 定尺寸（`quality` 写哪里都不生效，仍写进去，以后上游支持了就生效）；对守参数的上游是无害的重复。
15. **客户端可以用任何生图模型名调 Codex 账号，不按名字拒绝**：上游对工具里的模型名一律改写成 `gpt-image-2-codex` 出图；准入和计价照常按客户端写的名字（第 1.2.1 节）。哪些名字放给 Codex 账号，由管理员用账号模型列表控制。
9. **`response_format: "url"` 拒绝**：上游只给 base64，网关不存图、不发链接。
10. **其余生图参数原样搬进工具，值不检查**：上游认不认由上游答（如 `tools[0].n` 的 400），核心不复制上游的取值表。
11. **没有出图时回 502、生图项失败时回 400**：200 加空 `data` 会让客户端以为成功；状态码对应“上游没做到”和“请求被审核拦下”。
12. **默认开放**：声明了适配的类型的账号升级后自动开放两个生图协议（§110 的本平台协议规则）；实际调度还要管理员加模型、录价，不会意外出图。
13. **不加新的超时、上限或守卫**：流式暂存图片直到终态、断开处理都沿用现有规则。
14. **专用接口 (c) 另开一项先实测**：它若可用比 (a) 更省（无对话开销、核心零改动），但这次不允许直连，不把未验证的接口写进设计主线。

## 6. 没能确认的事

- **专用接口** `/backend-api/codex/images/generations`、`/images/edits`：只读了源码（第 7 节），没有实测；接不接受 sup2api 的标识、需要哪些头、`n`/`size` 的取值、是否另有额度。
- **`codex_oauth` 账号**：测试实例只有 `codex_device`；代码上同一上游、同一请求，推断相同。
- **各套餐的生图额度**：官方说生图计入 Codex 通用额度、平均消耗快 3–5 倍、Free 不可用；源码里有单独的限额 id `image_gen`、`imagegen_premium`（429 `usage_limit_reached`，响应头 `x-codex-active-limit: image_gen`）。各套餐具体多少不知道；codex 插件的额度窗口只读 `x-codex-primary/secondary-*`，生图限额的响应头没有接。
- **审核拦截在 Codex 后端的真实形状**：没测（不发违规内容），按官方文档设计。
- **`partial_images`**：两次请求都没有 partial 事件；是 Codex 后端不支持还是要别的条件，不知道。
- **驱动模型的选择**：只测了 `gpt-6-luna`；更便宜的对话模型（如 `gpt-5.6-luna`）能否驱动工具没测。
- **`gpt-image-2-codex` 的单价**：OpenAI 没公布；这里主用量按客户端模型计价，绕开了这个问题。
- **对象形式附加用量的 `map` 是否接受 `cache_read_tokens`**：实现时核对（3.5）。

## 7. 附：Codex 后端专用生图接口（源码，未实测）

出处 `openai/codex`：`codex-rs/codex-api/src/endpoint/images.rs`、`codex-api/src/images.rs`、`ext/image-generation/src/{tool.rs,backend.rs}`。

- `POST https://chatgpt.com/backend-api/codex/images/generations`、`/images/edits`，**非流式**，edits 也是 JSON。
- 请求：generations `{prompt, model, background?, n?, quality?, size?, output_format?("gif" 时带 frame_count/frame_duration_ms)}`；edits 另有 `images: [{image_url} | {file_id}]`。官方客户端固定 `model: "gpt-image-2"`、`quality: "auto"`、`size: "auto"`、不带 `n`，编辑最多 5 张图（本地文件转 data URL）；不带 `moderation`、`output_compression`、`partial_images`、`stream`、`mask`、`input_fidelity`。
- 应答：客户端解析 `{created, data:[{b64_json, generation_id?}], background?, quality?, size?}`，只取 `data[0]`；测试样例还带 `output_format`、`usage{input_tokens, input_tokens_details{image_tokens,text_tokens}, output_tokens, output_tokens_details{…}, total_tokens}`（客户端不读）。读应答头 `x-codex-imagegen-request-id`。
- 请求头：`Authorization: Bearer`、`chatgpt-account-id`、`originator`、`User-Agent`、`version`，另有 `x-codex-image-turn-id`、`x-codex-turn-metadata`；不带 `session_id`。
- 若可用，codex 类型原生声明 `images_generations`、`images_edits`（只有非流式：流式请求不会选到它），插件把请求体去掉不认的字段后发往这里，应答几乎就是生图接口的形状，用 §118.2 的规则计费。
