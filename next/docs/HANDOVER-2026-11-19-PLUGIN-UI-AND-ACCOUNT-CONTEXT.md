# sup2api `next/` 工作交接（2026-11-19 暂停时的状态）

> 这是 2026-11-19 暂停时的快照，用于交接；工作恢复并做完后应删除本文件和 `next/docs/handover-2026-11-19/` 目录（先例：仓库里其它 `HANDOVER*.md` 都是当时的快照，不能代替当前状态）。
> 配套材料在 `next/docs/handover-2026-11-19/`：`tasklog.md` 是这几天的逐条流水账（每个决定、每个子代理、每次合并；原始记录，未整理，其中关于"提交署名行"的早期说法已在后文更正），其余是各任务包的任务说明。本文与流水账不一致时，以 `git` 的实际状态为准，其次是本文。
> 文中的工作树路径（`/home/cvp/rc/sup2api/.claude/worktrees/…`）只在原来那台开发机上存在；在别处接手时只看分支名即可。

---

## 0. 一页概览

**这批工作是什么**：所有者提出的两项插件机制改动，加上若干附带项，计划随尚未发布的核心 **0.1.304** 一起出。

| 事项 | 状态 |
|---|---|
| 问题二：账号上下文统一并交给插件（凭证、设置、代理随每次调用给声明该账号类型的插件；移除旧的两个账号授权与旧接口） | **完成，已合入本地 `develop`** |
| 问题一：插件界面只用代码写（不保留表单 JSON；通用校验框架；18 个插件全部迁移） | **主体完成，在功能分支 `feat/plugin-ui-code-only` 上；收尾包做完未审查、未合并；文档包、终审、合回 develop、总验证未做** |
| 附带：建账号前拉模型时把表单代理交给插件 | 完成，已合入 `develop` |
| 附带：AC42 端到端用例（上游成功但没出图） | 已合入 `develop`，**用例本身还没实际跑过** |
| 附带：审核题集三条规则保存时拒绝 | 做完，在分支上，**未审查、未合并** |
| 附带：密钥不限格式 + 地址规则（`%2F`、版本后缀） | **只有设计文档**，方案 A/B 待所有者选，未实现 |
| 附带：删除 growth 插件；清理火山引擎 2 个账号的陈旧键 | 所有者已同意，**未开始** |

**暂停时全部在本地，没有推送，没有发布（推送安排见 13.5）。** 本地 `develop` 比 `origin/develop` 多 565 个提交、比 `upstream/develop` 多 616 个提交（含这批之前就没推的工作）。推送和发布都必须先问所有者。

**接手后第一步**：读第 2 节的状态表，然后按第 6 节的顺序往下做。

---

## 1. 背景

- 仓库 `/home/cvp/rc/sup2api`，主检出在 `develop`。新平台代码全在 `next/`（Go 1.27 多模块工作区：`next/server` 核心、`next/sdk` 插件 SDK、`next/plugins/*` 18 个第一方插件、`next/gateway`、`next/web` 控制台、`next/e2e`、`next/deploy`、`next/tools`）。`next/` 之外是旧版 Sub2API，只作参考。
- 插件是独立进程（go-plugin gRPC）。**平台插件严禁联网**，所有出站只经核心的 HTTP/SOCKS 客户端。
- 所有者给的原始设计稿（另一台机器上写的，已原文存档）：`next/docs/handover-2026-11-19/owner-design-draft.md`。两个问题都出自它。
- 仓库规则在根目录 `AGENTS.md`，必读。行为约定都写在 `next/docs/CONTRACTS.md`（按 § 编号）。

---

## 2. 当前状态表

### 2.1 分支与工作树

工作树都在 `/home/cvp/rc/sup2api/.claude/worktrees/` 下。

| 分支 | 工作树 | 头 | 内容 | 审查 | 合并去向 |
|---|---|---|---|---|---|
| `develop` | 主检出 | `e5c18571c` 之上只多交接文档的提交 | 已合入：账号上下文、表单代理、AC42，以及更早的上游插件/生图/计费等 | 各项均已审查 | — |
| `feat/plugin-ui-code-only` | `ui-code-only` | `ef1413472` | 问题一的主干：校验框架、核心/SDK、控制台挂载、构建与 CI、18 个插件迁移、公共接口补缺 | 各包均已独立审查并修复 | 最终合回 `develop`（落后 develop 6 个提交，见 6.6） |
| `feat/plugin-ui-d1a` | `ui-d1a` | `775028cb6` | 收尾包第一路：删除全部 JSON Schema / 声明式路径，`apikey.Spec` 总是拒绝未知键，界面包升 4.0，e2e | **未审查** | → `feat/plugin-ui-code-only` |
| `feat/plugin-ui-d1b` | `ui-d1b` | `2962eb1f0` | 收尾包第二路：修 CI 脚本、dynweight 不稳定用例、插件前端工程样板由 preset 承接 | **未审查** | → `feat/plugin-ui-code-only` |
| `feat/plugin-ui-jev` | `ui-jev` | `e68c5c8a1` | 审核题集三条规则保存时拒绝 | **未审查** | → `feat/plugin-ui-code-only` |
| `feat/plugin-key-url-rules` | `ui-keyurl` | `701df0d01` | 只有设计文档 `DESIGN-2026-11-20-KEY-AND-BASE-URL-RULES.md`（方案 A、B 并列） | 方案待所有者选 | 实现后 → `feat/plugin-ui-code-only` |
| `feat/upstream-plugins` | `upstream-plugins` | `9c31f9233` | 更早的集成分支，内容已全部在 `develop` | — | 工作树可删 |
| `feat/host-kv-batch`、`docs/sdk-account-requests` | `agent-abf352a42724d5cec`、`sdk-acct-conn` | — | **不是这批工作的**，别的会话留下的，不要动 | — | — |

