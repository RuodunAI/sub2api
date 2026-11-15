# 总览：插件权限声明与角色分配

> 基于 `touwaeriol/sup2api` develop @ dae6ba57（只读）。路径相对 `next/`，另注明的除外。本文是 5 章设计的入口：一页摘要、章节导航、跨章接口约定、定案清单、风险与回滚，以及交给子代理并行实现的任务拆分。各章细节以章节为准；本文与章节冲突时，以本文“定案清单”为准，并回头修章节。

## 1. 一页摘要

两类权限：

| | ① 插件申请的访问权限 | ② 插件声明给用户的权限 |
|---|---|---|
| 清单字段 | `hostPermissions` | `userPermissions`（本次重点） |
| 授权对象 | 插件本身（`plugin_permission_grants`） | 角色（`role_permissions` / `role_permission_scopes`） |
| 本次改动 | 语义不变；插件详情“权限”标签里展示、撤销，新增**收窄 / 重新授予**（`PUT /plugins/:key/grants/:permission`） | 清单新字段、目录新列、D2 挂起、D6 软删除、插件资源范围、三个入口共用矩阵 |

② 的主线：

1. **清单**（第 1 章）：`userPermissions[]` 新增 `group`、`risk`（low/sensitive/critical，取代 `sensitive`）、`requires`、`scope`（`{resource, options, own}` 或 `{values}`）、`defaults`（取代 `defaultRoles`）；顶层 `permissionGroups[]`；路由 `scopeFrom`。用了新字段的清单要求核心 ≥ `PluginPermissionsFirstCore`（推测 0.1.289，该核心自带 UI 3.16）：check 只按核心下限判断（`below_feature_core`），`uiFloors` 的 3.16 档与核心下限共用这个常量，只回答清单自己写的 `hostUICompat ^3.16`（第 1 章 §4.6）。旧字段不兼容，check 直接拒绝。
2. **key 语法**（第 1 章）：`perm` 按 `plugin.` 前缀语法解析插件范围 `*` / `own` / `<res>/<value>`，`perm` 仍是叶子包；语义（资源对不对、own 允不允许、值在不在枚举里）由 authz 在写入时对照目录校验。枚举形式的资源名固定为 `value`（`…:value/input`）。
3. **目录**（第 2 章）：迁移 0099 给 `permissions` 加 `risk/requires/group_*/scope/defaults`，核心 superOnly 映射为 critical；插件不再声明的 key 标 `removed`（授权保留不生效），重新声明时原样复活、不重套默认（D6）；`SyncPlugin` 不再做继承；新增按插件读写的矩阵接口。
4. **同意**（第 3 章）：`new_permission_roles` 改为“本地 key → 角色 → true | 范围数组”；默认值和继承也按“安装者能否命名该角色”判，不能命名的进入现有待复核队列（D2）；critical 只有超管能授（D3）；继承规则（同 scope_type 才带范围、critical 只从 critical 继承）集中在 `planRoleGrants`。
5. **运行时**（第 4 章）：插件路由按“任意范围持有”放行，声明 `scopeFrom` 时核心在转发前判值，范围随 `HTTPRequest.action_scope` 交给插件（own 由插件按 `created_by` 判）；菜单等 UI 按任意范围显示；`GET /plugins/:key/scope-options` 代理插件 options 路由。
6. **前端**（第 5 章）：共用 `PermissionMatrix`（确认页、插件详情“权限”标签），角色编辑页 `PermissionTree` 分组、风险徽标、插件建议与恢复；`implies.ts` 按语法解析插件范围；`ScopePicker`/`EntityPicker` 支持插件范围候选。

防提权只有一条规则：授出的 ⊆ 自己的授予集（`covers`/`perm.Implies`，插件范围同规则），改动的角色 ∈ 自己可分配清单（`editableRole`）；安装/升级新建权限时再加 `pluginGrantee`（可分配清单内且非自己持有，超管不限）和 critical 仅超管。

## 2. 章节导航

