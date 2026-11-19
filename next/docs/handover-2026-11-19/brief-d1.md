# D1 收尾包：删掉全部声明式/schema 路径，界面契约升 4.0（给收尾子代理；不提交到仓库）

在功能分支 `feat/plugin-ui-code-only` 上做（U3a、U3b 合入之后）。所有者的决定："我们就直接不用声明式 UI 了，直接只用 SDK 提供的相关组件来做"、"不保留json"、"一步做完"（不分两期、不留兼容开关）；未验证的第三方插件没有 native 界面可以接受，iframe 模式保持原样、不删也不扩。

权威清单在设计文档 `next/docs/DESIGN-2026-11-19-PLUGIN-UI-CODE-ONLY.md` 的任务包 D1 一节与 9.2"删除清单"；下面是几轮审查累积出来的增补与提醒，以代码实际为准，逐项核对后删。

## 一、核心与 SDK：删 schema 路径
- `next/server/internal/plugin/api/settings.go`：`validateSettings`、`schemaErrors`、`denyLoader`、`schemaSecrets`；校验闭包里的 schema 那一半；`Install.Package` 调用；`SettingsView.Schema`、`UISchema`；`settingsForm` 里 `f.Schema == ""` 的条件；最终规则："native / iframe 按 `secrets`（键不存在 = 全部掩码，`[]` = 没有密钥），其它一律全部掩码"。`topLevelCredentials`、`sortSecrets` 还在用，保留。
- `next/server/internal/plugin/api/plugins.go`：`has_settings` 条件现在是 `mode IN ('schema','native')` → 只剩 `native`（iframe 仍为 false，与 develop 上控制台不渲染 iframe 设置的现状一致）；同步 `TestHasSettingsMode`、CONTRACTS §75.5。
- `next/sdk/manifest/settings_secrets.go`：`SettingsSecrets`、`UnmaskableFields`、`MaxSecretRefDepth/Nodes`、`localRef`、`validToken`（`escapedName`、`splitPointer` 保留）。
- `next/sdk/manifest/check/validate.go`：`maskableSecrets`、`formSecrets` 里两处 `f.Schema` 判断（"有 schema 时可不写 secrets"与"secrets 与 schema 并存 → unsupported"）、schema/uiSchema 的 `needFile`；清单里 `form.mode: "schema"`（账号表单与设置）以及声明式页面（`type: "declarative"`/`form` 页、`columns` 等）一律报 `unsupported`（错误码与文案照同文件已有风格），`Form.Schema`/`UISchema` 字段从 `manifest.Form` 删除。
- `next/sdk/manifest/check/account_check.go` 的 `Warnings`；`next/tools/sub2api-plugin/validate.go`、`pack.go` 里相应部分。
- `next/server/internal/account/creds.go` 的 schema 校验（`validateSchema`；`prepare` 的四个调用点 `handlers.go`、`models.go`）、`account/types.go` 的 `FormSchema/FormUI`、`core/ports_plugin.go` 与 `registry` 的 `FormSchema`、`GET …/form` 接口（若只为 schema 表单服务）、`pkgtest`/`registrytest` 里 schema 的构造。删掉后各插件的 `ValidateCredentials` 是唯一的把关——确认每个第一方账号类型都已拒绝未知键（见三）。
- 已安装的旧包（清单仍是 schema 模式或 native+schema）：加载时不能让核心起不来；按设计文档写的方式处理（设置全部掩码；账号表单/页面在控制台显示"需要升级插件"之类的既定表现——以设计为准，不自创兜底）。
- `santhosh-tekuri/jsonschema` 等只为 schema 校验服务的依赖从 `go.mod` 去掉（若再无使用）。

## 二、控制台前端：删声明式渲染
- `next/web` 里的 `SchemaForm`、`SchemaObject`、`SchemaField`、`InputControl`、`validateSchema`、`withDefaults`、`uiGet`、`DeclarativeTable`、`DeclarativeForm`、声明式页面路由与其 spec、mock 里的 schema 夹具；`AccountEditor`、插件详情页"设置"页签、`pluginUtil.ts`（`consoleSettings`、`PluginSettings` 类型里的 `schema`/`ui_schema`）只留 native（与 iframe 账号表单/页面的既有路径）。
- `@sub2api/ui`、`@sub2api/host`、`@sub2api/vite-preset` 版本升 **4.0.0**（`HOST_UI_VERSION` 等常量、`ui-contract.spec`、README 里"4.0 开发中"的措辞），全部 18 个插件的 `hostUICompat` 统一改 `^4.0`；各插件 lockfile 随之重新生成。

