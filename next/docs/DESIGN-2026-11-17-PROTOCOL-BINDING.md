# 账号协议绑定、模型协议限定、显式协议转换

2026-11-17，用户要求："在账号上可以进行协议绑定；某个模型只在某个协议上提供服务；协议转换默认不支持，需要显式开启"。补充口径（同日）：

- "OpenAI 的 Chat 转 Responses 这种不算协议转换，协议转换一般是跨服务的"。
- "模型默认是不支持协议转换的，默认是客户什么协议访问什么模型，我们就转发"。
- 配置方式是"选中模型，指定协议"。

状态：**已定稿（用户 2026-11-17 确认），实现中**。定稿后契约写进 CONTRACTS 新一节（暂记 §110），迁移暂记 0106（合并时取下一个空号）。

本文修订 CONVERT-MATRIX 的 D3（2026-11-09 定的"所有账号类型自然接受转换，没有任何退出机制"）和风险第 11 条（"账号级关协议不在本期"）。

## 1. 现状

### 1.1 new-api 的做法（只读调研，分支 develop）

- **转换默认隐式开启**。选渠道只按分组 + 模型（`model/ability.go` 的 Ability 表没有协议维度），选中后由渠道适配器的 `Convert*Request` 自动转成渠道原生格式。
- **转不了的组合到转换阶段才报错，且不换渠道重试**（`relay/convert_request_error.go`，SkipRetry）。例如 `/v1/messages` 可以选中 Codex 渠道，然后直接失败。
- **没有"渠道只接受某协议"的设置**。相关开关只有：透传（不转换、原样发）、Chat → Responses 策略（全局，可按渠道和模型正则限定）、`tool_loss_policy`（只拦有损转换）。
- **唯一的显式绑定在 AdvancedCustom 渠道**：每条路由配 `incoming_path` + `models` + `converter`，选渠道时就按入站路径和模型过滤，`converter` 为空表示不转换。本设计的形态与它最接近，但做成所有账号通用。
- **"模型支持的端点类型"只用于展示**（价格页、`/v1/models`、渠道测试），请求时不校验。

### 1.2 sup2api 现状

- 协议 id 是 `<平台>.<名称>`（`openai.chat`、`openai.responses`、`openai.responses_ws`、`openai.embeddings`、`anthropic.messages`、`anthropic.count_tokens`、`gemini.generate`、`gemini.stream_generate`、`gemini.count_tokens`，以及插件自己声明的，如 `volcengine.images`）。入站协议就是命中端点的 `ep.Protocol`。
- 账号能服务哪些协议**只由账号类型决定**（manifest 的 `platforms[]` 和每个平台的端点子集 `endpoints[]`）。账号、分组上没有协议字段。
- `planRoutes`（`gateway/routing.go`）对每个账号类型给出一条路由，共三种：
  1. **原生**：类型声明了入站端点的平台，且子集含该端点。
  2. **同平台适配**：类型声明了该平台，但子集不含该端点，转到同平台子集内的另一个端点（codex 账号服务 `openai.chat` 就是这样转到 `openai.responses`，CONTRACTS §83.3）。
  3. **跨平台转换**：类型没有声明该平台，转到它其他平台的端点（OpenAI 账号服务 `/v1/messages`）。
- 三种路由对所有账号、所有模型**一律开启**，没有任何一级的开关。账号/分组上的"丢失策略"（allow / safe / strict）只管转换丢字段能不能接受。
- 账号的 `models`（白名单）、`model_mapping`、`model_limits` 都只看模型，不分协议。
- "OpenAI 生图"目前不是端点：`/v1/images/generations` 返回 404，`openai.images` 只在 DESIGN-2026-11-17-OPENAI-IMAGE-BILLING 里提出，尚未实现。

## 2. 定义

| 名称 | 定义 | 例子 |
|---|---|---|
| 本平台协议 | 入站端点所属的平台，是账号类型声明过的平台。包括原生和同平台适配两种路由 | OpenAI 账号的 chat、responses；codex 账号的 chat（适配到 responses） |
| 跨平台协议转换（下称"转换"） | 入站端点所属的平台，账号类型**没有声明** | OpenAI 账号服务 `anthropic.messages`、`gemini.generate`；Claude 账号服务 `openai.chat` |

