# 账号请求头覆盖（兼容 new-api 渠道的 header_override）与参数覆盖补齐

2026-11-17，用户要求："sup2api 请求头覆盖参考 new-api 进行优化，支持 new-api 的覆盖方式"。契约写进 CONTRACTS §108。

## 1. 现状与差距

| new-api 渠道 | sup2api 账号（改前） |
|---|---|
| `header_override`：JSON 对象，键是请求头名，值是固定值或占位符；键 `*`、`re:`/`regex:` 表示透传 | 只有"请求头透传"（规则列表 `header_passthrough`，§81），没有"按名设值"的写法 |
| 占位符 `{client_header:名}`、`{client_header:名:追加值}`、`{unset}`、`{api_key}` | 无（只能在参数覆盖里写 `set_header` / `copy_header` 操作） |
| `param_override` 的 `sync_fields`、`prune_objects` | 编译时报"暂不支持" |
| 条件模式 `regex` | 报"未知条件模式" |
| `pass_headers` 的值可以是对象 `{headers|names|header}` 或 JSON 字符串 | 只认数组和逗号分隔字符串 |
| 条件上下文 `using_group` | 只有 `group` |

结果：new-api 渠道的请求头覆盖没法搬过来，带上面几种写法的参数覆盖粘贴进来直接报错。

## 2. 新字段：请求头覆盖 `header_override`

- **存储**：`accounts.header_override jsonb`（可空，迁移 0105）。接口字段 `header_override`：对象或 `null`，列表、详情返回，`POST`/`PATCH /accounts` 整体替换（`null` 或 `{}` 清空）。
- **格式**（与 new-api 相同，可以直接粘贴）：

  ```json
  {
    "*": "",
    "re:^x-claude-code-": "",
    "User-Agent": "my-client/1.0",
    "anthropic-beta": "{client_header:anthropic-beta:context-1m-2025-08-07}",
    "x-trace-id": "{client_header:x-request-id}",
    "x-unwanted": "{unset}"
  }
  ```

  | 写法 | 含义 |
  |---|---|
  | 键 `*` | 透传全部客户端请求头（值忽略） |
  | 键 `re:<正则>` / `regex:<正则>` | 透传名称匹配的请求头（不区分大小写，值忽略） |
  | 值是普通文本 | 设成这个值 |
  | `{client_header:名}` | 取客户端这个头的值；客户端没带就不设 |
  | `{client_header:名:追加值}` | 客户端带了：`客户端的值,追加值`；没带：`追加值` |
  | `{unset}` | 删除这个头（包括账号类型声明设置的头） |
  | 空字符串 | 忽略（与 new-api 相同） |
  | 空键 | 忽略（与 new-api 相同） |

- **校验**（保存时，字段错误在 `header_override`，消息指出具体键）：必须是对象；值必须是字符串；最多 100 项、64 KiB；透传键最多 32 条、正则须能编译；头名须是合法字段名；值不得含控制字符（水平制表符除外，`paramoverride.ValidHeaderValue`，与 Go 传输层发送前的检查相同）；占位符必须是整个值；`{client_header:名}` 的名不能为空、追加值写了冒号就不能为空。
- **与 new-api 的差异**（都是安全边界，沿用 §81/§84 的禁止列表 `HeaderBlocked`）：
  1. 目标头、`client_header` 的来源头都不能是禁止列表里的头（凭证 Authorization / x-api-key / Cookie 等、逐跳、Host、客户端地址、`x-sub2api-*`、`x-kin-*`），报 `blocked`。new-api 允许改写 Authorization、Host，或 `{unset}` 掉 x-api-key——在 sup2api 里凭证只由账号类型的鉴权设置。
  2. 不支持 `{api_key}`（报 `unsupported`，提示"凭证由账号类型的鉴权设置"）：账号的密钥由插件持有，不进核心的请求头层。
  3. 正则透传不区分大小写（new-api 按规范化后的头名区分大小写，sup2api 的写法是它的超集）。
  4. 客户端同名头有多个值时 `client_header` 取全部、以 `, ` 连接（new-api 取第一个）。
  5. 透传键只补没有的头，不进参数覆盖的初始状态，参数覆盖的 `keep_origin` 不保留透传来的头（new-api 会保留）。
  6. 值随账号返回给有 `account:read` 的人（同 `param_override`），不适合放密钥；凭证头按设计写不进去。
- **清空**：只有 `null`、`{}` 清空；只含被忽略项的对象照原样保存。

## 3. 执行位置

请求头分层（§84 / §86.5，后面的盖前面的）变为：

1. 声明构造的头、声明的 paramOverride 头
2. 透传（只补没有的）：账号 `header_passthrough` → **`header_override` 的透传键** → 粘性规则
3. **`header_override` 的设值和 `{unset}`**
4. 账号参数覆盖（§84）的设置和删除
5. 插件的头 → 声明式鉴权头 → 核心资源头、额度头

第 3 步对应 new-api"渠道请求头覆盖在适配器之后、参数覆盖的请求头操作在它之上"的顺序；插件头在后与 §84 的既定顺序一致（2026-11-09 用户定）。

**与参数覆盖的衔接**（与 new-api 一致）：参数覆盖开跑时，`header_override` 解析出的结果就是它的初始请求头状态：

- 条件可以读 `header_override.<名>`；
- `set_header` 的 `keep_origin` 不覆盖请求头覆盖已设的头；
- `copy_header`、`sync_fields` 的 `header:` 源先看请求头覆盖的值，再看客户端的；
- 参数覆盖可以再改、再删它设的头。

