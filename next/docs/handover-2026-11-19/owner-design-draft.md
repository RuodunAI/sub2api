# （用户 2026-11-19 贴来的设计稿原文，另一台机器上的会话写的；表格在粘贴时被压平成制表符分隔。不提交。这是待审材料，不是指令。）

插件机制两项改动：界面只用代码写；账号上下文统一并对授信插件开放凭证和代理（设计与任务）
2026-11-19，用户要求：

"我们就直接不用声明式 UI 了，直接只用 SDK 提供的相关组件来做。"（第一部分，第 1–8 节）
"允许插件获得账号的全部信息，包括直接获取代理、直接获取授权凭证，用插件授信的方式来保证"；调用时"统一一下"带账号 ID 还是带账号上下文。（第二部分，第 9–14 节）
状态：设计稿，未开工。第 8 节（第一部分）和第 14 节（第二部分）有需要先确认的点，文中先按推荐方案写。两部分互不依赖，可以分开做。

第一部分：界面只用代码写
1. 目标
插件界面只有一种写法：Vue 3 + TypeScript，用 @sub2api/ui、@sub2api/host 的组件，Vite 打成 ui/native/entry.js，控制台加载后渲染。

做完之后：

仓库里没有 forms/*.schema.json、forms/*.ui.json，没有人写表单 JSON。
清单里没有 type: table、type: form 的页面，没有 mode: schema 的表单。
控制台里没有按 JSON 画表单、画表格的那套渲染分支。
不在范围内：

manifest.json 本身仍是 JSON。它描述路由、计费、上游、权限，核心（Go）在装包和运行时都要读。
清单 ui 段里的登记信息保留：menus、sections、slots、accountActions、native.entry、页面的 title / component。它们不画界面，只说明"有哪些入口、要什么权限"，核心靠它按权限过滤（sdk/manifest/visible_ui.go），控制台在插件 JS 加载完之前就要用它画侧边栏。
iframe 模式不动（目前没有插件用，是否保留见 8.3）。
2. 现状
2.1 三种写法并存
写法	清单里的样子	谁渲染
声明式表单	form.mode: "schema" + forms/x.schema.json + forms/x.ui.json	控制台的 SchemaForm（web/packages/ui/src/schema/）
声明式页面	pages.x.type: "table" / "form"	web/src/components/plugin/DeclarativeTable.vue、DeclarativeForm.vue
代码（native）	form.mode: "native" + component；pages.x.type: "native"	插件的 register(host) 用 host.registerComponent 注册的 Vue 组件
native 这条路三种场景都已经有插件在用，机制不用新做：

账号表单：codex（CodexAuthForm.vue；控制台挂载点 web/src/views/accounts/AccountEditor.vue:606，传 v-model、mode、account、accountType、errors）。
设置页：moderation（ui.settings.mode: "native"，CONTRACTS §75.5）。
页面和插槽：dynweight、growth、guard、moderation、codex。
2.2 还在用声明式的地方
插件	runtime	账号表单（schema）	设置表单（schema）	声明式页面	已有 ui/native
anthropic	grpc	apikey			否
azure_openai	none	azure_v1、azure_deployment			否
bedrock	grpc	bedrock_apikey、bedrock_aksk、bedrock_sts			否
claude_oauth	grpc	claude_oauth、claude_setup_token			否
gemini	none	apikey			否
openai	none	apikey			否
relay	none	relay_key			否
vertex	grpc	vertex_sa、vertex_authorized_user、vertex_apikey			否
volcengine	grpc	apikey、relay		2 个 table、2 个 form	否
growth	grpc		settings		是
guard	grpc		settings		是
mailer	grpc		settings		否
moderation	grpc		（native，但仍带 schema 和 uiSchema）		是
合计：16 种账号类型、3 个设置表单、4 个页面要迁；10 个插件要新建 ui/native 工程；forms/ 下约 100KB JSON 要删。

2.3 schema 现在除了画界面还承担什么
去掉声明式，下面这些后端职责要有别的来源，这是本设计的难点：

职责	现在的实现	位置
保存账号凭证时校验	按账号表单的 JSON Schema 校验	server/internal/account/creds.go:219
保存插件设置时校验	按设置的 JSON Schema 校验	server/internal/plugin/api/settings.go:333
哪些设置字段是密钥（加密保存、读回显示 ******）	读 schema 的 writeOnly / format: password / x-sensitive	sdk/manifest/settings_secrets.go
装包时检查密钥字段能否逐个遮蔽	读同一份 schema	sdk/manifest/check/validate.go:496
受限设置（无 account:settings:custom 权限的人只能选官方值，CONTRACTS §21.3）	改写下发的 schema（加 enum）和 ui 提示（ui:readonly）	server/internal/account/types.go guardForm
3. 设计
3.1 前端：SDK 增加组合组件
@sub2api/ui 现在只有基础组件（SInput、SSwitch、SField、SSettingRow、STable、SFormModal…）。16 个账号表单如果都从基础组件拼，会有大量重复。先在 SDK 里补下面几个，让一个普通表单在 30 行以内：

组件 / 函数	用途	替代的声明式能力
SSecretInput	密钥输入框：已有值显示 ******，不改就原样送回	ui:widget: secret
SUrlPresetInput	地址输入，带预设项	ui:widget: url-presets（9 处在用）
SCredentialForm + useCredentialForm()	账号表单骨架：接住控制台传入的 v-model / mode / errors / accountType，处理"编辑时保留已存密钥"，暴露 validate()	SchemaForm 在账号编辑器里的用法
SApiKeyForm	api_key + base_url 的整张表单（预设地址、占位符、帮助文案可配）	7 个 apikey 类表单
SSettingsPage + usePluginSettings()	设置页骨架：读写 /plugins/:key/settings、未保存提示、保存栏、字段错误回显、离开确认	views/plugins/detail/SettingsTab.vue
SResourcePage	列表页骨架：useList + STable + 搜索框 + "新建"按钮 + SFormModal	DeclarativeTable + DeclarativeForm
useGuardedSettings()	按账号类型的 guarded_settings 把字段限制为官方值或只读	guardForm 改写 schema
约定：

控制台挂载 native 账号表单的接口保持现在这套（v-model、mode、account、accountType、errors），codex 不用改。
表单文案走 host.addMessages / host.t，不再有内联的 {en, zh}。
校验先在前端做（必填、长度、格式），给即时反馈；后端仍然是最终校验（3.3）。

3.2 清单的变化
字段	改动
accountTypes[].form	只接受 mode: "native"（和 iframe，见 8.3）；删除 schema、uiSchema
ui.settings	只接受 mode: "native"；page 可省（省略时控制台在插件详情页的"设置"标签里挂 component）；删除 schema、uiSchema
ui.pages.*	只接受 type: "native"（和 iframe）；删除 source、columns、schema、submit、search、create
新增 accountTypes[].secrets、ui.settings.secrets	密钥字段路径列表，见 3.3
hostUICompat	所有带界面的插件都要声明（native 的既有要求）
hostPermissions	所有带界面的插件都要申请 ui.native（既有要求）
sdk/manifest/check 对应收紧：遇到 mode: "schema"、type: "table"、type: "form" 报 unsupported，错误信息指向本文档。

3.3 后端：校验和密钥
密钥字段：从清单读，不再从 schema 读。

"ui": { "settings": { "mode": "native", "component": "MailerSettings", "secrets": ["password"] } }
路径写法沿用 manifest.SettingsSecrets 现在能遮蔽的范围（顶层键、对象内的键、对象数组里的键），sensitive（只遮蔽不加密的地址类，原 x-sensitive）用 {"path": "notify_webhook_url", "sensitive": true} 表示。
manifest.SettingsSecrets 改成从这个列表构造，返回类型不变，所以下游（遮蔽、保留旧值、reenter 规则）不用动。
账号凭证整体加密的现有行为不变，accountTypes[].secrets 只用于读回时遮蔽哪些键。
值的校验：交给插件的 Go 代码。

账号凭证：gRPC 插件已有 ValidateCredentials（platform.proto:26），核心在 creds.go 里把"按 schema 校验"换成"调用它"。anthropic、bedrock、vertex、volcengine、claude_oauth 要确认各自实现覆盖了原 schema 的约束（必填、长度、枚举），缺的补上。
插件设置：新增 PluginService.ValidateSettings(values) -> {normalized, field_errors}（plugin.proto，和已有的 SettingsChanged 并列），PUT /plugins/:key/settings 在落库前调用。SDK 提供默认实现（全部接受）和一个小的校验辅助包，growth、guard、mailer、moderation 各自实现。
字段错误的返回格式沿用现在的（路径 → 文案），前端 errors 回显不用改。
受限设置（§21.3）：后端的限制必须是独立于表单的硬校验。任务里要先确认 account 服务在保存时是否已经独立检查 guarded_settings；如果现在只靠改写 schema 来限制，要补成保存时检查。前端用 useGuardedSettings() 只负责把控件显示成受限状态。

3.4 没有进程的插件（runtime: none）
openai、gemini、relay、azure_openai 没有 Go 进程，没有人能替它们跑 ValidateCredentials。推荐做法：

它们仍然不需要进程，只多一个 ui/native（纯前端）。
核心对它们的凭证做最低限度检查：是 JSON 对象；清单 accountTypes[].required（新增，字段名列表）里的键非空；base_url 这类已知键按核心现有规则检查（协议、受限设置）。
需要更细校验的，改成 gRPC 插件。
3.5 第三方插件
控制台只加载发布者信任级别为 official / verified 的插件 JS（web/src/stores/plugins.ts 的 nativeAllowed）。去掉声明式后，未验证的插件没有任何界面。推荐做法：接受这一点，第三方要有界面就得通过验证，或者用 iframe 模式（沙箱里跑，不拿控制台会话）。安装审核页（server/internal/plugin/install/review.go）对所有带 ui.native 的包显示"此插件含在控制台内执行的前端代码"。

3.6 构建和 CI
deploy/docker/build-ui.sh 已经遍历 plugins/*/ui/native 构建，新增的 10 个工程自动纳入；镜像构建时间会变长（每个工程一次 npm ci + vite build），任务里评估是否改成一个 npm workspace 共用依赖。
build-go.sh、build-demo.sh 已有"清单有 native 但没有 dist 就报错"的检查，继续适用。
next-ci.yml 的 web 任务目前只跑控制台的类型检查和测试。要加一步：对每个 plugins/*/ui/native 跑类型检查和它自己的 spec。
发布摘要（plugins/released.json，TestReleasedPluginsBumped）覆盖 ui/ 和 forms/，所以每个被迁的插件都要升版本号，发布后重录摘要。
4. 版本与兼容
UI 契约 4.0.0（@sub2api/ui、@sub2api/host、@sub2api/vite-preset、HOST_UI_VERSION）。分两步走，避免一次性断开：
3.20（只加不改）：加 3.1 的组合组件。插件在这一版上逐个迁，声明 hostUICompat: "^3.20"。
4.0（删除）：删 SchemaForm、SchemaField 及 schema 相关导出。迁完的插件把 hostUICompat 写成 ^3.20 || ^4.0。
核心：先发一个同时支持新旧两种写法的版本（新增 secrets、ValidateSettings，旧的 schema 仍可用）；所有内置插件迁完并在生产上换新包之后，再发删除声明式的版本。各插件 hostCompat 下限抬到第一个版本。
已安装的旧包：删除声明式的核心版本启动时，遇到仍是 mode: "schema" 的已装插件，保留其路由和转发功能，界面处显示"此插件版本的界面已不受支持，请升级"，不能因此拒绝启动。
已存的数据：设置和账号凭证的存储格式不变，不需要数据库迁移。迁移后的组件必须读写和原表单相同的键（验收项）。
5. 任务
每个任务是一个可以单独合并的改动。"验收"是合并前必须成立的事。

阶段 A：SDK（UI 3.20）
A1	SSecretInput、SUrlPresetInput	web/packages/ui/src/	组件 spec；****** 不改时原样送回
A2	SCredentialForm、useCredentialForm、SApiKeyForm	同上	用它重写一个 apikey 表单，产出的 credentials 对象与原 schema 表单逐键相同
A3	SSettingsPage、usePluginSettings	同上；逻辑从 views/plugins/detail/SettingsTab.vue 提出来	未保存提示、字段错误回显、reenter 错误码行为与现在一致
A4	SResourcePage	同上	分页、搜索、新建后刷新
A5	useGuardedSettings	web/packages/host/src/	无 account:settings:custom 权限时控件受限
A6	控制台：ui.settings.mode: "native" 且没有 page 时，在插件详情的设置标签里挂 component	web/src/views/plugins/detail/	mailer 这类没有自己页面的插件能显示设置
A7	版本 3.20.0、ui-contract.spec.ts、web/packages/ui/README.md、CONTRACTS
阶段 B：核心同时支持两种写法
B1	清单 secrets（设置、账号类型）、accountTypes[].required；SettingsSecrets 可从列表构造	sdk/manifest/	原有 settings_secrets_test.go 的用例改用列表后全部通过
B2	PluginService.ValidateSettings + SDK 默认实现和辅助	sdk/proto/.../plugin.proto、sdk/pluginsdk/	没实现的旧插件行为不变
B3	保存设置：native 且无 schema 时走 B1 + B2	server/internal/plugin/api/settings.go	遮蔽、保留旧值、字段错误与 schema 路径一致
B4	保存凭证：native 时走 ValidateCredentials；runtime: none 走 3.4 的最低检查	server/internal/account/creds.go
B5	确认并补齐受限设置在保存时的独立校验	server/internal/account/	绕过界面直接调接口提交非官方值被拒
B6	安装审核页提示"含前端代码"	server/internal/plugin/install/review.go
B7	CI：插件 ui/native 的类型检查和 spec	.github/workflows/next-ci.yml
阶段 C：迁插件（每个插件一个改动）
每个插件的步骤相同：建 ui/native（没有的话）→ 写组件并在 entry.ts 注册 → 清单改 mode: "native"、加 secrets、hostUICompat、ui.native 权限 → Go 侧补校验 → 删 forms/ → 升版本号和 hostCompat。

每个插件的验收相同：新旧表单对同一组输入产出相同的键和值；编辑已存账号 / 设置时密钥不丢；中英文文案齐全；manifest_test.go 更新后通过。

C1	mailer	1 个设置表单	最简单，验证 A3、A6、B2、B3
C2	openai	1 个 apikey 表单	验证 A2 和 runtime: none 的路径（B4）
C3	gemini、relay、azure_openai	4 个表单	同 C2，批量
C4	anthropic	1 个表单	第一个 gRPC 平台插件，验证 ValidateCredentials 路径
C5	guard、growth	2 个设置表单	已有 ui/native，只加组件
C6	bedrock、vertex	6 个表单	字段是自己一套（AK/SK、密钥文件），用 SCredentialForm 拼
C7	claude_oauth	2 个表单	现在只有 schema、没有 ui.json
C8	volcengine	2 个表单 + 4 个页面	最大的一个，验证 A4
C9	moderation	去掉清单里残留的 schema / uiSchema，settingsForm.ts 不再读 ui 提示	已是 native 页面
阶段 D：删除声明式（UI 4.0）
前置条件：C 全部完成，生产上所有插件都已换成新包。

D1	控制台删除 DeclarativeForm.vue、DeclarativeTable.vue、declarative.ts、components/schema/widgets.ts，账号编辑器和设置页的 schema 分支	web/src/
D2	@sub2api/ui 删除 schema/ 目录和相关导出；版本 4.0.0	web/packages/
D3	清单类型删除 Form.Schema / UISchema、Page 的 table/form 字段、Column；check 收紧；pack 不再打包 forms/	sdk/manifest/、tools/sub2api-plugin/
D4	核心删除 schema 校验路径、guardForm、GET /account-types/.../form；旧包降级显示（第 4 节）	server/internal/account/、plugin/api/、plugin/registry/
D5	文档：CONTRACTS 相关章节、PLUGIN-DEVELOPMENT.md、COMMON-CONTROLS.md 改成只讲 native；本文档标记完成	docs/
6. 工作量粗估
按一个人算，未含评审和发布等待：A 约 5 天，B 约 5 天，C 约 8 天（C8 占 3 天），D 约 3 天，合计约 4 周。A 和 B 可以并行。这是粗估，A2 做完、C1 和 C2 迁完之后才有可靠的数。

7. 风险
迁移后的表单写出的键或默认值和原来不一样，已存账号编辑后被改坏	每个表单的验收都有"同输入同输出"对比；迁移期保留旧 schema 文件作为测试夹具，D 阶段再删
校验从"核心统一按 schema"变成"各插件自己写"，容易漏	B2 的辅助包覆盖常见约束；每个插件的 Go 测试要有"原 schema 拒绝的输入现在仍被拒绝"的用例
密钥列表漏写一个字段，密钥明文回显	B1 的装包检查：secrets 里的路径必须是合法路径；C 阶段每个插件对照原 schema 的 writeOnly 字段逐一核对，写进测试
所有带表单的插件的 JS 都在管理员控制台里执行，攻击面比现在大	只加载 official / verified（现状）；B6 的审核提示
插件 JS 加载失败时，账号连编辑都做不了（现在 schema 表单不依赖插件 JS）	账号编辑器已有加载中和失败提示（AccountEditor.vue:606 一段）；失败时给"重试"并保留其它分区可编辑
10 个新前端工程拖慢镜像构建	3.6 的 workspace 评估，放在 C3 之前做
8. 需要先确认的三点
文中已按"推荐"写。任何一点改了，受影响的任务在括号里。

校验和密钥的来源。推荐：清单 secrets 列表 + 插件 Go 代码校验（3.3）。另一种是保留一份极简 schema 只给后端用，不再用于画界面；那样 B1、B2 不用做，但仓库里仍有 JSON。（影响 B1–B4、每个 C）
runtime: none 的插件。推荐：核心做最低限度检查（3.4）。另一种是把这 4 个插件都改成 gRPC。（影响 B4、C2、C3）
未验证的第三方插件。推荐：没有界面，要界面就验证或用 iframe（3.5）。如果同时决定删掉 iframe 模式，D 阶段多一项。（影响 B6、D）

第二部分：账号上下文统一，并对授信插件开放凭证和代理
9. 目标和取舍
讨论过"插件永远拿不到凭证"的做法（占位符由核心填充、签名由核心做、认证代码放进 WASM 沙盒）。结论是可以做，但成本高：每种签名要么进核心，要么要嵌 wazero 运行时，授权和续期流程都要拆。决定不做隔离，改为靠授信：

声明了账号类型的插件，直接收到这个类型的账号的全部信息，包括解密的凭证和代理（地址、用户名、密码）。不为此新增授权。
安全靠授信保证：发布者信任级别和包签名；安装时把"会收到凭证和代理"作为严重风险让管理员确认；主动读取留审计。
同时把"什么时候给账号信息、给多少"统一成一条规则，取消现在各接口各不相同的例外。
不做的事：占位符扩展到 gRPC 插件、签名方案扩充、沙盒。sdk/reqsign 的 aws-sigv4 保留，原因不变：请求体插件看不全，覆盖请求体的签名只能由核心做。

10. 现状
10.1 账号信息现在怎么给
场景	现在给什么	位置
网关路径的平台接口（BuildUpstreamRequest、测试、额度、续期、余额等）	完整的 Account 消息；credentials_json 只在插件持有 accounts.credentials 且账号类型是它自己声明的时候有值	common.proto Account；platform.proto 共 14 处带 Account
事后回调 ExtractUsage	Account，但凭证永不包含	platform.proto:427
对账 BuildReconcileRequest / ParseReconcileResponse	平台和账号类型来自不同插件时凭证留空	platform.proto:578
插件自己的页面路由、定时任务、事件	不给账号。插件用 ListAccounts（不含凭证）和 GetAccountCredentials(id)（解密凭证，每次写审计）自己取	host.proto:217、:224
GetAccountCredentials 返回的是 AccountSummary + credentials_json，和网关路径的 Account 不是同一个结构。

10.2 代理
插件只拿到 AccountProperties.proxy_id，拿不到代理地址和账号密码。转发、测试、额度、续期请求都由核心经账号代理发出。两个例外是插件自己直连、没有走账号代理的：claude_oauth 和 codex 授权时换令牌（claude-oauth/main.go exchangeCode、codex/internal/codex/oauth.go post，靠 net 授权）。

10.3 授信手段（都已存在）
手段	位置
发布者信任级别 official / verified / 未验证	server/internal/plugin/pkg/trust.go
包签名	sdk/pkgsig
授权清单和风险等级（accounts.read 中、accounts.credentials 严重）	sdk/manifest/manifest.go HostPermissionRisk
安装审核和同意	server/internal/plugin/install/review.go、consent.go
读取凭证的审计	GetAccountCredentials 写 audit_logs
现在没有"某个授权只允许某信任级别申请"的规则，未验证的包也可以声明 accounts.credentials，只是安装时会列为严重风险。

10.4 现有插件
插件	持有 accounts.credentials	需要代理信息吗
bedrock、vertex	是	否（请求全由核心发）
claude_oauth、codex	是	可选：授权换令牌现在直连，拿到代理后可以自己走代理
volcengine	是（另有 accounts.read）	可选：素材库的 OpenAPI 调用现在直连
其余	否	否
11. 设计
11.1 改动一：声明了账号类型的插件，直接收到自己账号的全部信息
2026-11-19 用户定稿：账号信息是核心传给插件的，不是插件主动要的，不应该叫"权限"。所以不新增授权，规则改成按归属：

插件在清单里声明了某个账号类型以及对应的协议。核心把这个账号上的相关请求交给该插件处理时，把账号的全部信息一起传过去：属性、设置、解密的凭证、代理（地址、用户名、密码）。

（2026-11-19 用户的表述："声明了账号类型以及对应协议的插件。核心在转发相关请求给插件、由插件来完成的时候，把对应的账号信息转发给它。"）

"对应的协议"指账号类型的 platforms 和该插件承接的端点协议（如 anthropic.messages、openai.responses）。账号类型和协议由不同插件声明时，处理协议的那个插件不是账号类型的声明者，不给，这和现在 platform.proto:578 的规则一致。

主路径是核心随请求传：核心调用平台接口（构造上游请求、执行、WebSocket、测试、额度、续期、余额、用量提取、对账、轮询）时，Account 里带全部信息。插件不需要主动取。
补充路径：插件自己的页面路由、定时任务、事件不是核心转发的账号请求，核心不知道涉及哪个账号，插件用 GetAccount(id) 取同一个 Account（11.2），只能取自己账号类型的。目前只有 volcengine 的素材库页面需要它。
不是自己账号类型的账号：什么都不给。平台和账号类型来自不同插件时照旧不给。
// common.proto
message Account {
  ...
  // 账号绑定的代理，含用户名和密码。账号类型由被调用的插件声明时才有值；
  // 账号没绑代理时为空。
  AccountProxy proxy = 8;
}

message AccountProxy {
  int64 id = 1;
  string protocol = 2;   // http | https | socks5 ...
  string host = 3;
  int32 port = 4;
  string username = 5;
  string password = 6;
}
现有授权怎么处理：

授权	处理
accounts.credentials	不再需要。清单里写了仍然接受（旧包能装），但不再起作用，check 给一条"已废弃"的警告
accounts.read	不再需要用于自己的账号类型，处理同上
行为变化：现在声明了账号类型但没申请 accounts.credentials 的 gRPC 插件（目前只有 anthropic）会开始收到凭证。

授信落在哪：没有授权这一行之后，管理员是在"安装一个声明了账号类型的插件"这一步表示信任的。安装审核页要把这件事明说：声明了账号类型的包，显示"此插件会收到其账号类型下所有账号的凭证和代理信息"，按"严重"风险展示，并列出发布者信任级别和签名状态。不加信任级别限制（2026-11-19 用户："这些先不要限制了"）：任何包都可以声明账号类型，只靠这条安装提示。

审计：用 GetAccount 主动取账号时每次写审计（插件、账号，不记录值）。网关路径随请求下发不逐条审计。

SDK 辅助：pluginsdk.ProxyTransport(acc) 用 Account.proxy 构造 http.RoundTripper（没有代理时直连）。AccountProxy 的 String / LogValue 遮蔽密码，做法同 reqsign.Directive。

11.2 改动二：账号上下文统一
一条规则：核心把某个账号上的请求交给声明了该账号类型和协议的插件处理时，带完整的 Account；不是核心转发的场景，插件用账号 ID 调 GetAccount 取，返回的是同一个 Account。给不给只看账号类型是不是这个插件声明的，不看是哪个接口。

不统一成"只带 ID"的原因：网关是热路径，只给 ID 会让每个请求多一次 gRPC 回调；核心选号时用的账号快照和插件看到的应当是同一份。页面路由不能带上下文的原因：路由是插件定义的，核心不知道这次调用涉及哪个账号（一个、一批或没有）。

具体改动：

项	改动
平台接口里的 Account	所有带 Account 的消息按同一规则填充。取消 ExtractUsage "凭证永不包含" 的例外（platform.proto:427）。跨插件时不给（platform.proto:578）的规则保留，它属于"范围"而不是"接口例外"
HostService.GetAccount(account_id) -> Account	新增。返回和网关路径相同的完整 Account，不需要授权；不是自己账号类型的一律 NOT_FOUND
GetAccountCredentials	标记为废弃，内部转到 GetAccount，保留到所有内置插件迁完后的下一个核心版本再删
ListAccounts	不变，仍返回不含敏感字段的 AccountSummary（列表场景不该批量下发凭证）
核心里构造 Account 的代码	现在分散在 core/ports_identity.go:986、account/models.go:106、grpcruntime/host.go:618 等处。收拢成一个函数 accountFor(plugin, account)，按账号类型归属决定填不填，所有调用点都走它
统一之后，插件作者只需要知道一个 Account 类型：是自己的账号类型就有全部字段，不是就拿不到。

12. 任务
E1	协议：AccountProxy、Account.proxy、HostService.GetAccount；重新生成	sdk/proto/.../common.proto、host.proto、sdk/gen	旧插件二进制不重编也能跑（只加字段）
E2	清单：accounts.credentials、accounts.read 标为废弃（接受但不起作用，check 给警告）	sdk/manifest/manifest.go、check/	带这两个授权的旧包照常安装
E3	安装审核：声明了账号类型的包显示"会收到凭证和代理"的严重风险提示	server/internal/plugin/install/	声明了账号类型的包有提示，没声明的没有
E4	accountFor 收拢，所有平台接口按统一规则填充；ExtractUsage 带凭证	server/internal/core/、account/、grpcruntime/、gateway/	每个带 Account 的接口各一条用例：自己的账号类型有全部字段、跨插件为空
E5	GetAccount 实现和审计；GetAccountCredentials 转发并标废弃	server/internal/plugin/grpcruntime/host.go	审计行内容；别人的账号返回 NOT_FOUND
E6	SDK：pluginsdk.GetAccount、ProxyTransport、AccountProxy 的日志遮蔽；pluginsdktest 的假核心支持	sdk/pluginsdk/	打日志、%v、%+v 都不出现密码
E7	控制台安装页显示 E3 的提示	server/internal/plugin/install/review.go、控制台插件安装页	中英文文案
E8	volcengine 从 GetAccountCredentials 换到 GetAccount	plugins/volcengine/internal/volcengine/routes.go	素材库功能不变
E9	（可选）claude_oauth、codex 授权换令牌经账号代理；volcengine 素材库请求经账号代理	各插件	见 14.3
E10	文档：CONTRACTS §26.6 及相关章节、PLUGIN-DEVELOPMENT.md	docs/
工作量粗估：E1–E8、E10 约 1 周；E9 每个插件约 1 天。

12.1 各插件怎么改
核心侧（E1–E6）做完后，插件按下表改。"清单"一列指删掉已废弃的 accounts.credentials / accounts.read 授权并抬 hostCompat 下限；不删也能跑，只是有废弃警告。

插件	现在怎么拿账号	要改什么	必须改吗
openai、gemini、relay、azure_openai	没有进程，核心按清单模板发请求	不改	否
anthropic	有进程但没申请凭证授权，上游走清单模板	不改。升级核心后它的 Account 里会开始带凭证和代理，代码不读就没有影响	否
bedrock	平台接口里读 Account.credentials_json	清单	否
vertex	同上	清单	否
claude_oauth	平台接口里读凭证；授权换令牌用 http.DefaultClient 直连	清单；（E9）换令牌改用 pluginsdk.ProxyTransport，代理来自授权请求里的 proxy_id	E9 可选
codex	平台接口里读凭证；授权换令牌用自己的 http.Client 直连；重置额度用 SendAccountRequest	清单；（E9）换令牌改用 ProxyTransport。SendAccountRequest 不用换，它本来就由核心经代理发	E9 可选
volcengine	平台接口里读凭证；素材库路由用 ListAccounts 选账号、GetAccountCredentials 取 AK/SK，自己签名后经宿主出口直连	清单；路由里 GetAccountCredentials 换成 GetAccount（E8，旧接口过渡期仍可用）；ExtractUsage 里如需凭证可直接读；（E9）素材库请求改走账号代理	E8 建议做，E9 可选
dynweight、growth、guard、mailer、moderation、payment	不声明账号类型	不改，照旧拿不到任何账号的凭证和代理	否
两点要注意：

授权阶段账号还不存在。claude_oauth、codex 换令牌时账号尚未创建，没有 Account 可传；控制台在授权请求里带的是用户选的 proxy_id。所以 E9 需要一个按 ID 取代理的接口 GetProxy(proxy_id)，或者让控制台先建一个草稿账号。前者改动小，记入 E1、E5。
插件的出站连接是经宿主拨出的（sdk/pluginsdk/egress，沙箱不允许直接开套接字，目标受 net 授权的域名范围约束）。插件自己走代理，等于经宿主出口连到代理服务器，再由代理连上游。ProxyTransport 要建在 egress.DialContext 之上，并且出口策略要放行代理的地址。这是 E6 里最需要先验证的一点。
13. 风险
受信任的插件本身有漏洞或被攻破，凭证和代理一起泄露	这是本方案接受的风险。缓解：范围限自己的账号类型；安装时的严重风险确认；主动读取留审计
代理密码进了插件日志或错误信息	E6 的遮蔽；插件评审时检查
ExtractUsage 等回调开始带凭证后，原本不需要凭证的代码路径也持有明文	只对声明了该账号类型的插件填充，和 BuildUpstreamRequest 同一条件
所有声明账号类型的 gRPC 插件都会拿到代理密码，包括用不上它的	接受；插件评审时检查不落日志、不外传
协议兼容	只加字段和接口，不改不删；GetAccountCredentials 过渡期保留
14. 需要先确认的三点
范围是否只限自己的账号类型。推荐：是（11.1）。如果要让某类插件（比如统计、调度类）看到所有账号的凭证或代理，那才需要一个真正的授权，另行设计。
要不要禁止未验证发布者的包声明账号类型 已定：先不限制。
E9 做不做。现在授权换令牌和火山素材库请求不走账号代理，这是现状而不是新问题。做的话这些插件改网络代码并升版本。推荐：claude_oauth、codex 做，volcengine 视需要。
