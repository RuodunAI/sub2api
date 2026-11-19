# U3a 新增的公共接口与换法（U3a 实现方的汇报摘录；以代码与 `next/web/packages/ui/README.md` 为准）

分支 `feat/plugin-ui-u3`：`5951f801f` apikey 地址规则、`89ee1a140` Spec.Fields 规则/DecodeSettings/apikeytest/SettingsSecrets、`0f52fe4e0` SFormSection/SDateTimeInput/nativeAccountFormProps/bindNumber、`dc340c3ba` @sub2api/ui/testing。

## `@sub2api/ui`
- **`SFormSection`**（`form/SFormSection.vue`）：props `title: string`、`help?: string`、`collapsed?: boolean`、`fields?: string[]`；默认插槽放字段。不折叠：上边线 + h4 标题 + `SHint xs` 说明。折叠：`SDisclosure variant="section"`，开始收起，`fields` 里有值或有错误时自动展开，用户点过按用户的选择。
  - `createApiKeyForm` 的 `ExtraField.section` 现在是 `Text | { title, help?, collapsed? }`。
  - bedrock、vertex：手写的 `<div class="X-section"><SSectionTitle …/><SHint …/></div>` + scoped 样式 → `<SFormSection :title :help>…字段…</SFormSection>`。
  - volcengine `RelayForm.vue`：去掉 `toggled`/`legacyOpen` → `<SFormSection :title="t('relay.legacy')" :help="t('relay.legacyHelp')" collapsed :fields="LEGACY" data-testid="volcengine-legacy">`。
  - volcengine apikey：`section: { title: () => t('asset.section'), help: () => t('asset.sectionHelp') }`。
  - openai：`const advanced = { title: () => t('advanced'), collapsed: true }`，两个字段都写 `section: advanced`。
- **`nativeAccountFormProps`**（`form/credential.ts`）：§23.5 props 的运行时对象。`const props = defineProps(nativeAccountFormProps)` 替换 vertex 的 `props.ts`、bedrock 的 `withDefaults(defineProps<…>)`、volcengine 手写的那份。
- **`SDateTimeInput`**：`modelValue?: string`（RFC 3339），本地时间显示，发 UTC 的 RFC 3339，清空发 `undefined`。bedrock：`<SDateTimeInput v-bind="form.bind('session_expires_at')" />`。
- **`bindNumber(path)`**（`FormApi` 与 `usePluginSettings` 返回对象上）：同 `bind`，`null` 变成删掉这个键。`<SInput type="number" v-bind="settings.bindNumber('port')" />` 替换 mailer 的 `setNumber`、growth 的 `num`/`setNumber`、guard 内联的 `?? undefined`。
- **`@sub2api/ui/testing`**（只能在 spec 里引用）：`fakeHost({granted?, messages?, plugin?, api?})`、`messagesT(messages)`、`mountAccountForm(form, {accountType, stored?, errors?})` → `{w, credentials, validate, saved}`、`control(w, label)`、`fillControls(w, labels, input)`、`keyAndUrlCases(key, url, required, fill?)`、`fakeCore(plugin, values, secrets?)` → `{api, puts, stored(), refuse(e)}`、`mountSettings(Settings, {plugin, setHost, values?, granted?, secrets?})` → `{w, core, settings}`、`checkMessages(messages, {sources, dynamic?})`、`cleanup()`（`afterEach(cleanup)`）、原有 `mountPinned`。
  - 插件 `tsconfig.json` 的 `paths` 要在 `@sub2api/ui` 之前加一行：`"@sub2api/ui/testing": ["../../../../web/packages/ui/src/testing/index.ts"]`（vitest 不用改）。
  - 示例（G1 类）：
    ```ts
    for (const c of keyAndUrlCases('sk-ant-api03-abcdef','https://x.example.com/anthropic',false)) it(c.name, async () => {
      const { w, saved, validate } = mountAccountForm(AnthropicKeyForm, { accountType, stored: c.stored })
      await fillControls(w, { api_key: 'API key', base_url: 'Base URL' }, c.input ?? {})
      expect({ value: saved(), ok: await validate() }).toEqual({ value: c.out, ok: c.ok })
    })
    describe('messages', () => checkMessages(messages, { sources: import.meta.glob<string>([...], { query: '?raw', import: 'default', eager: true }), dynamic: [...] }))
    ```
  - 设置类：`mountSettings(MailerSettings, { plugin: 'mailer', setHost, values, granted, secrets: ['password'] })`。bedrock、vertex：把 `ORDER` 数组换成"字段 → 标签"映射，用 `fillControls`。

## SDK
- **`apikey.NormalizeBaseURL`**：去首尾空白后若仍含任何 `unicode.IsSpace` 字符或 `?`、`#` 就拒绝；去首尾空白、scheme 转小写照旧。
- **`apikey.Spec.Fields`** 类型改为 `fieldcheck.Fields`：一处声明既是允许的键又带规则（先读凭证再读设置；其它键报 `unknown`）；只允许不检查写 `fieldcheck.Any()`。也可以给 `api_key`、`base_url` 加规则：只在 Spec 自己的检查通过后运行，对的是提交的**原值**（未去空白），同一字段只报一个错误。
  - openai：`t.Spec.Fields = fieldcheck.Fields{"organization": String().MaxLen(128).Pattern("^[!-~]*$"), "project": 同上, "api_key": String().MaxLen(512).Pattern(`^\s*[!-~]*\s*$`)}`，删 `Platform` 包装，`New()` 返回 `*standard.Platform`。
  - azure：`"api_version": String().Required().Pattern(APIVersionPattern.String())`；端点规则可写成 `"base_url": String().Pattern(...)`（匹配原值）。SDK 测试 `apikeytest.TestMinKey` 演示了 openai 的写法。
- **`apikey/apikeytest`**：`Form{AccountType, Key, BaseURL, RequireBaseURL, MinKey, Fill}.Cases() []Case`；`Run(t, New().ValidateCredentials, append(form.Cases(), 插件自己的用例...))`；`Case{Name, AccountType, Creds, Field, Code, Want}`；`MinKey` 必须显式写（一般 8，openai 10）。
- **`fieldcheck.DecodeSettings(cfg, fields, &out) error`**：替换"map → Check → struct"三步：`s := Settings{默认值}; if err := fieldcheck.DecodeSettings(cfg, settingsRules, &s); err != nil { return nil, err }`。
- **`pluginsdktest.SettingsSecrets(t, manifestJSON, want ...manifest.SecretField)`**：替换各插件"必须写出 `secrets`"与 `SecretFields()` 的清单测试；mailer 传 `{Path: ["password"], Credential: true}`，其它不传。

## 因地址规则变化而失败、待 U3b 改的测试
`TestCredentialsRefused` 里"路径空格转成 %20"那一条：anthropic、gemini、typesafe（`credentials_test.go:55`）、newapi、relay、sub2api（`:58`）、openai（`:65`）。
