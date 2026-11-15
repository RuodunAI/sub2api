# 第 5 章 前端：确认页矩阵、插件权限标签、角色编辑页

> 基于 `touwaeriol/sup2api` develop @ dae6ba57，代码根 `next/web/src`（下文路径省略此前缀，除非另写）。
> 仓库和 `next/web` 下都没有 `AGENTS.md`（已用 `find` 确认），本章按 BRIEF 的编码规范写：抽象、复用、解耦、低嵌套、简洁、低侵入。
> 后端字段名已经和第 2 章（authz-catalog）、第 3 章（install-consent）、第 4 章（runtime-scope）对齐，依据是三章回给本章的消息。

---

## 1. 本章范围

负责：

1. 一个共用的“权限 × 角色”矩阵组件 `PermissionMatrix`，用在三处：安装确认页、升级确认页（只列新增）、插件详情的“权限”标签。
2. 角色编辑页的 `PermissionTree`：插件模块下按 group 分组、风险徽标、“插件建议”标记、模块级“恢复插件建议”、分组整组勾选（D1）、“未持有不能授出”、依赖提示。
3. `ScopePicker` 支持插件范围（插件资源 + 静态枚举），`EntityPicker` / `lookup` 增加“插件范围候选”数据源。
4. 插件详情新增“权限”标签，取代现有“授权”标签：①访问权限（复用 `GrantsTab`）②矩阵加保存栏。
5. `authz/implies.ts`、`authz/scopeChoice.ts` 能解析带范围的插件 key（第 4 章明确交给本章）。
6. `api/types.ts` 新类型、i18n 键、手机布局、暗色、mock server、vitest 与 e2e 用例清单。

不负责：后端接口实现（第 2/3/4 章）、清单校验与 tsgen（第 1/2 章）。本章只写前端消费的接口形状。

---

## 2. 现状（均已在代码里核实）

### 2.1 安装 / 升级确认页

| 位置 | 现状 |
|---|---|
| `views/plugins/ConsentView.vue:44` | `permissionRoles = useNewPermissionRoles(review)`：每个新权限给哪些角色 |
| `ConsentView.vue:46-49` | `shownRoleGrants`：默认授予列表里，去掉了“已在上面改过角色的权限”的 default 项 |
| `ConsentView.vue:113-118` | 升级时 `blockedRoles`：需确认的授予里有调用者不能指定的角色，就不能提交（`canSubmit`） |
| `ConsentView.vue:172-176` | `POST …/consent`，body 为 `{grants, denied, new_permission_roles}` |
| `ConsentView.vue:287-293` | 页面顺序：`ProvidesSummary` → `NewRoleGrants` → `HostPermissionTable` |
| `consent/newPermissionRoles.ts:16-26` | `useNameableRoles()`：没有 `role:define` 返回空；超管返回全部可分配角色；其他人只返回 `grantable` 且自己不持有的角色。`RoleGrantsReview` 也用它（`parts/RoleGrantsReview.vue:22`） |
| `newPermissionRoles.ts:37-59` | `useNewPermissionRoles`：`initial = default_roles ∩ 可命名`（:43），`changed`（:46），`dropsDefaults`（:48），`payload()` 只发改过的权限（:56），值是 `string[]` 角色列表 |
| `consent/ProvidesSummary.vue:202-233` | 用户权限逐行列出，每行一排角色复选框（`data-testid="permission-roles"`），带锁图标表示 `sensitive` |
| `consent/NewRoleGrants.vue:13-18,39` | 授予列表 `PluginRoleGrant[]`，`sensitive` 显示锁，`needs_consent` 标橙 |
| `consent/HostPermissionTable.vue:14-20` | 访问权限表，props `rows/checked/showMissing`，emit `toggle` |
| 服务端 `server/internal/plugin/install/consent.go:22-29` | `NewPermissionRoles map[string][]string` |
| 服务端 `install/review.go:131-143` | `ReviewUserPerm{key,label,description,sensitive,default_roles,inherit_from,new}` |

### 2.2 角色编辑页

| 位置 | 现状 |
|---|---|
| `views/roles/RolesView.vue:39` | 并行拉取 `GET /permissions` 和 `/roles` |
| `RolesView.vue:69-72` | `builtinLocked`：内置角色非超管、且不在 `managed.assignable` 中时只读 |
| `RolesView.vue:141` | 保存：`PUT /roles/:id/permissions {permission_keys}`，替换整组 |
| `RolesView.vue:267-269` | “恢复默认”调 `POST /roles/:id/reset`，注释写明插件权限不动 |
| `RolesView.vue:359` | `<PermissionTree v-model:granted :modules :all-granted :read-only :scope-limit="auth.scopes">` |
| `views/roles/PermissionTree.vue:19` | props `modules, granted, allGranted, readOnly, scopeLimit` |
| `PermissionTree.vue:24` | 只分两组：核心模块、插件模块。模块内权限是平铺的网格（:120），没有分组 |
| `PermissionTree.vue:61` | 已移除（removed）的权限只能取消勾选 |
| `PermissionTree.vue:76-80` | 模块“全选” = 勾选模块内所有可选项，存具体 key（已经是 D1 的语义） |
| `PermissionTree.vue:106-107,135-136` | 模块级和单项都有“插件已禁用”“已移除”徽标 |
| `PermissionTree.vue:132` | `p.sensitive` 显示锁图标；没有风险分级，也不显示 `requires` |
| `PermissionTree.vue:140-149` | 带范围的项勾选后在下方展开 `ScopePicker`，`resource = p.scope_type as ScopeResource` |
| `views/roles/ScopePicker.vue:17,25-28` | props `action, resource('account'|'proxy'), scopes, offer`；分组、账号、代理三行写死 |
| `authz/implies.ts:21` | `SCOPABLE = SCOPE_TYPE`，取自生成的静态表 `generated/permissions.ts:72-81`，只有 8 个核心动作 |
| `implies.ts:53-61` | `parsePermission`：末段是 `*`、`own` 或带 `/` 时查 `SCOPABLE[action]`，**查不到返回 null**。所以 `plugin.m:flow:manage:flow/3` 现在解析不了，`auth.scopes()` 和 `permissionIndex` 都会忽略它 |
| `implies.ts:35-40` | `Scope{all,own,group,id}`，id 只能是数字 |
| `authz/scopeChoice.ts:215-226,281-300` | `ScopeChoice`、`offerOf`、`defaultScopes`（都是纯函数） |
| `stores/auth.ts:45-61` | `hasDynamic` 和 `scopes` 都基于 `permissionIndex`；超管直接通过 |
| 服务端 `server/internal/authz/roles.go:715-730` | `PermissionItem{key,label,description,sensitive,step_up,requires,scope_type,status}`，`requires` 目前只给核心项填 |

### 2.3 插件详情页

| 位置 | 现状 |
|---|---|
| `views/plugins/PluginDetailView.vue:70-85` | 标签依次为 overview、**grants**（访问权限）、nodes、hooks、jobs、events、egress、resources、settings、audit。`?tab=` 与路由同步（:87-90） |
| `PluginDetailView.vue:242` | 页面顶部 `RoleGrantsReview`：内置升级挂起的角色授予，可确认或拒绝 |
| `views/plugins/detail/GrantsTab.vue:38,57` | `GET /plugins/:key/grants` 列表；`DELETE /plugins/:key/grants/:permission` 撤销。服务端只有这两条路由（`plugin/api/api.go:96-97`），没有单独的“收窄 / 重新授予”接口 |
| `views/plugins/pluginUtil.ts:9,49` | 访问权限的风险是 `low|medium|high|critical`，和用户权限的 `low|sensitive|critical` 是两套 |

### 2.4 公共件

| 位置 | 现状 |
|---|---|
| `composables/useAssignableRoles.ts:37-87` | `GET /me/assignable-roles` 的单例缓存，提供 `get/assignable/manageable` |
| `components/entity/entities.ts:7,31` | `EntityKind` 是封闭联合类型，`ENTITIES[kind].path` 拼成 `/lookup/{path}` |
| `components/entity/lookup.ts:96,152,162` | 查找时取 `ENTITIES[kind].path`；`numeric` 决定是否按 id 反查 |
| `ENTITIES[…]` 的直接读取 | `EntityPicker.vue:63`、`EntityMultiPicker.vue:38,58`、`entity/entities.ts:76`、`lookup.ts:96,152,162`、`views/usage/usageFilters.ts:78` |
| `packages/ui/src/SOptionMatrix.vue:52` | 手机端粘性首列的写法 `max-md:sticky max-md:left-0 max-md:z-[1] max-md:bg-surface`。但它是“每行单选”（radiogroup），而我们每个格子是独立的值，还带范围，**不能直接复用组件**，只复用这套样式 |
| `components/SettingsSaveBar.vue` | 重置 + 保存按钮，props `dirty/saving/canManage` |
| `composables/confirmDiscard.ts` | 离开有未保存修改的编辑器时统一确认 |
| i18n `locales/{zh,en}/roles.ts`、`plugins.ts`；`i18n/locales.spec.ts` | 中英两份键必须一致；代码里出现的字面 `t()` 键必须存在 |
| mock：`web/mock/resources.ts:101-108`（`/permissions`）、`:202-216`（`PUT /roles/:id/permissions`，用 `parsePermission` 校验）、`mock/plugins.ts:368,832`（review） | 手机布局 e2e 跑的就是这个 mock |
| `web/e2e/phone-layout.test.mjs:74` | `TABS_PAGE = '/plugins/guard?tab=settings'`：标签栏横向滚动的回归用例 |

---

## 3. 接口形状（前端消费；字段名已与后端对齐）

### 3.1 `GET /permissions`（第 2 章，已对齐）

