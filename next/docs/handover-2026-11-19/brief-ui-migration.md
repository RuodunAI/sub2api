# 插件界面迁移包（G 系列）的共同任务说明（给各迁移子代理；不提交）

## 背景
所有者决定：插件界面只用代码写（Vue 3 + TypeScript + `@sub2api/ui`、`@sub2api/host`，Vite 打成 `ui/native/entry.js`），不保留任何表单 JSON；原话："我们就直接不用声明式 UI 了，直接只用 SDK 提供的相关组件来做"、"不保留json 字段校验前端封装一个通用的校验框架出来 参考主流ui框架的校验逻辑"、"一步做完"。保存时的最终校验仍在各插件的 Go 代码里；前端校验只负责填表时的即时反馈。

你做其中一个"迁移包"：把分给你的几个插件的声明式表单（`forms/*.schema.json` + `forms/*.ui.json`，清单里 `form.mode: "schema"`）改写成 native 组件，并把只写在 schema 里的约束搬进插件 Go。

## 必读
- 仓库根 `AGENTS.md`（总规则）。
- 设计文档（在分支 `feat/plugin-ui-code-only`）：`next/docs/DESIGN-2026-11-19-PLUGIN-UI-CODE-ONLY.md`——校验框架与组合组件两节、"逐插件迁移表"里你负责的插件那几行、风险一节。
- `next/web/packages/ui/README.md`（新组件怎么用：`useForm`、`form.bind`、`SForm`、`SField prop`、`SSecretInput`（`clearable`）、`SUrlPresetInput`、`useCredentialForm`/`SCredentialForm`、`createApiKeyForm`、`useGuardedSettings` 等）和 `next/web/packages/ui/src/form/`、组合组件源码及其 spec。
- `next/docs/CONTRACTS.md` §23（界面契约；§23.4 写法约定；§23.5 native 账号表单接口：`modelValue`、`mode`、`account`、`accountType`、`errors`、`proxyTicket`，expose `validate()`）、§21.3（受限设置）。
- 现成的参照：`next/plugins/codex/ui/native/`（工程结构、`package.json`、`vite.config`、`tsconfig`、`entry.ts` 的 `register(host)`、spec 写法）和 codex 的 `manifest.json`（`form.mode: "native"`、`component`、`ui.native.entry`、`hostUICompat`、`hostPermissions` 里的 `ui.native`）。

## 每个插件要做的
1. **读懂原表单**：`forms/<type>.schema.json`（properties、required、pattern、minLength/maxLength、enum、default、`additionalProperties:false`、`writeOnly`/`format: password`）和 `forms/<type>.ui.json`（`ui:widget`：secret、url-presets、hidden、select+`ui:enumNames`、date-time、textarea、switch、text 等；`ui:section`、`ui:visibleWhen`、`ui:errorMessages`、`ui:help`、`ui:placeholder`、`ui:order`、`ui:title`）。逐条列出它的字段、约束、默认值、文案（中英文）。
2. **建工程**（没有 `ui/native` 的插件）：照 codex 的结构建 `next/plugins/<name>/ui/native/`（`package.json` 带 `typecheck`、`test`、`build` 脚本，依赖与 codex 同版本；`src/entry.ts`、组件、`messages.ts`）。构建产物（`dist`、`entry.js`）不提交，看 codex 的 `.gitignore` 怎么处理就怎么处理。
3. **写组件**：能用 `createApiKeyForm` 的用它；不能的用 `useCredentialForm` + `SCredentialForm` + `SField prop` + `form.bind` 拼。必须做到：
   - 读写的键名与原表单完全相同；默认值与原表单相同（新建时预填）；原来 `ui:widget: hidden` 的键（如 vertex 的 `access_token`、`expires_at`、`token_for`）原样保留，不显示、不丢。
   - 密钥字段用 `SSecretInput`：已存值显示为已保存、不改则原样送回 `******`；规则里非必填的密钥带 `clearable`。
   - `base_url` 之类受限设置字段接 `useGuardedSettings`（没有 `account:settings:custom` 权限的人只能选官方值）。
   - 前端规则覆盖原 schema 的约束（必填、长度、pattern、枚举），文案走 `host.addMessages`/`host.t`，中英文齐全，不写内联 `{en, zh}`，不硬编码文字。
   - 写法遵守 §23.4：只用 `@sub2api/ui` 组件；不用控制台全局类和 Tailwind 工具类；颜色只用契约里的 CSS 变量；操作按钮写文字；私有样式类以插件 key 为前缀。
   - `proxyTicket` 等 §23.5 的 props 接住（用不上也要按接口声明）；expose `validate()`。
