# 第 1 章 清单声明与权限 key 解析

基于 `/home/claude/sup2api-dev` develop @ dae6ba57（只读）。下文路径均相对 `next/`。

> 存储为历史：本目录是 0.1.289 时的设计。0.1.290 起角色的授予（含范围授予）与用户的角色只存在 `casbin_rule`（迁移 0100，CONTRACTS §99），下文的 `role_permissions`、`role_permission_scopes`、`user_roles` 及引用它们的语句与行号都是当时的写法，那三张表已删除。版本号也是当时的推测：这部分随 0.1.290 与 Casbin、会话一起发布，0.1.289 没有单独发布，`PluginPermissionsFirstCore`、UI 3.16 的核心下限和内置插件的 `hostCompat` 都是 0.1.290；`0.1.289-plugin-perm-down` 只用于停在 0099 的开发库，线上的升级与回退见 `deploy/rollback/0.1.290-release.md`。

## 1. 本章范围

负责：

- `sdk/manifest`：`UserPermission` 的新字段（`group` / `risk` / `requires` / `scope` / `defaults`）、顶层 `permissionGroups`、`Route.scopeFrom` 的 Go 结构与 JSON；删除 `sensitive` / `defaultRoles` 之后，用来替代 `Roles()` 等的辅助函数。
- `sdk/manifest/check`：上述字段的全部校验规则、退役字段的拒绝、UI 3.16 / 核心下限的登记（`uiFloors`、`coreFloors`）。
- `server/internal/perm`：让插件资源范围（`*`、`own`、`<resource>/<value>`）通过 `Parse` / `Canonical` / `Implies`，不碰核心 `scopable` 表；说明对 `core.PermissionSet`（`Has` / `Missing` / `Restrict` / `KeyList`）的影响。
- `sdk/permvectors`：补插件 key 的测试向量。
- 内置插件 manifest 的字段迁移表。

不负责（只给出接口约定，见 §7）：

| 内容 | 章节 |
|---|---|
| authz 迁移、`SyncPlugin`、`resolvePermissions` 对 scope_type 的语义校验 | 第 2 章 |
| 安装确认、`DefaultGrants` 落库、pluginGrantee / withholdGrants | 第 3 章 |
| `scopeFrom` 的运行时执行、`AuthzCheck` 范围、options 路由的代理调用 | 第 4 章 |
| 前端 `implies.ts`、ScopePicker、矩阵组件 | 第 5 章 |

## 2. 现状（已核实）

### 2.1 manifest 结构

- `Manifest.UserPermissions []UserPermission` 在 `sdk/manifest/manifest.go:48`，目前没有 `PermissionGroups`。
- `UserPermission`（`sdk/manifest/manifest.go:878-897`）有这些字段：`Key`、`Label`、`Description`、`Sensitive bool`、`DefaultRoles []string`、`InheritFrom []string`。
  - `DefaultRoles` 的 tag 是 `json:"defaultRoles"`，没有 omitempty，这样才能区分 `[]` 和省略。
- `Route`（`sdk/manifest/manifest.go:900-910`）有这些字段：`Method`、`Path`、`Scope`（admin|user|public|webhook）、`Permission`、`Accounts *RouteAccounts`。
- `sdk/manifest/permission.go` 的内容：
  - `BuiltinRoleKeys = {admin, operator, supplier, user}`（:11）。authz 的 `TestManifestBuiltinRoleKeysMatchBuiltinRoles` 让它和核心内置角色保持一致（`server/internal/authz/plugin_catalog_test.go:22`）。
  - `DefaultPermissionRoles = {"admin"}`（:15）。
  - `UserPermission.Roles()`（:20-25）：nil 时返回 admin，`[]` 时返回空。
  - `PermissionModule` / `PermissionKey` / `LocalPermission`（:29-47）。
- `RouteAccounts.From` 的 DSL 和解析：`ParseAccountsFrom(from) (source, name, ok)`（`sdk/manifest/route_accounts.go:49`）。
  - source 只能是 path / query / body，name 须匹配 `^[A-Za-z0-9_-]+$`（:44）。
- 宿主风险常量：`RiskLow/RiskMedium/RiskHigh/RiskCritical`（`sdk/manifest/manifest.go:1133-1136`），目前只有 hostPermissions 用。
- `pkg.ParseManifest` 容忍未知字段（`server/internal/plugin/pkg/package.go:61-72`）。
  - 所以旧核心会**静默忽略**新字段，必须靠 hostCompat 下限把旧核心挡在外面。

### 2.2 manifest/check

- 本地 key 正则 `userPermKeyRe = ^[a-z0-9_]+(:[a-z0-9_]+)+$`（`sdk/manifest/check/validate.go:23`）。
- `validUserPermKey` 另外禁止以 `:own` 结尾（:775-777）。
- `userPermissions()`（:779-804）检查以下几项：
  - key 格式：`PermissionKey` 总长 ≤150，否则 `invalid_format`。
  - 重复：`duplicate`。
  - label：`required`。
  - `defaultRoles[j]` 必须在 `BuiltinRoleKeys` 里：`invalid_value`。
  - `inheritFrom[j]` 不能是自己，并且要符合 key 格式：`invalid_value`。旧 key 允许不在本清单中声明，因为改名后旧 key 已经不再声明。
- `userPerm(p)`（:806-813）按本地 key 查找是否已声明。
- `routes()`（`sdk/manifest/check/routes_check.go:11-53`）：
  - admin/user 路由要求 permission 已声明。
  - public/webhook 路由写了 `permission` 或 `accounts` 报 `unsupported`（`noCallerMsg`）。
  - `routeAccounts`（:60-78）逐条用 `ParseAccountsFrom` 检查 from；path 参数不存在时报 `unknown_param`。
- `routeRef(field, ref, defaultMethod)`（`sdk/manifest/check/ui.go:34-48`）按 `RouteSignature` 找已声明的路由。错误码有两个：
  - `invalid_route_ref`
  - `unknown_route`
- `pricing()`（`validate.go:563-571`）读 `files["manifest.json"]` 原文，**拒绝已退役字段**（`unsupported`）。这是现成的“退役字段”写法。
- `uiFloor` / `uiFloors` / `uiCoreFloor`（`ui.go:484-551`）：
  - `uses func(*manifest.UI) bool`。
  - `uiCoreFloor` 只在 `v.m.UI != nil` 时由 `ui()` 调用（`ui.go:155`）。
  - 最新一档是 `{minor: 15, lastUI: "3.14.0", lastCore: "0.1.285", firstCore: "0.1.286"}`。
- `coreFloor` / `coreFloors` / `checkCoreFloors`（`sdk/manifest/check/corefloor.go:67-195`）：
  - 用来处理“旧核心接受但忽略的声明”。
  - `routes[].accounts` → `RouteAccountsFirstCore = "0.1.286"`（:48、:91）。
  - 错误码 `below_feature_core`。
- 现有错误码（统计 `v.add` 调用）：`invalid`、`required`、`duplicate`、`unsupported`、`invalid_format`、`unknown`、`unknown_param`、`unknown_route`、`mutually_exclusive`、`reserved`、`too_many`、`below_feature_core`、`below_ui_core` 等。**本章只复用这些码，不新增。**

### 2.3 perm

- 包注释说明这是叶子包，不 import 任何 internal 包（`server/internal/perm/key.go:1-4`）。
- `scopable`（`server/internal/perm/pattern.go:32-41`）写死了 8 个核心动作。`ScopeType(action)`（:45）返回这张表的值。
- `Scope{All, Own bool; Group, ID int64}`（:59-62）。
- `Parse`（:75-93）的处理：
  - 末段既不是 `*`、`own`，也不含 `/` 时，按无范围处理。
  - 否则查 `scopable[action]`，查不到返回 `ErrInvalidKey`。
  - 所以 `plugin.x:y:own` 现在是非法 key，向量里也写成了 invalid（`sdk/permvectors/vectors.json`，`"in": "plugin.x:y:own", "invalid": true`）。