| 章 | 文件 | 主要内容 | 主要代码 |
|---|---|---|---|
| 1 | `01-manifest-perm.md` | 清单结构与校验、下限、`perm` 插件范围语法、向量、内置插件清单迁移 | `sdk/manifest`、`sdk/manifest/check`、`server/internal/perm`、`sdk/permvectors`、`plugins/*/manifest.json` |
| 2 | `02-authz.md` | 迁移 0099 与回滚、`PermissionDef`/`PermissionCatalog`、`SyncPlugin`（removed/复活/pruneScopes）、critical、`PluginGrantees`、角色写入 `setRolePart`、`GET /permissions` 新字段、矩阵接口 | `server/internal/{migrations,authz,core/ports_identity.go}`、`deploy/rollback` |
| 3 | `03-install.md` | review 矩阵数据、consent 新格式、`settleRoleGrants`（D2 挂起）、待复核带范围、升级/内置路径、`SetGrant` | `server/internal/plugin/{install,grants,api}`、`core/ports_plugin_infra.go`、`e2e/plugins.go` |
| 4 | `04-runtime.md` | `PluginScope`/`HasSome`、路由 `scopeFrom` 判定、`action_scope` proto、`qualify` 修正、scope-options 代理、UI 可见性、SDK 封装 | `server/internal/{core/plugin_scope.go,plugin/routes,plugin/grpcruntime,plugin/api,lookup,authz/menus.go}`、`sdk/proto`、`sdk/pluginsdk` |
| 5 | `05-web.md` | 类型、纯函数、矩阵组件、确认页、角色编辑页、插件详情“权限”标签、GrantsTab 收窄/重新授予、iframe、UI 3.16、mock、vitest 与浏览器 e2e | `web/src`、`web/mock`、`web/e2e`、`web/packages` |

## 3. 跨章接口约定

| 名称 | 形状 | 负责 | 使用方 |
|---|---|---|---|
| `manifest.UserPermission` 新字段 | `Group`, `Risk`, `Requires`, `Scope *PermissionScope`, `Defaults map[string]RoleDefault`；删 `Sensitive`、`DefaultRoles` | 1 | 2、3、4 |
| `manifest.PermissionScope` | `{Resource, Options string; Own bool; Values []ScopeValue{Key, Label}}`，`ResourceName()`（枚举为 `"value"`），`GrantSuffix(v) (string, bool)` | 1 | 2（直接作为 `PermissionDef.Scope` 与 `PermissionItem.Scope`）、3、4、5（TS `PluginScopeDecl`） |
| `manifest.RoleDefault` | JSON `true \| [...]` | 1 | 3（`ConsentRequest.NewPermissionRoles` 解码） |
| `up.RiskLevel()`、`up.DefaultGrants() map[string][]string` | 后者：角色 → 规范后缀，`""` = 全部，critical 为 nil | 1 | 3（`PermissionDefs`、review） |
| `m.FindUserPermission(local)`、`m.UserPermissionScope(local)` | | 1 | 1（check）、4 |
| `Route.ScopeFrom` | `"path:<p>" \| "query:<n>" \| "body:<f>"`，`ParseAccountsFrom` 解析 | 1 | 4 |
| `check.PluginPermissionsFirstCore` | `"0.1.289"`（推测）；`uiFloors` 3.16 档同一版本 | 1 | 2（回滚脚本命名）、9 个内置插件 `hostCompat` |
| `perm.Scope{All, Own, Group, ID, Res, Value}`、`perm.PluginPrefix`、`perm.ValidScopeValue` | 插件范围存储后缀 `own` 或 `<res>/<value>`；资源正则不导出 | 1 | 2、4 |
| `sdk/permvectors/vectors.json` | Go 与 TS 共跑 | 1 | 5（`implies.ts`） |
| `permissions.scope_type` | 核心 `account`/`proxy`；插件 `"plugin:" + ResourceName()` | 2 | 3、4、5 |
| `core.PermissionDef` | `Key, Module, ScopeType, Scope *manifest.PermissionScope, Label, Description, Risk, Sort, Group, GroupLabel, GroupSort, Requires, Defaults map[string][]string, GrantRoles map[string][]string`；删 `Sensitive`、`InheritFrom` | 2 | 3（`install.PermissionDefs` 映射） |
| `PermissionCatalog.CanGrantToRoles(ctx, tx, actorID, roleKeys, critical bool)` | critical 时非超管 403，`details{reason:"critical", superuser:true}` | 2 | 3 |
| `PermissionCatalog.PluginGrantees(ctx, tx, actorID, roleKeys) (core.PluginGrantees, error)` | `{Superuser bool, Denied map[role]reason}`，reason ∈ `superuser_role\|not_editable\|not_assignable\|held`；只读不 Bump | 2 | 3 |
| `PermissionCatalog.GrantPluginPermission(ctx, tx, key, roles map[string][]string)` | 跳过 removed 与超管角色 | 2 | 3（复核确认） |
| `PermissionCatalog.SetPluginActive` | `WHERE … AND status <> 'removed'` | 2 | 3（启停）、rollout |
| `SyncPlugin` | removed 软删除、复活不授 GrantRoles、`pruneScopes`、**不继承** | 2 | 3 |
| `GET /permissions` 新字段 | `risk`、`requires`、`scope_type`、`scope{resource, own?, options?, values?}`、`group`、模块 `groups[]`、`defaults`（完整 key）；去掉 `sensitive` | 2 | 5 |
| `GET`/`PUT /api/v1/permissions/plugins/:plugin` | `role:read` / `role:define`；Go `PluginRoleMatrix{plugin_key, module, roles: PluginRoleRow[]}`，`PluginRoleRow{id,key,name,builtin,superuser,editable,denied,permission_keys}`；PUT body `{roles: {role: keys[]}}`；403 details 加 `role` | 2 | 5 |
| `RoleGrant` | `{version?, permission, role, scope, risk, via: default\|override\|inherit, needs_consent, nameable, withheld}` | 3 | 5（`PluginRoleGrant`）、`PluginSummary.RoleGrantsToReview` |
| `ConsentRequest.new_permission_roles` | `{local: {role: true \| ["*" \| "own" \| "value/<k>" \| "<res>/<id>" \| 裸枚举 key]}}`；出现的 key 对可命名角色整体覆盖；删除 `role_keys_for_new_permissions` | 3 | 5（`toManifestGrant`） |
| `ConsentResult.withheld` | `[]RoleGrant`，本次挂起待复核的授予 | 3 | 5（toast） |
| review 新字段 | `permission_groups`、`role_columns[]{key,name,builtin,superuser,nameable,reason}`、`user_permissions[].{risk,group,requires,scope,defaults(规范后缀),new,revived,renamed,cells{role:{grant,nameable,reason,withheld}}}`、`removed_user_permissions[]`；`Review(ctx, key, version, actorID)` 按查看者计算 | 3 | 5 |
| `PendingRoleGrants` | 读 `plugin.consent` 与 `plugin.consent.auto` 的 `detail.pending`；元素带 `scope`、`risk`（取目录列）；范围已不合当前声明的不列出；按 (permission, role, scope) 去重 | 3 | 插件详情 `RoleGrantsReview` |
| `PUT /plugins/:key/grants/:permission` | body `{scope?}`；收窄只要 `plugin:manage`，重新授予按风险要 `plugin:grant:*`；挂在 grant step-up 下；返回 `Grant` | 3 | 5（`GrantsTab`） |
| `core.PluginScope`、`PermissionSet.PluginScope(action)`、`PermissionSet.HasSome(k)`、`core.AuthorizePluginScope` | 新文件 `core/plugin_scope.go` | 4 | 4（routes、menus、`plugin/api/ui.go`、scope-options） |
| `HTTPRequest.action_scope`（`ActionScope{permission, all, own, values}`） | 路由权限声明 scope 时总是带；不升 Host API | 4 | 插件（`pluginsdk.RequestScope/RequireScope`） |
| `GET /api/v1/plugins/:key/scope-options?permission=&q=&ids=&limit=` | `role:read` 或 `plugin:read` 任一；响应 `{items:[{id(string),name,hint?,status?}], more}`；枚举由核心从清单回答，插件资源转发，插件未运行 503 `plugin_unavailable` | 4 | 5（`entityOf('pscope:…')`） |
| UI 版本 | `HOST_UI_VERSION` 3.15.0 → 3.16.0 | 5 | 1（`uiFloors` 3.16 档） |