已合并、工作树已删但分支保留的：`feat/plugin-account-context`、`feat/acct-form-proxy`、`feat/up-p9`、`feat/plugin-ui-{k1,k2,t1,k1-fix,g1,g3,g4,g5,g6,u3,u3x,u3y}` 等。

### 2.2 三个"做完未审查"的分支要注意什么

见第 5 节。**这三个分支的结论都只是实现方自报，没有经过独立审查**；按工作流程（第 10 节）必须先审查、修复，再合并。

---

## 3. 已完成并合入 `develop` 的工作

### 3.1 问题二：插件账号上下文（合并提交 `4422fb554`）

- 设计：`next/docs/DESIGN-2026-11-19-PLUGIN-ACCOUNT-CONTEXT.md`；契约：`CONTRACTS.md` **§124**（另 §123 账号出口、§117 代理票据）。
- 规则：核心调度到哪个账号，就把那个账号的信息（凭证、设置、这次实际使用的代理 `Account.proxy`）随调用交给**声明该账号类型的插件**。转发、账号测试、额度与余额查询、凭证续期、拉模型列表各入口一致。代理信息只是数据，联网仍只由核心做。
- "同源"：插件看到的代理必须就是这次请求实际走的那条。WebSocket 后续轮次给的是建连时那条连接的代理。出口取不到时不调用插件、不走直连。
- 移除（不留过渡）：`accounts.credentials`、`accounts.read` 两个授权和 `GetAccountCredentials` 接口；旧清单里还申请它们的包按 `outdated_contract` 拒绝；迁移 `0116_drop_account_grants.sql`。
- 新增：`HostService.GetAccount`（只能读自己声明的账号类型，先写审计 `plugin.account.read`）、`pluginsdk.Host.Account`；`core.ProxySpec/ProxyRef` 带 `Format`，任何打印格式都遮蔽密码。
- 已知边界（已告知所有者，保持现状）：有 `db.schema` 的插件能读账号/代理表里未加密的列；`GetEnvironment` 不鉴权（有意）；`AccountScopeCheck` 不再需要授权。

### 3.2 建账号前拉模型时把表单代理交给插件（合并提交 `2db82ebf4`）

- 所有者原话："代理这时候已经填进去了就用 没填进去就不用"、"调用的时候账号信息发给他就行了嘛"、"就和调用api的时候一样"。
- 表单里选了已有代理或粘贴了代理串：请求经它发出，同一条代理交给插件（粘贴的没有入库，`id` 为 0，含密码）；没填：直连，代理为空；代理不可用：在调插件前失败，不直连。契约 §124.2。

### 3.3 AC42 端到端用例（合并提交 `e5c18571c`）

- 守的是所有者的决定"上游成功但没出图也按成功返回，上游是什么就是什么"（Codex 生图转换，设计 `DESIGN-2026-11-18-CODEX-IMAGES.md`，契约 §121）。
- 审查时补了"账号没有被记失败/冷却"的断言。`next/e2e` 能编译、`go vet` 通过；**用例本身没实际跑过**，要在总验证里跑：`sh next/deploy/e2e/test.sh -run '^TestAC42_CodexImages$'`（需 e2e compose 项目已启动、有 `next/deploy/e2e/.env`）。

### 3.4 更早合入的（背景，不用再动）

账号测试按协议（§122）、使用记录附加用量与主用量金额（§120）、代理跟随与票据（§117、§123）、Codex 授权后续、Codex 生图（§121）、上游插件集成（§119）。迁移已到 0116。

---

## 4. 功能分支 `feat/plugin-ui-code-only` 上已完成的工作（问题一）

设计：`next/docs/DESIGN-2026-11-19-PLUGIN-UI-CODE-ONLY.md`（在功能分支上；第 2 节是所有者原话，第 11 节是任务拆分，9.2 是删除清单，第 10 节是逐插件迁移表）。

所有者的决定（原话）："我们就直接不用声明式 UI 了，直接只用 SDK 提供的相关组件来做。"、"不保留json 字段校验前端封装一个通用的校验框架出来 参考主流ui框架的校验逻辑"、"一步做完"、未验证的第三方插件没有 native 界面"接受"；iframe 模式保持原样。

| 包 | 内容 | 状态 |
|---|---|---|
| U1、U2 | `@sub2api/ui` 校验框架与组合组件：`next/web/packages/ui/src/form/`（`rules.ts`、`path.ts`、`useForm.ts`、`SForm.vue`、`pluginSettings.ts`、`apiKeyForm.ts`、`credential.ts`）；`useForm`/`FormApi`、`SField prop`、`SSecretInput`（`clearable`、`plain`）、`SUrlPresetInput`、`useCredentialForm`/`SCredentialForm`、`createApiKeyForm`、`usePluginSettings`/`SSettingsPage`、`SResourcePage`；`@sub2api/host` 的 `useGuardedSettings`、`toFieldPath`、`settingsFieldErrors` | 已审查两轮，已合入 |
| K1 + K1 修复 | 核心与 SDK：`ui.settings.secrets`（键不存在 = 全部掩码；`[]` = 没有密钥；数组路径整体掩码）、可选 RPC `PluginService.ValidateSettings`（带授权）、`pluginsdk/fieldcheck`、`apikey.Spec.Fields`、`accountform.Form.Only` | 已审查并复审，已合入 |
| K2 | 控制台挂载 native 设置页/组件；`has_settings` 只对 schema、native 为真（iframe 设置控制台本来就不渲染） | 已合入 |
| T1 | 构建与 CI：`next/deploy/docker/build-go.sh`、`build-ui.sh`、`.github/next-ci/next-check-plugin-ui.sh`、`next-ci.yml` | 已合入；**CI 脚本有个拦不住失败的错，D1b 在修** |
| G1 | anthropic、gemini、typesafe、openai、relay、newapi、sub2api、azure-openai 的账号表单 | 已审查，已合入 |
| G3 | bedrock、vertex 的账号表单 | 已审查，已合入 |
| G4 | volcengine 账号表单与素材库页面 | 已审查，已合入 |
| G5 | mailer、codex、growth、guard 的设置表单 | 已审查，已合入 |
| G6 | moderation 设置页（39 个设置项）；删掉了 `tools/settings_cleanup.py` 的 `clean` 子命令（它在无 schema 的版本上会把线上审核设置清空） | 已审查，已合入 |
| U3a | 公共接口补缺：`SFormSection`（说明与折叠）、`nativeAccountFormProps`、`SDateTimeInput`、`bindNumber`、`ExtraField.mono`、`@sub2api/ui/testing`（只能在 spec 里引用）；SDK：`NormalizeBaseURL` 拒绝空白/`?`/`#`、`Spec.Fields` 带规则（对将要保存的值）、`apikey/apikeytest`、`fieldcheck.DecodeSettings`、`pluginsdktest.SettingsSecrets` | 已审查，已合入 |
| U3b | 15 个插件换用上述接口；补齐迁移时漏掉的"拒绝未知键"（vertex、bedrock、volcengine）；统一地址规则；openai 去掉包装（azure 保留，见下） | 已审查，已合入 |