```ts
// api/types.ts
export type PermRisk = 'low' | 'sensitive' | 'critical'

export interface PermissionGroup { key: string; label: LText; sort: number }

/** 插件权限的范围声明（后端 manifest.PermissionScope 原样输出），只有插件项有。GET /permissions 与 review 同形。 */
export interface PluginScopeDecl {
  /** 资源名，不带 "plugin:" 前缀；静态枚举时为 "value"（后端总是填 ResourceName()）。 */
  resource: string
  /** 可选“自己创建的”（omitempty，缺省为 false）。 */
  own?: boolean
  /** 插件内的候选路由（如 "/scopes/flow"）；只是展示用，前端统一走第 4 章的 scope-options 接口。 */
  options?: string
  values?: Array<{ key: string; label: LText }>
}

export interface PermissionItem {
  key: string
  label: LText
  description?: LText
  /** 取代 sensitive（第 2 章：JSON 去掉 sensitive）。核心项：superOnly→critical，sensitive→sensitive，其余 low。 */
  risk: PermRisk
  step_up?: boolean
  /** 核心："account"|"proxy"；插件："plugin:<resource>"，静态枚举："plugin:value"；"" 表示不带范围。 */
  scope_type?: string
  scope?: PluginScopeDecl
  /** 插件 permissionGroups 的 key，只有插件项有。 */
  group?: string
  /** 弱依赖，完整 key（核心和插件都填）。 */
  requires?: string[]
  /** 内置角色的建议：角色 → 完整规范 key（如 ["plugin.m:flow:manage:own"]），能直接和 permission_keys 比较；critical 没有这个字段。 */
  defaults?: Partial<Record<'admin' | 'operator' | 'supplier' | 'user', string[]>>
  status: 'active' | 'disabled' | 'removed' | string
}

export interface PermissionModule {
  module: string
  label: LText
  source: string
  plugin_key?: string | null
  status: string
  /** 已排序，只有插件模块有。模块内权限按 group_sort、sort 排好。 */
  groups?: PermissionGroup[]
  permissions: PermissionItem[]
}
```

`generated/permissions.ts` 里的 `PermMeta.sensitive` 改为 `risk`，由第 2 章的 tsgen 生成。前端凡是读 `sensitive` 的地方都要改，见 §5 改动清单。

### 3.2 按插件的角色授权（第 2 章新增，已对齐；TS 类型名与 Go 的 `PluginRoleMatrix`/`PluginRoleRow` 一致；不用 `PluginRoleGrants`，避免和已有的 `PluginRoleGrant` 混淆）

```ts
export interface PluginRoleRow {
  id: number
  key: string
  name: LText
  builtin: boolean
  superuser: boolean
  /** 调用者能否改这个角色（服务端 editableRole 的结论）。 */
  editable: boolean
  denied?: 'superuser_role' | 'not_editable' | 'not_assignable'
  /** 只含该插件的 key，removed 的也在内。 */
  permission_keys: string[]
}
export interface PluginRoleMatrix {
  plugin_key: string
  module: PermissionModule
  roles: PluginRoleRow[]
}
```

- `GET /api/v1/permissions/plugins/:plugin`，需要 `role:read`，返回 `PluginRoleMatrix`。
- `PUT /api/v1/permissions/plugins/:plugin`，需要 `role:define`，body 为 `{ roles: Record<roleKey, string[]> }`。只替换列出的角色在该插件上的 key，单事务。403 时 `details` 为 `{missing?, missing_assignable?, role}`。成功返回与 GET 相同的结构。
- 被移除（removed）的 key：保存时留着就保留，去掉就删除授权；新加一个 removed 的 key 会被服务端静默丢弃，所以前端不允许勾选它。
- “恢复插件建议”完全由前端完成（已定）：后端不提供 reset 接口，角色编辑页和插件详情页都在本地按 `defaults` 改勾选，跟着整页一起保存，理由见 §4.6。

### 3.3 插件范围候选（第 4 章，已对齐）

```
GET /api/v1/plugins/:key/scope-options?permission=<本地 key>&q=&ids=a,b&limit=20
→ { items: [{ id: string, name: string, hint?: string, status?: string }], more: boolean }
```

- 和 `/lookup/{kind}` 同形，但 id 总是字符串。limit 取 1..50，ids 最多 100 个。给了 ids 时忽略 q 和 limit。
- 鉴权是 `role:read` 或 `plugin:read`。候选值按调用者自己对该动作的范围过滤：编辑者只看到自己能授出的值，超管看到全部。不在范围内的 id 不返回，界面显示原值。
- 静态枚举（`values`）由核心直接从清单回答，插件停用时也能用。插件资源由核心转发给插件；插件没运行时返回 `503 plugin_unavailable`。
- `own` 不出现在 items 里，由清单 `scope.own` 决定显示。
- 存储形式：`<resource>/<id>`，枚举为 `value/<key>`。完整 key 为 `plugin.<p>:<local>:<resource>/<id>`。
- 范围语法（第 1 章 `perm`，`vectors.json` 锁定）：`res = ^[a-z][a-z0-9_]{0,29}$`，`value = ^[A-Za-z0-9_.-]{1,64}$`。

### 3.4 安装 / 升级 review 与 consent（第 3 章，已对齐）

```ts
/** consent 里一格的值：true（无范围），或范围数组："*"（全部）、"own"、规范后缀 "value/<key>" / "<res>/<id>"（后端也接受清单写法的裸枚举 key）。 */
export type ManifestGrant = true | string[]

export interface ReviewCell {
  /** 这次同意不覆盖时实际授予的规范化后缀：[] = 不授予，[""] = 全部，"own"、"value/input"、"flow/3"。 */
  grant: string[]
  /** 查看者能否命名这一格（服务端算，已包含 critical 规则）。 */
  nameable: boolean
  reason?: string                // superuser_role|not_editable|not_assignable|held|role_define|critical
  /** 这一格的默认 / 继承授予会进待复核（D2）。 */
  withheld: boolean
}
export interface ReviewRoleColumn { key: string; name: LText; builtin: boolean; superuser: boolean; nameable: boolean; reason?: string }

export interface ReviewUserPermission {
  key: string                    // 本地 key
  label: LText
  description?: LText
  risk: PermRisk                 // 取代 sensitive
  group?: string
  requires?: string[]            // 本地 key
  scope?: PluginScopeDecl        // 与 GET /permissions 同形，resource 总是填
  /** 插件建议：角色 → 规范后缀（up.DefaultGrants()，"" = 全部），与 cells.grant 同形，前端用 keysFromSuffixes 转完整 key。取代 default_roles。 */
  defaults?: Record<string, string[]>
  inherit_from?: string[]
  new?: boolean
  /** 上一版 removed、本版重新声明：带原授权恢复（D6），只展示。 */
  revived?: boolean
  /** inheritFrom 命中已有 key：按继承授予，不套 defaults。 */
  renamed?: boolean
  /** 覆盖 role_columns 里的所有角色。 */
  cells?: Record<string, ReviewCell>
}
export interface PluginReview {
  // …原有字段…
  permission_groups?: PermissionGroup[]
  removed_user_permissions?: Array<{ key: string; label: LText; holders: string[] }>
  /** 矩阵的列，按当前查看者计算。 */
  role_columns?: ReviewRoleColumn[]
}
export interface PluginRoleGrant {
  version?: string
  permission: string             // 完整 key
  role: string
  scope?: string                 // 后缀，"" = 全部
  risk: PermRisk                 // 取代 sensitive
  via: 'default' | 'inherit' | 'override'
  needs_consent: boolean
  nameable?: boolean
  withheld?: boolean
}
/** POST …/consent：本地 key → 角色 → true | 范围数组。出现的 key 整体覆盖该权限：未列出的角色 = 不授予，{} = 都不给。 */
export type NewPermissionRoles = Record<string, Record<string, ManifestGrant>>
export interface ConsentResult { withheld?: PluginRoleGrant[] }
```

第 3 章定下的语义，前端照此实现：

1. **置灰以服务端的 `role_columns` / `cells[].nameable` 为准**，确认页不再自己调用 `useNameableRoles` 计算。原因：editableRole 要比对角色现有授予是否在操作者之内，前端拿不到这些数据。`useNameableRoles` 只留给 `RoleGrantsReview`。
2. 不可命名格的默认 / 继承授予**一律挂起待复核**，包括这一行被覆盖的情况（第 3 章采纳了本章的建议）。覆盖时，可命名角色里没写到的算“不授予”；覆盖里如果写了不可命名的角色，返回 403 `details.role`。所以前端：覆盖的行只发可命名角色；原来的 `dropsDefaults` 提示整个删除，不再需要继任者。`ConsentResult.withheld` 会列出全部挂起项。
3. 升级时，未覆盖的不可命名格进入待复核，不再阻止提交。`ConsentView` 的 `blockedRoles`（`ConsentView.vue:113-118`）删除。
4. critical 被拒：403 `permission_denied`，`details {reason:"critical", superuser:true, permissions:[本地 key…]}`。前端已经锁住这些格子，正常情况下碰不到；万一碰到，通过 `notifyError` 显示 `plugins.matrix.criticalOnly`。
5. `role_keys_for_new_permissions` 删除，前端从来没有发过它。

---

## 4. 设计

### 4.1 纯函数层：插件 key 的范围解析（`authz/implies.ts`、`authz/scopeChoice.ts`）

插件 key 的范围**按语法解析**，不依赖目录注册表。清单规则保证插件本地 key 的末段不会是 `*`、`own`，也不含 `/`（BRIEF），所以“插件动作 + 范围末段”没有歧义。auth store 在 `/permissions` 还没加载时也能正确解析 `me.permissions`。

```ts
// implies.ts
export interface Scope {
  all: boolean; own: boolean; group: number; id: number
  /** 插件范围 "<res>/<value>"（如 "flow/3"、"value/input"）；核心范围不设。 */
  ref?: string
}
const PLUGIN_REF = /^[a-z][a-z0-9_]{0,29}\/[A-Za-z0-9_.-]{1,64}$/
const isPluginAction = (action: string) => action.startsWith('plugin.')

export function parsePermission(k: string): Pattern | null {
  const i = k.lastIndexOf(':')
  const last = k.slice(i + 1)
  if (i < 0 || (last !== '*' && last !== 'own' && !last.includes('/'))) return { action: k, scope: ALL_SCOPE }
  const action = k.slice(0, i)
  if (isPluginAction(action)) return pluginPattern(action, last)      // 新增：一层分支，不嵌套
  const resource = SCOPABLE[action]
  const scope = resource && parseScope(resource, !takesRowIds(action), last)
  return scope ? { action, scope } : null
}
function pluginPattern(action: string, last: string): Pattern | null {
  if (last === '*') return { action, scope: ALL_SCOPE }
  if (last === 'own') return { action, scope: OWN_SCOPE }
  return PLUGIN_REF.test(last) ? { action, scope: { all: false, own: false, group: 0, id: 0, ref: last } } : null
}
// scopeSuffix：在 own 之后加一行  if (s.ref) return s.ref
// scopeImplies：在 own 判断之后加一行  if (h.ref || w.ref) return h.ref === w.ref
// scopeAllowsRow：s.ref → false（控制台不判断插件行，插件自己判断）
```