错误码汇总：清单 check 只复用现有码（`invalid_value`、`unknown`、`mutually_exclusive`、`reserved`、`too_many`、`invalid_route_ref`、`unknown_route`、`unsupported`、`unknown_param`、`below_feature_core`、`below_ui_core` 等）；consent 覆盖值非法为 400 `invalid`（与 authz 非法 key 同码）；矩阵 PUT 外插件 key 为 400 `foreign`；critical 拒绝为 403 `permission_denied` + `details.reason="critical"`；宿主权限放宽为 400 `scope_too_wide`（现有码）。

## 4. 定案清单

已定方案（BRIEF）：

| # | 结论 |
|---|---|
| D1 | 通配 `a:*` 只是勾选快捷方式，保存时展开为具体 key，DB 不存通配（`plugin.m:flow:*` 保存报 400 `unknown`） |
| D2 | defaults 给了安装者不可命名的角色 → 不直接授予，进待复核（`withholdGrants`/`PendingRoleGrants`），超管复核 |
| D3 | 风险三级，critical = 核心 superOnly：不进默认值；安装时只有超管能授；角色编辑时只有持有者能授（初始只有超管持有） |
| D4 | 插件操作账号时用路由 `accounts` 与核心 `account:*` 取交集，插件 key 不带账号范围（check 保留资源名 account/proxy/group/value） |
| D5 | 卸载 = 删除权限与授权，重装重新确认 |
| D6 | 升级时不再声明的 key 改 `status='removed'`（授权保留不生效），重新声明时原样复活、不重套默认 |

