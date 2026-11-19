# newapi 插件：New API 站点的 API Key

> 状态：2026-11-18 按分支 `feat/up-p5` 的代码写成，**未发布**（插件 0.1.0，需核心 ≥0.1.304）。只用单元测试、合规套件和 openai / anthropic / gemini 三份冻结黄金样例验证过，**没有对真实 New API 站点验证**；站点的接口形状来自 New API 的官方文档与源码行为（只参考行为，不含它的代码）。
> 设计见 [DESIGN-2026-11-18-UPSTREAM-PLUGINS-AND-BALANCE.md](DESIGN-2026-11-18-UPSTREAM-PLUGINS-AND-BALANCE.md) 3.5.1、3.5.2、3.6、3.7 与 D6、D7、D11–D15、D20；写法是 SDK 的 `standard.Merge`（`sdk/pluginsdk/standard/README.md` 例子四）。

## 1. 它做什么

- 一个账号类型 `apikey`（控制台显示 "New API"），不声明自己的平台：账号同时服务内置的 `openai`、`anthropic`、`gemini` 平台，TypeSafe 插件启用时还服务它的 `typesafe` 平台。
- **同协议原样转发**：客户端用哪个协议来，就按这个协议的官方路径发给站点，请求体不改（OpenAI Chat 流式照 openai 预设补 `stream_options.include_usage`，站点默认也返回用量，加了无害）。插件不做任何协议转换；跨平台转换照协议绑定（CONTRACTS §110）默认关闭。
- 各协议用各自官方的鉴权头：OpenAI 系与 TypeSafe `Authorization: Bearer`，Anthropic `x-api-key`（客户端没带 `anthropic-version` 时补默认值），Gemini `x-goog-api-key`。New API 对这几种头都接受。
- 能查 Key 的余额（第 4 节）。
- 插件只构造请求，由核心发送；插件不联网（不申请 `net`）。
- 内置、仅安装、默认不启用（D11）：管理员在插件页启用后才能建这类账号。

## 2. 建账号

| 字段 | 必填 | 说明 |
|---|---|---|
| 站点地址（`base_url`） | 是 | 站点根地址，如 `https://your-new-api.example.com`；粘贴时带的 `/v1`、`/v1beta` 会被去掉 |
| API Key（`api_key`） | 是 | 站点的令牌，8–512 个不含空格的可见字符（不限前缀）；加密保存 |

- 地址由运营方填，插件不声明 `guardedSettings`、`externalServices`：转发和余额请求只能发往账号自己的 `base_url` 主机（§112.17 的主机规则）。这是"可自定义上游"的类型（`custom_upstream` 为真）：**只有 own 级权限的用户**新建或改地址时需要 `account:relay`（CONTRACTS §21.3.1，与 relay 插件相同）；全部级的账号管理员不受这条限制。
- 默认模型：不写（站点没有固定的模型表，账号默认服务所有模型）。用"拉取模型"读站点的 `GET /v1/models`（Bearer，取 `data[].id`）。
- 账号测试：核心按账号开放的协议给出偏好顺序，插件取第一个有测试请求的协议（D14）：默认是 OpenAI Chat（`gpt-4o-mini`）；账号绑定成只开 Messages 时测 `/v1/messages`（`claude-haiku-4-5`），只开 Gemini 时测 `generateContent`（`gemini-2.5-flash-lite`），只开 TypeSafe 时测 systemone（`jev-latest`）。测试时也可以指定模型。

## 3. 协议

| 平台 | 端点 | 说明 |
|---|---|---|
| openai | 全部：`/v1/chat/completions`、`/v1/responses`（含 WebSocket）、`/v1/embeddings`、`/v1/images/generations`、`/v1/images/edits` | 生图两条都只收 JSON 请求体；multipart 形式的 edits 在网关就以 400 拒绝 |
| anthropic | 全部：`/v1/messages`、`/v1/messages/count_tokens` | 站点的 count_tokens 不计费，转发照常 |
| gemini | `generateContent`、`streamGenerateContent` | 不含 `countTokens`：New API 对它直接回 404；这个协议由核心本地估算（§88.2） |
| typesafe | `/v1/systemone` | 平台由 TypeSafe 插件声明（D15）。TypeSafe 插件没启用时这个协议不存在（注册表每次换代记一条警告，`/v1/systemone` 404）；启用后，上游站点自己也要装了 TypeSafe 扩展才有这个接口，否则请求得到站点的 404 原样返回 |