- 判定只看账号类型的声明，与具体请求无关。代码里就是 `planRoutes` 中 `b.Supports(p)` 的结果，不新增分类表。
- volcengine 这类同时声明 openai、anthropic、volcengine 三个平台的类型，这三家的协议都算本平台协议；Gemini 请求打到它才算转换。
- 同平台适配不算转换，默认照常工作，但可以用协议绑定关掉（§3.1）。

## 3. 设计

默认行为：**客户用什么协议访问什么模型，就按这个协议转发**，只走本平台协议；任何模型都不做跨平台转换。要转换，必须在账号上选中模型、勾上转换协议。

账号上加两个字段。

### 3.1 协议绑定：`protocols`

账号对哪些**本平台协议**提供服务。

- `null`（默认）：账号类型能服务的本平台协议全部开放。旧账号行为不变。
- 数组：只开放列出的协议。可以是空数组，表示本平台协议全部关闭（此时 `model_protocols` 里必须至少开了一个转换协议，否则保存报错"账号没有开放任何协议"；要停用账号用"可调度"开关）。
- 用途：只有 Chat 没有 Responses 的 OpenAI 兼容站点，只勾 `openai.chat`；不想让 codex 账号接 chat 请求，去掉 `openai.chat`；关掉 WebSocket，去掉 `openai.responses_ws`。
- 指定后，账号类型以后新增的端点不会自动开放（界面提示"有 N 个协议未开放"）。

### 3.2 模型的协议限定与显式转换：`model_protocols`

选中模型，指定它在哪些入站协议下提供。转换也在这里开：给模型勾上跨平台协议，才对这个模型做转换。

```json
[
  {"models": ["claude-sonnet-4-5"],      "protocols": ["anthropic.messages"]},
  {"models": ["gpt-5.6"],                "protocols": ["openai.chat", "openai.responses", "anthropic.messages"]},
  {"models": ["gpt-image-1"],            "protocols": ["openai.images"]},
  {"models": ["text-embedding-3-large"], "protocols": ["openai.embeddings"]},
  {"models": ["*"],                      "protocols": ["openai.chat", "openai.responses"]}
]
```

（上例是一个 OpenAI 账号：`claude-sonnet-4-5` 是经模型映射指到 GPT 的名字，只在 Messages 协议下经转换提供；`gpt-5.6` 原生加 Messages 转换；生图、向量模型各只走自己的协议；其余模型只走 chat 和 responses。）

- **被列到的模型**：只在这条列出的协议下提供。协议可以是本平台的，也可以是跨平台的。
- **没被列到的模型**：在账号开放的全部**本平台协议**下提供，**不做转换**。默认 `[]`，即所有模型都是这样。
- `"*"` 是唯一的通配，表示"其余模型"，只能单独成一条。用它可以表达"某协议只提供某些模型"（上例中 `openai.images` 只提供 `gpt-image-1`），也可以给全部模型开转换（不建议，见 §6 第 3 条）。
- 模型按**映射前的客户端模型**匹配，协议按**入站协议**匹配（与 `models`、`model_limits`、计费一致）。一个模型只能出现在一条里。
- 本平台协议实际生效的是它与 `protocols` 的交集；列了账号没开放的本平台协议不报错，界面给警告。
- 丢失策略不变：模型开了转换之后，照旧按账号 > 分组 > allow 判定有损转换。

### 3.3 count_tokens

`anthropic.count_tokens`、`gemini.count_tokens` 不单独列，跟随同族的生成协议（`anthropic.messages`、`gemini.generate`）：该模型在生成协议下提供，计数就提供（原生转发或本地估算，规则同 CONVERT-MATRIX §8）。

### 3.4 生效位置

| 环节 | 改动 |
|---|---|
| `planRoutes`（`gateway/routing.go`） | 路由上记下 `cross`（是否跨平台），不改选路逻辑 |
| 调度快照 `core.AccountRef` | 加 `Protocols`、`ModelProtocols`（`account/directory.go` 的 `readGroupSnap` 加列），一个判定方法"模型在该协议下是否提供" |
| `ineligibleReason`（`gateway/sched_trace.go`） | 取到路由后调用上面的方法；引用模型同样判断 |
| 调度记录 | 三个新跳过原因（见下），使用记录详情里能看到账号为什么没被选 |

判定顺序与跳过原因：

1. 模型命中 `model_protocols` 的某条（精确，其次 `"*"`）：入站协议不在这条里 → `model_protocol_closed`。
2. 模型没命中任何一条，且路由是跨平台的 → `conversion_not_enabled`。
3. 路由是本平台的，`protocols` 非 null 且不含入站协议 → `protocol_closed`。