本次统一口径（写入各章）：

| # | 结论 | 章 |
|---|---|---|
| A1 | scope-options 代理接口：`role:read` 或 `plugin:read` 任一 | 1 §4.1、4 §4.3.7、5 §3.3 |
| A2 | `claude_oauth:manage`、`codex:manage`、`moderation:read` 提为 sensitive；只影响新 key 的自动授予，已有授权不变 | 1 §8.2 |
| A3 | 枚举形式资源名固定为 `value`，不加可选 resource | 1 §11 Q1 |
| A4 | `moderation:exempt` 标 critical、不写 defaults、去掉 inheritFrom；线上已有授权保留不动（0.12.0 已随 0.1.286 上线，推测 admin 已持有） | 1 §2.6、§11 R1 |
| A5 | 第三方插件不做旧字段兼容 | 1 §11 Q4 |
| A6 | 第 3 章 3.9 的行为变化全部采纳：操作者遇到不可命名角色从 403 改挂起；继承也按可命名规则；上传沿用按上传者判 | 3 §3.9 |
| A7 | 采纳 `PUT /plugins/:key/grants/:permission`（收窄/重新授予），第 5 章 GrantsTab 接入 | 3 §3.6、5 §4.8 |
| A8 | “恢复插件建议”由前端完成，后端 reset 接口首版不做 | 2 §2.3.10、5 §4.6 |

审查时顺带定下的（理由见各章，均为收紧或减少代码）：

| # | 结论 |
|---|---|
| R1 | 不升 Host API：`action_scope` 只出现在声明 scope 的清单上，而这类清单已被 coreFloor 限定在新核心 |
| R2 | `AuthzCheckResponse.scope`、SDK `ResolveScope`/`Filter`、`Host.ActionScope` 首版不做（无调用方）；`qualify` 认带范围本地 key 的修正保留 |
| R3 | `core.PermissionDef.Scope`、`PermissionItem.Scope` 直接复用 `manifest.PermissionScope`，不新增 `core.PermissionScope`/`PermissionScopeValue`/`PermissionScopeView` |
| R4 | review 的 `defaults` 输出规范后缀（`DefaultGrants()`），前端去掉 `keysFromManifest` |
| R5 | consent 覆盖值同时接受清单写法和规范后缀（`value/input`），非法值码为 `invalid` |
| R6 | `SyncPlugin` 对已有 key 一律 `pruneScopes`，去掉 `pluginRows` 预读；`withholdGrants`/`settleRoleGrants` 不再返回拷贝的 manifest |
| R7 | 删 `perm.PluginScopeResource`，资源正则不导出 |
| R8 | 已有 key 风险升高（如 exempt）：确认页首版不单独列出，不撤销 |
| R9 | 清单 `scope` 首版不加资源显示名 `label` |
| R10 | 宿主权限“收窄”首版只支持数组型范围；收窄也走 step-up（一条路由） |
| R11 | `SetGrant` 放新文件 `plugin/install/grant_set.go` |

## 5. 风险与回滚要点

**迁移与发布**

- 唯一核心迁移：`server/internal/migrations/0099_plugin_permission_meta.sql`（当前最新 0098）。全部 `IF NOT EXISTS`/可收敛，满足 `FirstIdempotentMigration` 规则；`sensitive` 列保留一版（新代码写 `risk <> 'low'`、不读），下一个迁移再删。
- 带 schema 变化，按 0.1.286 的 primary-first 方式发布（短暂停服），不与旧核心滚动混跑；迁移发布需用户点头。
- 版本：核心 `0.1.289`（推测）同时带 UI 3.16；9 个声明了 userPermissions 的内置插件 `hostCompat` 升到 `>=0.1.289 <0.2.0`，随核心一起发布。

**回滚脚本** `deploy/rollback/0.1.289-plugin-perm-down.sql` + `.md`（第 2 章 §2.3.2）：

1. 删除 `source='plugin' AND status='removed'` 的权限行（写审计 `via "rollback 0.1.289"`）：旧核心的 `SetPluginActive` 会把它们复活。D6 保留的授权随之丢失，等同旧语义“删除”。
2. 删除插件的 `role_permission_scopes` 行：旧 `perm` 解析不了，本来就不生效。
3. 删除 7 个新列和 `permissions_risk_check`，插件行 `scope_type` 置 `''` 后改回 `varchar(20)`。
4. 删除 `schema_migrations` 的 0099 行，推进 authz 版本。