**最后一次整分支验证（头 `ef1413472`）**：SDK、核心的插件与账号模块、打包工具、18 个插件的 Go 测试全过；`next/web` 类型检查通过、2536 个测试全过、lint 0 错误；17 个插件前端工程里 16 个全过，剩下 1 个是 dynweight 一条间歇超时的用例（D1b 在查）。**没有跑过带 `-race` 的全量、`next/web` 的 build、整套 e2e、`check.sh plugins`。**

几个有意保留的差异（都经过审查确认，不是缺陷）：
- azure-openai 保留包住 `standard.Platform` 的包装：端点规则要看"规范化但不剥后缀"的地址，并带专门的提示文案，放不进 `Spec.Fields`。
- moderation 的 `Configure` 对已存的越界值/退役键保持宽松（只记不含值的警告、把值夹回范围），其它插件的 `Configure` 与 `ValidateSettings` 共用同一个严格函数。
- moderation 没用 `usePluginSettings`/`SSettingsPage`：七个设置页共存，需要"只写本页改过的键"和"读入时转换旧值"。
- codex 的账号类型从来没有"拒绝未知键"的规定，所有者决定不加。

---

## 5. 做完但未审查、未合并的三个分支

### 5.1 `feat/plugin-ui-jev`（`e68c5c8a1`）审核题集三条规则保存时拒绝

- 所有者决定：选 A（保存时拒绝）。
- 改动：同一类别出现两次（`items/<i>/category` `duplicate`）、自定义类别没填名称（`…/label` `required`）、问题只有空白（`…/question` `required`）。`checkJevSet` 与运行时的 `JevQuestions.Validate` 共用判断；`Configure` 仍宽松。
- 实现方没按原话做的一处：错误显示在设置页"题集"那一行并注明"第 N 题"，没有做到在编辑器的具体格子里显示（`JevQuestionsEditor` 与审核器编辑弹窗共用，逐格显示要给共享组件加参数）。**审查时判断是否可接受。**
- 它改了 `CONTRACTS.md` §72.10，合并时可能冲突。
- 后果要告诉用户：库里已存着违反这三条的题集时，之后在任何设置页保存都会被拒，直到把题集改对。

### 5.2 `feat/plugin-ui-d1a` 收尾包第一路

任务说明：`next/docs/handover-2026-11-19/brief-d1.md`（一、二、三、五节）。**结果与审查要点见 13.1。**

审查重点：
- 删核心的 schema 校验之后，各插件的 `ValidateCredentials` 是保存账号时唯一的把关——全部第一方账号类型都要拒绝未知键且允许键齐全（U3b 审查已逐类核对过一遍，D1a 改了 `Spec` 的语义后要再核一遍）。
- 已安装的旧包（schema 模式、native+schema、声明式页面）不能让核心起不来；它们的表现以设计文档第 7 节为准。
- iframe 路径保持原样。
- e2e 里 AC04、AC18、AC20、AC23 等改成断言新行为，不是简单删断言。
- 它不重新生成各插件的 lockfile（与 D1b 的分工），升 4.0 后个别插件 `npm ci` 可能要等合并 D1b 后才过。

### 5.3 `feat/plugin-ui-d1b` 收尾包第二路

任务说明在派给它的消息里（要点见 `next/docs/handover-2026-11-19/tasklog.md` 末尾几节）。三件事：
1. `.github/next-ci/next-check-plugin-ui.sh` 把 `set -e` 放在 `if ( … )` 子 shell 里，测试失败仍报通过、退出码 0。必须修。
2. `next/plugins/dynweight/ui/native` 的 `permissions.spec.ts > DegradedBadge > asks nothing and stays hidden without account:read` 间歇超时；要求查根因，不许用调大超时掩盖。
3. 17 个 `ui/native` 的配置样板由 `@sub2api/vite-preset` 承接。要求先核实不破坏第三方作者在仓库外独立构建，没把握就停在评估。

**结果与审查要点见 13.2。**

**合并顺序建议**：D1b 先（它删 lockfile、改工程配置），再 D1a（它改版本号与清单），最后 Jev；每次合并后跑第 9 节的验证。

---

## 6. 后续工作（按顺序）

### 6.1 审查并合并 5.1–5.3 三个分支
每个分支派一名独立审查员（只审查不改），按六条规范出"必须改 / 建议改 / 可不改"，交回原实现方或新的子代理修复，再由合并员合入 `feat/plugin-ui-code-only`。