- `Suffix`（:178-202）的默认分支写 `scopable[p.Action] + "/" + id`。
- `Implies`（:216-236）：`case h.Group == 0: return h.ID == w.ID`（:227）。
- `ImpliesAny`、`CanonicalSet`（:239-262）。
- 调用方共 18 处（不含测试）：
  - `authz/grants.go:34,42,139,150,158`
  - `authz/catalog.go:301,373`
  - `authz/menus.go:283`
  - `authz/tsgen.go:38,43`
  - `iam/access_key.go:82,91,93`
  - `core/ports_identity.go:48,81,89,279,286`

### 2.4 core.PermissionSet

`server/internal/core/ports_identity.go`：

- 结构：`Keys map[string]struct{}`，`Scoped map[perm.Key][]perm.Scope`（:36-43）。
- `Add` → `perm.Parse`（:47-69），解析失败的 key **静默丢弃**。
- `Has`（:74-92）用 `perm.Implies`。
- `KeyList`（:129）用 `Pattern.Key()`。
- `Missing`（:238）基于 `KeyList` + `Has`。
- `Restrict`（:276-293）基于 `Has` + `Implies`。
- `DataScope` → `Compile`（`core/datascope.go:31-48`）。它的 default 分支把 `sc.ID` 当账号 id 收集。

### 2.5 现有调用点（删字段后编译会失败，归属各章）

| 位置 | 用了什么 | 归属 |
|---|---|---|
| `server/internal/plugin/install/defaults.go:70` | `up.Sensitive` | 第 3 章 |
| `server/internal/plugin/install/defaults.go:114` | `up.Roles()` | 第 3 章 |
| `server/internal/plugin/install/rolegrants.go:81-82` | `up.Sensitive` | 第 3 章 |
| `server/internal/plugin/install/review.go:350` | `up.Sensitive` | 第 3 章 |
| `server/internal/plugin/install/rolegrants_test.go:56`、`defaults_test.go:31-34` | `DefaultRoles` | 第 3 章 |
| `server/internal/authz/plugin_catalog.go:61`、`sync.go:47` | `d.Sensitive`（`core.PermissionDef`，不是 manifest 字段） | 第 2 章 |
| `plugins/growth/internal/growth/plugin_test.go:107` | `stats.Sensitive`、`stats.Roles()`、`use.DefaultRoles` | 本章（迁移） |
| `plugins/moderation/manifest_test.go:90` | `up.DefaultRoles` | 本章（迁移） |
| `sdk/manifest/permission_test.go:51-80` | `Roles()` | 本章 |
| `sdk/manifest/check/user_permissions_test.go` | `DefaultRoles` | 本章 |

### 2.6 内置插件

`plugins/` 下有 15 个目录。其中 9 个声明了 `userPermissions`：anthropic、claude-oauth、codex、dynweight、growth、guard、moderation、payment、volcengine。另外 6 个没有声明，本章不改：ccgateway、custom、gemini、mailer、openai、relay。

BRIEF 里说“10 个”，按代码实际数是 9 个需要改。

`moderation:exempt` 是 moderation 0.12.0 新加的（commit `ca4f36aa`，2026-11-13）。0.12.0 已随核心 0.1.286 作为内置插件上线（`deploy/gateway/ovh/README.md:27,30`：重建的内置集合含 moderation 0.12.0，10 个插件两节点 enabled）。所以它的 `inheritFrom: ["moderation:read"]` **已经在生产上执行过**：推测当时持有 `moderation:read` 的角色（默认 admin）已经持有 `moderation:exempt`。处理见 §11 R1（已定：保留不动）。

## 3. 设计：manifest（sdk/manifest）

### 3.1 JSON 形状

```jsonc
{
  "hostCompat": ">=0.1.289 <0.2.0",            // ≥ PluginPermissionsFirstCore（§4.6）
  "permissionGroups": [
    { "key": "flow",   "label": { "en": "Flows",   "zh": "审核流程" } },
    { "key": "record", "label": { "en": "Records", "zh": "审核记录" } }
  ],
  "userPermissions": [
    { "key": "flow:read", "group": "flow", "label": { "en": "View flows", "zh": "查看审核流程" },
      "defaults": { "admin": true, "operator": true } },
    { "key": "flow:manage", "group": "flow", "risk": "sensitive", "requires": ["flow:read"],
      "label": { "en": "Edit flows", "zh": "编辑审核流程" },
      "scope": { "resource": "flow", "options": "/scopes/flow", "own": true },
      "defaults": { "admin": ["*"] } },
    { "key": "record:read", "group": "record", "label": { "en": "View records", "zh": "查看审核记录" },
      "scope": { "values": [ { "key": "input",  "label": { "en": "Input",  "zh": "输入" } },
                             { "key": "output", "label": { "en": "Output", "zh": "输出" } } ] },
      "defaults": { "admin": ["*"], "operator": ["input"] } },
    { "key": "exempt", "risk": "critical", "label": { "en": "Exempt", "zh": "免于审核" } }
  ],
  "routes": [
    { "method": "GET", "path": "/scopes/flow", "scope": "admin", "permission": "flow:read" },
    { "method": "PUT", "path": "/flows/:id",   "scope": "admin", "permission": "flow:manage",
      "scopeFrom": "path:id" }
  ]
}
```

授予后存进目录的 key（规范形式，由 perm 解析）：

- `plugin.moderation:flow:manage`：全部范围，`["*"]` 规范化后去掉后缀。
- `plugin.moderation:flow:manage:own`
- `plugin.moderation:flow:manage:flow/3`
- `plugin.moderation:record:read:value/input`：values 形式的隐含资源是 `value`。

### 3.2 Go 结构（`sdk/manifest/manifest.go`）

```go
type Manifest struct {
	// ...
	// PermissionGroups are the groups userPermissions[].group names; the role
	// editor shows plugin → group → permission in this order (UI 3.16).
	PermissionGroups []PermissionGroup `json:"permissionGroups,omitempty"`
	UserPermissions  []UserPermission  `json:"userPermissions,omitempty"`
	// ...
}

// PermissionGroup is one group of a plugin's permissions; its position in
// permissionGroups is its order.
type PermissionGroup struct {
	Key   string        `json:"key"` // keyRe, unique
	Label LocalizedText `json:"label"`
}

// UserPermission is registered into the RBAC catalog as
// "plugin.<key>:<Key>".
type UserPermission struct {
	Key         string        `json:"key"` // ^[a-z0-9_]+(:[a-z0-9_]+)+$, not ending in :own
	Label       LocalizedText `json:"label"`
	Description LocalizedText `json:"description,omitempty"`
	Group       string        `json:"group,omitempty"` // a permissionGroups key
	// Risk is RiskLow (default), RiskSensitive or RiskCritical. A critical
	// permission is super-admin only: no defaults, and only a holder may
	// grant it (initially only the super admin).
	Risk string `json:"risk,omitempty"`
	// Requires are local keys of this manifest the permission is of little
	// use without; the role editor hints at them, authorization ignores them.
	Requires []string `json:"requires,omitempty"`
	// Scope lets a grant reach part of the plugin's data; nil is unscoped.
	Scope *PermissionScope `json:"scope,omitempty"`
	// Defaults are the built-in roles (BuiltinRoleKeys) the permission is
	// suggested for when the catalog first creates it: true for an unscoped
	// permission, a list of scopes ("*", "own" or a values key) for a scoped
	// one. A role left out gets nothing; the super admin is never listed.
	Defaults map[string]RoleDefault `json:"defaults,omitempty"`
	// InheritFrom: unchanged (renamed or split permissions keep holders).
	InheritFrom []string `json:"inheritFrom,omitempty"`
}

// PermissionScope is where the scopes of a permission come from: the
// plugin's own resource (Resource, with candidate ids from the Options
// route and/or Own), or a fixed list of Values. Never a core resource
// (accounts are scoped by routes[].accounts, decision D4).
type PermissionScope struct {
	Resource string       `json:"resource,omitempty"` // ScopeResourcePattern, not reserved
	Options  string       `json:"options,omitempty"`  // "/path" of a declared GET admin route
	Own      bool         `json:"own,omitempty"`      // "own" (created by the holder) may be granted
	Values   []ScopeValue `json:"values,omitempty"`
}

type ScopeValue struct {
	Key   string        `json:"key"` // ScopeValuePattern
	Label LocalizedText `json:"label"`
}

type Route struct {
	// ... unchanged fields ...
	// ScopeFrom names where the request carries the id the core checks
	// against the caller's scope of Permission before forwarding:
	// "path:<param>", "query:<name>" or "body:<field>" (the accounts.from
	// DSL, ParseAccountsFrom). Admin/user routes whose permission declares a
	// scope only (UI 3.16).
	ScopeFrom string `json:"scopeFrom,omitempty"`
}
```