- 只做内存比较，在可用性预检、粘性、转换、取并发槽之前。被过滤的账号不转换请求、不下载媒体、不计尝试次数。
- HTTP、WebSocket、粘性绑定走同一个入口，不需要分别处理。
- **错误**：组内候选账号全部因这三个原因被过滤 → 404 `no account in this group serves this model on this endpoint`（客户端不会当成过载重试）。只要有一个账号是因别的原因（冷却、限流、忙）没选上，就按现有规则返回那个原因。
- 优先级、等级排序、丢失策略、粘性、故障策略都不变。

### 3.5 性能

现有热路径：分组快照缓存在每个节点的内存里（30 秒，账号变更经 `account:changed` 立即失效，未命中时 singleflight 读一次库）；每个请求从快照取候选、用一次 Redis 管道查冷却，再对每个候选跑 `ineligibleReason`。本设计不给这条路径加任何数据库或 Redis 访问，约束如下：

1. **解析只在建快照时做一次**。`readGroupSnap` 多读两列；`model_protocols` 为 `[]` 时在 SQL 里取成 NULL，不传输、不解析。绝大多数账号走这条路。
2. **编译成只读索引**。建快照时把两个字段编译成一个不可变结构，`AccountRef` 上只多一个指针（没有配置的账号为 nil）：
   - `protocols`：协议数量是个位数，存小切片，顺序比较；
   - `model_protocols`：`map[模型] → 协议切片`，加一个"其余模型"的协议切片。
   每个请求按值拷贝 `AccountRef` 时只多拷 8 字节，索引本身在快照内共享、不拷贝。
3. **每个候选的判断是 O(1)**：nil 判断 → 一次 map 查找 → 在个位数的协议切片里比较。不随规则条数或模型数增长。没有配置且路由是本平台的账号，只有一次 nil 判断。
4. **跨平台请求比现在便宜**。现在跨平台的候选要走到挑选阶段才转换请求体（多 MB 图片时很重，转 Gemini 还要下载媒体）；改后没开转换的账号在转换之前就被内存比较过滤掉。全部被过滤时的 404 直接由跳过原因得出，不查库。
5. **内存上限**：单账号最多 50 条 × 500 个模型，索引最多 2.5 万项；账号在多个分组时每个分组快照各持一份（与现有 `models`、`model_mapping` 相同）。

验收：按 `store/gormtest/BASELINE.md` 的命令前后各跑一次 `HotGroupSnapshot`，无配置时 allocs/op 不增加；新增一个 `ineligibleReason` 的基准（500 个候选，分别为无配置、有 50 条规则），有规则时每候选不分配内存。

## 4. 存储与接口

迁移 0106（`0106_account_protocols.sql`）：

```sql
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS protocols       text[];                      -- NULL = 本平台全部
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS model_protocols jsonb NOT NULL DEFAULT '[]';
```

不回填。升级后所有账号都不做跨平台转换（§6 第 1 条）。

| 接口 | 字段 |
|---|---|
| `GET/POST/PATCH /accounts`、`/accounts/:id` | `protocols`: `string[] \| null`；`model_protocols`: `[{models, protocols}]`。PATCH 整体替换 |
| `GET /accounts/:id` | 另返回只读的 `served_protocols`: `[{protocol, via: native\|adapted\|converted, grade, hops, models}]`，即账号实际开放的协议；`models` 为 `null` 表示不限模型，界面不再自己推算 |
| `GET /groups/:id` | 另返回只读的 `served_protocols`: `[{protocol, accounts}]`，成员账号开放协议的并集 |
| 批量编辑 | 加"本平台协议""模型的协议限定"两项（替换） |

校验（字段错误定位到对应字段）：

- `protocols[i]`：必须是该账号类型的本平台协议且能服务（原生或同平台适配）；不收 count_tokens。
- `model_protocols`：最多 50 条、每条最多 500 个模型；模型重复报 `duplicate`；`"*"` 只能单独成条且最多一条；`protocols` 不能为空。
- `model_protocols[i].protocols[j]`：必须是该账号类型能服务的协议（原生、同平台适配或经转换）；不收 count_tokens；跨平台的不收 WebSocket、异步任务提交（这些只走原生）。
- `protocols` 是空数组且 `model_protocols` 里没有任何跨平台协议：`protocols: no_protocol`。