4. **清单**：`accountTypes[].form` 改为 `{"mode": "native", "component": "<注册名>"}`（删掉 `schema`、`uiSchema`）；加 `ui.native.entry`、`hostUICompat`（现在界面包版本是 3.20.x，写 `^3.20`；最后统一升 4.0 时由收尾包改）、`hostPermissions` 里加 `ui.native`（照 codex 的写法和 reason 文案风格）。`sensitiveFields`、`settingsFields`、`upstreamFields`、受限设置等已有字段不动，并核对 `sensitiveFields` 覆盖了原 schema 里所有 `writeOnly`/密码类字段（缺的补上并写进测试）。
5. **插件 Go**：原 schema 里的约束，凡是插件的 `ValidateCredentials`（或它用的 SDK `apikey.Spec`/`accountform`）现在没有覆盖的，补进插件 Go（必填、长度、pattern、枚举、格式）。"拒绝未知键"由 SDK 统一补（另一个包在做），你不用在插件里各写一遍；默认值按设计只在前端预填，不在 Go 里新增默认值——但设计里点名要后端处理的除外（如 azure 的 `api_version` 改为后端必填）。为每个账号类型写一条**永久的** Go 测试："原 schema 会拒绝的输入，现在 `ValidateCredentials` 仍然拒绝"（把原 schema 的每条约束各取一个反例，直接写死在测试里，不读 schema 文件）。
6. **对比测试**（前端 spec）：趁 `forms/` 还在，先写一条临时对比——同一组输入（覆盖每个字段的合法值、非法值、空值、编辑已存账号的 `******`、带未知键、带 hidden 键）同时喂给旧的 `SchemaForm`（读原 schema/ui.json）和新组件，确认 `v-model` 产出与 `validate()` 结论逐键相同；确认一致后，把这组输入和期望值**写死**成永久 spec（不依赖 schema 文件），再删掉临时对比。汇报里写明比了多少组、有没有不一致（不一致的逐条说明以谁为准、为什么）。
7. **删 `forms/`**：该插件的账号表单 JSON 文件删掉；全仓搜这个插件对 `forms/` 的引用（Go 测试、SDK 的 provider 测试、`next/web` 里读它的 spec——例如 `next/web/packages/ui/src/form/composite.spec.ts` 里有一条同时挂 anthropic 旧表单的对比用例，注明了"删 schema 时随之删除"，轮到 anthropic 的那个包把它删掉、保留手写期望值的那一版）并改掉。**设置表单（`ui.settings`）和声明式页面不在账号表单迁移包的范围**，除非你的任务单独写明。
8. **版本**：插件内容变了要过 `tools/sub2api-plugin` 的 `TestReleasedPluginsBumped`（对照 `next/plugins/released.json`；这一批未发布的版本上继续累计，已高于已发布版本的不用再升，首发 0.1.0 且不在 `released.json` 里的不用升）。`hostCompat` 不动。
9. **文档**：各插件自己的文档（`next/docs/PLUGIN-<NAME>*.md` 若有表单说明）和 `CHANGELOG.md`"未发布"加一句；通用文档（CONTRACTS、PLUGIN-DEVELOPMENT、COMMON-CONTROLS）由收尾包统一改，你不用动。