`scopeFrom` 只写一个字符串，不像 `accounts.from` 那样是数组。原因是一个路由只对应一个权限的一种资源；body 字段本身可以是 id 数组，这时每个值都要在范围内，这一点由第 4 章执行。

### 3.3 RoleDefault 的 JSON 类型（`sdk/manifest/permission.go`）

```go
// RoleDefault is one value of UserPermission.Defaults: JSON true
// (Unscoped) or an array of scopes. JSON false decodes to the zero value,
// which check rejects; any other JSON type fails to decode.
type RoleDefault struct {
	Unscoped bool
	Scopes   []string
}

func (d RoleDefault) MarshalJSON() ([]byte, error) // Scopes != nil → array; else bool(Unscoped)
func (d *RoleDefault) UnmarshalJSON(b []byte) error // bool → Unscoped; array of strings → Scopes; else error
```

为什么不直接用 `json.RawMessage` 或 `any`：调用方（check、第 3 章的 DefaultGrants 消费者）拿到的应该是类型确定的值。把 false 解成零值而不是解码失败，是为了让 check 能报出字段级错误（`userPermissions[i].defaults.admin`），而不是笼统的 `manifest invalid_json`。

### 3.4 常量与辅助函数（`sdk/manifest/permission.go`，替代 `Roles()`）

```go
// RiskSensitive joins RiskLow and RiskCritical (manifest.go) as a
// userPermissions risk; hostPermissions keep their four levels.
const RiskSensitive = "sensitive"

// UserPermissionRisks are the risks a user permission may declare.
var UserPermissionRisks = []string{RiskLow, RiskSensitive, RiskCritical}

// ScopeValueResource is the resource of a values scope: its grants are
// "value/<key>".
const ScopeValueResource = "value"

// Scope text patterns; server/internal/perm parses the same (a perm test
// keeps them equal).
const (
	ScopeResourcePattern = `^[a-z][a-z0-9_]{0,29}$`
	ScopeValuePattern    = `^[A-Za-z0-9_.-]{1,64}$`
)

// ReservedScopeResources may not be a plugin scope resource: the core's own
// scope resources (D4: plugins scope accounts through routes[].accounts)
// and the values resource.
var ReservedScopeResources = []string{"account", "proxy", "group", ScopeValueResource}

// RiskLevel is up.Risk, RiskLow when omitted.
func (up UserPermission) RiskLevel() string

// ResourceName is the resource of the scope's grants: Resource, or
// ScopeValueResource for a values scope.
func (s PermissionScope) ResourceName() string

// GrantSuffix is the canonical stored scope of one defaults entry v: "*" is
// "" (every row, a role_permissions row), "own" is "own" when s.Own, a
// values key k is "value/k"; false for anything else (ids of an options
// resource are runtime data and never a default).
func (s PermissionScope) GrantSuffix(v string) (string, bool)

// DefaultGrants maps each role of up.Defaults to its canonical suffixes,
// sorted and deduplicated ("" alone when it holds "" — every row implies
// the rest). nil for a critical permission. Replaces Roles().
func (up UserPermission) DefaultGrants() map[string][]string

// FindUserPermission returns the declared permission with local key local.
func (m *Manifest) FindUserPermission(local string) (UserPermission, bool)

// UserPermissionScope is the scope of local, nil when undeclared or unscoped.
func (m *Manifest) UserPermissionScope(local string) *PermissionScope
```

删除的内容：

- `UserPermission.Sensitive`、`UserPermission.DefaultRoles`
- `DefaultPermissionRoles`
- `UserPermission.Roles()`

保留 `BuiltinRoleKeys`，`defaults` 的键只能是它列出的角色。

关于 `DefaultGrants` 的写法：

- 函数本身不校验输入，非法条目直接跳过。check 已经保证合法，这样消费方不需要处理 error。
- 循环体只有一层：
  1. 对每个角色，`Unscoped` 产出 `""`。
  2. 对每个 `Scopes` 条目调用 `GrantSuffix`。
  3. 结果中含 `""` 时只保留 `[""]`，否则 `slices.Sort` 加 `slices.Compact`。

复用说明：

- `FindUserPermission` 和 check 里已有的 `userPerm` 做的是同一件事。实现后 `userPerm(p)` 改为 `_, ok := v.m.FindUserPermission(p)`，只剩一处遍历。
- 第 4 章原本打算自己加 `UserPermissionScope`，现在由本章提供，名字用它提议的。

## 4. 设计：manifest/check

### 4.1 校验规则总表

字段路径沿用现有风格：数组用 `[i]`，map 用 `.key`。错误码全部来自现有集合。

| # | 字段 | 条件 | code |
|---|---|---|---|
| G1 | `permissionGroups[i].key` | 不匹配 `keyRe`（复用插件 key 的正则） | `invalid_format` |
| G2 | `permissionGroups[i].key` | 重复 | `duplicate` |
| G3 | `permissionGroups[i].label` | 空 | `required` |
| P1 | `userPermissions[i].key` | 现有规则不变 | `invalid_format` / `duplicate` |
| P2 | `userPermissions[i].label` | 现有规则不变 | `required` |
| P3 | `userPermissions[i].group` | 非空且不在 permissionGroups 中 | `unknown` |
| P4 | `userPermissions[i].risk` | 非空且不在 `UserPermissionRisks` 中 | `invalid_value` |
| P5 | `userPermissions[i].requires[j]` | 等于自身，或不是本清单已声明的 key | `unknown` |
| P6 | `userPermissions[i].inheritFrom[j]` | 现有规则不变 | `invalid_value` |
| P7 | `userPermissions[i].inheritFrom[j]` | 本权限是 critical，且所引 key 在本清单中声明为非 critical（防止通过继承把超管专属权限授给别人） | `invalid_value` |
| S1 | `userPermissions[i].scope` | 同时写了 `values` 和 `resource`/`options`/`own` 中任一项 | `mutually_exclusive` |
| S2 | `userPermissions[i].scope` | 既没有 `values` 也没有 `resource` | `required` |
| S3 | `userPermissions[i].scope.resource` | 不匹配 `ScopeResourcePattern` | `invalid_format` |
| S4 | `userPermissions[i].scope.resource` | 属于 `ReservedScopeResources` | `reserved` |
| S5 | `userPermissions[i].scope` | 资源形式下 `options` 为空且 `own` 为 false（能授的只有全部，那就不必声明 scope） | `required` |
| S6 | `userPermissions[i].scope.options` | `v.routeRef(f, options, "GET")`：格式不对或路由未声明 | `invalid_route_ref` / `unknown_route` |
| S7 | `userPermissions[i].scope.options` | 找到的路由不是 GET，或 scope ≠ admin，或 path 含 `:` / `*` 参数 | `invalid` |
| S8 | `userPermissions[i].scope.values` | 超过 50 项 | `too_many` |
| S9 | `userPermissions[i].scope.values[j].key` | 不匹配 `ScopeValuePattern` | `invalid_format` |
| S10 | `userPermissions[i].scope.values[j].key` | 重复 | `duplicate` |
| S11 | `userPermissions[i].scope.values[j].label` | 空 | `required` |
| D1 | `userPermissions[i].defaults.<role>` | role 不在 `BuiltinRoleKeys` 中（`super_admin` 也算不在） | `invalid_value` |
| D2 | `userPermissions[i].defaults` | risk 为 critical 且 defaults 非空 | `unsupported` |
| D3 | `userPermissions[i].defaults.<role>` | 值为 false 或 `[]`（不想给就别写这个角色） | `invalid_value` |
| D4 | `userPermissions[i].defaults.<role>` | 无范围权限写了数组，或有范围权限写了 true | `invalid_value` |
| D5 | `userPermissions[i].defaults.<role>[j]` | `Scope.GrantSuffix(v)` 返回 false（例如资源形式写了 `"flow/3"`，或 `own` 但 `scope.own` 为 false，或不在 values 里的值） | `invalid_value` |
| U1 | `ui.accountActions[i].permission` | 引用的权限声明了 scope。运行时这里只看 accounts∩account:*（D4），范围没有意义 | `unsupported` |
| U2 | `ui.settings.permission` | 引用的权限声明了 scope。运行时按全量 `CanAny` 判断 | `unsupported` |
| R1 | `routes[i].scopeFrom` | 路由是 public/webhook | `unsupported`（`noCallerMsg`） |
| R2 | `routes[i].scopeFrom` | 路由的 permission 没有声明 scope | `unsupported` |
| R3 | `routes[i].scopeFrom` | `ParseAccountsFrom` 返回 not ok | `invalid` |
| R4 | `routes[i].scopeFrom` | `path:<p>` 指向的参数不在路由路径中 | `unknown_param` |
| X1 | `userPermissions[i].sensitive`、`userPermissions[i].defaultRoles` | 原文里仍有这两个已退役字段（读 `files["manifest.json"]`，与 `pricing()` 同一写法） | `unsupported` |
| F1 | `hostCompat` | 用了新字段（§4.6）却允许 `PluginPermissionsFirstCore` 之前的核心 | `below_feature_core` |
| F2 | `hostCompat` | `hostUICompat` 要求 ≥3.16，却允许 UI 3.15 的核心 | `below_ui_core` |