只用于不经网关的环境：网关不允许跨 schema 回退（`deploy/gateway/ovh/README.md:31`），线上出问题按“修复前进或从数据库备份恢复”处理。内置插件回滚：新清单需要核心 ≥0.1.289，旧核心不会接受，回滚核心时内置集合也要回到旧版本。

**行为变化（发布说明要写）**

- `GET /permissions`、review、`RoleGrant` 的 JSON 去掉 `sensitive`，改为 `risk`；consent 去掉 `role_keys_for_new_permissions`，`new_permission_roles` 变成嵌套格式。外部脚本需要同步改。
- 操作者升级同意遇到不可命名的默认/继承角色，从 403 改为挂起待复核；首装也走 D2。
- 角色编辑页可以取消勾选已移除（removed）权限；通过 API 回存角色、又不带 removed key 会删掉这些授权。
- 角色编辑页禁用编辑者自己没有的权限（核心项也一样）。
- iframe 桥 `permissions.check(本地 key)` 改为“任意范围持有”；插件 UI 菜单按任意范围显示。
- review 按查看者计算（同一 URL 不同人看到的 cells 不同）。
- `moderation:exempt` 变为 critical：新部署只有超管持有；存量持有者保留。

**主要风险**

- own 的正确性依赖插件按 `created_by` 检查（第 4 章 §4.6.2）；本轮内置插件都不声明 scope，暂无实际暴露面。
- options 资源 id 不校验是否存在，插件数据删除后授权残留（不越权，界面显示原值）。
- 前端与后端必须同版本：`vectors.json` 一改，`implies.spec.ts` 立即失败；`PermMeta.risk` 替换 `sensitive` 影响前端编译。

## 6. 实现任务拆分

约定：

- 同一 worktree，每个任务交给一个子代理，只改“负责文件”列出的文件；带 ⚠ 的是多个任务都会改的共享文件，同一阶段内只能由标注的任务改，别的任务需要时提交补丁说明给它。
- Go 命令在 `next/` 下执行（`go.work` 覆盖 server、sdk、plugins、e2e）；数据库测试用 `testutil.DB`（`TEST_DATABASE_URL` 或自动起本地 PG），无需额外准备。前端命令在 `next/web/` 下。
- 每个任务完成时：本任务的验收命令全绿，且 `go build ./...`（`cd next && go build ./server/... ./sdk/... ./plugins/...`）不破坏；改了前端还要 `npm run typecheck`。
- `docs/CONTRACTS.md`、`CHANGELOG.md` 统一由 T4.4 改，其他任务在交付说明里写出要改的段落。

共享文件一览：

| 文件 | 阶段内唯一写者 | 其他需要者 |
|---|---|---|
| ⚠ `sdk/permvectors/vectors.json` | T0.1 | T0.5 只读 |
| ⚠ `sdk/manifest/manifest.go`、`permission.go` | T0.2（加字段）→ T4.1（删退役字段） | 所有后端任务只读 |
| ⚠ `server/internal/core/ports_identity.go` | T1.1 → T2.1（删 `InheritFrom`） | T2.3 不改它（新文件 `core/plugin_scope.go`） |
| ⚠ `server/internal/authz/grants.go`、`plugin_catalog.go`、`sync.go` | T1.1 → T2.1（删继承）/T2.2（`writeGrants`），T2.1 与 T2.2 分别只改 `plugin_catalog.go`/`sync.go` 与 `grants.go`/`roles.go` | |
| ⚠ `server/internal/authz/roles.go` | T1.1（`editDenial`）→ T2.2 | |
| ⚠ `server/internal/plugin/api/api.go` | T2.3、T2.4 各加一行路由（同阶段，后合入者 rebase） | |
| ⚠ `server/internal/plugin/api/plugins.go` | T2.1（`Review` 传 actor）、T2.4（`setGrant` handler） | 同上 |
| ⚠ `server/internal/plugin/install/*.go` | T1.1 只做编译适配 → T2.1 | T2.4 只新增 `grant_set.go` |
| ⚠ `web/src/api/types.ts` | T3.0 | T3.1–T3.3 只读，需要新类型时交给 T3.0 补 |
| ⚠ `web/src/i18n/locales/{zh,en}/plugins.ts`、`roles.ts` | T3.1（roles、entity）、T3.2（plugins.matrix）、T3.3（plugins.permissions、plugins.grants）分键块改 | 合入时按键块解决冲突 |
| ⚠ `web/mock/resources.ts`、`web/mock/plugins.ts` | T3.0（`/permissions` 新字段、矩阵、scope-options、PUT grants）、T3.2（review/consent） | |

