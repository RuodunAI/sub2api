# U3 统一补缺包（公共组件与 SDK 的缺口 + 各迁移包审查出的遗留项；不提交到仓库）

在功能分支 `feat/plugin-ui-code-only` 上做（所有迁移包 G1、G3、G4、G5、G6 与 k1-fix 合入之后）。分两步：U3a（`@sub2api/ui` 与 SDK）先做，U3b（各插件跟进）依赖 U3a。

所有者的要求：插件界面只用代码写、不保留表单 JSON；保存时的最终校验在插件 Go；不得擅自加硬编码超时、上限、默认值、兜底、任务外的防御性检查；不写兼容/过渡代码；平台插件严禁联网；六条规范：抽象、复用、解耦、低嵌套、简洁、低侵入。

## U3a：`@sub2api/ui`（`next/web/packages/ui`）与 SDK（`next/sdk`）

### A. `@sub2api/ui`
1. **`createApiKeyForm` 的分节**：`ExtraField.section` 现在只能写标题。扩成能带说明与折叠（例如 `section: Text | { title: Text, help?: Text, collapsed?: boolean }`，或加 `sectionHelp`/`collapsed` 两个可选项——选与现有选项命名最一致的一种）。
   - 说明：`SSectionTitle` 后渲染 `SHint size="xs"`（旧 SchemaObject 的分节样式：上边线 `1px solid rgb(var(--line))`、`padding-top: 1rem`——bedrock `BedrockAccountForm.vue`、vertex `VertexApiKeyForm.vue`、volcengine `RelayForm.vue` 各手写了一份，收进组件）。
   - 折叠：用 `SDisclosure variant="section"`；规则照旧 `SchemaObject.vue`：`collapsed` 的一节默认收起；节里有值或有错误时自动展开；用户点过之后按用户的选择（volcengine `RelayForm.vue` 的 `legacyOpen` 是现成的同一套逻辑）。
   - 若分节渲染在 `createApiKeyForm` 之外也要用（bedrock、volcengine relay 是手拼的表单），抽成一个可单独使用的小组件（如 `SFormSection`），`createApiKeyForm` 与手拼表单共用。
2. **导出 `nativeAccountFormProps`**（§23.5 的 props：`modelValue`、`mode`、`account`、`accountType`、`errors`、`proxyTicket`）：`apiKeyForm.ts:115-123` 与 vertex `props.ts:6-13` 逐行相同，bedrock 用 `withDefaults(defineProps<…>)` 另一种写法，volcengine `RelayForm.vue` 逐项手写。运行时对象形式导出（`defineProps(nativeAccountFormProps)`），这样插件工程不需要 typescript 来解析导入的类型（`defineProps<导入类型>()` 在插件工程里编译失败：`[@vue/compiler-sfc] Failed to load TypeScript`）。README 与设计文档 5.10 的示例改成这种写法。
3. **`SDateTimeInput`**：`SInput type=datetime-local` 与 RFC 3339 互转、清空时不发该键。bedrock `BedrockAccountForm.vue:60-64`（`toLocalInput/fromLocalInput … || undefined`）与 `apiKeyForm.ts` 里 `control()` 的 `date-time` 分支是同一段逻辑，抽成组件两处共用。设计文档里把它写成 `SDateTime`（那是只读显示组件）的那一行改正。
4. **数字框清空**：`SInput type=number` 清空时发 `null`，`form.bind` 原样存下，Go 收到后报 `type`；mailer `MailerSettings.vue` 的 `setNumber`、growth `GrowthSettings.vue` 的 `num`/`setNumber`、guard `GuardSettings.vue` 冷却时间那行内联的 `?? undefined` 各自把 `null` 改成"不发这个键"。在 `useForm` 加 `bindNumber(path)`（或等价的最小接口），三处改用它。
5. **测试辅助** `@sub2api/ui/testing`（不进运行时的 import map，只供 spec 用）：
   - 账号表单：`fakeHost`、挂载（`mountAccountForm`）、填控件（`fillControls`）、`keyAndUrlCases`（api_key/base_url 的共用用例）、`checkMessages`（zh/en 同键、无 vue-i18n 特殊字符、无未用键）。现在的重复：G1 八个插件 `forms.spec.ts` 前约 125 行与 `messages.spec.ts` 27 行完全相同；bedrock、vertex 的 `form.testkit.ts` 70 行相同。
   - 设置表单：`fakeCore`、`mountSettings`（mailer、codex、growth、guard 四份各约 40 行）。
6. README 同步；每项带 spec（断言行为）。

### B. SDK（`next/sdk/pluginsdk`）
1. **`apikey.NormalizeBaseURL`**（`apikey/apikey.go:306-330`）：`TrimSpace` 之后，若还含任何 `unicode.IsSpace` 字符，或含 `?`、`#`，就拒绝。保留去首尾空白与 scheme 转小写（不改变发往上游的地址）。已存账号不受影响：已存值都是它的输出且通过过旧 schema（`^(https?://[^\s?#]+)?$`），不含这些字符；请求时对已存值重新规范化结果不变——用测试钉住这一点（对一组"规范化后的值"再规范化，结果相同且不报错）。不要动 `RawPath` 的处理（`%2F` 变 `/` 是已有问题，等所有者决定）。
2. **校验挂点**：让插件能在 `apikey.Spec`（或 `standard.Type`，看哪处更自然）上挂一组 `fieldcheck.Fields` 检查额外字段，这样 openai（`organization`、`project`）和 azure-openai（`api_version`、端点规则）不用再包一层 `Platform` 改写 `ValidateCredentials`，`New()` 不改类型，`TestGolden` 回放覆盖的是插件实际运行的 `ValidateCredentials`。与已有的 `Spec.Fields`（额外允许的键）的关系要理顺：最好一处声明既是"允许的键"又带规则，不要两处各写一遍。
3. **`apikey/apikeytest`**：八个插件的 `TestCredentialsRefused` 表骨架（共用的 api_key/base_url 反例与合法边界正例）抽成共用辅助，各插件只写自己的额外用例。
4. **设置解析辅助**：mailer、growth、guard 的 `parseSettings` 都是"解码成 map → `fieldcheck.Check` → 解码成结构体"三步，抽一个 SDK 辅助（放 `fieldcheck` 或 `pluginsdk`，看哪处合适）；"native 设置的清单必须写出 `secrets`"这段清单测试在四个插件里各写了一遍，抽到 `pluginsdktest` 或清单测试辅助。