`scopeChoice.ts`：

```ts
export interface ScopeChoice { /* …原字段… */ refs: string[] }       // 插件 "<res>/<value>"
// choiceOf：在 own 之后加  else if (s.ref) c.refs.push(s.ref)
// scopesOf：并入 ...c.refs.map(refScope)
export type RefOffer = 'any' | string[] | null
export interface ScopeOffer { /* …原字段… */ refs: RefOffer }
// offerOf：refs = all ? 'any' : (limit 中有 ref 的值列表，空则为 null)
```

两份实现跑同一份向量 `next/sdk/permvectors/vectors.json`，插件向量由第 1 章（sdk-perm）添加，前端 `implies.spec.ts` 不需要改代码就会读到。改完以后，`auth.scopes('plugin.m:flow:manage')`、`hasAction`、`has('plugin.m:flow:manage:flow/3')` 自动支持插件 key。第 4 章要求的插件页面 `permissions.hasAction(...)` 依赖的就是这一点。

### 4.2 插件授权的纯函数（新文件 `authz/pluginGrants.ts`）

矩阵、PermissionTree 的插件模块和确认页共用一个数据形状：**角色 → 该插件的完整规范 key 列表**（`RoleKeys`）。它与第 2 章的 `PUT` body、`roles[].permission_keys`、`defaults` 的值完全同形，读写都不用转换。

```ts
import { canonicalPermission, parsePermission, type Scope, patternKey } from './implies'

/** 角色 → 该插件上的完整规范 key。 */
export type RoleKeys = Record<string, string[]>

export const actionOf = (key: string) => parsePermission(key)?.action ?? key
/** keys 里属于 action 的那几条（一个格子）。 */
export function cellKeys(keys: readonly string[], action: string): string[]
/** 把 action 的格子换成 next，其余不变；结果规范化、去重、排序。 */
export function withCell(keys: readonly string[], action: string, next: readonly string[]): string[]
/** 格子 → ScopePicker 的 Scope[]，以及反向转换。 */
export function scopesOfCell(keys: readonly string[], action: string): Scope[]
export function cellOfScopes(action: string, scopes: readonly Scope[]): string[]
export const sameKeys = (a: readonly string[], b: readonly string[]) => a.length === b.length && a.every((k) => b.includes(k))

/** 插件建议：从各权限的 defaults 汇总出 RoleKeys。 */
export function suggestedOf(perms: readonly { defaults?: Record<string, string[] | undefined> }[]): RoleKeys

export interface CellChange { action: string; role: string; from: string[]; to: string[] }
/** base 与 next 的差异，每个格子一条（用于“已改”标记、保存栏计数、未保存摘要）。 */
export function diffCells(base: RoleKeys, next: RoleKeys, actions: readonly string[], roles: readonly string[]): CellChange[]

/** 被风险等级挡住的格子统计（“将把 N 项敏感权限授予 M 个角色”）。 */
export function riskSummary(value: RoleKeys, perms: readonly { key: string; risk: PermRisk }[], risk: PermRisk): { perms: number; roles: number }
```

为什么要新建一个文件：`scopeChoice.withScopes`（`scopeChoice.ts:257`）操作的是 `Set<string>`，只针对单个动作；矩阵需要的是“多角色 × 多动作”的比较和汇总。`withCell` 内部直接复用 `withScopes`，不重新实现。

### 4.3 矩阵状态：`useGrantMatrix`（新文件 `components/permission/useGrantMatrix.ts`）

```ts
export type CellLock =
  | null                // 可编辑
  | 'superuser'         // 超管列：固定全有
  | 'pending_review'    // 安装 / 升级：安装者不能命名 → 待超管复核（D2）
  | 'critical'          // 安装 / 升级：critical 行，非超管（D3）
  | 'not_held'          // 详情页：编辑者自己没有该 key（覆盖规则），只能取消
  | 'role_readonly'     // 详情页：角色不可编辑（roles[].editable=false）
  | 'removed'           // 已移除：只能取消

export interface MatrixPerm {
  /** 完整动作 key：plugin.<p>:<local>。 */
  action: string
  local: string
  label: LText
  description?: LText
  risk: PermRisk
  group?: string
  requires?: string[]         // 完整 key
  scope?: PluginScopeDecl
  defaults?: Record<string, string[] | undefined>
  status: string
}
export interface MatrixRole { key: string; name: LText; builtin: boolean; superuser: boolean }

export function useGrantMatrix(opts: {
  perms: Ref<MatrixPerm[]>
  roles: Ref<MatrixRole[]>
  /** 已保存的状态：确认页 = 不覆盖时的实际授予（cells.grant）；详情页 = 服务端当前授权。 */
  baseline: Ref<RoleKeys>
  lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
}) {
  const value = ref<RoleKeys>({})                       // 当前选择；baseline 变化时重置
  const suggested = computed(() => suggestedOf(opts.perms.value))
  const cell = (p: MatrixPerm, r: MatrixRole) => cellKeys(value.value[r.key] ?? [], p.action)
  function setCell(p: MatrixPerm, r: MatrixRole, keys: string[]): void        // 锁住的格子只接受“更少”
  function setGroup(group: string, r: MatrixRole, on: boolean, offer: (a: string) => ScopeOffer): void // D1：展开成具体 key
  function restore(p?: MatrixPerm): void                // 行级 / 整表恢复为插件建议，跳过锁住的格子
  const differsFromSuggested = (p: MatrixPerm, r: MatrixRole) => !sameKeys(cell(p, r), cellKeys(suggested.value[r.key] ?? [], p.action))
  const changes = computed(() => diffCells(opts.baseline.value, value.value, actions, roleKeys))
  const dirty = computed(() => changes.value.length > 0)
  const reset = () => (value.value = clone(opts.baseline.value))
  return { value, suggested, cell, setCell, setGroup, restore, differsFromSuggested, changes, dirty, reset }
}
```

- 两种标记分开算。“与建议不同”（`differs`，三处同名、中性灰点，见 §4.9）在三处都显示。“未保存的修改”（`changes`）只在详情页作为保存栏显示；确认页用它决定发哪些行（payload）。确认页的 baseline 是 `cells.grant`，大多数情况下等于插件建议，带继承时比建议多。
- `setGroup`（现为 `toggleGroup`）落实 D1。勾选时，无范围的项写裸 key，带范围的项写 `defaultScopes(offer)` 的 key（`scopeChoice.ts:297-300`，取能提供的最宽范围），跳过锁住、removed 和 critical 的项（critical 只能逐项勾，§4.9）。所以存下来的永远是具体 key，不会有通配。

### 4.4 共用组件 `PermissionMatrix.vue`（新文件 `components/permission/PermissionMatrix.vue`）

```ts
const props = withDefaults(defineProps<{
  matrix: ReturnType<typeof useGrantMatrix>
  perms: MatrixPerm[]
  groups?: PermissionGroup[]
  roles: MatrixRole[]
  pluginKey: string
  lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
  /** 编辑者对动作的范围（auth.scopes），决定范围下拉框的可选项；缺省为全部范围。
   *  确认页不传：同意时不要求持有新权限（consent.go），能否命名由 Review 的 cells.nameable 决定。 */
  scopeLimit?: (action: string) => Scope[]
  /** 自定义角色收进“更多角色”（确认页 true；详情页 false）。 */
  collapseCustom?: boolean
  /** 首屏显示的行数，超过时“另有 N 项，展开查看”（详情页传 5；确认页不折叠）。 */
  foldAfter?: number
  readOnly?: boolean
}>(), { groups: () => [], collapseCustom: false, foldAfter: 0, readOnly: false })
// 不发事件：状态都在 matrix（useGrantMatrix 返回值）里，父组件读 matrix.value / changes。
// 插槽：
//   #toolbar   表头右侧（父组件放额外按钮）
//   #trailing="{ perm }"   每行最后一列（升级页的“需要确认”列）
//   #trailing-head
```

**为什么传 composable 而不是 `v-model`**：三个入口都需要读 `changes`、`dirty`、`restore`、`suggested`。放在 composable 里，父组件和矩阵共用一份状态，不必再定义一组 emit 往回传。`PermissionTree` 用 `v-model:granted` 是因为它只有一个 Set，这里不是。

结构（`<table>`，行列表头有无障碍语义）：

```
┌ 工具栏：与插件建议不同 N 处 · [恢复插件建议] · [更多角色 ▾] · #toolbar
├ <div class="overflow-x-auto"> <table>
│   thead: 权限(sticky) │ 超级管理员 │ 管理员 │ 运维 │ 供应商 │ 普通用户 │ [自定义…] │ #trailing-head
│   ── 分组行（groups 有时）：组名(sticky) │ 每列一个三态 SCheckbox（D1 整组）│
│   ── 权限行：
│      th(sticky)：名称 · RiskBadge · 依赖提示 · font-mono 完整 key · 行内“恢复”(行 differs 时)
│      td×角色：
│        无范围 → SCheckbox
│        有范围 → SSelect(sm)：不授予 / 全部 / 自己创建的(scope.own) / 指定…（当前为指定时显示“指定 N 项”）
│        格子 differs → 右上角圆点 ●（title=与插件建议不同）
│        锁 → disabled + 图标 + title（见下表）
│      td：#trailing
│   </table></div>
├ 展开面板（表格下方、滚动区域外）：「运维 · 编辑审核流程」的范围 + ScopePicker + [完成]
└ 图例：风险徽标 · ● 与插件建议不同 · 待超管复核
```

各种锁的显示：