几点说明：

- 原型里有一条“不能同时写 `sensitive` 和 `risk`”。现在由 X1 取代：`sensitive` 一律拒绝，提示改用 `risk`。
- `group` 是可选的。没有分组的权限，在角色编辑页直接列在插件模块下，由第 5 章处理。
- 声明了 permissionGroups 但没有权限引用的分组不报错，前端隐藏空分组即可。这里不为它新增一条规则。
- `inheritFrom` 仍然允许引用未声明的旧 key，因为改名后旧 key 已经不再声明。
  - 旧 key 的风险等级 check 看不到。critical 是否从非 critical 继承，由第 3 章 `install.planRoleGrants` 在同意时按 `permissions.risk` 再判一次（第 2 章删除了 `SyncPlugin` 的继承步骤，§7.2）。
- U1 和 U2 加在现有检查的旁边，各加一行条件，判断条件是 `v.m.UserPermissionScope(p) != nil`：
  - U1 在 `ui.go:427`，`uiAccountActions` 的 permission 检查。
  - U2 在 `validate.go:520`，`form` 的 settings permission 检查。
  - 这两条是第 4 章提出的。
- `scopeFrom` 只用于“作用于请求中点名的一行”的路由。请求没带这个值时，和 accounts 一样只有全部范围能通过，所以列表路由不能声明 scopeFrom，应该改为读第 4 章的 `HTTPRequest.action_scope` 自行过滤。check 不去猜哪些是列表路由，只在 CONTRACTS 里写明。
- `options` 路由的 permission 可以是本插件任意已声明的 key，建议用同一资源的 read 权限。
  - 角色编辑页和插件“权限”标签经核心专用端点 `GET /api/v1/plugins/:key/scope-options?permission=` 调用它，按 `role:read` 或 `plugin:read` 任一放行（已定，第 4 章 §4.3.7）。
  - check 只保证路由存在、是 GET admin 路由、没有路径参数，这样核心能直接调用。

### 4.2 `userPermissions()` 改写（`sdk/manifest/check/validate.go:779`）

```go
func (v *validator) userPermissions() {
	groups := v.permissionGroups()
	seen := map[string]bool{}
	for i, up := range v.m.UserPermissions {
		f := fmt.Sprintf("userPermissions[%d]", i)
		// P1/P2: unchanged key, duplicate and label checks
		if up.Group != "" && !groups[up.Group] {
			v.add(f+".group", "unknown", "group %q is not declared in permissionGroups", up.Group)
		}
		if up.Risk != "" && !slices.Contains(manifest.UserPermissionRisks, up.Risk) {
			v.add(f+".risk", "invalid_value", "risk must be one of %v", manifest.UserPermissionRisks)
		}
		for j, r := range up.Requires {
			if r == up.Key || !v.userPerm(r) {
				v.add(fmt.Sprintf("%s.requires[%d]", f, j), "unknown", "requires %q must be another permission declared here", r)
			}
		}
		v.inheritFrom(f, up) // P6 (existing loop, moved) + P7
		v.permissionScope(f+".scope", up.Scope)
		v.permissionDefaults(f+".defaults", up)
	}
	v.retiredUserPermissionFields()
}

// permissionGroups validates permissionGroups and returns the declared keys.
func (v *validator) permissionGroups() map[string]bool

// permissionScope validates a user permission's scope (S1–S11).
func (v *validator) permissionScope(f string, s *manifest.PermissionScope)

// permissionDefaults validates a user permission's defaults (D1–D5); roles
// are visited in sorted order so the errors are stable.
func (v *validator) permissionDefaults(f string, up manifest.UserPermission)

// retiredUserPermissionFields rejects the userPermissions fields UI 3.16
// retired (sensitive → risk, defaultRoles → defaults) instead of ignoring
// them, as pricing() does for model prices.
func (v *validator) retiredUserPermissionFields()
```

为什么拆出 `inheritFrom`、`permissionScope`、`permissionDefaults` 三个方法：每个方法对应一个可以单独测试的业务概念（继承、范围、默认值），拆开后主循环只剩一层，嵌套不超过两层，和现有的 `routeAccounts`、`uiSections` 等写法一致。

`permissionDefaults` 的骨架如下，每个角色先过 D1–D4，通过后再逐条检查 D5：

```go
func (v *validator) permissionDefaults(f string, up manifest.UserPermission) {
	if len(up.Defaults) == 0 {
		return
	}
	if up.RiskLevel() == manifest.RiskCritical {
		v.add(f, "unsupported", "critical permissions take no defaults; only the super admin grants them")
		return
	}
	for _, role := range slices.Sorted(maps.Keys(up.Defaults)) {
		d, ff := up.Defaults[role], f+"."+role
		switch {
		case !slices.Contains(manifest.BuiltinRoleKeys, role):
			v.add(ff, "invalid_value", "role %q is not one of %v", role, manifest.BuiltinRoleKeys)
		case !d.Unscoped && len(d.Scopes) == 0:
			v.add(ff, "invalid_value", "leave the role out instead of false or []")
		case up.Scope == nil && !d.Unscoped:
			v.add(ff, "invalid_value", "an unscoped permission's default is true")
		case up.Scope != nil && d.Unscoped:
			v.add(ff, "invalid_value", `a scoped permission's default is a list of scopes, e.g. ["*"]`)
		default:
			for j, s := range d.Scopes {
				if _, ok := up.Scope.GrantSuffix(s); !ok {
					v.add(fmt.Sprintf("%s[%d]", ff, j), "invalid_value", "scope %q is not \"*\", an allowed \"own\" or a declared value", s)
				}
			}
		}
	}
}
```

### 4.3 路由（`sdk/manifest/check/routes_check.go`）

把 `routeAccounts` 里单条 from 的检查抽成 `valueFrom`，`accounts.from` 和 `scopeFrom` 共用。这是复用而不是新逻辑：

```go
// valueFrom checks one "path:|query:|body:" entry of route r (accounts.from,
// scopeFrom): a known source and plain name, a path parameter of r.
func (v *validator) valueFrom(field string, r manifest.Route, from string)