### 6.2 密钥与地址规则（所有者要求"一起改了"）
- 设计文档：`next/docs/DESIGN-2026-11-20-KEY-AND-BASE-URL-RULES.md`（分支 `feat/plugin-key-url-rules`）。
- 所有者的决定（语音输入，有错字，按意思理解）：
  - 密钥："密钥不要做任何限制，因为用户可以自定义 base url 和密钥，不要对格式有任何要求。密钥的长度放宽到二零四八字符，或者说直接就字符串吧，因为其他的密钥可能会很长。" → 只要求非空字符串，保存时去首尾空白。
  - `%2F`："第四个你也规划一下，一起改了" → 存下的地址保留转义形式。
  - 版本后缀："我们的插件自己处理兼容，不要去多也不要少。没有就加上，有就不管，应该是这样的，[new-api]和[Sub2API]原来的代码都是这样的"。
- **待所有者选的方案**：
  - 方案 A（字面规则）：地址已以本路由的版本段结尾就不加，否则加完整路径。会让现在能用的常见填法失效：newapi/sub2api 填 `/v1`（Gemini 路径与余额查询坏）、gemini 填 `/v1`、volcengine 密钥类填 `/api/v3`、azure 部署类填 `/openai/v1`、没有自定义权限的人提交带版本的官方地址被拒；部分类型的已存地址请求路径会变，需要查生产库。
  - 方案 B（协调者建议，设计者已验证并写进文档 5.5、6.4、6.5、7.1）：保存时不剥；请求时把地址末尾**至多一个**本类型声明的版本段（整段、区分大小写、按转义形式、取最长）当作版本去掉，再接上路由的完整路径。验证结论：没有上述倒退；**已存地址对所有类型全部等价，不需要查生产库**（前提是存值都经过了插件的保存校验）；`…/v1/v1` 这类站点能配通；`…/a%2Fv1` 不会被当成以 `/v1` 结尾。SDK 里只要三件事：删掉保存时那次剥离、保留转义、`FromAccount` 用 `Versions`（原 `StripSuffixes`）算出根；`standard` 的拼接代码与 golden 的 execute/test/models 用例不变。
  - 选 B 时还要所有者确认一处契约改动（文档 Q3）：没有自定义地址权限的人提交带版本的官方地址，要让它继续通过，核心的受限设置比较（`next/server/internal/account/creds.go` 的 `checkGuarded`、`guardNorm`）得先去掉至多一个版本段；核心看不到插件代码里的 `Versions`，建议在清单的 `GuardedSetting` 加可选字段 `versions`（涉及 `sdk/manifest/manifest.go`、`check/account_check.go`、五个插件清单、`CONTRACTS.md` §21.3、插件 `hostCompat` 下限，并加测试保证清单的 `versions` 与 `Spec.Versions` 一致）。备选：把带版本的地址加进 `allowed`（缺点：允许值变两项，表单从锁定变成下拉）。
  - 选 B 仍有的一个小缺陷（文档 Q11，建议接受）：azure Foundry 项目名恰好叫 `openai` 时，`…/api/projects/openai` 的末尾会被当成版本段；现在也是这样；线上没有 azure 账号。
- 还要所有者确认：bedrock 的 `api_key`（另一套代码，现限 8–2048 位可见字符）要不要一起放开（建议放开；AK/SK、vertex 密钥文件不动）。
- 已定（设计者建议、协调者同意）：密钥里的控制字符不加检查（SDK、核心、Go 网络库三处都会拒发，无请求头注入；代价是这种密钥能保存但每次请求失败）；volcengine relay 不动；只认类型自己声明的版本段。
- 调查里与所有者印象不同的事实：new-api 转发时并不做"有就不加"（地址与固定路径直接拼，只在界面提示别带 `/v1`）；旧版 Sub2API 只有 OpenAI 一类做了；本仓库 moderation 的 `joinURL` 才是所有者描述的规则。
- 任务拆分在设计文档第 10 节（按方案 B）：T1 SDK 与 T2 核心受限地址（契约）先做、可并行 → T1 合入后并行 T3 简单插件（含清单 `versions` 与一致性测试）/ T4 azure / T5 volcengine / T6 `@sub2api/ui` 与 vertex（bedrock 等所有者确认后）→ T7 文档、插件版本与 `hostCompat`、CHANGELOG。选 A 时的差异写在第 10 节末尾。**实现要等 D1a 合入**（都改 `next/sdk/pluginsdk/apikey/apikey.go`）。
- new-api 是 AGPL，只能作行为参考，不能复制代码。

### 6.3 删除 growth 插件 + 清理火山引擎陈旧键（所有者已"同意"）
- growth 是什么、来历、删除牵连面：见 `next/docs/handover-2026-11-19/tasklog.md` "growth 调查结果"一节。要点：2026-10-05 由一个子代理生成后合入，没有需求记录；仓库记录显示线上从未安装（`CHANGELOG.md:752`）。
- 删除做法（先例：claude-oauth，`CONTRACTS.md` §116）：删 `next/plugins/growth/`、`next/go.work:13`、`next/plugins/released.json` 的条目；`next/server/internal/plugin/install/reserved.go` 里把 `growth` 从 `reservedKeys` 移到 `removedKeys` 并记核心版本；改 `next/server/internal/authz/plugin_catalog_test.go:188-241`（它读真实的 growth 清单）；`CONTRACTS.md` §45 标"已删除"，及几处文档里的插件清单与计数；`CHANGELOG.md`。
- **发布前必须确认线上现在没有安装 growth**（所有者没有授权再读生产库，留到发布前例行检查）；若装过，补一步清理数据的迁移。
- 火山引擎：生产上 2 个 `volcengine`/`apikey` 账号的 `settings` 里有 `video_api_prefix`，这个类型不允许也不读它，编辑凭证时会被拒且界面删不掉。加一步核心迁移（下一个迁移号；`develop` 已到 0116，注意功能分支上有没有新增）：删除 `plugin_key='volcengine' AND type='apikey'` 的账号 `settings` 里的 `video_api_prefix`、`api_prefix`。
- 两件事可以合成一个小包，排在 D1a 合入之后。