## U3b：各插件跟进（依赖 U3a）

### bedrock、vertex（G3 审查）
- **必须改**：`vertex/internal/vertex/form_constraints_test.go:26` 的 32768 上限反例 `strings.Repeat("x", 32769)` 不是合法 JSON，去掉长度检查测试仍过 → 反例改成超过 32768 字符的**合法**密钥文件（如 `saJSON` 里加一个很长的额外字段），并断言错误码 `too_long`。
- bedrock `ui/native/src/fields.ts:40-43` 的 `typeOf`：未知类型退回 `'bedrock_apikey'` 是没人要求的兜底 → 去掉（直接用 `accountType?.type || account?.type`）。
- 两个 Go 测试注释 "deleted with UI 4.0" 措辞不对（forms 是这次删的）。
- 改用 U3a 的分节组件、`nativeAccountFormProps`、`SDateTimeInput`、测试辅助；vertex_apikey 改成直接用 `createApiKeyForm({ baseUrl: false, extra: [project_id, location] })`，删掉 `VertexApiKeyForm.vue`、`props.ts`、`TargetFields` 里随之不用的部分。
- vertex 拒绝未知键：两种密钥文件类型的允许键必须包含 `access_token`、`expires_at`、`token_for` 三个 hidden 键（续期写回后控制台编辑账号会把它们带回来再走校验）。

### volcengine（G4 审查）
- **必须改**：apikey 表单"素材库（可选）"分节说明丢失 → `asset_*` 字段用 U3a 的分节说明（`text('asset.sectionHelp')`）；`forms.spec` 的 labels 用例断言 `messages.en.asset.sectionHelp`；`PLUGIN-VOLCENGINE-ARK.md` §18"文案与原表单相同"措辞改准确（视频端点帮助文字 `/{task_id}` 改成了 `/task_id`）。
- `prefix.go:186` `normalizePrefix` 里新加的 `hasSpace`：这个函数每次请求都经 `prefixesOf`（`prefix.go:66`）调用，会让含 `\v`/NBSP 的已存前缀每次请求报错 → 空白检查挪到只在保存时跑的 `validatePrefixFields`（`prefix.go:215` 附近），`normalizePrefix` 恢复原样；它上方"only runs when an account is saved"的注释改对。
- SDK 统一拒绝空白后：删 `validateBaseURLSpace` 与 `assets.go:264` 的 `|| hasSpace(base)`（确认 `asset_base_url` 也走 SDK 的 `NormalizeBaseURL`，否则保留对应检查并说明）。
- `pages.spec` 补三条只让一条规则失败的用例：`NewAssetForm` url `max: 2048`（`'https://x/' + 'u'.repeat(2039)`）、`asset_type` `max: 64`（`'a'.repeat(65)`）、`account_id` 的 `integer`/`min: 1`（单独填 `0`）。
- `forms.spec` relay 补：`asset_endpoint` 清空/URL 写法/2049 长度、`video_api_prefix` 合法值、`api_prefix` 清空、`asset_base_url` 清空。
- `RelayForm.vue` 的 `legacyOpen` 折叠与手写 props 改用 U3a 的组件。

### 八个密钥类插件（G1 审查）
- openai"高级"一节用 U3a 的折叠分节（原表单默认收起）。
- SDK 拒绝空白后：七个插件 `TestCredentialsRefused` 里"空格转 %20"那一行改成反例；azure 改为对 `apikey.NormalizeBaseURL(base, nil)` 的结果匹配 `EndpointPattern`，与其它插件一致。
- openai、azure 去掉 `Platform` 包装，改用 U3a 的校验挂点；`TestGolden` 恢复为覆盖真实的 `ValidateCredentials`；openai 的 `visible`、azure 的"必填/格式"改用 `fieldcheck`。
- 拒绝未知键：openai 声明 `organization`、`project`，azure_deployment 声明 `api_version`；各 `TestCredentialsRefused` 加一条"未知键被拒"。（收尾包会把 `apikey.Spec` 改成总是检查未知键；这里先把该声明的声明好。）
- 测试改用 `@sub2api/ui/testing` 与 `apikey/apikeytest`。

### mailer、codex、growth、guard（G5 审查）
- 三处数字框清空改用 `bindNumber`；四个 spec 改用测试辅助；`parseSettings` 与清单测试改用 SDK 辅助。
- growth 补用例："签到关闭时，非字符串的 `checkin_timezone` 不被前端检查"（删掉它的 `when` 现在测试不失败）。

## 不做（等所有者决定或留给收尾包）
- `NormalizeBaseURL` 清空 `RawPath`（`%2F` 变 `/`）；openai 前端 key 规则与 Go 预设前缀不一致；moderation Jev 题集三条规则保存时拒绝；growth 未保存过设置时全关。
- 八个以上 `ui/native` 工程的配置样板由 `@sub2api/vite-preset` 承接、UI 包升 4.0、`hostUICompat` 统一 `^4.0`、删所有 schema 路径、`apikey.Spec` 总是检查未知键：收尾包 D1。