| CellLock | 显示 | 能否改 |
|---|---|---|
| `superuser` | 整列勾选、置灰，表头 title “超级管理员拥有全部权限” | 否 |
| `pending_review` | 灰底（`bg-warning-50 dark:bg-warning-900/20`）。`cells.withheld` 时显示建议值（只读），旁边 SBadge “待超管复核”；否则只显示锁和 `reason` 文案（title） | 否 |
| `critical` | 行内所有非超管格子禁用，行首提示“仅超级管理员可授予” | 否 |
| `not_held` | 未勾选时禁用，title “你没有这项权限，不能授出”；已勾选的可以取消 | 只能减少 |
| `role_readonly` | 整列置灰，表头锁图标，title “你不能修改此角色” | 否 |
| `removed` | 行首 SBadge “已移除”；未勾选时禁用 | 只能减少 |

**范围格子用“下拉 + 展开 ScopePicker”。** 下拉选项由 `offerOf(scopeLimit(action))` 决定：只有 `offer.all` 时出现“全部”，只有 `scope.own && offer.own` 时出现“自己创建的”，只有 `offer.refs` 时出现“指定…”。选“指定…”或点“指定 N 项”时，设置 `expanded = {action, role}`，在表格**下方**渲染 `ScopePicker`（复用组件，见 §4.5），其中 `scopes = scopesOfCell(...)`，`@update:scopes` 调 `matrix.setCell(p, r, cellOfScopes(...))`。面板放在表格下方而不是插在行内，原因是行内面板在手机上会跟着表格横向滚动、被截断。同一时间只展开一个格子。

**手机布局**：
- 外层 `overflow-x-auto`。首列 `th` 在所有宽度都加 `sticky left-0 z-[1] bg-surface`，悬停时 `group-hover:bg-surface-2`，写法同 `SOptionMatrix.vue:52`。差别是这里在所有宽度都 sticky，因为角色列可能超过 7 个。
- 首列宽度：手机 `min-w-[9rem] max-w-[11rem]`（名称最多两行截断，完整 key 隐藏到 title），`md` 起 `min-w-[16rem]`。角色列 `min-w-[5.5rem]`，表头文字截断，完整名称放 title。
- 工具栏 `flex-wrap`；展开面板宽度 100%，不会让页面横向滚动（e2e 断言，见 §6.3）。

**暗色**：只用语义 token（`bg-surface`、`bg-surface-2`、`border-line`、`text-fg`、`text-fg-subtle`，与 `PermissionTree` 相同）和 SBadge 的 tone。少数手写色都同时写 `dark:` 变体：差异圆点 `bg-primary-500 dark:bg-primary-400`，待复核格子 `bg-warning-50 dark:bg-warning-900/20`。

**风险徽标**：新文件 `components/permission/PermRiskBadge.vue`，props 为 `{risk: PermRisk}`。`low → SBadge tone="gray"`，`sensitive → tone="warning"`，`critical → tone="danger"`，文字取 `roles.risk.<risk>`。之所以不复用 `views/plugins/parts/RiskDot.vue`：RiskDot 对应的是访问权限的 4 级（`pluginUtil.ts:63`），两套风险等级不能共用 i18n 键。

### 4.5 `ScopePicker` 支持插件范围；`EntityPicker` 增加数据源

**ScopePicker**（`views/roles/ScopePicker.vue`）只增加一个 prop 和一块模板，原有三块不变：

```ts
const props = defineProps<{
  action: string
  resource: ScopeResource | ''        // 插件动作传 ''
  scopes: Scope[]
  offer: ScopeOffer
  disabled?: boolean
  /** 插件动作：清单 scope 声明与插件 key。 */
  plugin?: { key: string; local: string; scope: PluginScopeDecl }
}>()
```

```
<!-- <res>/<value>：插件资源或静态枚举 -->
<div v-if="plugin && (offer.refs || choice.refs.length)" data-testid="scope-refs">
  <SHint>指定{{ 资源名 }}</SHint>
  静态枚举：SChip 切换按钮，列出 plugin.scope.values（不在 offer.refs 里的置灰，已持有的可取消）
  插件资源：已选的 SChip（可关闭，文字 = 候选名，查不到时显示原值）
            + EntityPicker :kind="pluginScopeKind(plugin.key, plugin.local)"  （只在 offer.refs==='any' 时显示）
            或 offer.refs 为列表时，用 SChip 切换按钮列出
</div>
```

“全部”“自己创建的”两个复选框保持原样（`ScopePicker.vue:52-69`），其中 own 在插件场景下按 `offer.own || choice.own` 显示。分组、账号、代理三块已经由 `grouped` / `rows` 控制，插件动作时 `resource=''`，这三块自然不显示。资源名：枚举时用 `roles.scope.values`（“指定值”）；插件资源时用 `roles.scope.items`（“指定项”）。清单没有资源的显示名，见 §7 未决问题。

PermissionTree 调用时，`resource` 改为 `p.scope_type?.startsWith('plugin:') ? '' : p.scope_type`，并传 `plugin`。

**EntityPicker / lookup 新数据源**：不扩展 `ENTITIES` 这张静态表，而是新增一个按 kind 取描述的函数。插件 kind 是动态的：

```ts
// components/entity/entities.ts
export type CoreEntityKind = 'user' | 'group' | 'api_key' | 'account' | 'model' | 'proxy'
/** 插件范围候选：pscope:<plugin>/<本地权限 key>。 */
export type PluginScopeKind = `pscope:${string}/${string}`
export type EntityKind = CoreEntityKind | PluginScopeKind

export interface EntityDescriptor {
  /* …原字段… */
  /** 完整接口路径（不填时为 /lookup/{path}）。 */
  url?: string
  /** 固定查询参数（插件：{ permission }）。 */
  query?: Record<string, string>
  /** id 是不透明字符串：按 ids= 反查名称；收起时显示名称，查不到显示原值，不显示 "#id"。 */
  opaqueIds?: boolean
}
export const pluginScopeKind = (plugin: string, local: string): PluginScopeKind => `pscope:${plugin}/${local}`
export function entityOf(kind: EntityKind): EntityDescriptor {
  if (!kind.startsWith('pscope:')) return ENTITIES[kind as CoreEntityKind]
  const [plugin, local] = kind.slice(7).split(/\/(.*)/s)
  return {
    path: '', url: `/plugins/${encodeURIComponent(plugin)}/scope-options`, query: { permission: local },
    icon: 'plugin', label: 'entity.kind.pluginScope', numeric: false, opaqueIds: true,
    permissions: ['role:read', 'plugin:read']
  }
}
```

- 把 `ENTITIES[kind]` 的 7 处直接读取（§2.4）都改成 `entityOf(kind)`。改动是机械替换。
- `lookup.ts` 的 `lookupEntities` 用 `entity.url ?? '/lookup/' + entity.path`，并合并 `entity.query`。`name()` 和 `label()` 的判断从 `!numeric` 改为 `!numeric && !opaqueIds`。`ask()` 对 opaque id 不做 `isEntityId` 校验。
- `503 plugin_unavailable`（插件没运行）**不**放进 `UNAVAILABLE` 集合，因为插件稍后可能启动，不应在整个登录期间记住。这次查询算失败，选择器提示“候选暂不可用，可直接输入值”。对 opaque kind，复用 model 那条“使用 “xxx””的末行（`EntityPicker.vue` 中 `numeric=false` 且只绑 id 的分支），输入的值要符合 `value` 正则才接受。
- 缓存 realm 用 kind 本身（含 permission）。第 4 章说同一资源的多个权限会走同一插件路由，可以按资源共享缓存。前端暂不按资源合并：权限之间的过滤范围（调用者对各动作的范围）可能不同，合并会显示调用者授不出的值。

### 4.6 `PermissionTree` 的改动（角色编辑页）

新增 props（都可选，不传时行为与现在完全一样，`RolesView.spec` 现有用例不受影响）：

```ts
const props = defineProps<{
  modules: PermissionModule[]; granted: Set<string>; allGranted?: boolean; readOnly?: boolean
  scopeLimit?: (action: string) => Scope[]
  /** 正在编辑的角色 key：用来取插件 defaults[roleKey]，显示“插件建议”、差异计数和“恢复插件建议”。 */
  roleKey?: string
  /** 编辑者能否授出 key（auth.hasDynamic）；不能授出的未勾选项禁用。 */
  grantable?: (key: string) => boolean
}>()
```

1. **插件模块按 group 分组**：在模块内部再渲染一层。`m.groups?.length` 时，把权限按 `p.group` 分到各组，每组一个小标题行（组名 + 三态 SCheckbox“整组”）加原来的网格；没有 group 的项放进最后一个未命名组。核心模块没有 groups，渲染保持原样。整组勾选复用现有的 `withItem` reduce（`PermissionTree.vue:79` 的写法），范围项照旧取能提供的最宽范围，所以保存的是展开后的具体 key（D1）。
2. **风险徽标**：`:132` 的锁图标改为 `<PermRiskBadge>`。`low` 不显示徽标（核心权限大多是 low，满屏的“低风险”是噪音）。原型 C 在插件项上也标了“低风险”，所以插件模块内的 `low` 显示灰色徽标。图例 `sensitiveLegend` 改为三级说明。
3. **“插件建议”标记**：`roleKey` 存在且是插件项时，按 `sameKeys(cellKeys(granted, p.key), p.defaults?.[roleKey] ?? [])` 判断。相等且非空时显示 SBadge“插件建议”（tone=gray）；不相等时显示“已改”（tone=primary）。
4. **模块级“恢复插件建议”**：插件模块标题行显示“与插件建议不同 N 处”，后面是“恢复插件建议”按钮，只在 `roleKey ∈ {admin,operator,supplier,user}`、非只读、有差异时显示。点击后在本地把该模块的 key 换成 defaults，自己不能授出（`grantable`）的建议 key 跳过，然后 toast “N 项你不能授出，已跳过”。后端不提供 reset 接口（已定），在前端做的原因有两个：（a）与“重置”“保存”同属一个未保存编辑流程，用户能先看到再保存；（b）即时写入的 reset 会和编辑器里其他未保存的改动交错。内置角色的“恢复默认”（`RolesView.vue:269`）保持只管核心权限。
5. **不能授出**：`grantable` 存在且 `!grantable(p.key) && !isChecked(p.key)` 时禁用，副文本为“你没有这项权限，不能授出”。范围项交给 `offerOf`，现状已经是这样。这条对核心项同样生效，与服务端“授出 ⊆ 自己”一致；对应原型 C 的“只能勾选你自己拥有的权限”。
6. **依赖提示**：项已勾选且 `p.requires` 中有未勾选的 key 时，在副文本显示“依赖“{名称}””（warning 色）。名称从 modules 里按 key 查 label。只提示，不自动勾选。
7. **removed / disabled**：保留现有逻辑（`:61`、`:106-107`、`:135-136`）。模块 disabled 时标题徽标文案改为“插件已停用，授权保留”（`roles.pluginDisabledKept`），与原型 C 一致。