### P0 基础（全部可并行）

| 任务 | 内容（章节） | 负责文件 | 依赖 | 验收 |
|---|---|---|---|---|
| T0.1 perm 语法 | 1 §5–6 | `server/internal/perm/pattern.go`、`pattern_test.go`；`sdk/permvectors/vectors.json`；`server/internal/core/datascope.go`；`server/internal/core/ports_identity_test.go`（只加 `TestPermissionSetPluginScopes`） | 无 | `go test ./server/internal/perm/ ./server/internal/core/ ./sdk/permvectors/`；与 T0.5 同一提交合入 |
| T0.2 清单结构与校验 | 1 §3–4（**只加不删**：保留 `Sensitive`/`DefaultRoles`/`Roles()` 到 T4.1，避免中间状态编译失败；X1 拒绝退役字段照常实现） | `sdk/manifest/manifest.go`、`permission.go`、`permission_test.go`；`sdk/manifest/check/{validate,routes_check,corefloor,ui}.go` 及 `user_permissions_test.go`、`routes_check_test.go`、`ui_test.go` | 无 | `go test ./sdk/manifest/...` |
| T0.3 proto | 4 §4.3.3 | `sdk/proto/sub2api/plugin/v1/{common,app,host}.proto`、`sdk/gen/pluginv1/*`（`cd next/sdk && buf generate`） | 无 | `go build ./sdk/... ./server/...` |
| T0.4 迁移与回滚 | 2 §2.3.2 | `server/internal/migrations/0099_plugin_permission_meta.sql`、`migrations_db_test.go`（`TestPluginPermissionMetaMigration`）、`authz_hardening_db_test.go` 旁新增回滚测试；`deploy/rollback/0.1.289-plugin-perm-down.{sql,md}` | 无 | `go test ./server/internal/migrations/` |
| T0.5 TS 范围解析 | 5 §4.1 | `web/src/authz/implies.ts`、`scopeChoice.ts` 及 spec | T0.1（向量） | `npx vitest run src/authz/implies.spec.ts src/authz/scopeChoice.spec.ts` |

### P1 authz 目录

| 任务 | 内容（章节） | 负责文件 | 依赖 | 验收 |
|---|---|---|---|---|
| T1.1 目录核心 | 2 §2.3.3–2.3.7、§2.3.9 的 tsgen 部分：`PermissionDef` 新字段（暂留 `InheritFrom`）、`corePerm.risk()`、`SyncCore` risk/requires、`SyncPlugin` 新列 + removed/复活 + `pruneScopes` + 范围 `grantPlugin`、`SetPluginActive` 排除 removed、`CanGrantToRoles(critical)`、`PluginGrantees`、`editDenial`/`granteeDenial`、`pluginScope.fits` 与 `resolvePermissions` 校验、`PermMeta.risk` | `core/ports_identity.go`；`authz/{plugin_catalog,grants,sync,catalog,self,roles,tsgen}.go`（`roles.go` 只抽 `editDenial`）；`web/src/generated/permissions.ts`（`UPDATE_GOLDEN=1`）；`plugin/install/{defaults,consent,rolegrants}.go` **仅编译适配**（`GrantRoles` 用 `{role:[""]}`、`critical=false`、`Risk` 取 `RiskLevel()`）；前端读 `PermMeta.sensitive` 处机械改为 `risk !== 'low'` | T0.1、T0.2、T0.4 | `go test ./server/internal/authz/ ./server/internal/plugin/install/`；`UPDATE_GOLDEN=1 go test ./server/internal/authz/ -run TestGeneratedPermissionsTS` 后再跑一遍；`cd web && npm run typecheck` |

### P2 同意、角色写入、运行时、宿主授权（四个任务并行）