// routeScopeFrom validates routes[i].scopeFrom of an admin or user route.
func (v *validator) routeScopeFrom(f string, r manifest.Route) {
	if r.ScopeFrom == "" {
		return
	}
	if v.m.UserPermissionScope(r.Permission) == nil {
		v.add(f, "unsupported", "permission %q declares no scope", r.Permission)
		return
	}
	v.valueFrom(f, r, r.ScopeFrom)
}
```

`routes()` 里要改两处：

- admin/user 分支：在 `v.routeAccounts(...)` 后面加 `v.routeScopeFrom(f+".scopeFrom", r)`。
- public/webhook 分支：增加 `if r.ScopeFrom != "" { v.add(f+".scopeFrom", "unsupported", noCallerMsg) }`。

### 4.4 退役字段

```go
func (v *validator) retiredUserPermissionFields() {
	var raw struct {
		UserPermissions []map[string]json.RawMessage `json:"userPermissions"`
	}
	if json.Unmarshal(v.files["manifest.json"], &raw) != nil {
		return
	}
	for i, up := range raw.UserPermissions {
		for field, repl := range map[string]string{"sensitive": "risk", "defaultRoles": "defaults"} {
			if _, ok := up[field]; ok {
				v.add(fmt.Sprintf("userPermissions[%d].%s", i, field), "unsupported", "%s was removed; use %s", field, repl)
			}
		}
	}
}
```

宿主（`pkg.Validate`）、`sub2api-plugin pack`、插件自己的 manifest 测试三处都会把 `manifest.json` 放进 `files`，已核实 moderation 的测试（`plugins/moderation/manifest_test.go:57`）。只有 files 为 nil 的单测会跳过这一项，这和 `pricing()` 的行为一致。

### 4.5 不考虑旧字段兼容的后果

已安装的第三方插件，如果 manifest 里还写着 `defaultRoles`：

- 核心升级后不会重新校验这份 manifest（推测：只有安装和升级时才调用 check）。
- 下次同步时新字段为空，所以**不新增任何授权**，已有授权不变。这是向收紧方向失败，不会提权。
- 这类插件重新打包时会被 X1 拦下，提示改用新字段。

### 4.6 下限登记（`corefloor.go` + `ui.go`）

```go
// PluginPermissionsFirstCore is the first core that honours
// userPermissions[].group/risk/requires/scope/defaults, permissionGroups
// and routes[].scopeFrom, and ships UI 3.16 (the permission matrix, plugin
// resource scopes). An older core ignores them: it would grant a
// permission without defaultRoles to admin (critical ones included), treat
// every risk as not sensitive and forward scopeFrom routes unchecked.
const PluginPermissionsFirstCore = "0.1.289" // 推测：线上之后的第一个核心版本，发布时定
```

`coreFloors` 增加一条：

```go
{field: "userPermissions (group, risk, requires, scope, defaults), permissionGroups, routes[].scopeFrom",
	firstCore: PluginPermissionsFirstCore, uses: (*validator).usesPluginPermissions},
```

```go
// usesPluginPermissions reports a declaration of the UI 3.16 permission
// model. A permission with none of the new fields means the same on every
// core but for the old admin default, which its own hostCompat answers.
func (v *validator) usesPluginPermissions() bool {
	return len(v.m.PermissionGroups) > 0 ||
		slices.ContainsFunc(v.m.UserPermissions, func(up manifest.UserPermission) bool {
			return up.Group != "" || up.Risk != "" || len(up.Requires) > 0 || up.Scope != nil || len(up.Defaults) > 0
		}) ||
		slices.ContainsFunc(v.m.Routes, func(r manifest.Route) bool { return r.ScopeFrom != "" })
}
```

`uiFloors` 在最前面加一档，不带 `uses`：

```go
// ... 0.1.288 is the last with UI 3.15 (3.16 starts at 0.1.289): 3.16 adds
// the permission model; its manifest declarations live outside ui and are
// floored by coreFloors (PluginPermissionsFirstCore), so this entry only
// answers hostUICompat ^3.16.
{minor: 16, lastUI: "3.15.0", lastCore: "0.1.288", firstCore: PluginPermissionsFirstCore},
```

为什么不把判断写进 `uiFloor.uses`：

1. `uses` 的签名是 `func(*manifest.UI) bool`，而 `uiCoreFloor` 只在 `m.UI != nil` 时才跑。payment、anthropic、claude-oauth 这几个插件没有 ui，却声明了 userPermissions。
2. 这些新字段真正被“忽略”的地方在核心（defaults、scopeFrom），这正是 `coreFloor` 要处理的场景。
3. 两档共用同一个 `firstCore` 常量，所以“UI 下限 3.16”和“核心下限”对应同一个版本，不会分叉。

谓词只看新字段，不看“有没有 userPermissions”。这样现有测试夹具（例如 `ui_test.go:141`、`:538`、`:623`，它们只写 key 和 label，`hostCompat >=0.1.0`）不会因为下限检查而失败。

## 5. 设计：perm（server/internal/perm）

### 5.1 选择：纯语法解析，不做运行时注册

| 方案 | 问题 |
|---|---|
| `Parse(k, resolver)` | 18 个调用方都要传 resolver。`PermissionSet.Add` 和 `PermissionSetOf` 在很多地方脱离目录使用（访问密钥、缓存反序列化），改动面太大 |
| perm 包维护全局注册表 `Register(action, res)` | 引入全局可变状态，有初始化顺序问题，多节点之间要同步，测试互相干扰。插件升级改了资源时还得撤销注册 |
| **按 `plugin.` 前缀语法识别（选用）** | perm 仍是叶子包和纯函数，`scopable` 不动。语义校验（资源是否匹配、是否允许 own、值是否在枚举内）在**写入时**由 authz 对照 `permissions.scope_type` 完成（第 2 章） |

为什么读路径可以只看语法：

- 存储的授予在写入时已经校验过。
- 如果插件升级把资源从 flow 改成了 pipeline，旧授予 `flow/3` 照样能解析，但运行时构造的 key 是 `pipeline/…`，二者永远不会蕴含。结果是向收紧方向失败。

这个方案已经和第 2 章（authz-catalog）、第 4 章（runtime-scope）对齐。

### 5.2 改动

```go
// pattern.go

// PluginPrefix starts the action of every plugin permission
// (manifest.PermissionModule). Such an action takes the plugin scope
// grammar; whether it takes a scope at all, and which resource, own or
// values, is the catalog's to say (permissions.scope_type), checked by
// authz when a grant is written.
const PluginPrefix = "plugin."

const (
	scopeResourcePattern = `^[a-z][a-z0-9_]{0,29}$` // = manifest.ScopeResourcePattern
	scopeValuePattern    = `^[A-Za-z0-9_.-]{1,64}$` // = manifest.ScopeValuePattern
)

var scopeResourceRe, scopeValueRe = regexp.MustCompile(scopeResourcePattern), regexp.MustCompile(scopeValuePattern)

// ValidScopeValue reports whether v may be a plugin scope value (a row id
// or a values key); the runtime checks a scopeFrom value with it before
// building a key.
func ValidScopeValue(v string) bool { return scopeValueRe.MatchString(v) }

// Scope gains two fields; still comparable (PermissionSet keeps []Scope and
// compares with ==).
type Scope struct {
	All, Own   bool
	Group, ID  int64
	Res, Value string // a plugin scope "<Res>/<Value>"; then the rest are zero
}
```

只导出有包外调用方的 `ValidScopeValue`（第 4 章校验 scopeFrom 取到的值和 options 代理返回的 id）。资源名正则只在 `parseScope` 里用，不导出；读资源直接用 `p.Scope.Res`，不再加 `PluginScopeResource` 这类没有调用方的访问函数（审查时删除）。

`Parse` 只改一行判断，然后复用 `parseScope` 里已有的 `*` / `own` 分支：

```go
	res := scopable[action]
	if res == "" && !strings.HasPrefix(string(action), PluginPrefix) {
		return Pattern{}, ErrInvalidKey
	}
	s, err := parseScope(res, action == AccountCreate, last)
```

`parseScope` 在 `switch raw {"*","own"}` 之后插入：

```go
	if res == "" { // a plugin action: "<resource>/<value>", nothing else
		r, val, _ := strings.Cut(raw, "/")
		if !scopeResourceRe.MatchString(r) || !ValidScopeValue(val) {
			return Scope{}, ErrInvalidKey
		}
		return Scope{Res: r, Value: val}, nil
	}