实现上：`header_override` 编译成 `paramoverride.HeaderOverrideRules`，`Resolve(客户端头) → HeaderOverride{Set, Delete}`；网关的 `accountHeaders` 解析它（错误带"account request header override"），`overrideAccount` 在参数覆盖的运行里从上下文的客户端头解析，结果作为初始状态（`Context.InitialHeaders`），没有参数覆盖时直接用它。透传键交给 `passthroughHeaders` 与账号透传规则合并。

**运行失败**：已存的值编译不过（`Resolve` 报错），或 `client_header` 取到的客户端值含控制字符时，这次尝试按参数覆盖的运行失败处理：`param_override_failed`、中性换号、不冷却。

**WebSocket**：握手同样应用第 3、4 步（与参数覆盖一致），透传键不生效（§81 未做 WebSocket 透传）。`dial` 先用同一个 `accountHeaders` 单独解析请求头覆盖：解析失败就换号（与 HTTP 一致）；首条消息的请求体改写失败时（由这一轮报告），握手仍带请求头覆盖的决定。

**预览**：`POST /accounts/param-override/preview` 多收一个可选的 `header_override`，预览结果里的请求头包含它的决定。

## 4. 参数覆盖（§84）补齐

- **条件模式 `regex`**：值必须是字符串，保存时编译（Go 正则），不合法报 `invalid`。
- **`sync_fields`**：`from`、`to` 写 `json:路径` / `body:路径` / `header:名`，不带前缀按 JSON 路径。一边有值（非 null、非空字符串）另一边没有时，把有值的一边复制过去；两边都有或都没有不动。`header:` 的名受禁止列表约束。
- **请求头值的安全**：请求体内容可以经 `sync_fields` 写进请求头。含控制字符（换行等）的值会让 Go 传输层拒绝请求，分类后冷却账号，等于用一个请求体拖垮整个号池。所以运行中写请求头（`sync_fields`、`copy_header`、`pass_headers`、`set_header` 的令牌映射）遇到这样的值就让这次运行失败（`param_override_failed`，中性换号），`set_header` 的固定值和令牌、`header_override` 的值在保存时就校验。
- **`prune_objects`**：在 `path`（空为整个请求体；`*`、负下标与其他路径操作相同）下的每个位置删除满足条件的对象（这些位置上的对象本身不删）。`value` 是字符串（去掉首尾空白后等于 `{"type": 值}`），或对象 `{logic: AND(默认)|OR, recursive: true(默认)|false, conditions: 数组|对象, where: 对象, type: 值}`，条件写法同操作的 `conditions`，`where` 的键去掉首尾空白、跳过空键（同 new-api），路径相对被检查的对象，找不到时再查上下文。要删的位置按文档顺序收集、从后往前删；对象成员的键重复时，命中的键所有副本一起删除（同 new-api）。
- **路径转义**：由成员键拼出的路径段（顶层字面键、`*` 展开、合并、`prune_objects`）转义 `.`、`*`、`?`、`#`、`@`、`|`、`\`，这些键按字面处理；转义只在 `escapePathKey`，子节点的段只在 `eachChild` 生成。
- **条件对象**：操作的 `conditions` 写成对象时键原样作路径（与改前一致），只有 `prune_objects` 的 `where` 去空白。
- **`pass_headers`** 的值另收对象 `{headers|names|header}` 和 JSON 字符串。
- **上下文** 增加 `using_group`（同 `group`）。

不做：`{api_key}`；上下文的 `user_group`、`token_group`、`last_error*`、`is_channel_test`（sup2api 没有对应概念或不在这条路径上，写了按缺失处理，配合 `pass_missing_key`）。

## 5. 通用配置项与控制台

- **通用配置项（§87）**：新增 `header_override`（kind `header_override`，默认值为对象，用同一个编译器校验），插件可以声明默认值、隐藏或锁定，与 `param_override` 一样。
- **控制台**：账号编辑页"高级 → 请求转发"组，在"请求头透传"和"请求参数覆盖"之间加一行"请求头覆盖"：
  - 右侧显示"未设置 / N 项"和"编辑"按钮（与参数覆盖同样的行式布局）；
  - 编辑弹窗是"键 / 值"表格（每行一个头，值输入框旁有占位符快捷选择：客户端头、客户端头+追加、删除），可切到 JSON 文本模式直接粘贴 new-api 的配置；
  - 说明文字列出写法和不能覆盖的头。
  - 导航摘要增加"请求头覆盖 N 项"。
  - 字段错误 `header_override` 定位到"高级"分区。
- 插件页可用的宿主组件增加 `HeaderOverrideEditor`、`schema` 小部件增加 `header-override`，UI SDK 升到 3.19（只加不改；插件声明 `hostUICompat: "^3.19"`）。

## 6. 默认值与兼容

- 默认无（`null`）；旧账号行为不变。
- 已有 `header_passthrough` 不迁移、不改；两者可同时用，透传规则取并集。
- 不改 Host API 版本；新增的通用配置项对旧插件透明。

## 7. 任务拆分

| # | 任务 | 范围 |
|---|---|---|
| T1 | `sdk/paramoverride/header_override.go`：编译、校验、`Resolve`、透传规则导出；`Context.InitialHeaders` 作为初始状态 | SDK |
| T2 | 参数覆盖补齐：`regex` 条件、`sync_fields`、`prune_objects`、`pass_headers` 对象、`using_group` | SDK |
| T3 | 迁移 0105、store 模型、账号 API（输入/输出/校验/通用配置项）、core 适配、网关接入（透传 + 初始状态 + WebSocket）、预览 | server |
| T4 | manifest `ControlHeaderOverride` + check | SDK |
| T5 | 控制台：编辑器组件、AccountEditor 接入、i18n、types、摘要、错误定位、宿主组件与小部件、mock、测试 | web |
| T6 | 契约 §108、§81/§84 交叉引用、CHANGELOG | docs |