- 新建账号的 `protocols` 为 `null`：上表的协议全部开放（以后新增的也自动开放）。上游令牌只开通了某些协议（例如它的分组里只有 Claude 渠道）时，在账号编辑器"协议与模型"的卡片一选"指定"、只勾真正能用的协议；被绑定排除的协议在调度时就不会选到这个账号，也不会发请求（§110.4）。需要某个模型只在某些协议下提供时用卡片二（`model_protocols`）。
- 上游不支持某个协议时，后果按该协议预设的失败分类（D20）：400 / 404 原样返回客户端、账号不受影响；5xx 换号并短暂冷却；401 / 403 停用账号。New API 对"分组里没有这个模型的渠道"回什么状态码未核实。

## 4. 余额

两个请求，都在账号的 `base_url` 上（每 600 秒定时刷新一次，控制台可随时手动刷新）：

1. `GET /api/usage/token/`（末尾带斜杠），带 `Authorization: Bearer <Key>`：令牌的额度（站点原始额度单位）。令牌过期或用尽也能查。
2. `GET /api/status`，**不带 Key**（公开接口）：`data.quota_per_unit`，多少额度单位折 1 美元。

| 情况 | 显示 |
|---|---|
| 令牌限额 | `total_available ÷ quota_per_unit` 美元；站点同时给了用户额度（`user_quota.total_available`）时取两者较小 |
| 令牌不限额，站点给了用户额度 | 用户额度折成美元 |
| 令牌不限额，站点没给用户额度 | **不限**（D7）：令牌这一层确实不限，但用户自己的余额可能已经不多 |
| `/api/usage/token/` 回 401 / 403 | 认证被拒 |
| 应答 `code` 不为真 | 暂时失败，信息带站点的 `message` |
| `/api/status` 读不到正数的 `quota_per_unit` | 暂时失败"无法读取站点的额度换算比例"——**不按 New API 的默认值 500000 猜**（D6），猜错会差几个数量级 |
| 传输错误、429、5xx、其它非 2xx | 暂时失败 |

- 币种一律 `USD`：站点"额度显示为人民币 / token"只是它自己的显示设置，不影响这里。额度为负（透支）时显示负数。
- 余额只显示，不停用账号、不影响调度、不告警（D1）。
- 要看到用户余额：在站点上给这个令牌开启"允许查询用户额度"（新建令牌默认开，旧令牌默认关）。

## 5. 权限

- 宿主权限：`platform.register`、`accounts.credentials {"types":"own"}`；没有 `net`、没有文件权限、没有 `gateway.endpoint`（不声明端点）。
- 能力：`platform.adapter.v1`、`platform.websocket.v1`（Responses 的 WebSocket 模式）。
- 不在清单里写价格：模型价格由管理员在核心录入；生图模型按设计 3.4.5 录表达式价格；转发 Jev 时按 TypeSafe 的价格（PLUGIN-TYPESAFE.md 第 4 节）。

## 6. 已知限制

- 无限额令牌在站点没开"允许查询用户额度"时显示"不限"，看不到用户真实余额。
- Gemini 的 `countTokens` 不转发，按本地估算。
- 生图只收 JSON 请求体。
- TypeSafe 的转发取决于两件事：本平台装了并启用 TypeSafe 插件、上游站点装了 TypeSafe 扩展。
- 上游令牌能用哪些协议插件看不出来，要运营方用协议绑定收窄；站点对"不支持"回 403 时会误停用账号，这种情况先用协议绑定关掉，或用故障策略（§58）改写分类。