### 6.4 文档包 D2
`CONTRACTS.md` §21.3、§23、§75.5 及 :2372、:4012、:4088、:4780 里 moderation 的 schema/`writeOnly`/`x-sensitive` 表述；`COMMON-CONTROLS`、`PLUGIN-DEVELOPMENT`；D1a 汇报里列出的"留给 D2 的位置"。设计文档任务包 D2 一节。

### 6.5 整个功能分支的最终审查
按六条规范（抽象、复用、解耦、低嵌套、简洁、低侵入）过一遍 `develop..feat/plugin-ui-code-only`。

### 6.6 合回本地 `develop`
功能分支落后 `develop` 6 个提交（表单代理、AC42）。预计冲突：`CONTRACTS.md` §117/§124、`CHANGELOG.md`。

### 6.7 按 CI 标准的总验证（合回之后）
- 各模块 Go：`gofmt`、`go vet`、`go test -race -count=1 ./...`（`next/server`、`next/sdk`、`next/gateway`、`next/tools/sub2api-plugin`、18 个插件）。
- `next/web`：`npm ci && npm run typecheck && npm test && npm run lint && npm run build`。
- 插件前端：`.github/next-ci/next-check-plugin-ui.sh`（修好之后）。
- 整套 e2e：`next/deploy/e2e/`（含 AC42、AC43，以及 D1a 改过的 AC04/18/20/23）。
- 插件打包检查：`check.sh plugins` 与 `realcore`（在没有 node 的容器里跑，**必须先 `build-ui.sh` 构建插件界面**）。
- 不推送、不发布，除非所有者发话。

### 6.8 发布前检查清单（发布要所有者明确授权）
- 线上没有安装 growth（或已处理）。
- 线上没有非第一方的 schema 模式插件（它们升级后没有 native 界面）。生产库账号表里出现过的 `plugin_key`：anthropic、gemini、openai、relay、volcengine，以及已下线的 ccgateway（104 个有意保留的孤儿账号）。
- 非内置插件（relay、guard、payment、azure-openai、vertex、bedrock）要管理员重新安装新版本；写进升级须知（设计文档 7.3）。
- 审核题集、越界设置值：升级后第一次保存可能被拒，提前告知。

---

## 7. 所有者做过的决定（按主题；原话加引号）

**通用规则（一直有效）**
- 回复用中文；任务都交给子代理做，主会话只协调；子代理用 Opus（"子代理用opus5.5就行了"）。
- 流程：设计文档 → 任务细分 → 子代理小组实现 → 独立审查（六条规范）→ 修复 → 合并。
- 不要擅自加硬编码的超时、上限、防御性检查、默认值、兜底；不写兼容/过渡代码。
- 推送前先问；不发布；只支持 Linux；new-api 是 AGPL，只作行为参考。
- "上游是什么就是什么"：上游成功就按成功返回和记录，核心不自己合成状态码，不因此罚账号；上游模型名记上游回显的。计费只按客户端请求的模型取价，缺价格是错误；账号测试不计费。
- 代理：配了代理的账号绝不直连、绝不泄露服务器 IP，代理失败不兜底；没配代理就直连；已建立的 WebSocket 上游连接不因改代理而断开。
- "还是要通过核心的http /socks 客户端去连接，谢谢 ，插件严禁联网！！！！"
- 上游报的数值不折算。

**这一批**
- 问题一："不保留json 字段校验前端封装一个通用的校验框架出来 参考主流ui框架的校验逻辑"、"一步做完"、"接受"（第三方插件没有 native 界面）。
- 问题二："调度的那个账号 让插件执行 ，就给哪个账号 ok？"；旧的两个权限和旧接口："移除"。
- "两个 问题 纳入本次修复范围 进行改"。
- 表单代理："代理这时候已经填进去了就用 没填进去就不用"、"就和调用api的时候一样"。
- 残留测试环境："清理残留环境哦"（已清）。AC42："合进来有好处嘛，那就合进来呗。"
- 八项待定事项（2026-11-19）：
  1. 审核题集三条规则：选 A（保存时拒绝）。
  2. 密钥不做任何格式限制（原话见 6.2）。
  3. growth："我们目前的新版本 Subtool API 没有这个功能吧？那就应该删掉吧这个东西，这是啥？" → 查清后所有者回"同意"（删除）。
  4. `%2F`：规划并一起改。
  5. 版本后缀："没有就加上，有就不管"（原话见 6.2）。
  6. codex 不加"拒绝未知键"。
  7. 已署错的历史提交："不管"。
  8. 只读查一次生产库：已授权并已查（结果见第 11 节）。**授权只够那一次**，再查要重新授权。
- 清理火山引擎陈旧键："同意"。

---

## 8. 还没定的事

| 事项 | 建议 |
|---|---|
| 地址规则选方案 A 还是 B（见 6.2） | B |
| 选 B 时，清单 `GuardedSetting` 加 `versions` 字段这处契约改动（见 6.2） | 同意 |
| bedrock 的 `api_key` 是否一起放开格式限制 | 放开 |
| 发布前要不要再只读查一次生产库的插件列表（确认没装 growth） | 查，或留到发布例行检查 |
| 审核题集错误只显示到"第 N 题"而不是具体格子，是否接受 | 审查后再定 |
| 何时推送、何时发布 0.1.304 | 所有者决定 |

已知但所有者没有要求处理的原有问题（不在这批范围，除非另行指示）：
- openai 前端密钥规则比后端宽——实现 6.2 后自然消失。
- growth 没保存过设置时实际全关——删除 growth 后消失。
- 某个旧账号的**加密凭证**里若有多余键，编辑凭证会被拒且界面删不掉（旧表单时期同样）；设置字段已查过，凭证是加密的查不了。

---

## 9. 环境与测试方法（原开发机；别处接手时按那里的环境来）