## 三、SDK 行为
- `apikey.Spec.Fields`：改成**总是检查未知键**——nil 等同空（只允许 `api_key`、`base_url`）。现在各第一方插件都已显式写了 `Fields`，所以只是去掉"nil = 不检查"这条分支及其测试；`Spec.Fields` 注释、CONTRACTS 同步。
- `apikey.Spec` 永远把 `base_url` 当已知键：没有 base_url 的类型（`vertex_apikey`）靠插件里额外的 `form.Only(api_key, project_id, location)` 拒绝它。评估给 `Spec` 加一个声明"本类型没有 base_url"的最小选项（若 SDK 已有等价开关就用它），有了就删掉 vertex 的那层 `Only`；没有把握就保持现状并说明。
- vertex 等用 `accountform.Form.Only` 的：确认允许键齐全（vertex 含 `access_token`、`expires_at`、`token_for`）。
- **删核心的 schema 校验之前，逐个核对全部第一方账号类型**（anthropic、gemini、typesafe、openai、relay、newapi、sub2api、azure 两种、bedrock 三种、vertex 三种、volcengine 两种、codex 的各类型）：插件 Go 的 `ValidateCredentials` 拒绝未知键（凭证与设置两个对象），且表单会发的键、续期写回的键全部允许。原 schema 是 `additionalProperties:false` 而插件 Go 还没把关的，补上并加测试；原 schema 本来就允许多余键的，保持允许并说明。

## 四、插件工程样板由 `@sub2api/vite-preset` 承接
- preset 提供 `tsconfig.plugin.json`（paths 指向 web 的 packages 与 node_modules，含 `@sub2api/ui/testing`）、vitest 基础配置、可用 `--config` 指定的构建配置；插件工程的 typecheck/test/build 都用 `next/web/node_modules`。删掉各插件的 lockfile、devDependencies、`.npmrc`、重复的 `tsconfig`/`vite.config`/`vitest.config` 内容（保留最小的继承文件）。
- `next/deploy/docker/build-ui.sh`、`.github/next-ci/next-check-plugin-ui.sh` 改为"有 lockfile 才 `npm ci`"（第三方插件仍可独立安装）；`build-go.sh` 只检查 `dist`，不受影响；Dockerfile 的 ui 阶段与 CI 都先装 web，确认相容。
- 顺带解决：插件工程里 `defineProps<导入类型>()` 需要 typescript 的问题（用 web 的 node_modules 后是否自然解决，验证并在 README 说明）。
- **这一项与"升 4.0"关系紧密但改动面大；若实现中发现风险（CI 的 node 24、第三方独立构建），停下来汇报，不要硬做。**

## 五、测试与 e2e
- e2e：AC04、AC18、AC20、AC23 等断言 `GET …/form`、schema 字段、声明式页面的用例按新契约改；全仓搜 `forms/`、`schema`、`ui_schema`、`uiSchema`、`declarative` 的残留引用。
- 设计/契约之外的夹具：SDK `providers/typesafe/typesafe_test.go` 用的内存 forms 文件、`pkgtest`、`registrytest`。
- `tools/sub2api-plugin` 的 `--allow-missing-ui` 开关保留（测试里打包真实插件用）。
- `check.sh plugins` 在没有 node 的容器里跑：迁移后必须先 `build-ui.sh`——把这一步写进脚本或其说明。

## 六、规则
- 不留兼容/过渡代码；不擅自加超时、上限、默认值、兜底、任务外的防御性检查；平台插件严禁联网；六条规范。文档（CONTRACTS §21.3/§23/§75.5、COMMON-CONTROLS、PLUGIN-DEVELOPMENT、CONTRACTS:2372/4012/4088/4780 里 moderation 的 schema/writeOnly/x-sensitive 表述）由文档包 D2 统一改，D1 只改与代码强绑定、不改就会误导的条文，并把留给 D2 的位置列出来。
- 不做：`NormalizeBaseURL` 的 `RawPath`（`%2F`）与多级后缀不幂等；openai 前端 key 规则与 Go 预设前缀不一致；moderation Jev 题集三条规则保存时拒绝；growth 未保存过设置时全关——都等所有者决定。