RolesView 只改一行：

```vue
<PermissionTree v-model:granted="perms" :modules="modules" :all-granted="selected.superuser" :read-only="readOnly"
  :scope-limit="auth.scopes" :role-key="selected.key" :grantable="auth.hasDynamic" />
```

### 4.7 安装 / 升级确认页

`consent/newPermissionRoles.ts` 改写：保留 `useNameableRoles`（`RoleGrantsReview` 还在用），`useNewPermissionRoles` 换成 `useConsentMatrix`，底层用 `useGrantMatrix`。列、初值、锁全部取自服务端的 `role_columns` / `cells`（§3.4 第 1 条）：

```ts
export function useConsentMatrix(review: Ref<PluginReview | null>) {
  const pluginKey = computed(() => review.value?.plugin_key ?? '')
  const rows = computed(() => asArray<ReviewUserPermission>(review.value?.user_permissions).filter((p) => p.new && !p.revived))
  /** 矩阵行：本次新建的权限（new 且非 revived，含 renamed）。升级页由此“只列新增”。 */
  const perms = computed<MatrixPerm[]>(() => rows.value.map((p) => toMatrixPerm(pluginKey.value, p)))
  const roles = computed<MatrixRole[]>(() => asArray<ReviewRoleColumn>(review.value?.role_columns))
  const cellOf = (p: MatrixPerm, r: MatrixRole) => rows.value.find((x) => x.key === p.local)?.cells?.[r.key]
  /** 初值 = 不覆盖时的实际授予（cells.grant：defaults 加继承），不是 defaults 本身。 */
  const baseline = computed<RoleKeys>(() => keysFromCells(perms.value, roles.value, cellOf))
  const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => {
    if (r.superuser) return 'superuser'
    const c = cellOf(p, r)
    if (c?.nameable) return null
    return c?.reason === 'critical' ? 'critical' : 'pending_review'   // pending_review 格显示 withheld 的建议值
  }
  const matrix = useGrantMatrix({ perms, roles, baseline, lockOf })
  /** new_permission_roles：只发与 baseline 不同的行；每行只含可命名角色（未列出的 = 不授予）。 */
  function payload(): NewPermissionRoles {
    return Object.fromEntries(perms.value.filter((p) => changedActions.value.has(p.action)).map((p) => [p.local,
      Object.fromEntries(roles.value.filter((r) => lockOf(p, r) === null).map((r) => [r.key, toManifestGrant(p, matrix.cell(p, r))]).filter(([, g]) => g !== null))]))
  }
  const changedActions = computed(() => new Set(matrix.changes.value.map((c) => c.action)))
  const summary = computed(() => ({
    sensitive: riskSummary(matrix.value.value, perms.value, 'sensitive'),
    pending: countWithheld(perms.value, roles.value, cellOf)   // 覆盖与否都计入（第 3 章：不可命名格总是挂起）
  }))
  return { matrix, perms, roles, lockOf, payload, summary }
}
```

`pluginGrants.ts` 里补两个转换函数（纯函数，有单测）。review 的 `defaults` 和 `cells.grant` 都是规范后缀，所以只要一个“后缀 → 完整 key”的函数（审查时去掉了原来解析清单写法的 `keysFromManifest`）：

```ts
/** 规范后缀 → 完整 key："" → action；其余 → action:<suffix>。用于 cells.grant 和 review 的 defaults。 */
export function keysFromSuffixes(action: string, suffixes: readonly string[]): string[]
/** 完整 key → consent 的值：无范围项 → true；带范围项 → 规范后缀数组（全部用 "*"，其余如 "own"、"value/input"、"flow/3"）；没有授予 → null（该角色不列出）。 */
export function toManifestGrant(p: MatrixPerm, keys: readonly string[]): ManifestGrant | null
```

`toMatrixPerm` 负责把本地 key 换成完整 key（`action = plugin.<p>:<local>`），`requires` 同样转换，`defaults` 用 `keysFromSuffixes` 转成 `Record<role, string[]>`。这样 `useGrantMatrix.suggested` 在三个入口里拿到的都是完整 key。`renamed` 行没有 defaults，`suggested` 为空，“恢复插件建议”对这些行不显示，行上显示“继承自 {旧 key}”。

不再需要单独的“列来源” composable：确认页用 `role_columns`，详情页用 `GET /permissions/plugins/:plugin` 的 `roles`，两处的列都由服务端按查看者算好。

**ConsentView 的改动**（只换装配，不改其余逻辑）：

```vue
<ProvidesSummary :review="review" />                         <!-- 去掉 permission-roles 复选框，见下 -->
<SCard v-if="consent.perms.value.length" :title="t('plugins.matrix.title')" :subtitle="t('plugins.matrix.hint', { n })" data-testid="permission-matrix">
  <PermissionMatrix :matrix="consent.matrix" :perms="consent.perms.value" :groups="review.permission_groups"
    :roles="consent.roles.value" :plugin-key="review.plugin_key" :lock-of="consent.lockOf" collapse-custom>
    <template v-if="isUpgrade" #trailing-head>{{ t('plugins.matrix.needsConfirm') }}</template>
    <template v-if="isUpgrade" #trailing="{ perm }"><ConsentNeed :grants="grantsOf(perm)" /></template>
  </PermissionMatrix>
</SCard>
<UpgradePermissionChanges v-if="isUpgrade" :review="review" />  <!-- 已有 N 项不变、revived、removed_user_permissions -->
<NewRoleGrants :grants="inheritGrants" … />                    <!-- 只剩 via==='inherit' 的授予 -->
<HostPermissionTable … />                                       <!-- 不动 -->
```

- `ProvidesSummary.vue:202-233` 的用户权限条目改为“名称 + PermRiskBadge + key”的只读列表，删去角色复选框、`default_roles` 文案和 `permissionRoles` prop。矩阵已经覆盖了这部分功能，避免同一件事出现两个入口。
- `NewRoleGrants` 在确认页只显示 `via === 'inherit'` 的授予。default 授予已经由矩阵表达，`shownRoleGrants`（`ConsentView.vue:46-49`）删除。`RoleGrantsReview` 继续用 `NewRoleGrants` 显示挂起的授予：锁图标改为 `PermRiskBadge`，`permission` 原样显示（已带范围后缀）。
- `blockedRoles`（`:113-118`）删除，前提是第 3 章确认升级时不可命名角色走挂起（§3.4 第 2 条）。`canSubmit` 不再含这个条件。
- 页脚（`:296-317`）的 `<p>` 链在 `missingRequired` 之后加两行。summary 非零时显示“将把 {n} 项敏感权限授予 {m} 个角色”和“{k} 项建议待超管复核，确认后进入复核队列”。
- `submit()` 中 `new_permission_roles: consent.payload()`。响应若带 `withheld`，追加一个 info toast “{n} 项授权待超级管理员复核”。
- 升级页的“需要确认”列由新组件 `consent/ConsentNeed.vue` 显示（约 15 行）：从 `new_role_grants` 里取该权限的授予，有 `needs_consent` 时显示“需确认（敏感 / 自定义角色）”，否则显示“免确认，自动授予”。
- `UpgradePermissionChanges.vue`（新增，约 40 行，放在 `consent/`）对应原型 B 下半部分：“已有 N 项权限的授权保持不变，包括你在角色里撤销过的”；`revived` 列表“带原授权恢复”；`removed_user_permissions` 列表“本版移除 N 项：持有它的 M 个角色的授权会保留但不生效”。

### 4.8 插件详情“权限”标签

`PluginDetailView.vue:74` 的 `grants` 标签改为 `permissions`：

```ts
{ key: 'permissions', label: t('plugins.detail.tabs.permissions'), badge: d?.grants?.length }
```

旧链接 `?tab=grants` 在 `load()` 的标签校验（`:127`）之前映射为 `permissions`：

```ts
const TAB_ALIASES: Record<string, string> = { grants: 'permissions' }
```

新文件 `views/plugins/detail/PermissionsTab.vue`：

```vue
<script setup lang="ts">
// 插件的两类权限：①插件申请的访问权限（授权对象是插件本身，原“授权”标签：列表与撤销）；
// ②插件提供给用户的权限 × 角色（与角色页是同一份数据，GET/PUT /permissions/plugins/:plugin）。
const props = defineProps<{ detail: PluginDetail }>()
const emit = defineEmits<{ (e: 'changed'): void; (e: 'dirty', v: boolean): void }>()
const auth = useAuthStore()
const data = ref<PluginRoleMatrix | null>(null)
const perms = computed(() => (data.value?.module.permissions ?? []).map(toMatrixPermFromItem))
const roles = computed(() => data.value?.roles ?? [])
const baseline = computed<RoleKeys>(() => Object.fromEntries(roles.value.map((r) => [r.key, r.permission_keys])))
const lockOf = (p: MatrixPerm, r: PluginRoleRow): CellLock =>
  r.superuser ? 'superuser' : !r.editable ? 'role_readonly' : p.status === 'removed' ? 'removed'
  : !auth.hasDynamic(p.action) && !auth.hasAction(p.action) ? 'not_held' : null
const matrix = useGrantMatrix({ perms, roles, baseline, lockOf })
async function save() {
  const changed = new Set(matrix.changes.value.map((c) => c.role))
  const body = { roles: Object.fromEntries([...changed].map((k) => [k, matrix.value.value[k] ?? []])) }
  data.value = await api.put<PluginRoleMatrix>(`/permissions/plugins/${encodeURIComponent(props.detail.key)}`, body)
  // 403：details.role + missing / missing_assignable → SAlert（复用 RolesView 的 missingText，抽到 utils）
}
</script>
<template>
  <div class="space-y-4">
    <SCard :title="t('plugins.permissions.hostTitle')" :subtitle="t('plugins.permissions.hostHint')" :padded="false">
      <GrantsTab :detail="detail" embedded @changed="emit('changed')" />
    </SCard>
    <SCard v-if="auth.has('role:read')" :title="t('plugins.permissions.userTitle')" :subtitle="t('plugins.permissions.userHint')" data-testid="plugin-user-permissions">
      <SAlert v-if="detail.status === 'disabled'" tone="info">{{ t('plugins.permissions.disabledKept') }}</SAlert>
      <SEmpty v-if="!perms.length" :text="t('plugins.matrix.none')" />
      <PermissionMatrix v-else :matrix="matrix" :perms="perms" :groups="data?.module.groups" :roles="roles"
        :plugin-key="detail.key" :lock-of="lockOf" :scope-limit="auth.scopes" :fold-after="5" :read-only="!auth.has('role:define')" />
      <!-- 保存栏：sticky bottom，写法同 ConsentView 页脚 -->
      <div v-if="matrix.dirty.value" class="sticky bottom-0 …">
        <span>{{ t('plugins.matrix.unsaved', { n: matrix.changes.value.length }) }}：{{ 第一条变更摘要 }}</span>
        <SettingsSaveBar :dirty="true" :saving="saving" @save="save" @reset="matrix.reset" />
      </div>
    </SCard>
  </div>
</template>
```