```

`Suffix` 在 `case s.Own:` 后面加一行：

```go
	case s.Res != "":
		return s.Res + "/" + s.Value
```

`Implies` 不加新分支，只扩展 :227 这一行：

```go
	case h.Group == 0: // account/X, proxy/X or a plugin <res>/<value>: that row, with or without a group
		return h.ID == w.ID && h.Res == w.Res && h.Value == w.Value
```

逐种情况核对：

- All 和 Own 在前面已经处理。
- have 是插件范围时，`h.Group == 0` 成立，比较资源和值。
- want 是插件范围、have 是核心分组范围时，走 `w.Group == 0 → false`。
- 两边动作不同时，在第一个 case 已经返回 false。

这和第 2、4 章约定的语义（“任一方有 Res 时，在 All/Own 之后比较 Res 和 Value”）等价。

插件 key 的语法规则：

- 只接受 `*`、`own`、`<res>/<value>` 三种范围。
- `flow/*` 不做 trailing wildcard 折叠，直接判为非法：`*` 不满足 value 的正则。这也符合 D1，通配只是勾选时的快捷方式。
- 本地 key 本身不会被误读成范围：check 保证它的末段不是 `own`，正则也排除了 `*` 和 `/`。

同步更新注释（不改行为）：

- `pattern.go:10-21` 的语法注释，加入插件那一行。
- `Pattern` 注释里“Unscoped keys (… plugin keys)”这句。

### 5.3 不变的部分

- `scopable` 和 `ScopeType` 继续只描述核心动作，`tsgen.go` 生成的 `SCOPE_TYPE` 也不变。插件权限的 scope_type 由第 2 章从 manifest 写进 `permissions.scope_type`，格式约定为 `"plugin:" + scope.ResourceName()`。
- `Canonical` / `CanonicalSet` 不用改，插件 key 自动走新语法。
- `AccountAction` 不变。

### 5.4 对 core.PermissionSet 的影响

| 方法 | 影响 |
|---|---|
| `Add` / `PermissionSetOf` | 插件带范围的 key 从“解析失败被丢弃”变为写进 `Scoped[action]`。这是授予生效的前提 |
| `Has` | 不改代码。持有 `plugin.m:flow:manage` 就蕴含 `…:flow/3`；`own` 只蕴含 `own` |
| `HasAction` | 不改代码。持有任意范围都返回 true，第 4 章的列表接口会用到 |
| `KeyList` | 不改代码。通过 `Pattern.Key()` 输出 `…:flow/3` |
| `Missing` | 不改代码（基于 KeyList + Has）。授出 `…:flow/3` 时，授予人只持有 `…:flow/2` 会被拒；持有全部范围则通过（BRIEF 防提权“插件资源范围同规则”） |
| `Restrict` | 不改代码。访问密钥写 `plugin.m:flow:manage`、用户只有 `flow/2`、`flow/5` 时，收窄为这两个 |
| `ScopeGroups` | 无影响，插件范围的 Group 为 0 |
| `DataScope` → `Compile` | 插件动作不会传进来（只用于账号和代理的行谓词）。为防误用，在 `core/datascope.go:35` 的 switch 加一个 `case sc.Res != "": continue`，避免 default 分支把 ID 0 收进 IDs。这是一行防御代码 |

第 4 章在新文件 `core/plugin_scope.go` 里新增 `PermissionSet.PluginScope(action)`（返回 `{All, Own, Values}`）和 `PermissionSet.HasSome(k)`，不属于本章；本章不改 `core/ports_identity.go`（该文件归第 2 章）。

`iam/access_key.go:82` 和 `authz/menus.go:283` 只调用 `Parse`，不需要改。

## 6. permvectors（`sdk/permvectors/vectors.json`）

Go（`perm/pattern_test.go`）和控制台 TS（`web/src/authz/implies.spec.ts`）共用这份文件，所以第 5 章必须同步实现 `parsePermission` 的插件分支，否则 TS 测试会失败。

canonical：

| in | out / invalid |
|---|---|
| `plugin.x:y:own` | `plugin.x:y:own`（**改**：原来是 invalid） |
| `plugin.x:y:*` | `plugin.x:y` |
| `plugin.moderation:flow:manage:flow/3` | 原样 |
| `plugin.x:rule:apply:value/input` | 原样 |
| `plugin.x:y:flow/a.b-C_9` | 原样 |
| `plugin.x:y:flow/*` | invalid |
| `plugin.x:y:Flow/3` | invalid |
| `plugin.x:y:flow/` | invalid |
| `plugin.x:y:/3` | invalid |
| `plugin.x:y:flow/3/4` | invalid |
| `plugin.x:y:flow/a b` | invalid |
| `plugin.x:y:flow/` 后接 65 个字符 | invalid |
| `x:y:own` | invalid（非插件、不可带范围的动作，回归用例） |

implies（`have → want = ok`）：

| have | want | ok |
|---|---|---|
| `plugin.m:f:m` | `plugin.m:f:m:flow/3` | true |
| `plugin.m:f:m` | `plugin.m:f:m:own` | true |
| `plugin.m:f:m:flow/3` | `plugin.m:f:m` | false |
| `plugin.m:f:m:flow/3` | `plugin.m:f:m:flow/3` | true |
| `plugin.m:f:m:flow/3` | `plugin.m:f:m:flow/4` | false |
| `plugin.m:f:m:flow/3` | `plugin.m:f:m:record/3` | false |
| `plugin.m:f:m:own` | `plugin.m:f:m:flow/3` | false |
| `plugin.m:f:m:flow/3` | `plugin.m:f:m:own` | false |
| `plugin.m:f:m:own` | `plugin.m:f:m:own` | true |
| `plugin.m:f:m:flow/3` | `plugin.n:f:m:flow/3` | false |

`allows` 不加：插件范围不做核心行判定。

## 7. 与其他章节的接口约定

### 7.1 第 2 章（authz）

- 使用 `perm.Scope{Res, Value}`、`perm.ValidScopeValue`、`perm.PluginPrefix`。
- `permissions.scope_type` 写 `"plugin:" + up.Scope.ResourceName()`；范围的其余部分整体存 `core.PermissionDef.Scope *manifest.PermissionScope`（第 2 章，复用本章类型，不另定义 core 版本），映射由第 3 章 `install.PermissionDefs` 完成。
- `resolvePermissions` 拒绝以下几种写入：资源对不上、写了 own 但不允许、值不在枚举内、无范围权限带了范围。perm 只负责语法。
- risk 取 `up.RiskLevel()`。`group_key` 取 `up.Group`；分组的 label 和顺序来自 `m.PermissionGroups`（第 2 章冗余存到每行）。`requires` 取 `up.Requires`，加插件前缀。这些映射都在第 3 章 `install.PermissionDefs` 里做。
- `inheritFrom` 指向 critical 时，只在旧 key 本身也是 critical 时才继承（check 的 P7 看不到已经不再声明的旧 key）。这条在第 3 章 `planRoleGrants` 实现（第 2 章删除了 `SyncPlugin` 的继承步骤）。

### 7.2 第 3 章（install）

- `up.DefaultGrants() map[string][]string` 替代 `up.Roles()`：键是角色，值是规范后缀，`""` 表示全部范围。
- 单个值的规范化用 `PermissionScope.GrantSuffix`。
- `up.Sensitive` 改为 `up.RiskLevel() != manifest.RiskLow`，或者按三级分别处理，由第 3 章决定。
- 风险常量用 `manifest.RiskLow`、`RiskSensitive`、`RiskCritical`。

### 7.3 第 4 章（runtime）

- `Route.ScopeFrom string`，用 `manifest.ParseAccountsFrom` 解析。
- `m.UserPermissionScope(local)`、`m.FindUserPermission(local)`。
- check 保证三件事：有 scopeFrom 的路由一定是 admin/user 路由；它的 permission 一定声明了 scope；options 一定指向已声明的、无路径参数的 GET admin 路由。

### 7.4 第 5 章（web）

- `implies.ts` 的 `parsePermission`：action 以 `plugin.` 开头时走插件语法（`*` / `own` / `<res>/<value>`，正则同 §5.2），Scope 增加 `res` 和 `value`。
- `scopeImplies` 和 `scopeSuffix` 同步修改，以 `vectors.json` 为准。

## 8. 内置插件 manifest 迁移

### 8.1 统一改动

- 删除 `sensitive` 和 `defaultRoles`，改写成 `risk` 和 `defaults`：原来是 `sensitive: true` 的改为 `"risk": "sensitive"`；另外按已定结论把 `claude_oauth:manage`、`codex:manage`（会创建凭证）和 `moderation:read`（能看被审核的原文）提为 sensitive；其余不写 risk（即 low）。风险只影响**新建 key** 的自动授予和同意页展示，这些 key 在存量部署上都已存在，已有授权不变（`SyncPlugin` 只更新 `risk` 列）。
- `defaultRoles: [a, b]` 改为 `"defaults": {"a": true, "b": true}`。
- 本轮不加 `permissionGroups` / `group`：9 个插件每个最多 3 项权限，分组没有信息量。分组留给以后权限多的插件，例如原型里的 moderation 1.x。
- 本轮不加 `scope` / `scopeFrom`：现有路由没有插件自有资源范围的需求，账号相关的已经用 `accounts` 表达（D4）。
- 用了 `defaults`，所以 `hostCompat` 下限要升到 `>=0.1.289 <0.2.0`（`PluginPermissionsFirstCore`，推测值）。各插件按惯例升 minor 版本，具体版本号由发布者定。
- 原有授权不迁移，也不变化：已存在的 key 在同步时不重新套默认值（第 2、3 章的现状）。

### 8.2 逐项表

| 插件 | 本地 key | 现状（sensitive / defaultRoles / inheritFrom） | 新 risk | 新 defaults | group | 备注 |
|---|---|---|---|---|---|---|
| anthropic | `model_catalog:read` | – / [admin] / – | low | `{admin:true}` | – | |
| claude-oauth | `claude_oauth:manage` | – / [admin, operator] / – | **sensitive** | `{admin:true, operator:true}` | – | 已定：提为 sensitive（会创建凭证） |
| codex | `codex:manage` | – / [admin, operator] / – | **sensitive** | `{admin:true, operator:true}` | – | 同上 |
| codex | `codex:reset` | – / [admin, operator] / – | low | `{admin:true, operator:true}` | – | 账号范围由路由 `accounts` 与 `account:update` 取交集（D4） |
| dynweight | `dynweight:read` | – / [admin] / – | low | `{admin:true}` | – | |
| dynweight | `dynweight:manage` | – / [admin] / – | low | `{admin:true}` | – | |
| growth | `growth:use` | – / [user, supplier, operator, admin] / [growth:read] | low | `{user:true, supplier:true, operator:true, admin:true}` | – | inheritFrom 保留 |
| growth | `growth:stats:read` | ✓ / [admin] / [growth:manage] | sensitive | `{admin:true}` | – | inheritFrom 保留 |
| guard | `rules:read` | – / [admin] / – | low | `{admin:true}` | – | |
| guard | `rules:manage` | – / [admin] / – | low | `{admin:true}` | – | |
| guard | `stats:read` | – / [admin] / – | low | `{admin:true}` | – | |
| moderation | `moderation:read` | – / [admin] / – | **sensitive** | `{admin:true}` | – | 已定：提为 sensitive（说明里写了“包括被审核的原文”） |
| moderation | `moderation:manage` | – / [admin] / – | low | `{admin:true}` | – | |
| moderation | `moderation:exempt` | – / [] / [moderation:read] | **critical** | **不写** | – | **删除 `inheritFrom`**（P7）；线上已有授权保留，见 R1 |
| payment | `recharge:use` | – / [user, supplier, operator, admin] / – | low | `{user:true, supplier:true, operator:true, admin:true}` | – | |
| payment | `config:manage` | ✓ / [admin] / – | sensitive | `{admin:true}` | – | |
| volcengine | `assets:read` | – / [admin] / – | low | `{admin:true}` | – | |
| volcengine | `assets:manage` | – / [admin] / – | low | `{admin:true}` | – | |

moderation 改后的片段：

```json
{ "key": "moderation:read",   "risk": "sensitive", "defaults": { "admin": true }, "label": {…}, "description": {…} },
{ "key": "moderation:manage", "defaults": { "admin": true }, "label": {…}, "description": {…} },
{ "key": "moderation:exempt", "risk": "critical",            "label": {…}, "description": {…} }
```

## 9. 改动清单

| 文件 | 改什么 |
|---|---|
| `sdk/manifest/manifest.go` | `Manifest.PermissionGroups`；`PermissionGroup`；`UserPermission` 增加 Group/Risk/Requires/Scope/Defaults，删除 Sensitive/DefaultRoles；`PermissionScope`、`ScopeValue`；`Route.ScopeFrom`；`RiskSensitive` 放在 `RiskLow…` 常量块里 |
| `sdk/manifest/permission.go` | `RoleDefault` 及其 JSON 方法、`UserPermissionRisks`、`ScopeValueResource`、`ScopeResourcePattern`、`ScopeValuePattern`、`ReservedScopeResources`、`RiskLevel`、`ResourceName`、`GrantSuffix`、`DefaultGrants`、`FindUserPermission`、`UserPermissionScope`；删除 `DefaultPermissionRoles` 和 `Roles` |
| `sdk/manifest/check/validate.go` | 改写 `userPermissions()`；新增 `permissionGroups`、`inheritFrom`、`permissionScope`、`permissionDefaults`、`retiredUserPermissionFields`；`userPerm` 改为调用 `FindUserPermission` |
| `sdk/manifest/check/routes_check.go` | 新增 `valueFrom`（从 `routeAccounts` 抽出）和 `routeScopeFrom`；public/webhook 分支增加 scopeFrom 的 unsupported |
| `sdk/manifest/check/corefloor.go` | `PluginPermissionsFirstCore`；`coreFloors` 加一条；`usesPluginPermissions` |
| `sdk/manifest/check/ui.go` | `uiFloors` 前面加 3.16 档，补注释；`uiAccountActions` 增加 U1 |
| `sdk/manifest/check/validate.go:520` | `form` 的 settings permission 检查增加 U2 |
| `server/internal/perm/pattern.go` | `PluginPrefix`、两个正则（不导出）、`ValidScopeValue`、`Scope.Res/Value`；`Parse`、`parseScope`、`Suffix`、`Implies` 各一处小改；更新注释 |
| `server/internal/core/datascope.go` | `Compile` 增加一行防御 case |
| `sdk/permvectors/vectors.json` | §6 的向量 |
| `plugins/{anthropic,claude-oauth,codex,dynweight,growth,guard,moderation,payment,volcengine}/manifest.json` | §8 |
| `plugins/moderation/internal/moderation/staff.go:9-19` | 只改注释：exempt 不再从 `moderation:read` 继承，持有者由超管授予（线上已有的保留） |
| `plugins/{moderation,volcengine,codex,anthropic,guard}/manifest_test.go`、`plugins/growth/internal/growth/plugin_test.go` | 断言改成 `Defaults`/`RiskLevel()`/`DefaultGrants()`，hostCompat 改为新下限；moderation 测试改为断言 exempt 是 critical、没有 defaults、没有 inheritFrom |
| `docs/CONTRACTS.md` | §4 那段（插件权限 defaults/risk/critical 语义，替换 defaultRoles 的描述）；§5 `/api/v1/p` 那段增加 `scopeFrom` 声明格式与 `PluginPermissionsFirstCore`；§20 moderation exempt；§21.0 增加插件范围语法、向量说明，并把“末段不能是 own”这一条扩展到 scope 字段 |
| `docs/ARCHITECTURE.md:482` | manifest 示例改用新字段 |

## 10. 测试计划

以下测试用 `testify/require` 和 `assert`，表驱动。这是 BRIEF 规定的通用规范；sup2api 现有测试多数直接用 `t.Errorf`，新增和重写的测试按 require/assert 写。

### 10.1 sdk/manifest（`permission_test.go`，删除 `TestUserPermissionRoles`）

- `TestRoleDefaultJSON`
  - `true`、`["*","own"]` 能往返序列化。
  - `false` 解成零值。
  - `"x"`、`{}`、`[1]` 解码报错。
- `TestDefaultGrants`（表驱动）：

| 输入 | 期望 |
|---|---|
| 无范围权限 `{admin:true}` | `{admin:[""]}` |
| 有范围 `{admin:["own","*"]}` | `{admin:[""]}` |
| `{operator:["own"]}` | `{operator:["own"]}` |
| values 形式 `["output","input","input"]` | `["value/input","value/output"]` |
| critical | nil |
| 没写 defaults | nil |

- `TestGrantSuffix`：
  - `*` → `""`
  - `own` 在 `Own=false` 时返回 false
  - 资源形式写 `flow/3` 返回 false
  - values 中的 key → `value/k`
- `TestUserPermissionScopeLookup`：
  - 本地 key 查得到。
  - 未声明的 key 返回 nil。
  - 无范围的 key 返回 nil。
- `TestScopeResourceName`：资源形式返回 `resource`，values 形式返回 `value`。

### 10.2 sdk/manifest/check

`user_permissions_test.go`：沿用现有测试的风格，用 `codes()` 加“只允许出现 want 里的 userPermissions 字段”的断言。

- `TestPermissionGroups`：覆盖 G1–G3、P3。
- `TestUserPermissionRiskAndRequires`：覆盖 P4、P5，包括 requires 引用自身、引用未声明的 key。
- `TestUserPermissionDefaults`：覆盖 D1–D5。具体用例：
  - role 写 `super_admin` 或 `custom`
  - critical 写了 defaults
  - false 和 `[]`
  - 无范围权限写数组
  - 有范围权限写 true
  - 写 `"flow/3"`
  - 写 `"own"` 但 `own: false`
  - 写枚举外的值
- `TestUserPermissionScope`：覆盖 S1–S11。options 部分的用例：
  - 路由未声明，报 `unknown_route`
  - POST 路由，报 `invalid`
  - user 路由，报 `invalid`
  - 带 `/:id` 参数，报 `invalid`
  - resource 写 `account` 或 `value`，报 `reserved`
- `TestCriticalInheritFrom`：覆盖 P7。critical 从已声明的 low key 继承报错；从未声明的旧 key 继承不报错。
- `TestRetiredUserPermissionFields`：`files["manifest.json"]` 里含 `sensitive` 或 `defaultRoles` 时报 `unsupported`；files 为 nil 时不报。
- 保留 `TestUserPermissionKeyNotAScope`。

`routes_check_test.go`：

- `TestRouteScopeFrom`：覆盖 R1–R4，并确认现有 `accounts.from` 的用例仍然通过（`valueFrom` 抽取后回归）。

`ui_test.go`：

- 在现有的 `TestAccountActions` 中加一个用例：permission 声明了 scope，报 `unsupported`。
- 在现有的 settings permission 测试中加一个用例：权限带 scope，报 `unsupported`（U2）。

下限测试，放在 `user_permissions_test.go` 里，不新开文件：

- `TestPluginPermissionsFloor`：
  - 用了 defaults 且 `hostCompat >=0.1.286` → `below_feature_core`。
  - 用了 defaults 且 `>=0.1.289` → 不报。
  - 只写 key 和 label，且 `>=0.1.0` → 不报（保护现有夹具）。
  - `hostUICompat ^3.16` 且 `hostCompat >=0.1.286` → `below_ui_core`。

### 10.3 perm（`pattern_test.go`）

- `TestCanonicalVectors` 和 `TestImpliesVectors` 自动覆盖 §6 的向量。
- `TestImpliesPreorder` 原来只收集 `AccountUpdate` 的模式，改为按 action 分组收集，覆盖一个插件 action。
- 新增 `TestParsePluginScope`：`…:flow/3` 解析为 `Scope{Res:"flow", Value:"3"}`；`…:own`、无范围、核心的 `account/5` 的 `Res` 都为空。
- 新增 `TestScopePatternsMatchManifest`，确保两边定义一致：
  - `scopeResourcePattern == manifest.ScopeResourcePattern`
  - `scopeValuePattern == manifest.ScopeValuePattern`
  - `manifest.PermissionModule("k") == PluginPrefix+"k"`
  - 测试可以 import sdk/manifest，perm 的非测试代码仍保持叶子包。

### 10.4 core（`ports_identity_test.go`）

`TestPermissionSetPluginScopes`：

- `PermissionSetOf("plugin.m:f:m:flow/2")`：`Has("…:flow/2")` 为 true，`Has("…:flow/3")` 为 false，`Has("plugin.m:f:m")` 为 false，`HasAction` 为 true。
- `Restrict([]string{"plugin.m:f:m"})` 得到只含 `flow/2` 的集合。
- 授予人持有 `flow/2`、想授 `flow/3`：`Missing(...).Keys` 等于 `["plugin.m:f:m:flow/3"]`。
- `KeyList` 输出规范 key。

### 10.5 插件

- 9 个插件的 manifest 测试都调用 `check.Validate(…Tooling)`，全部通过。
- moderation：断言 exempt 的 `RiskLevel()` 为 critical，`DefaultGrants()` 为 nil，`InheritFrom` 为空。
- growth：断言 stats 为 sensitive，`DefaultGrants()` 为 `{admin:[""]}`。
- volcengine：hostCompat 兼容性表中 `"0.1.288": false`、`"0.1.289": true`。

### 10.6 e2e

本章不需要新增 e2e。安装和角色流程的 e2e 由第 3、5 章负责，它们会间接覆盖新的 manifest 字段。

## 11. 风险与未决问题

- **R1（已定：exempt 改 critical，线上授权保留不动）**
  - 现状：moderation 0.12.0 已随核心 0.1.286 上线（§2.6），`inheritFrom: ["moderation:read"]` 已执行，推测 admin 角色已持有 `moderation:exempt`。
  - 做法：exempt 标为 critical、不写 defaults、按 P7 删除 inheritFrom。key 在线上已存在，升级时不会再套默认或继承，`SyncPlugin` 只把 `risk` 列改为 critical，**已有授权保留不动**（不做撤销迁移）。
  - 后果：以后新装的部署只有超管持有 exempt；存量部署里已持有的角色继续生效，角色编辑页显示“关键”徽标，只有持有者能再授出（第 2 章 §2.3.7）。
- **R2（推测的版本号）**：`PluginPermissionsFirstCore = "0.1.289"` 和 `lastCore "0.1.288"` 都是推测，按线上最新是 0.1.288 推算，发布时要改成真实版本。所有内置插件的 hostCompat 和测试里的兼容表也跟着改。
- **R3（前端联动）**：`vectors.json` 中 `plugin.x:y:own` 从 invalid 改为 valid，控制台的 `implies.spec.ts` 会立刻失败，所以第 5 章必须在同一个 PR 里实现 TS 的插件语法。
- **R4（资源改名）**：插件升级改了 `scope.resource` 时，旧授予永远不会匹配，属于向收紧方向失败。第 2 章 `SyncPlugin` 的 `pruneScopes` 会删掉不再适配的范围授权并写审计（`via: scope_changed`）。check 无法比对版本之间的差异。
- **Q1（已定）values 形式的资源名固定为 `value`**：存储 key 为 `…:value/input`，不加可选 `resource`，S1 不变。
- **Q2（已定）options 路由的权限**：本章只要求 options 是 GET admin 路由，它的 permission 可以是任意已声明的 key。核心通过 `GET /api/v1/plugins/:key/scope-options?permission=` 代理调用（`role:read` 或 `plugin:read` 任一放行），带上**正在配置的那个权限**的 `action_scope`；SDK 提供 `pluginsdk.ScopeOptions(fn)`（第 4 章）。check 不需要为此改动。
- **Q3（已定）风险等级**：`claude_oauth:manage`、`codex:manage`、`moderation:read` 提为 sensitive（§8.2）。只影响以后新建 key 的自动授予，已有授权不变。
- **Q4（已定）第三方插件**：不做旧字段兼容（生产发布者只有官方）。旧 manifest 重新打包时被 X1 拒绝；已安装的插件在核心升级后不再获得默认授予，已有授权保留（§4.5）。