- 本机没有 Go，也没有全局 node。磁盘 160G，cvp 与 erio 两个用户共用，经常紧张（暂停时可用约 16–18G）；子代理的约定是可用低于 6G 就停。
- **Go**：
  - 不需要数据库：`gotest.sh`（原开发机上在 `/home/cvp/rc/sup2api/.claude/worktrees/`，副本见配套材料）：`gotest.sh <工作树路径> <next 下的目录> go <参数...>`，例如 `gotest.sh /home/cvp/rc/sup2api/.claude/worktrees/ui-code-only plugins/bedrock go test -count=1 ./...`。要带 `go` 这个词；不接受 `-e`。
  - 需要数据库：同目录 `pgtest.sh <工作树路径> <next 下的目录> go <参数...>`，自动起临时 PostgreSQL（tmpfs）与 Redis、跑完清理；`export PGTEST_PREFIX=<前缀>` 给容器起名。
  - 两个脚本自己包了 `sg docker`，直接调用，不要再套一层。
  - 已知与负载有关的不稳定测试：`TestClientDisconnectAfterFinalUsageIsServed` 等几条；`internal/config` 的 `TestConfigSurvivesEnvScrub` 在测试库密码含 `pw@` 时误报（`pgtest.sh` 已避开）。
- **node**：`export PATH=/home/erio/.nvm/versions/node/v26.13.0/bin:$PATH`。插件 `ui/native` 的类型检查与测试依赖 `next/web/node_modules`，所以先 `cd next/web && npm ci`。跑完一个工程就删它的 `node_modules`/`dist`。CI 用的 node 版本看 `.github/workflows/next-ci.yml`。
- **proto**：`buf` 与 protoc 插件在 `/home/cvp/go/bin`。
- **e2e**：`next/deploy/e2e/`（compose 项目、`test.sh`）；Playwright 镜像 `mcr.microsoft.com/playwright:v1.56.0-noble`。
- **磁盘回收**：可以删自己的 `~/.cache/go-build` 里长时间没用的条目；所有者批准过一次 `docker builder prune -f`，**不是长期授权**；不要动别人的容器、在用的卷、测试镜像、`agent-teams-gomod-cvp` 卷。
- **生产**（主机别名 `ovh`）：默认只读，任何读写都要所有者当次明确授权；重复导出、取主密钥会被权限系统拒绝，被拒不要绕。发布流程见 `next/deploy/gateway/ovh/README.md`。

---

## 10. 工作方式与踩过的坑

- **提交身份**：用仓库历史上的作者身份（`git log -1 --format='%an <%ae>' develop` 可查），命令形如 `git -c user.name=… -c user.email=… commit`；标题英文 `<scope>: <lowercase sentence>`；`git add` 写明路径，不用 `-A`。
- **提交结尾的署名行**：各会话按**自己的**署名设置写，不要规定子代理写别的模型名。之前协调会话误把自己的设置当成"所有者定的仓库约定"强加给子代理，造成两天里一批提交署名与实际不符、四个子代理拒绝提交；所有者已表示历史不用改。
- **压缩摘要里的"规则"要查原话**：上面这件事就是摘要把会话设置写成了用户规则。转述"所有者规定"之前，没见过原话的先到会话记录里查。
- **被权限系统拒绝的操作**：不绕过、不换个会话代做，原样报给所有者。子代理自己决定不做的事（没有被拒）可以另行安排。
- **子代理不要用 `git stash`**（工作树共用同一个 stash 栈）。
- **审查要独立**：审查员只审查不改；审查结论里"必须改"的交回实现方或新子代理；合并前至少跑受影响模块的测试。几次关键问题都是审查或合并时才发现的：清单没写密钥列表导致明文读回、清理脚本会清空线上审核设置、迁移漏掉"拒绝未知键"、CI 脚本拦不住失败。
- **并行分工要划清文件范围**：D1a 与 D1b 就是按文件划的（版本号与清单 vs 工程配置与 lockfile）。
- **等价性靠对比测试**：迁移每个表单时，先让新旧实现吃同一组输入对比，再把输入与期望值写死成永久测试。

---

## 11. 生产库只读查询结果（2026-11-19，所有者授权的一次；只看键名与数量，没看任何值）

- 账号数：anthropic/apikey 15、gemini/apikey 2、openai/apikey 2、relay/relay_key 2、volcengine/apikey 2、volcengine/relay 2、ccgateway/apikey 1、ccgateway/managed 103。
- 账号 `settings` 里出现的键：anthropic `base_url`×9；gemini、openai、relay 各 `base_url`×2；volcengine relay `base_url`×2、`video_api_prefix`×2；**volcengine apikey `base_url`×2、`video_api_prefix`×2（后者是陈旧键，见 6.3）**；ccgateway 的 `account_email`、`locale`、`timezone`（已下线插件的孤儿账号，无关）。
- 没有 azure、newapi、sub2api、typesafe、bedrock、vertex、codex 的账号。
- 没有查：地址的具体值、插件安装列表、加密的凭证。

---

## 12. 文档与文件索引

**仓库内（设计）**——`next/docs/`：
- `DESIGN-2026-11-19-PLUGIN-UI-CODE-ONLY.md`（问题一；在 `feat/plugin-ui-code-only`）
- `DESIGN-2026-11-19-PLUGIN-ACCOUNT-CONTEXT.md`（问题二；在 `develop`）
- `DESIGN-2026-11-20-KEY-AND-BASE-URL-RULES.md`（密钥与地址；在 `feat/plugin-key-url-rules`）
- `DESIGN-2026-11-18-CODEX-IMAGES.md`、`…-UPSTREAM-PLUGINS-AND-BALANCE.md`、`…-PLUGIN-FORWARDING-V2.md`、`…-ACCOUNT-PROXY-TICKET.md`、`…-PLUGIN-DB-CORE-USER.md`、`…-PLUGIN-NETWORK-ACCESS.md`、`…-PLUGIN-SANDBOX-FS.md`、`…-PLUGIN-PACKAGE-ANY-NODE.md`（更早的相关设计）