权限同账号其他字段。变更经现有的 `account:changed` 广播失效各节点快照。

## 5. 控制台

可点击的示意图：[mockups/protocol-binding.html](mockups/protocol-binding.html)（账号编辑器三种账号类型、账号列表、分组详情、使用记录；数据为虚构）。

### 5.1 账号编辑器

"模型配置"分区改名为"**协议与模型**"，卡片顺序：**本平台协议**（新）→ 模型列表 → 模型映射 → **模型的协议限定**（新）。理由：先定账号开放哪些协议，再定模型，最后选中模型指定协议。

**卡片一：本平台协议**

```
┌ 本平台协议（OpenAI）──────────────────────── (●) 全部   ( ) 指定 ┐
│   ☑ Chat Completions    POST /v1/chat/completions     原生        │
│   ☑ Responses           POST /v1/responses            原生        │
│   ☑ Responses WebSocket GET /v1/responses             原生        │
│   ☑ Embeddings          POST /v1/embeddings           原生        │
│                                                                   │
│ ⓘ 其他平台的协议（Anthropic、Gemini）默认不提供。需要协议转换时，    │
│   在下面"模型的协议限定"里选中模型、勾上对应协议。                   │
└───────────────────────────────────────────────────────────────────┘
```

- 选"全部"时复选框全勾且置灰（对应 `null`）；选"指定"后可逐个取消。
- codex 账号这里的 Chat Completions 行徽标显示"适配 → responses"。
- 左侧导航摘要：`协议 4 · 限定 3 个模型 · 转换 2 个模型`。

**卡片二：模型的协议限定**

列 = 卡片一开放的本平台协议 + 该账号类型经转换能到达的协议（列头带"转换"徽标和等级）。行 = 模型。两种视图，按模型列表自动选择，可手动切换，也可切到 JSON。

- **矩阵视图**（账号模型列表非空时默认）：

```
┌ 模型的协议限定 ─────────────────────────────────── [矩阵 | 列表 | JSON] ┐
│                     ├──── 本平台 ────────────┤├───── 转换 ──────────┤  │
│ 模型 \ 协议          Chat  Responses  Embed.   Messages·一般  Gemini·一般 │
│ claude-sonnet-4-5    ☐      ☐         ☐          ☑             ☐       │
│ gpt-5.6              ☑      ☑         ☐          ☑             ☐       │
│ text-embedding-3-l.  ☐      ☐         ☑          ☐             ☐       │
│ gpt-5.6-mini         ▣      ▣         ▣          ☐             ☐  默认 │
│ 其余模型              ▣      ▣         ▣          ☐             ☐  默认 │
│                                                                        │
│ ▣ = 默认（不限定，按客户的协议原样转发，不转换）  [全部恢复默认]         │
└────────────────────────────────────────────────────────────────────────┘
```

  - 行来自账号的模型列表，末尾固定一行"其余模型"（对应 `"*"`）。
  - 每行初始是"默认"：本平台列灰色勾选，转换列不勾。点任一格后该行变成显式限定；行尾有"恢复默认"。
  - 可以多选几行后一次指定协议（"选中模型，指定协议"）。
  - 勾转换列时，等级为"不推荐"（两跳）的提示一句；给"其余模型"行勾转换时提示"所有未单独列出的模型都会接受转换"。
  - 保存时把勾选相同的行合并成一条 `model_protocols`，默认行不写。复选矩阵的交互参考 `PermissionMatrix.vue`。
- **列表视图**（账号模型列表为空 = 全部模型时默认；或模型很多时）：照搬"指定模型限速"的卡片式（`ModelLimitsEditor.vue`），每张卡 = 一组模型（`ModelTagSelect`）+ 一组协议（`StickyProtocolPicker`，候选按本平台 / 转换分组），外加一张可选的"其余模型"卡。
- 某条里的本平台协议账号没有开放时，在该处显示警告"账号未开放该协议，这一项不生效"。
- Gemini 的 `generate` 和 `stream_generate` 在界面上合成一列，一起勾选（存储仍是两个协议 id）。

**"高级 → 请求转发"**：现有"协议转换丢失策略"一行保留原位；账号没有任何模型开转换、也没有同平台适配时，下拉置灰并提示"未开启协议转换"。

### 5.2 其他页面