- **①复用 GrantsTab**，不复用 HostPermissionTable。HostPermissionTable 是“同意前勾选”的形态（`checked`、`showMissing`、`toggle`）；GrantsTab 才是“已授予、可撤销”的形态，接口也已经接好。GrantsTab 加一个 `embedded` prop，去掉外层 `SCard`（由 PermissionsTab 包）。
- **原型 D 的“收窄 / 重新授予”（已定采纳）**：接第 3 章新增的 `PUT /plugins/:key/grants/:permission`（body `{scope?}`，省略时为清单请求的范围；返回更新后的 `PluginGrant`）。GrantsTab 操作列在“撤销”旁加两个按钮，写法照 `revoke()`（`GrantsTab.vue:47-65`）：
  - 已授予且范围里有数组项（`{domains:[…]}`、`{fields:[…]}` 这类）时显示“收窄”：新组件 `detail/NarrowGrantDialog.vue`（约 50 行）用 `scopeEntries`（`pluginUtil.ts`，`ScopeChips` 已在用）把范围列成可取消的标签，每个数组至少留一项，提交 `{scope}`；服务端用 `ScopeWithin` 拒绝更宽的范围（400 `scope_too_wide`）。确认页现在没有访问权限范围的编辑控件（`HostPermissionTable` 只用 `ScopeChips` 展示），所以这里不复用它；非数组形状（如 `limits`）首版不提供收窄按钮，只能撤销。
  - `status === 'revoked'`（或被拒）时显示“重新授予”：确认后提交 `{}`（按清单请求的范围）。
  - 两者都需要 `plugin:manage`；重新授予还需要 `plugin:install`（403 `details.permission=plugin:install`，没有时按钮置灰并在 title 说明），高 / 关键风险另需 `plugin:grant:high/critical`；路由挂在 grant step-up 下，HTTP 客户端遇到 step-up 会自动弹验证框（`components/stepUp/stepUp.ts:2`），GrantsTab 不需要自己处理。成功后 `load()` 并 `emit('changed')`，与撤销相同。
- **保存**只发有变更的角色，body 是这些角色在该插件上的完整 key（第 2 章：只替换列出的角色）。成功后用响应替换 `data`，baseline 随之重置。若编辑者自己持有被改的角色，调用 `auth.refreshPermissions()`，与 `RolesView.vue:183` 相同。
- **错误展示**：把 `RolesView.vue:196-207` 的 `missingText` 抽到 `utils/permErrors.ts`（`missingText(e, t)`），两处共用，多出的 `details.role` 显示为“{角色}：”前缀。
- **离开保护**：PermissionsTab 自己挂 `onBeforeRouteUpdate` / `onBeforeRouteLeave`（换标签、换插件、离开页面都是路由变化），有未保存的格子时 `confirmDiscard`（复用 `composables/confirmDiscard.ts`）；不再 `emit('dirty')`，PluginDetailView 只从 `?tab=` 推出当前标签。
- `RoleGrantsReview` 仍在页面顶部（`:242`），不挪进标签：它的主体是“待复核的版本”，要在所有标签上都看得见。
- 原型 D 的列顺序与确认页相同，自定义角色直接展开（`collapseCustom=false`）。

### 4.9 修复轮的前端变更（review2 / FIX.md）

- **术语**：两类权限处处同名——“插件申请的访问权限”（`plugins.permissions.hostTitle`）与“插件提供给用户的权限”（`userTitle`）；“与建议不同”一个名字、一个中性灰色；挂起统一叫“待超管复核”；英文风险等级分开（宿主 `Very high`，用户权限 `Security-critical`）。
- **批量跳过 critical**：矩阵的组勾选框与角色编辑页的模块 / 组全选都不勾 critical 项（`groupRows` / `bulk`）；只有组内含 critical 时复选框的 title 才说明要逐项勾，图例另写一次。
- **组件位置**：`ScopePicker`、`ScopeChipRow`、`scopeLabels` 移到 `components/permission/`（矩阵与角色页共用，不再由 components 反向依赖 views/roles）；“分组内账号”一行也用 `ScopeChipRow`，两步选择放进它的 `picker` 插槽。手机宽度下行内“恢复”图标隐藏，用工具栏的整表恢复。
- **保存前确认**：角色编辑页与详情页保存时，用 `newRiskyGrants(baseline, next, perms)` 列出新授出的 sensitive / critical 项（收窄不算），`confirmRiskyGrants` 弹确认（含 critical 时为 danger）。critical 插件 key 新授出时服务端还要 `plugin.grant` step-up（C3），由 HTTP 客户端的现有验证框处理。
- **版本与合并（C1）**：`GET /permissions/plugins/:plugin` 与角色对象带 `version`，矩阵 PUT 与 `PUT /roles/:id/permissions` 带上读到的版本。409 `version_conflict` 时自动重读，两处都用纯函数 `pluginGrants.replayCells(base, edited, fresh, …)` 把本地按格的改动重放到新数据上（矩阵在 `useGrantMatrix` 的 baseline watch 里调用，并过一遍锁；同一机制也让详情页在确认待复核后刷新 baseline）。全局版本号会因无关写入推进：重读后本次要保存的角色（详情页：本插件上的这些角色；角色页：本角色）与读到时相同，就带新版本自动重发一次，不再打扰；否则提示再保存，两边都改过的格子在提示里点名（以本地为准）。重读失败时保留已显示的数据与本地改动，可以再点保存。重放时被新锁拒绝的改动（角色不再可编辑、编辑者失去该动作）如实提示“N 处改动未能保留：…”。
- **授出视图（逻辑 #3）**：编辑者能授出的 = `/me` 的 key ∪ 自己角色的 `permission_keys`（排除 removed 动作），见 `useEditorGrants`。停用插件的 key 不在 `/me` 里但仍在角色上，所以不会被锁成“未持有”。`scopeLimit` 由组合函数内部使用，不再经 props 传。
- **恢复建议的共用规则（H1）**：`pluginGrants.restoreCell / suggestionState`，与服务端同口径按蕴含判断新授（格子已有的 key 蕴含的不算新授，所以把 `A` 收窄成建议的 `A:own` 不需要持有 A）；编辑者不能整格恢复的格子不动并计入“跳过 N 项”（toast），矩阵与角色编辑页同一函数，测试用例表共用（`authz/testing/restoreCases.ts`）。只读角色不显示“不能授出”。
- **继承行（C2 / C4）**：改名行可逐格编辑，初值取 Review 的 `cells.grant`，格上标“继承自 {旧 key}”；改过的行按普通行发送（可命名角色缺省或 `[]` = 不授予，不可命名的仍挂起）。查看者无 role:read 时服务端省略 `cells.<role>.grant` / `holders` / `via: inherit` 条目：继承格无初值时锁定为只读（`holders_hidden`），格内显示中性徽标“按继承”而不是空框，锁图例说明看不到持有者、提交时保持原授权（此时服务端也不返回该格的 `withheld`）；`holders` 缺省时“已移除”只说移除不列角色。
- **页面**：确认页自定义角色列在含授予或待复核格时自动展开（按钮带数量）；升级页用户权限只在变更卡里一行汇总，矩阵只列新建项，重新声明 / 移除项单列；未保存栏显示“角色 · 权限：原 → 新”，多条可展开；“指定…”保持选中，范围面板滚入视野并取得焦点，面板标题用资源名、权限名作提示；手机宽度下超管列收起为一行说明，锁原因有图例；依赖提示从全部声明的权限取名字。

---

## 5. 改动清单