**仓库内（契约 `next/docs/CONTRACTS.md`）**：§21.3 受限设置；§23 界面契约（§23.4 写法约定、§23.5 native 账号表单接口）；§45 growth；§72.10 moderation 题集；§75 插件设置（掩码、`secrets`、`ValidateSettings`）；§113.3 插件不直连；§116 claude-oauth 删除先例；§117 代理票据；§119 上游插件；§120 附加用量；§121 Codex 生图；§122 账号测试按协议；§123 账号出口；§124 插件账号上下文。

**仓库内（其它）**：根 `AGENTS.md`；根 `CHANGELOG.md`（未发布段是 0.1.304 的内容）；`next/web/packages/ui/README.md`（组件与测试辅助的用法）；`next/plugins/released.json`（已发布版本；改了插件内容要过 `tools/sub2api-plugin` 的 `TestReleasedPluginsBumped`）；`next/deploy/gateway/ovh/README.md`（发布记录与流程）；`next/deploy/e2e/README.md`（e2e 用例清单）。

**配套材料（`next/docs/handover-2026-11-19/`）**：
- `tasklog.md`：流水账（约 16 万字节，从后往前读最有用）。
- `owner-design-draft.md`：所有者原始设计稿全文（两个问题的出处）。
- `brief-ui-migration.md`：各迁移包的共同任务说明（9 步、规则、验证、汇报格式）。
- `brief-u3.md`、`u3a-interfaces.md`：补缺包说明与新接口换法。
- `brief-d1.md`：收尾包说明（含累积的删除清单）。
- `gotest.sh`、`pgtest.sh`：原开发机上跑 Go 测试的包装脚本（那台机器没有本机 Go，用 golang 镜像跑；路径是那台机器的，别处有 Go 就不需要，仅供参考）。

**截图**（只在原开发机上）：`/home/cvp/up-billing-shots/`（附加用量公式中英文、native 设置页）。

---

## 13. 暂停时的最终状态（2026-11-19 收尾）

所有子代理都已结束。相关工作树全部干净（`git status` 为空），没有残留的测试容器、依赖目录；没有推送任何东西。下面三项的结果**都是实现方自报，没有经过独立审查**。

### 13.1 `feat/plugin-ui-d1a`（头 `775028cb6`，基于 `ef1413472`，6 个提交，166 个文件，+788 / −4647）

| 提交 | 内容 |
|---|---|
| `e8d261f55` | `apikey.Spec.Fields` 为 nil 时也拒绝未知键；新增 `Spec.NoBaseURL`（本类型没有地址字段），`vertex_apikey` 用它，去掉 vertex 里多包的那层 `form.Only` |
| `496b9e233` | 核心、`sdk/manifest`、打包工具：删除 schema 表单与声明式页面 |
| `e45f744a7` | 控制台删除声明式渲染；`@sub2api/ui`、`@sub2api/host`、`@sub2api/vite-preset` 升 4.0.0；17 个带界面的插件 `hostUICompat` 改 `^4.0`（payment 没有界面） |
| `87be4233a` | e2e 用例按新契约改 |
| `5edc0d1d6` | CONTRACTS 里与代码强绑定的几条、CHANGELOG 未发布段、`check.sh` 说明 |
| `775028cb6` | 两处 SDK 测试跟上"总是检查未知键" |

- 自报验证：`next/sdk`、`next/server`（64 个包）、`next/tools/sub2api-plugin`、18 个插件的 `gofmt`/`go vet`/`go test` 全过；`next/e2e` 只 `go vet`（整套环境没起）；`next/web` 的 typecheck、test（277 个文件）、lint、build 通过；openai、volcengine、moderation、mailer 四个插件前端在 4.0 下通过。`next/gateway` 没改、没跑。
- 保留没删的：`santhosh-tekuri/jsonschema` 依赖（`accountTypes[].extensionsSchema` 的校验还在用，`next/server/internal/account/extensions.go`）。
- 已安装旧包的表现：能加载；设置全部遮蔽、不返回 schema（`TestOldSchemaSettingsPackage`）；账号表单显示"此插件版本的界面已不受支持，请升级插件"，其余分区照常保存、凭证原样送回；table/form 页面落到已有的"不支持的页面类型"；新上传报 `unsupported`。
- e2e 改了：AC04、AC18、AC20（断言 `form.mode=native` 与组件名、`GET …/form` 返回 404，新辅助 `ExpectNativeForm`）、AC23（改读 `guarded_settings`）、AC03、AC21（只改注释）、AC27（stats 菜单由 table 页改为 iframe 页，包里加 `ui/iframe/stats.html` 并重签）；`DerivedGuardPackage` 同时删掉 native 的 `ui.settings`。
- 控制台部分（约 −2479 行）是它再派的子代理做的。
- **审查要点**：`NoBaseURL` 的语义与命名；直接用 SDK 预设又不写 `Fields` 的第三方插件现在会拒绝多余键（行为变化，是否要写进升级须知）；AC27 改成 iframe 页是否仍在测原来要测的东西；控制台子代理自己做的决定（`SettingsTab` 去掉 `saved` 事件、`ProxyPicker`/`GroupPicker` 恢复默认的属性透传、mock 里 guard 的 `host_ui_compat` 改 `^4.0`）；旧包表现是否与设计文档第 7 节一致；删除清单（设计 9.2、`brief-d1.md`）有没有漏。
- **留给文档包 D2 的位置**（实现方列的）：CONTRACTS 约 22 处（§5.4、§12、§15.8 正文、§20.2、§21.3"表单不设 default"一段、§23、§45.6、§72.10、§75.1、§75.5、§96.7，及 2372/4012/4088/4780 行附近 moderation 的 `writeOnly`/`x-sensitive`）；`COMMON-CONTROLS` §6.3 与第 55 行；`PLUGIN-DEVELOPMENT` 6 处；`ARCHITECTURE` 讲声明式 UI 的部分；`AGENTS.md` 第 80 行（`forms/`）与第 97 行（"目前 3.20"）。
- **待决**：要不要整体 `go work sync`。在这个工作树里跑会把 52 个 `go.mod`/`go.sum` 升到工作区里的更高版本，是分支上原有的漂移，与本包无关，实现方已撤销，只提交了 `next/server` 的 `go mod tidy` 结果。
- **使用提示**：控制台的 mock 现在依赖 anthropic、openai、gemini、relay、volcengine 的 `ui/native/dist`，跑 mock 前要先构建这些插件；`check.sh plugins` 之前先执行 `sh next/deploy/docker/build-ui.sh next <out>`。