- **账号列表**：不加列。名称下方已有的平台图标改为按账号实际开放的协议显示：本平台图标照旧，有模型开了转换的平台图标带"转"角标，悬停列出协议和模型数；本平台协议被限定时图标旁加"部分"标记。高级筛选加"开放协议"。
- **账号详情**："可服务的端点"按 `served_protocols` 显示，转换项列出开了转换的模型；账号类型能服务但账号没开放的，置灰标"未开放"。
- **分组详情**："可调用端点"改用接口返回的 `served_protocols`，每个协议显示有几个成员账号提供。删掉前端按"所有类型都能转换"推算的 `convertedEndpoints.ts`。
- **新建账号的类型选择**：端点列表里跨平台的项加说明"需在账号上为模型开启"。
- **使用记录详情**：调度记录里出现新的跳过原因"协议未开放""模型未开启协议转换""模型在该协议下不提供"。
- i18n zh/en；`web/src/api/types.ts` 加字段。

## 6. 已知影响与注意事项

1. **升级后存量转换流量直接 404**（用户 2026-11-17 定：存量默认 404 即可）。字段在升级后才存在，没法提前配好，所以从升级完成到配好之间，依赖转换的客户端会 404。发布手册里放一条只读查询：列出近 30 天实际服务过跨平台转换的账号、入站协议和模型，升级前导出，升级后照着在批量编辑里配。codex 账号接 chat 请求属于同平台适配，不受影响。
2. **模型列表接口不分协议**。`/v1/models` 和分组的模型列表仍按分组和账号的模型返回，客户端可能看到一个在当前协议下调不通（404）的模型。
3. **给"其余模型"开转换要谨慎**。账号模型列表为空（= 全部模型）时，Anthropic 客户端发来的 `claude-*` 模型名会原样（或经映射）发给 OpenAI 上游，上游报模型不存在，按故障策略可能冷却账号。建议转换只开给明确列出的、有映射的模型。
4. **粘性会话**：升级后，已绑定到某账号的转换会话因该账号不再提供转换而失效，按现有"绑定账号不可用"的规则处理。
5. **分组展示变少**：分组详情的"可调用端点"不再包含"经转换也可调用"的部分，直到成员账号里有模型开了转换。

## 7. 不做

- **分组级、全局级的转换开关**：只在账号上设，批量编辑用于成批配置。
- **插件通过通用配置项声明默认值或锁定**（`commonControls`）：等有插件需要时再加。
- **`/v1/models` 按入站协议过滤模型**（§6 第 2 条）。
- **生图协议本身**：`openai.images` 端点由生图计费那份设计落地；落地后它自动成为 OpenAI 账号的一个本平台协议，`gpt-image-*` 只走生图协议用 §3.2 配置，本设计不需要再改。
- 协议 id 不支持通配（`openai.*`）。

## 8. 已定决策（用户 2026-11-17）

| # | 事项 | 决定 |
|---|---|---|
| 1 | 存量账号 | 不回填，升级后跨平台转换默认 404 |
| 2 | 配置方向 | 选中模型，指定协议 |
| 3 | 全部未开放时的错误 | 404；模型没开转换、模型在该协议下不提供也一样（§3.4） |
| 4 | 账号类型新增端点 | 已"指定"本平台协议的账号不自动开放；转换默认不开放 |
| 5 | 模型默认行为 | 不判断模型支持哪种协议：客户用什么协议访问就按什么协议原样转发，不转换；只有配置了绑定或转换才生效 |
| 6 | 转换开在哪 | 只在模型上开，账号上没有单独的转换开关（§3.2） |
| 7 | 同平台适配（Chat ↔ Responses） | 不算协议转换，默认对所有模型生效 |

## 9. 任务拆分（定稿后细化）

| # | 任务 | 范围 |
|---|---|---|
| T1 | 迁移 0106、模型生成、账号输入/输出/校验/审计字段、`AccountRef` 两个字段与判定方法、`served_protocols` | server（account、core） |
| T2 | 路由记 `cross`、`ineligibleReason` 判断、三个跳过原因、全部未开放时的 404、count_tokens 跟随 | server（gateway） |
| T3 | 分组 `served_protocols`、批量编辑 | server（group、account） |
| T4 | 控制台：两张卡片、分区改名与摘要、错误定位、列表图标、详情、分组详情、筛选、i18n、类型、测试 | web |
| T5 | 发布手册：升级前的存量转换查询 | deploy |
| T6 | CONTRACTS 新一节、CONVERT-MATRIX D3 与风险 11 的修订说明、CHANGELOG | docs |