| 文件 | 改什么 |
|---|---|
| `authz/implies.ts` | `Scope.ref?`；`parsePermission` 增加插件分支 `pluginPattern`；`scopeSuffix`、`scopeImplies`、`scopeAllowsRow` 各加一行 |
| `authz/scopeChoice.ts` | `ScopeChoice.refs`；`ScopeOffer.refs: RefOffer`；`choiceOf`、`scopesOf`、`offerOf` 各加一行 |
| `authz/pluginGrants.ts`（新） | §4.2 纯函数 |
| `api/types.ts` | §3.1、§3.2、§3.4 的类型；`PermissionItem.sensitive` 改 `risk`；`PluginRoleGrant.sensitive` 改 `risk`；`ReviewUserPermission` 换字段；新增 `PermRisk`、`PermissionGroup`、`PluginScopeDecl`、`PluginRoleRow`、`PluginRoleMatrix`、`NewPermissionRoles`、`ConsentResult` |
| `generated/permissions.ts` | 由第 2 章 tsgen 生成：`PermMeta.risk`。前端凡是用 `PERMISSIONS[k].sensitive` 的地方改为 `risk !== 'low'`（实现时 `grep -rn "\.sensitive" src` 逐个改） |
| `components/permission/useGrantMatrix.ts`（新） | §4.3 |
| `components/permission/PermissionMatrix.vue`（新） | §4.4 |
| `components/permission/PermRiskBadge.vue`（新） | 三级风险徽标 |
| `components/entity/entities.ts` | `EntityKind` 拆成核心 kind 和插件 kind；描述增加 `url/query/opaqueIds`；新增 `entityOf`、`pluginScopeKind`；`mayLookUp` 改用 `entityOf` |
| `components/entity/lookup.ts` | 用 `entityOf`、url、query、opaqueIds；503 不记入 unavailable |
| `components/EntityPicker.vue`、`EntityMultiPicker.vue`、`views/usage/usageFilters.ts` | `ENTITIES[kind]` 改为 `entityOf(kind)`；opaque kind 走“使用 “xxx””行和 value 正则校验 |
| `views/roles/ScopePicker.vue` | `plugin` prop；`resource` 允许 `''`；新增 refs 一块模板 |
| `views/roles/PermissionTree.vue` | §4.6 第 1–7 点 |
| `views/roles/RolesView.vue` | 传 `role-key`、`grantable`；`missingText` 移到 `utils/permErrors.ts` |
| `utils/permErrors.ts`（新） | `missingText(e, t)`，带 role 前缀 |
| `views/plugins/consent/newPermissionRoles.ts` | 保留 `useNameableRoles`；`useNewPermissionRoles` 替换为 `useConsentMatrix` |
| `views/plugins/consent/ProvidesSummary.vue` | 用户权限改为只读列表；删除 `permissionRoles` prop |
| `views/plugins/consent/NewRoleGrants.vue` | `sensitive` 锁图标改为 `PermRiskBadge` |
| `views/plugins/consent/ConsentNeed.vue`（新）、`UpgradePermissionChanges.vue`（新） | 升级页的需确认列和权限变化 |
| `views/plugins/ConsentView.vue` | 装配矩阵；删除 `shownRoleGrants`、`blockedRoles`；页脚汇总；payload；withheld toast |
| `views/plugins/detail/PermissionsTab.vue`（新） | §4.8 |
| `views/plugins/detail/GrantsTab.vue` | 增加 `embedded` prop；操作列加“收窄”“重新授予”（`PUT /plugins/:key/grants/:permission`） |
| `views/plugins/detail/NarrowGrantDialog.vue`（新） | 收窄对话框：`scopeEntries` 列出数组项，可取消，至少留一项 |
| `components/plugin/PluginIframe.vue` | 第 4 章 §4.3.8：`permissions.check` 的本地 key 不带范围时按“任意范围持有”判断（`hasAction`），带范围时按蕴含判断（`permissionIndex(me.permissions).has`，与服务端同口径：持有全部范围即持有 `…:own`、`…:flow/2`）；仍不用 `auth.has` |
| `views/plugins/PluginDetailView.vue` | 标签 `grants` 改为 `permissions`，加别名；离开确认 |
| `i18n/locales/{zh,en}/roles.ts`、`plugins.ts`、`entity.ts` | §5.1 |
| `web/mock/resources.ts` | `/permissions` 加 risk/group/groups/requires/scope/defaults，去掉 sensitive；新增 `GET/PUT /permissions/plugins/:plugin`、`GET /plugins/:key/scope-options`、`PUT /plugins/:key/grants/:permission`。guard 模块加一个带范围的项和一个 critical 项（演示用，对应原型 C/D）；`PUT /roles/:id/permissions` 的校验天然支持插件范围，因为 mock 引用的就是 `implies.ts` |
| `web/mock/plugins.ts` | review 的 user_permissions 换成新字段；加 `permission_groups`、`removed_user_permissions`；consent 接受新的 `new_permission_roles` 形状 |
| `web/e2e/phone-layout.test.mjs` | §6.3 |
| `web/packages/host/src/index.ts:15`、`web/packages/{host,ui,vite-preset}/package.json`、`web/packages/ui/src/ui-contract.spec.ts:17` | UI 版本 3.15.0 → **3.16.0**（与第 1 章 `uiFloors` 新增的 3.16 档同一版本；审查时发现原来没人负责）；`HostPermissions`（`index.ts:49-66`）的 `has/hasAction/scope` 注释补“插件 key 同样适用”（第 4 章 §4.3.8），类型不变 |

### 5.1 i18n 键（zh / en 两份都加，下面写中文）

`roles.ts`：

```ts
risk: { low: '低风险', sensitive: '敏感', critical: '关键' },
riskLegend: '风险：敏感 = 涉及资金、凭证、用户原文或封禁；关键 = 能绕过安全控制，只有超级管理员能授出。',
suggested: '插件建议',
changed: '已改',
differsCount: '与插件建议不同 {n} 处',
restoreSuggested: '恢复插件建议',
restoreSkipped: '{n} 项你不能授出，已跳过',
notHeld: '你没有这项权限，不能授出',
requiresHint: '依赖“{names}”',
groupSelectAll: '整组',
pluginDisabledKept: '插件已停用，授权保留',
scope: { …原有…, values: '指定值', items: '指定项', optionsUnavailable: '候选暂不可用，可直接输入值' }
```

`sensitive`、`sensitiveLegend` 删除（改用 `risk.*`）。删除前用 `locales.spec` 的“字面键存在”用例确认已经没人引用。

`plugins.ts`：

```ts
detail: { tabs: { permissions: '权限' /* 取代 grants */ } },
permissions: {
  hostTitle: '插件申请的访问权限', hostHint: '插件自己访问系统资源用，安装时批准。',
  userTitle: '插件提供给用户的权限', userHint: '分给哪些角色。与角色页同步，改这里等于改对应角色。',
  disabledKept: '插件已停用：授权保留，停用期间不生效，重新启用后原样恢复。'
},
matrix: {
  title: '插件权限', hint: '插件声明 {n} 项，可以分给角色。之后在“角色”页随时能改。',
  permission: '权限', superFixed: '超级管理员拥有全部权限', moreRoles: '更多角色', fewerRoles: '收起自定义角色',
  notGranted: '不授予', scopeAll: '全部', scopeOwn: '自己创建的', scopeCustom: '指定…', scopeCount: '指定 {n} 项',
  scopeOf: '「{role}」的「{perm}」范围', done: '完成',
  pendingReview: '待超管复核', pendingReviewHint: '你不能给这个角色分配，按插件建议挂起，由超级管理员复核。',
  inheritedFrom: '继承自 {keys}',
  criticalOnly: '仅超级管理员可授予', roleReadonly: '你不能修改此角色', differs: '与插件建议不同',
  foldMore: '另有 {n} 项，展开查看', foldLess: '收起',
  summarySensitive: '将把 {n} 项敏感权限授予 {m} 个角色。', summaryPending: '{n} 项建议待超管复核，确认后进入复核队列。',
  withheld: '{n} 项授权待超级管理员复核', unsaved: '未保存的修改 {n} 处',
  needsConfirm: '需要确认', autoGrant: '免确认，自动授予', needConfirmSensitive: '敏感，需确认', needConfirmCustom: '自定义角色，需确认',
  upgradeKept: '已有 {n} 项权限的授权保持不变，包括你在角色里撤销过的。',
  revivedTitle: '重新声明 {n} 项', revivedHint: '上一版移除、本版重新声明的权限，带着原授权恢复，不重新套用插件建议。',
  removedTitle: '本版移除 {n} 项', removedHint: '持有它的 {m} 个角色的授权会保留但不生效；以后版本再声明时按原样恢复。',
  none: '这个插件没有声明给用户的权限。', saved: '已保存角色授权'
},
grants: { /* …原有… */
  narrow: '收窄', narrowTitle: '收窄「{perm}」的范围', narrowHint: '去掉不再需要的项，至少保留一项。', narrowed: '已收窄',
  regrant: '重新授予', regrantConfirm: '按插件申请的范围重新授予「{perm}」？', regranted: '已重新授予'
}
```

删除：`consent.replacesDefaults`、`consent.defaultRoles`、`consent.noDefaultRoles`、`consent.grantToRoles`、`consent.roleGrantsBlocked`、`consent.roleNotNameable`（`blockedRoles` 删除之后没人用；`RoleGrantsReview` 不用它们），以及 `detail.tabs.grants`。

`entity.ts`：`kind.pluginScope: '插件数据'`。

---

## 6. 测试计划

### 6.1 单元测试（vitest，纯函数）

| 文件 | 用例 | 断言要点 |
|---|---|---|
| `authz/implies.spec.ts` | 共跑 permvectors（第 1 章加插件向量） | 插件 canonical / implies 向量全部通过 |
| 同上 | `parses plugin scoped keys by syntax without a catalog` | `plugin.m:flow:manage:flow/3` 解析得到 `ref='flow/3'`；`plugin.m:flow:manage:*` 等价于裸 key；`plugin.m:k:value/input` 得到 ref；`plugin.m:x:Flow/3`（res 大写）返回 null |
| 同上 | `plugin refs imply only themselves; all implies refs` | `scopeImplies(ALL, ref)` 为真；`ref≠ref'` 为假；own 与 ref 互不包含 |
| `authz/scopeChoice.spec.ts` | `choiceOf/scopesOf round-trip refs`、`offerOf offers refs any for all, held refs otherwise` | |
| `authz/pluginGrants.spec.ts`（新） | `cellKeys/withCell replace one action only`、`suggestedOf merges defaults per role`、`diffCells one change per cell`、`riskSummary counts perms and roles`、`keysFromSuffixes maps "" to the bare action`、`toManifestGrant: unscoped → true, scoped all → ["*"], value/own/id kept as suffixes, none → null` | withCell 结果规范化且排序；diffCells 对 `['plugin.m:a:*']` 与 `['plugin.m:a']` 视为相同；`action:value/input` 转成 `["value/input"]` |
| `components/entity/lookup.spec.ts` | `plugin scope kind queries /plugins/:key/scope-options with permission`、`opaque ids are named by ids= and shown raw when unknown`、`503 is not remembered as unavailable` | 请求路径和参数；`label()` 不出现 `#` |

### 6.2 组件测试

**`components/permission/PermissionMatrix.spec.ts`（新）**