## 规则
- 不得擅自加硬编码超时、上限、防御性检查、默认值、兜底；不写兼容/过渡代码（不保留"schema 和 native 都能用"的开关）。六条代码规范：抽象、复用、解耦、低嵌套、简洁、低侵入。发现 `@sub2api/ui` 的组合组件缺能力或有 bug：**不要在插件里绕过或复制一份**，停下来在汇报里写清楚（要什么、为什么），小而明确的修正可以直接改 `next/web/packages/ui` 并带 spec、单独提交、汇报里列出。
- 平台插件严禁联网（所有者原话）；界面里的请求只走 `host.api` / `host.pluginApi`。
- 不 `git push`、不发布、不用 `git stash`、不向真实上游发请求。
- 提交：`git -c user.name="eriol touwa" -c user.email="asakifeng@gmail.com" commit`，`git add` 写明路径（不用 `-A`），英文 `<scope>: <lowercase sentence>`（如 `anthropic: ...`、`plugins(openai, gemini): ...`），结尾的署名行按你自己会话的署名规则写。一个插件一个提交（或一小组提交）。
- 并行情况：别的子代理同时在别的工作树里做——核心与 SDK（`next/sdk`、`next/server`）、构建与 CI（`next/deploy`、`.github`）、别的插件目录、以及另一条功能分支在改 12 个平台插件 `manifest.json` 的 `hostPermissions`（删 `accounts.credentials`/`accounts.read`）。你只动分给你的插件目录、必要时 `next/web/packages/ui`（见上）、`CHANGELOG.md`、插件文档。清单里 `hostPermissions` 的改动保持最小（只加 `ui.native` 一项），便于之后合并。

## 验证（实际跑，结果写进汇报）
- node 在 `/home/erio/.nvm/versions/node/v26.13.0/bin`（`PATH=/home/erio/.nvm/versions/node/v26.13.0/bin:$PATH`）。每个插件的 `ui/native`：`npm ci`、`npm run typecheck`、`npm test`、`npm run build`（产物不提交）。如果你改了 `next/web/packages/ui`：`cd next/web && npm ci && npm run typecheck && npx vitest run packages`。
- 本机没有 Go：`/home/cvp/rc/sup2api/.claude/worktrees/gotest.sh <你的工作树> <next 下的目录> <go 参数...>`（脚本自己包了 `sg docker`，直接调用，不要再套 `sg docker -c`；不接受 `-e`，要 `TEST_DATABASE_URL` 的测试需读脚本后手写 `docker run`，起临时 `postgres:17-alpine`（tmpfs），容器名用你任务里给的前缀，用完删掉；不动别人的容器、卷、镜像、构建缓存）。每个插件：`gofmt -l`、`go vet ./...`、`go test -count=1 ./...`；`tools/sub2api-plugin`：`go test -count=1 ./...`（带 PG；含 `TestReleasedPluginsBumped`）；`next/sdk` 里引用了你删掉的 `forms/` 的测试包。
- 磁盘紧张、机器上有多个代理同时在跑：一个工程装完依赖、跑完就删它的 `node_modules`，再做下一个；开工和每次 `npm ci` 前看 `df -h /`，可用低于 6G 停下来汇报，只可清你自己的临时容器和你自己工作树里的 `node_modules`。
- 结束时 `git status --porcelain` 为空（没有遗留的 `node_modules`、`dist`）。

## 汇报
- 提交列表（哈希 + 标题）。
- 每个账号类型一张小表：字段、原约束 → 前端规则 / Go 校验各在哪、默认值、密钥与 hidden 键的处理、对比了多少组输入及结论。
- 原 schema 里有、而你判断不需要搬的约束（逐条理由）。
- 对 `@sub2api/ui` 的改动或发现的缺口。
- 命令与结果。
- 你自己做的、任务里没写的决定单独列一节。被拒的操作不要绕过，原样说明。