| 任务 | 内容（章节） | 负责文件 | 依赖 | 验收 |
|---|---|---|---|---|
| T2.1 安装与同意 | 3 §3.3–3.5（`ConsentRequest`、`overrideGrants`、`roleGrants`、`PermissionDefs` 新映射、`planRoleGrants`+继承规则、`settleRoleGrants`、`checkGrantable`、`withholdGrants`、`PendingRoleGrants`、`ReviewRoleGrants`、review 矩阵数据、`catalogKeys` revived）；**并删除** `SyncPlugin` 继承步骤、`grantInherited`、`PermissionDef.InheritFrom` | `plugin/install/{consent,defaults,rolegrants,review,upload,builtin}.go` 及测试（新 `consent_matrix_test.go`）；`core/ports_plugin_infra.go`；`core/ports_identity.go`（删 `InheritFrom`）；`authz/plugin_catalog.go`、`authz/sync.go`（删继承）及 `plugin_catalog_test.go` 的继承用例；`plugin/api/plugins.go`（`Review` 传 actor）；`e2e/plugins.go`（`ConsentAll`） | T1.1 | `go test ./server/internal/plugin/install/ ./server/internal/authz/ ./server/internal/plugin/api/`；`go vet ./e2e/` |
| T2.2 角色写入与矩阵接口 | 2 §2.3.8–2.3.10：`grantPart`、`setRolePart`、`writeGrants(part)`、removed 可取消勾选、`PermissionItem`/`PermissionModule` 新字段、`ListPermissions`、`PluginRoleGrants`/`SetPluginRoleGrants`、两条路由 | `authz/{roles,grants,http}.go`、`roles_test.go`、`scoped_grants_test.go`、`catalog_test.go` | T1.1 | `go test ./server/internal/authz/ ./server/internal/iam/` |
| T2.3 运行时 | 4 §4.3.1–4.3.8 | 新 `core/plugin_scope.go`(+test)；`plugin/routes/{routes,scope,accounts}.go`(+`scope_test.go`、`accounts_test.go`)；`plugin/grpcruntime/host.go`(+test)；新 `plugin/api/scope_options.go`(+test)；`plugin/api/api.go` 一行路由；`plugin/api/ui.go`；`authz/menus.go`(+`menus_test.go`)；`lookup/lookup.go`；新 `sdk/pluginsdk/action_scope.go`(+test)、`sdk/pluginsdk/http.go` 注释 | T0.1、T0.2、T0.3（可在 P1 期间开始；`menus.go` 与 T1.1 无交集） | `go test ./server/internal/core/ ./server/internal/plugin/routes/ ./server/internal/plugin/grpcruntime/ ./server/internal/plugin/api/ ./server/internal/authz/ -run 'Menu' ./server/internal/lookup/ ./sdk/pluginsdk/...` |
| T2.4 宿主授权收窄/重新授予 | 3 §3.6 | 新 `plugin/install/grant_set.go`；`plugin/grants/grants.go`（`Narrow`、解除 staged 撤销）；`plugin/api/plugins.go`（`setGrant`）、`api.go` 一行路由；`install/revoked_grants_test.go`、`api/api_test.go` | 无（只依赖现有代码，可从 P0 开始） | `go test ./server/internal/plugin/install/ -run SetGrant ./server/internal/plugin/grants/ ./server/internal/plugin/api/` |

### P3 前端（可用 mock 与 P2 并行；T3.0 先行，其余三个并行）

| 任务 | 内容（章节） | 负责文件 | 依赖 | 验收 |
|---|---|---|---|---|
| T3.0 前端基础 | 5 §3、§4.2–4.4：类型、`pluginGrants.ts`、`useGrantMatrix`、`PermissionMatrix`、`PermRiskBadge`；mock 的 `/permissions` 新字段、矩阵接口、scope-options、PUT grants；UI 3.16 版本号 | `web/src/api/types.ts`；`web/src/authz/pluginGrants.ts`(+spec)；`web/src/components/permission/*`(+`PermissionMatrix.spec.ts`)；`web/mock/resources.ts`；`web/packages/host/src/index.ts`、`web/packages/{host,ui,vite-preset}/package.json`、`web/packages/ui/src/ui-contract.spec.ts` | T0.5 | `npx vitest run src/authz/pluginGrants.spec.ts src/components/permission packages/ui/src/ui-contract.spec.ts && npm run typecheck` |
| T3.1 角色编辑页与范围选择 | 5 §4.5–4.6 | `web/src/views/roles/{PermissionTree,ScopePicker,RolesView}.vue`(+spec)；`web/src/components/entity/{entities,lookup}.ts`、`EntityPicker.vue`、`EntityMultiPicker.vue`、`views/usage/usageFilters.ts`(+`lookup.spec.ts`)；新 `web/src/utils/permErrors.ts`；i18n `roles.ts`、`entity.ts` | T3.0 | `npx vitest run src/views/roles src/components/entity src/i18n/locales.spec.ts` |
| T3.2 确认页 | 5 §4.7 | `web/src/views/plugins/consent/*`（含新 `ConsentNeed.vue`、`UpgradePermissionChanges.vue`）、`views/plugins/ConsentView.vue`(+spec)、`views/plugins/parts/RoleGrantsReview.vue`；`web/mock/plugins.ts`；i18n `plugins.ts` 的 `matrix`、`consent` 键块 | T3.0 | `npx vitest run src/views/plugins/ConsentView.spec.ts src/views/plugins/parts src/i18n/locales.spec.ts` |
| T3.3 插件详情“权限”标签 | 5 §4.8 | 新 `views/plugins/detail/PermissionsTab.vue`(+spec)、`NarrowGrantDialog.vue`；`detail/GrantsTab.vue`(+新 spec)；`views/plugins/PluginDetailView.vue`(+新最小 spec)；`components/plugin/PluginIframe.vue`(+spec)；i18n `plugins.ts` 的 `permissions`、`grants`、`detail.tabs` 键块 | T3.0（T3.1 的 `permErrors.ts` 若未合入，先合 T3.1 再接） | `npx vitest run src/views/plugins/detail src/views/plugins/PluginDetailView.spec.ts src/components/plugin/PluginIframe.spec.ts src/i18n/locales.spec.ts` |