- `starts at the suggested values and marks cells changed from them`：改一个格子后出现 `●`，行内“恢复”只恢复该行。
- `restore suggested skips locked cells`。
- `super admin column is ticked and fixed`。
- `pending_review cells show the suggestion read-only with the badge`。
- `critical row is disabled for a non-super editor`。
- `scoped cell offers only what scopeLimit implies, and "custom" opens ScopePicker below the table`：下拉选项随 offer 变化；面板出现在 table 之后；`update:scopes` 写回 cell。
- `group checkbox ticks every editable row of the group as concrete keys (D1)`：值里没有 `*` 结尾的模块通配，范围项取最宽范围。
- `custom roles fold under "more roles" when collapseCustom`。
- `keeps the structure: no unresolved components`：沿用 ConsentView.spec 的 `warnHandler` 写法。

**`views/plugins/ConsentView.spec.ts`（扩展）**

- 替换 `sends the ticked permissions as grants and roles only for the new permissions whose roles changed`：改为断言 `new_permission_roles = {'flow:manage': {operator: ['plugin.demo:flow:manage:own']}}`，未改的权限不出现。
- 替换 `unticking every default sends an empty list; ticking back to the defaults sends nothing`：某角色清空后发 `[]`；恢复建议后整个权限不发。
- 替换 `a non-super admin names only roles it manages and does not hold`：锁以 review 的 `role_columns`/`cells` 为准（mock 给 admin 列 `nameable:false, withheld:true`），admin 格显示“待超管复核”，payload 不含 admin，页脚出现待复核计数；改动该行后待复核计数不变，payload 仍不含 admin。
- 删除 `leaves the default grants of a permission given other roles out of the grant list`（`shownRoleGrants` 已删）；新增 `shows only inherit grants in the grant list`。
- 替换 `on an upgrade, approves grants that need consent only for whoever may name their roles`：改为 `on an upgrade, unnameable roles become pending instead of blocking`（未覆盖的 withheld 格不阻止提交）。
- 新增：`upgrade lists only new permissions, with revived and removed ones as information`、`critical permission cannot be granted by a non-super installer`、`footer sums sensitive grants and pending cells`、`toasts withheld grants after approval`。
- 保留其余 host permission 相关用例，它们不受影响。

**`views/roles/PermissionTree.spec.ts`（扩展）**

`groups plugin permissions under their group headings`、`group checkbox ticks the group as concrete keys`、`shows risk badges; low hidden in core modules`、`marks "suggested" and "changed" against defaults of the edited role`、`restore suggested sets the module to defaults and skips keys the editor cannot grant`、`disables an unticked permission the editor does not hold, allows unticking it`、`hints unmet requires`、`plugin scoped action opens ScopePicker in plugin mode`。

**`views/roles/ScopePicker.spec.ts`（扩展）**

`plugin values: toggles enum chips, offered per editor`、`plugin resource: picks values through EntityPicker of the plugin scope kind`、`hides group/account/proxy rows for plugin actions`。

**`views/roles/RolesView.spec.ts`（扩展）**

- `passes role key and grantable to the tree`（stub PermissionTree，断言 props）。
- `saves plugin scoped keys canonical with PUT /roles/:id/permissions`。
- `restore defaults still leaves plugin permissions to the plugin restore`（文案和接口不变）。

**`views/plugins/detail/PermissionsTab.spec.ts`（新）**

`shows host grants (GrantsTab) and the matrix`、`saves only changed roles with PUT /permissions/plugins/:plugin`、`a 403 shows the role and what was missing`、`role columns not editable are read-only`、`removed rows can only be unticked`、`without role:read only host grants show`、`disabled plugin shows grants kept`。

**`views/plugins/detail/GrantsTab.spec.ts`（新）**

`narrow sends PUT with the remaining items`、`narrow keeps at least one item per list`、`no narrow button for non-list scopes`、`regrant on a revoked row sends PUT {}`、`a 400 scope_too_wide shows the error`。

**`components/plugin/PluginIframe.spec.ts`（扩展，已有）**

`permissions.check of an unscoped local key is true for a holder of any scope`、`a scoped local key matches exactly`。

**`views/plugins/PluginDetailView.spec.ts`（若无则新建，最小）**

`?tab=grants opens the permissions tab`、`switching tabs with unsaved matrix asks first`。

**`i18n/locales.spec.ts`**：不改，自动覆盖新键的中英一致和字面键存在。

### 6.3 e2e

`next/e2e` 是 Go 的 API 级测试，不驱动浏览器（`phone-layout.test.mjs:3-4` 的注释）。前端 e2e 都是 `next/web/e2e` 下 node:test + Playwright 跑 mock：

1. 扩展 **`phone-layout.test.mjs`**：
   - 在页面清单里加 `/plugins/guard?tab=permissions` 和 `/plugins/guard/versions/<mock 待确认版本>/consent`，自动套上“无横向页面滚动”的断言。
   - 新增 `matrix scrolls inside itself with the first column pinned`：在 360px 宽度把矩阵容器滚到最右，首列 `th` 的 `getBoundingClientRect().left` 不变，`documentElement.scrollWidth ≤ innerWidth`。
   - 新增 `scope panel fits the screen`：打开某个格子的“指定…”，面板宽度不超过视口。
2. 新文件 **`next/web/e2e/plugin-permissions.test.mjs`**（桌面视口 + 一台手机）：
   - `install consent: matrix prefilled from defaults, change one cell, approve → mock receives new_permission_roles with only that permission`。
   - `as admin identity: admin column pending review, critical row disabled`（mock 已有多个身份）。
   - `upgrade consent: only new permissions listed; removed and revived shown`。
   - `plugin permissions tab: change operator scope via picker (search plugin options), save → role page shows the same keys`：验证两个入口改的是同一份数据。
   - `role editor: plugin module grouped, restore suggested, save`。
   - 暗色：每个用例用 `colorScheme: 'dark'` 再截一张，只断言矩阵首列背景不透明（`getComputedStyle(th).backgroundColor` 的 alpha 为 1），避免滚动内容透过粘性首列显示。
3. API 级的同一份数据校验（插件页与角色页一致、critical 拒绝、D2 挂起）归第 2/3 章的 Go e2e（`ac02_rbac_test.go`、`ac03_install_test.go`、`ac05_upgrade_test.go`），本章不写。

---

## 7. 风险与未决问题

1. **（已解决）覆盖一行时待复核建议的去留**：第 3 章已改为“不可命名格总是挂起”，前端不再需要 `dropsPending` 提示。剩下的约束是：可命名角色里覆盖没写到的算“不授予”，所以 payload 必须把该行所有可命名角色的当前值都带上（`useConsentMatrix.payload` 就是这么做的），由 `ConsentView.spec` 的 payload 用例保证。
2. **（已定）原型 D 的“收窄 / 重新授予”**：接第 3 章 `PUT /plugins/:key/grants/:permission`（§4.8）。首版只对数组型范围提供“收窄”，其它形状只能撤销。
3. **（审查定）插件资源的显示名**：清单 `scope` 首版不加 `label`（与“枚举资源名固定为 `value`、不加可选 resource”同一口径），ScopePicker 用通用文案“指定项 / 指定值”。以后要加只是可选展示字段，不影响存储。
4. **范围候选的缓存键**：第 4 章建议按资源共享缓存。前端按 permission 区分，原因是各权限的过滤范围可能不同（§4.5）。代价是同一资源会多请求几次，影响很小。
5. **`grantable` 对核心权限也生效**：角色编辑页会禁用“自己没有的核心权限”，这是行为变化，以前可以勾、保存时被拒。这与服务端规则一致，属于改进，但会改变现有用户的操作习惯，发布说明里要写。
6. **`sensitive` 字段整体删除**：第 2 章确认 JSON 去掉 `sensitive`，前端的 `PermissionItem.sensitive`、`PluginRoleGrant.sensitive`、`PermMeta.sensitive` 必须和后端同一版本一起改（不考虑兼容）。访问密钥（iam）选择器也读这个结构，第 2 章说会一起改。前端对应的组件在实现时 `grep sensitive` 找出来，列入同一个 PR。
7. **矩阵行数多时的性能**：插件权限一般不超过 30 项、角色不超过 10 个，表格不超过 300 个单元格，不需要虚拟滚动。`cell()` 每次都从 `value[role]` 里过滤，规模可以接受。
8. **双入口并发**：插件页和角色页改的是同一份数据，第 2 章的 PUT 只替换该插件的 key，所以和角色页对核心权限的编辑互不覆盖。但两个人同时在角色页编辑同一个角色，仍然是后写者覆盖（现状如此），不在本章范围。

---

## 8. 自审

- **复用**：`useNameableRoles`（仍用于 RoleGrantsReview）、`useAssignableRoles`、`fetchFirstPage`、`ScopePicker`、`EntityPicker` 与 lookup 缓存、`offerOf`/`defaultScopes`/`withScopes`、`GrantsTab`、`NewRoleGrants`、`RoleGrantsReview`、`SettingsSaveBar`、`confirmDiscard`、`SOptionMatrix` 的粘性样式、`missingText`（抽出共用）都直接复用。新增的抽象及原因：`pluginGrants.ts`（多角色 × 多动作，现有函数只处理单动作）、`useGrantMatrix`（三处入口共用的状态）、`PermissionMatrix`（每格独立取值且带范围，`SOptionMatrix` 是每行单选）、`PermRiskBadge`（用户权限三级，访问权限四级，不能共用）、`entityOf`（插件 kind 是动态的）。
- **低侵入**：`PermissionTree` 和 `ScopePicker` 新增的 props 都是可选的，不传时行为与现在一致；`implies.ts` 的插件分支是一层独立函数；`ENTITIES[...]` 换成 `entityOf` 是机械替换。
- **与已定方案一致**：D1 整组勾选保存为具体 key；D2 不可命名的格子标“待超管复核”且不发送；D3 critical 只有超管能授，矩阵锁定、PermissionTree 通过 `grantable` 禁用；D4 插件 key 不带账号范围，前端不为插件动作显示分组 / 账号行；D6 removed 只能取消、revived 只展示；停用时授权保留并照常显示。
- **边界**：只写前端消费的接口形状，接口实现和清单校验留给第 1–4 章；Go e2e 也留给后端章节。