### 13.2 `feat/plugin-ui-d1b`（头 `2962eb1f0`，基于 `ef1413472`，7 个提交）

| 提交 | 内容 |
|---|---|
| `9c124139f` | `.github/next-ci/next-check-plugin-ui.sh`：任何一步失败都让该目录记为失败、脚本非零退出，并打印 `::error::<目录>: <命令> failed` |
| `e1a9ec032` | dynweight `permissions.spec.ts` 顶部加 `import '@sub2api/ui'`，让冷编译发生在收集阶段 |
| `f11a7bfd7` | `@sub2api/vite-preset` 新增 `tsconfig.plugin.json`、`vite.plugin.config.js` |
| `9fb8737b7` | `build-ui.sh` 与 CI 脚本：有 `package-lock.json` 才 `npm ci` |
| `2a15dba7b`、`a4eaddc63` | 17 个插件前端工程改用控制台的工具链：只剩 `package.json`、`tsconfig.json`、`src`、`.gitignore`；删掉 lockfile、`.npmrc`、`vite.config.ts`、`vitest.config.ts` 共 66 个文件、约 2.4 万行 |
| `2962eb1f0` | CONTRACTS §23、设计文档第 8 节、ui README |

- CI 脚本：原来 `set -e` 写在 `if ( … )` 的条件里不生效，只要最后一步成功就报通过。实现方用故意弄坏的 guard 用例证明：旧脚本退出码 0，新脚本退出码 1 并指出目录。
- dynweight 间歇超时的根因（自报）：那条用例在测试体内第一次 `import` 组件，触发对整个 `@sub2api/ui` 源码的冷编译（约 6.5 秒），计入 5 秒的单测超时；是否失败取决于别的文件有没有先把它编译过，所以表现为间歇。不是产品代码的问题，也不是这批改动引入的回归（区间内每个提交单跑都超时）。修后单跑 10 次、整套 10 次全过。没有调大超时。
- 配置样板：17 个工程的产物里 16 个与改前逐字节相同；codex 的差异是一个 prop 的类型声明由 `{}` 变成 `{ type: Function }`——原来插件工程解析不了从别的包导入的类型，现在能解析了。Node 24 与 26 的产物一致。`@sub2api/vite-preset` 是私有包、从未发布，第三方作者本来就是按 `file:` 依赖加 tsconfig `paths` 的写法，自带依赖的工程带着 lockfile 仍会先 `npm ci`，所以不受影响。
- 自报验证：`next/web` typecheck、test（279 个文件、2536 条）通过；修好的 CI 脚本逐个检查 17 个工程全过；按 Dockerfile 的方式完整构建一遍通过；`next/tools/sub2api-plugin` 的 Go 测试通过。
- **审查要点**：第三方独立构建的说法是否属实（CONTRACTS §23 的写法）；`tsconfig.plugin.json` 用 `typeRoots`/`types: []` 的理由；插件工程的三个脚本直接引用 `../../../../web/node_modules` 下的可执行文件是否可接受；`next-ci.yml` 的 `cache-dependency-path` 里插件 lockfile 的通配现在匹配不到文件（实现方认为无害，保留）。

### 13.3 `feat/plugin-key-url-rules`（头 `701df0d01`，2 个提交，只有文档）

见 6.2。文档里方案 A、B 并列；设计者验证了方案 B：已存地址对所有类型等价，不需要查生产库。

### 13.4 合并这四个分支时要注意

- 建议顺序：D1b → D1a → Jev →（实现后）密钥与地址规则。
- D1a 与 D1b 的分工：D1a 改版本号与 `manifest.json`，不动工程配置与 lockfile；D1b 相反。两者都改了 `CONTRACTS.md` §23、设计文档、`next/web/packages/ui/README.md` 的不同位置，预计有文本冲突；各插件 `ui/native/package.json` 若两边都动过也要核对。
- Jev 改了 `CONTRACTS.md` §72.10；D1a 把 §72.10 列进了留给 D2 的清单。
- D1a 把 `Spec.Fields` 改成总是检查后，各插件里显式写的 `Fields = fieldcheck.Fields{}` 成了多余的写法，实现方没删（删了算插件内容变化）；终审时决定去留。
- 合并 D1a 之后再跑一次第 6.7 节的全量验证；特别是整套 e2e（D1a 改了 7 条用例，但从没实际跑过）。

### 13.5 推送状态

暂停时**什么都没推**。所有者的指示：远端用 `upstream`；"现在你先提交文档就行了 其他的慢慢等合并 后面文档一起提交上去就行了"。要在别处接着做，除了 `develop`（含本文），还需要 `feat/plugin-ui-code-only`、`feat/plugin-ui-d1a`、`feat/plugin-ui-d1b`、`feat/plugin-ui-jev`、`feat/plugin-key-url-rules` 这五个分支；推之前跟所有者确认清单。只推分支、不打标签（标签 `core-v<版本>` 推到 upstream 会触发发布）。