### P4 内置插件清单、文档与 e2e

| 任务 | 内容（章节） | 负责文件 | 依赖 | 验收 |
|---|---|---|---|---|
| T4.1 内置插件清单迁移 + 删退役字段 | 1 §8、§3.4 删除项：9 个清单改 `risk`/`defaults`、hostCompat 升下限；删 `UserPermission.Sensitive/DefaultRoles`、`Roles()`、`DefaultPermissionRoles` 及 check 里 `defaultRoles` 循环；moderation `staff.go` 注释 | `plugins/{anthropic,claude-oauth,codex,dynweight,growth,guard,moderation,payment,volcengine}/manifest.json` 与各自 manifest 测试、`plugins/growth/internal/growth/plugin_test.go`、`plugins/moderation/internal/moderation/staff.go`；`sdk/manifest/{manifest,permission}.go`、`permission_test.go`、`check/validate.go`；`docs/ARCHITECTURE.md:482` | T2.1（install 不再用 `Roles()`/`Sensitive`） | `go build ./...`（`next/` 下）；`go test ./sdk/manifest/... ./plugins/anthropic/... ./plugins/claude-oauth/... ./plugins/codex/... ./plugins/dynweight/... ./plugins/growth/... ./plugins/guard/... ./plugins/moderation/... ./plugins/payment/... ./plugins/volcengine/... ./server/internal/plugin/install/` |
| T4.2 Go e2e | 2 §2.5、3 §3.8、4 §4.5 的 e2e | `e2e/ac27_plugin_scope_test.go`（新）、`ac02_rbac_test.go`、`ac05_upgrade_test.go`、`ac06_disable_test.go`、`ac22_moderation_test.go` | P2 全部、T4.1 | 部署 e2e 环境后 `cd next/e2e && E2E_BASE_URL=… E2E_ADMIN_PASSWORD=… go test -run 'TestAC02\|TestAC05\|TestAC06\|TestAC22\|TestPluginScope\|TestGatewayPathIgnoresPluginScopes' ./...` |
| T4.3 浏览器 e2e | 5 §6.3 | `web/e2e/phone-layout.test.mjs`、新 `web/e2e/plugin-permissions.test.mjs` | P3 全部 | `npx vite --mode mock --port 5185 &` 后 `NODE_PATH=$(npm root -g) E2E_WEB_URL=http://localhost:5185 node --test e2e/phone-layout.test.mjs e2e/plugin-permissions.test.mjs` |
| T4.4 契约文档与发布说明 | 各章“改动清单”里的 CONTRACTS 段落：§4（risk/critical、removed、目录字段、矩阵接口、consent 格式、D2 挂起）、§5/§5.7（新接口、`scopeFrom`、`PluginPermissionsFirstCore`）、§20（moderation exempt）、§21.0（插件范围语法、`action_scope`、scope-options、iframe 语义）；CHANGELOG 写第 5 节的行为变化与迁移说明 | `docs/CONTRACTS.md`、`CHANGELOG.md` | 各任务交付说明 | 人工审阅；`npx vitest run src/i18n/locales.spec.ts` 不受影响 |

依赖图：

```text
P0: T0.1 ─┬─ T0.5 ─────────────── T3.0 ─┬─ T3.1
          │                              ├─ T3.2
    T0.2 ─┼─ T1.1 ─┬─ T2.1 ── T4.1 ─┐    └─ T3.3 ── T4.3
    T0.4 ─┘        └─ T2.2           ├─ T4.2
    T0.3 ──────────── T2.3 ──────────┘
    T2.4（随时）                      T4.4（最后汇总）
```

全量回归（合并前）：`cd next && go test ./server/... ./sdk/... ./plugins/...`；`cd next/web && npm run typecheck && npm run lint && npm test`。
