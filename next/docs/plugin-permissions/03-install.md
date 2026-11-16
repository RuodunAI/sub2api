# 第 3 章 安装、启用、升级、停用、卸载流程

> 基于 `touwaeriol/sup2api` develop @ dae6ba57（只读）。路径相对 `next/server/internal/`，除非另写。
> 本章只写安装侧（`plugin/install`、`plugin/api`、e2e 辅助）。authz 目录、`SyncPlugin`、`CanGrantToRoles`、`PluginGrantees`、按插件矩阵读写接口属于第 2 章；manifest 字段与校验属于第 1 章；运行时 `scopeFrom` 属于第 4 章；前端属于第 5 章。本章用到的跨章接口已经和对应章节对齐过，见 §3.8。
>
> 存储为历史：本目录是 0.1.289 时的设计。0.1.290 起角色的授予（含范围授予）与用户的角色只存在 `casbin_rule`（迁移 0100，CONTRACTS §99），下文的 `role_permissions`、`role_permission_scopes`、`user_roles` 及引用它们的语句与行号都是当时的写法，那三张表已删除。版本号也是当时的推测：这部分随 0.1.290 与 Casbin、会话一起发布，0.1.289 没有单独发布，`PluginPermissionsFirstCore`、UI 3.16 的核心下限和内置插件的 `hostCompat` 都是 0.1.290；`0.1.289-plugin-perm-down` 只用于停在 0099 的开发库，线上的升级与回退见 `deploy/rollback/0.1.290-release.md`。

---

## 3.1 本章范围

1. 安装确认页的数据（`GET /plugins/:key/versions/:version/review`）：`user_permissions[]` 带上 risk、group、requires、scope、defaults，每一格给出“安装者能否命名”和“将挂起复核”；新增角色列、权限分组和“本版移除”。
2. 同意请求（`POST …/consent`）：`new_permission_roles` 改成“本地 key → 角色 → 范围列表”，删除 `role_keys_for_new_permissions`。改造 roleGrants、planRoleGrants、withholdGrants、checkRoleGrantRights：默认值也要过 pluginGrantee（D2），过不了的进入现有的待复核队列；RoleGrant 加 scope 和 risk；critical 只有超管能授（D3）。
3. 升级：只给新 key 套默认值。上一版 removed、这一版重新声明的 key 算作“已有”，不套默认值（D6）。autoGrantRoles 规则按三级风险映射。说明内置插件随核心升级时的系统同意路径。
4. 启用、停用、卸载、重装：核对现有代码路径，说明本方案要改哪些地方。
5. 插件详情“权限”标签里的 ①访问权限：盘点现有的撤销接口，补上缺的“收窄 / 重新授予”。

---

## 3.2 现状（逐条核对过代码）

### 3.2.1 同意请求与角色授予

| 位置 | 现状 |
|---|---|
| `plugin/install/consent.go:22-33` | `ConsentRequest{Grants, Denied, NewPermissionRoles map[string][]string, RoleKeysForNewPermissions []string}` |
| `consent.go:37-42` `overrideRoles` | 先查 `NewPermissionRoles[local]`；查不到再用 `RoleKeysForNewPermissions`（非空时作用于其余全部新权限） |
| `consent.go:106-110`（`decide` 内） | `new_permission_roles` 里写了未声明的本地 key 时返回 400 `unknown` |
| `consent.go:268-344` `decideAndApply` | 顺序：`decide` → `checkGrantRights`（宿主权限）→ `catalogKeys` → `checkRoleGrantRights` → `roleGrants` → `upgradeRoleGrants` → `grants.Record` → 首装时 `ApplyDefaults(syncM, roles)`，升级且有运行版本时 `SyncPlugin(unionDefs(old(nil), new(roles)))` → 审计 `plugin.consent`（detail 带 `role_keys`、`new_permission_roles`） |
| `consent.go:363-381` `upgradeRoleGrants` | 首装（`!c.upgrade`）**直接返回，默认值不做任何检查**。升级分三种：操作者同意时，对“需要同意”的授予所去的角色（扣除请求里点名、已检查过的）调用 `checkRolesGrantable`，过不了返回 403；auto（上传沿用）且非系统时，只要有需要同意的就返回 `errRoleGrantsNeedConsent`；系统同意时调用 `withholdGrants` 挂起需要同意的，再 `auditAutoGrants` |
| `consent.go:496-507` `checkRoleGrantRights` | 只检查请求**点名**的角色，并且只针对目录里还没有的 key：`role:define` + `CanGrantToRoles` |
| `consent.go:511-529` `checkRolesGrantable` | 系统或角色为空时直接通过；否则要求 `role:define`，再调用 `Permissions.CanGrantToRoles` |
| `consent.go:534-562` `checkNamedRolesExist` | 未知角色返回 400，字段为 `role_keys_for_new_permissions` 或 `new_permission_roles.<local>` |
| `consent.go:567-584` `catalogKeys` | `SELECT key FROM permissions WHERE key = ANY(...)`，**不按 status 过滤**；范围是本插件的 userPermissions 及其 inheritFrom |
| `defaults.go:104-118` `roleGrants` | 优先级：覆盖 > 改名（inheritFrom 命中已有 key 时给 `[]`）> `up.Roles()`（defaultRoles，省略时为 admin） |
| `defaults.go:58-77` `PermissionDefs` | `grants == nil` 时 InheritFrom 也清空（激活路径：没有任何同意支撑的 key 不授予）；`Sensitive: up.Sensitive`；`GrantRoles: grants[up.Key]` |
| `defaults.go:34-49` `ApplyDefaults` | `SyncPlugin(PermissionDefs(m, grants))`，然后删除 manifest 不再请求的宿主权限授权行，revoked 行保留（0.1.287） |
| `rolegrants.go:29` | `autoGrantRoles = admin, operator, supplier` |
| `rolegrants.go:43-55` `RoleGrant` | `{Version, Permission, Role, Sensitive, Via: default\|inherit, NeedsConsent}`，**没有范围** |
| `rolegrants.go:62-102` `planRoleGrants` | 只为目录里没有的 key 列出授予：`roles[up.Key]` 标为 builtin（覆盖里的自定义角色因此也需要同意）+ inheritFrom 当前持有者。`NeedsConsent = Sensitive \|\| !builtin \|\| role ∉ autoGrantRoles` |
| `rolegrants.go:111-132` `inheritHolders` | 只读 `role_permissions`（无范围行），**不读 `role_permission_scopes`** |
| `rolegrants.go:157-181` `withholdGrants` | 新 key 只授予不需要同意的角色，并把 InheritFrom 清空（继承在计划时就解析成显式角色） |
| `rolegrants.go:187-203` `auditAutoGrants` | 审计 `plugin.consent.auto`，detail `{version, granted, pending, review_required}` |
| `rolegrants.go:212-247` `PendingRoleGrants` | 只从 `plugin.consent.auto` 的 `detail.pending` 读；条件为 `created_at >= plugins.installed_at`（重装后清零）、尚无同版本的 confirm/reject、reject 按 (permission, role) 跨版本清除、权限和角色都存在、角色还没有 `role_permissions` 行；按 (permission, role) 去重 |
| `rolegrants.go:255-296` `ReviewRoleGrants` | 整个版本一起确认或拒绝；需要 `checkRolesGrantable`；确认时 `grantReviewed` → `GrantPluginPermission(key, roles)` |

### 3.2.2 review

- `review.go:130-143` `ReviewUserPerm{Key, Label, Description, Sensitive, DefaultRoles, InheritFrom, New}`。
- `review.go:50-55` `NewRoleGrants []RoleGrant`、`RoleGrantsNeedConsent bool`。
- `review.go:408-428` `resolveDefaultRoles`：以“不覆盖”的同意算 DefaultRoles、New、plan。
- `review.go:439-494` `Review(ctx, key, version)`：**不带查看者**，所以给不出“这一格你能不能命名”。
- 调用方：`plugin/api/plugins.go:383`（review 路由）、`plugins.go:235`（插件详情 `manifest` 字段）、`upload.go:72-81`（上传后把 review 返回，并据此调用 `carryOverConsent`）。

### 3.2.3 升级沿用与内置插件

- `upload.go:259-276` `carryOverConsent`：宿主权限既没有新增也没有扩大、没有 revoked，并且（`!RoleGrantsNeedConsent` 或者是系统）时，以 `auto=true` 调用 `consent`；遇到 `errRoleGrantsNeedConsent` 时留在 awaiting。
- `builtin.go:163-176`：内置版本处于 awaiting 时，用系统 ctx（`withSystem`）和 actor 0 调用 `Consent`，注释写着“new permissions go to their manifest defaultRoles”；`builtin.go:197-211`：从未启用过的调用 `Rollout.Enable(…, 0)`，已启用的版本较低时调用 `Rollout.Upgrade(…, 0)`。
- 系统身份：`isSystem(ctx)` 为真时，`checkGrantRights` 和 `checkRolesGrantable` 都放行；`authz.pluginGrantor` 把 actor 0 当作超管（`authz/plugin_catalog.go:148-154`）。

### 3.2.4 启用、停用、卸载

| 路径 | 现状 |
|---|---|
| 首次启用 | `rollout/controller.go:327-376` `Enable`：状态需为 installed/disabled，目标版本取已批准的 active，否则取最新已批准版本。`coordinator.go` commit 时 `upgraded := action=="upgrade" \|\| (prevActive!=nil && prevActive!=target)`，**首次启用 prevActive 为 nil，不调 ApplyDefaults**（权限和角色授权在同意时已经写好，状态为 disabled），之后调用 `SetPluginActive(true)`（`coordinator.go:412`）。`plugin/api/plugins.go` 的 `enable` 对 awaiting_consent 返回 409 |
| 升级激活 | `coordinator.go:403` `ApplyDefaults(target manifest, nil)`：`PermissionDefs(nil)` 表示激活时新建的 key 一律不授予、不继承；`SyncPlugin` 会把 target 不再声明的 key **DELETE** 掉（`authz/plugin_catalog.go:98`） |
| 停用 | `controller.go:449` `SetPluginActive(false)`。`authz/plugin_catalog.go:201-215` 执行 `UPDATE permissions SET status=$2 WHERE plugin_key=$1`，**不排除 removed**。角色授权保留 |
| 重新启用 | 同“首次启用”的 commit，不需要确认，原样恢复 |
| 卸载 | `uninstall.go:185-189`：quiesce 时 `SetPluginActive(false)`；`uninstall.go:91-95`：`DeletePlugin` 删除全部权限行，`role_permissions` 和 `role_permission_scopes` 的外键都是 `ON DELETE CASCADE`（`migrations/0001_core.sql:180-184`、`0097_authz_hardening.sql:110-117`）；`permissions.plugin_key` 也会随 plugins 行级联删除 |
| 重装 | 重装是一次新的首装（`StatusAwaitingConsent`），会再走一遍确认页。`PendingRoleGrants` 用 `installed_at` 过滤（`rolegrants.go:218`），上一生命周期挂起的授予不会带过来 |

### 3.2.5 ①访问权限（宿主权限）的现有接口

| 接口 | 实现 | 说明 |
|---|---|---|
| `GET /plugins/:key/grants`（`plugin:read`） | `plugin/api/plugins.go:592` → `install.LoadGrants` | 列出 `plugin_permission_grants`（permission, risk, scope, status, version, granted_by/at） |
| `DELETE /plugins/:key/grants/:permission`（`plugin:manage`） | `plugins.go:605` → `consent.go:630-650` `RevokeGrant` → `grants.Revoke` | 活动行和所有 staged 集里 granted 的行都改成 revoked，审计 `plugin.grant.revoke`，然后 `Notify` |
| `GET /plugins/:key` | `plugins.go:235` `manifest`（Review）+ `grants` | `manifest.host_permissions[]` 带请求范围、risk、`requires`（授出所需的控制台权限）、`revoked` |
| 撤销保持（0.1.287，commit c1212440） | `grants.Revoked`；`decide`（consent.go:72-74、134-146）；`ApplyDefaults` 保留 revoked 行；`grants.Withdraw` 把 staged 集的 revoked 行搬回去；`carryOverConsent` 遇到 revoked 时不沿用 | 撤销之后升级不会再自动加回来，只有在确认页上明确点名才会恢复 |

**缺口**：没有“收窄范围”的接口，也没有“重新授予”的接口。现在想恢复一个被撤销的权限，只能等下一个版本的同意时点名。原型 D 里的“收窄 / 重新授予”目前没有后端接口（见 §3.6）。

---

## 3.3 设计：数据结构

### 3.3.1 RoleGrant（`rolegrants.go`）

```go
// RoleGrant is one (role, scope) a permission created by a consent goes to.
type RoleGrant struct {
	Version    string `json:"version,omitempty"` // review list only
	Permission string `json:"permission"`        // qualified key
	Role       string `json:"role"`
	// Scope is the canonical suffix ("" = every row, a role_permissions
	// row; "own", "value/input", "flow/3" go to role_permission_scopes).
	Scope string `json:"scope"`
	Risk  string `json:"risk"` // manifest.RiskLow | RiskSensitive | RiskCritical
	// Via: "default" (manifest defaults), "override" (the consent named it),
	// "inherit" (a holder of an inheritFrom key).
	Via          string `json:"via"`
	NeedsConsent bool   `json:"needs_consent"` // upgrade rule (autoGrantRoles)
	Nameable     bool   `json:"nameable"`      // the consenting actor may name the role (D2/D3)
	Withheld     bool   `json:"withheld"`      // waits for review instead of being granted
}

const (
	viaDefault  = "default"
	viaOverride = "override"
	viaInherit  = "inherit"
)
```

改动：`Sensitive` 改成 `Risk`；新增 `Scope`、`Nameable`、`Withheld`；新增 `Via = override`。override 必须单独区分，因为它和默认值的处理不一样：点名了不可命名的角色返回 403，默认值不可命名则挂起。

### 3.3.2 ConsentRequest（`consent.go`）

```go
type ConsentRequest struct {
	Grants []GrantInput `json:"grants"`
	Denied []string     `json:"denied"`
	// NewPermissionRoles overrides, per local userPermission key, the grants
	// of a permission created now: role -> true (every row) or scope values
	// ("*", "own", an enum key of scope.values or its canonical "value/<key>",
	// a runtime id "flow/3").
	// A key present replaces the defaults for the roles the actor may name:
	// those it leaves out get nothing, {} grants none; a default to a role
	// the actor may not name still goes to review (D2). Keys absent keep
	// the manifest defaults.
	NewPermissionRoles map[string]map[string]manifest.RoleDefault `json:"new_permission_roles,omitempty"`
}
```

- 删除 `RoleKeysForNewPermissions` 和 `overrideRoles`。
- `manifest.RoleDefault`（第 1 章）是 `{Unscoped bool; Scopes []string}`，JSON 写法为 `true | [...]`，复用它解码，不另定义类型。前端（第 5 章 `toManifestGrant`）发 `true`，或 `"*"` 加规范后缀（`own`、`value/input`、`flow/3`）。

范围值规范化，新增一个函数（同意请求会改到三处：校验、检查、计划，所以抽出来；第 1 章的 `GrantSuffix` 只认 `*`、`own`、枚举 key，不认运行时 id）：

```go
// overrideGrants normalizes the consent's overrides to role -> canonical
// suffixes, per local key; field errors name new_permission_roles.<local>.<role>.
func overrideGrants(m *manifest.Manifest, in map[string]map[string]manifest.RoleDefault) (map[string]map[string][]string, []core.FieldError)
```

规则如下（不满足的返回 400，code 写在括号里）：
1. 本地 key 必须是本插件声明的（`unknown`，沿用 `decide` 里现有的检查，并把它挪进这个函数）。
2. `Unscoped` 或任意值为 `"*"` 时结果为 `[""]`（全部包含其他值，和 `perm.Implies` 一致）。
3. 权限没有 `scope` 却写了范围值（`invalid`，与 authz `resolvePermissions` 拒绝非法 key 的码一致，不新造 `invalid_scope`）。
4. 每个值先试 `up.Scope.GrantSuffix(v)`（第 1 章，清单写法：`*`、`own`、枚举 key）；不通过时用 `perm.Parse(manifest.PermissionKey(m.Key, local) + ":" + v)`，要求解析出的 `Scope.Res == up.Scope.ResourceName()`，枚举形式还要求 `Scope.Value` 是 `scope.values` 的 key，取 `p.Suffix()`。所以规范后缀（`value/input`、`flow/3`）和清单写法都接受，前端可以直接回传后缀（第 5 章）。`own` 在 `!scope.Own` 时一律拒绝（`invalid`）。
5. 去重并排序。`false` 或空数组表示这个角色不授予（第 1 章的 `RoleDefault` 零值）。

`checkNamedRolesExist` 改为遍历嵌套 map 的角色 key，字段用 `new_permission_roles.<local>`。

### 3.3.3 roleGrants 与 PermissionDefs（`defaults.go`）

```go
// roleGrants resolves, per local key, the role -> suffixes a permission
// created now is planned with: none for a key the catalog has (existing
// includes removed rows, D6) or for a rename; else the manifest defaults
// (up.DefaultGrants(), chapter 1) with the override's roles replacing
// theirs. named reports, per overridden key, the roles the override names
// (an overridden key with {} is present and names none).
func roleGrants(m *manifest.Manifest, overrides map[string]map[string][]string, existing map[string]bool) (roles map[string]map[string][]string, named map[string]map[string]bool)

func PermissionDefs(m *manifest.Manifest, grants map[string]map[string][]string) []core.PermissionDef
```

`PermissionDefs` 按第 2 章定稿的字段填写：

| PermissionDef 字段 | 来源 |
|---|---|
| `Key`/`Module`/`Label`/`Description`/`Sort` | 不变 |
| `Risk` | `up.RiskLevel()`（不写时为 low） |
| `Group`、`GroupLabel`、`GroupSort` | `up.Group`；在 `m.PermissionGroups` 里按 key 查 label，下标 ×10 作为 sort |
| `Requires` | `up.Requires` 逐个转成 `manifest.PermissionKey(m.Key, r)` |
| `ScopeType`、`Scope` | `up.Scope == nil` 时为 `""`/nil；否则为 `"plugin:" + up.Scope.ResourceName()`，`Scope` 为 `up.Scope` 的拷贝且 `Resource = ResourceName()`（第 2 章复用 `manifest.PermissionScope`） |
| `Defaults` | `up.DefaultGrants()`（插件建议，只存不授，供“恢复插件建议”使用） |
| `GrantRoles` | `grants[up.Key]`（nil 表示不授予） |
| `InheritFrom` | **字段删除**（第 2 章同时删掉 `grantInherited` 和 SyncPlugin 的继承步骤）。所有同意路径都会经过 `withholdGrants`，继承已在计划时解析成显式 `GrantRoles`（§3.4.2–3.4.3），SyncPlugin 只负责授予 GrantRoles |
| `Sensitive` | 删除 |

暂存升级用 `stageDefs(next, running)`（二审取代 `unionDefs`）：新旧都声明的 key 的范围与 `defaults` 取运行版本，风险取较高的，其余取新版本；调用顺序见 §3.4.5。

### 3.3.4 Review 输出（`review.go`）

```go
type ReviewUserPerm struct {
	Key         string                          `json:"key"` // local
	Label       core.LocalizedText              `json:"label"`
	Description core.LocalizedText              `json:"description,omitempty"`
	Risk        string                          `json:"risk"`
	Group       string                          `json:"group,omitempty"`
	Requires    []string                        `json:"requires,omitempty"` // local keys
	Scope       *manifest.PermissionScope       `json:"scope,omitempty"` // Resource = ResourceName(), as GET /permissions
	Defaults    map[string][]string             `json:"defaults"`        // up.DefaultGrants(): canonical suffixes, as cells.grant
	InheritFrom []string                        `json:"inherit_from,omitempty"`
	New         bool                            `json:"new"`     // the catalog has no such key yet
	Revived     bool                            `json:"revived"` // removed earlier, declared again: grants come back as they were
	Renamed     bool                            `json:"renamed"` // inherits from a key the catalog has: no defaults
	// Cells cover every role of Review.RoleColumns.
	Cells map[string]ReviewCell `json:"cells"`
}

// ReviewCell is one (permission, role) of the consent matrix, for the viewer.
type ReviewCell struct {
	// Grant is what a consent without override gives (canonical suffixes;
	// [] none, [""] every row). For a revived key, the grants it revives
	// with; for any other existing key, [].
	Grant    []string `json:"grant"`
	Nameable bool     `json:"nameable"`
	Reason   string   `json:"reason,omitempty"` // role_define|critical|superuser_role|not_editable|not_assignable|held
	Withheld bool     `json:"withheld"`         // Grant goes to review instead (D2)
}

type ReviewRoleColumn struct {
	Key       string             `json:"key"`
	Name      core.LocalizedText `json:"name"`
	Builtin   bool               `json:"builtin"`
	Superuser bool               `json:"superuser"`
	Nameable  bool               `json:"nameable"`
	Reason    string             `json:"reason,omitempty"`
}

type ReviewRemovedPerm struct {
	Key     string             `json:"key"` // local
	Label   core.LocalizedText `json:"label"`
	Holders []string           `json:"holders,omitzero"` // role keys, super admin aside; nil without role:read (C4)
}
```

`Review` 新增的字段：`PermissionGroups []manifest.PermissionGroup \`json:"permission_groups"\``、`RoleColumns []ReviewRoleColumn \`json:"role_columns"\``、`RemovedUserPermissions []ReviewRemovedPerm \`json:"removed_user_permissions"\``（只在升级时填）。`NewRoleGrants` 和 `RoleGrantsNeedConsent` 保留，元素换成新的 RoleGrant。

`Review` 的签名加上查看者：

```go
func (s *Service) Review(ctx context.Context, key, version string, actorID int64) (*Review, error)
```

JSON 示例（管理员 id=7 安装提示词审核 1.0.0；admin 持有 admin 角色，可分配清单为 operator/supplier/user/自定义）：

```json
{
  "plugin_key": "moderation", "version": "1.0.0",
  "permission_groups": [{"key": "flow", "label": {"zh": "审核流程"}}, {"key": "action", "label": {"zh": "处置"}}],
  "role_columns": [
    {"key": "super_admin", "name": {"zh": "超级管理员"}, "builtin": true, "superuser": true, "nameable": false, "reason": "superuser_role"},
    {"key": "admin", "name": {"zh": "管理员"}, "builtin": true, "superuser": false, "nameable": false, "reason": "held"},
    {"key": "operator", "name": {"zh": "运维"}, "builtin": true, "superuser": false, "nameable": true},
    {"key": "support", "name": {"zh": "客服"}, "builtin": false, "superuser": false, "nameable": true}
  ],
  "user_permissions": [
    {
      "key": "flow:manage", "label": {"zh": "编辑审核流程"}, "risk": "sensitive", "group": "flow",
      "requires": ["flow:read"],
      "scope": {"resource": "flow", "options": "/scopes/flow", "own": true},
      "defaults": {"admin": [""], "operator": ["own"]},
      "new": true, "revived": false, "renamed": false,
      "cells": {
        "super_admin": {"grant": [], "nameable": false, "reason": "superuser_role", "withheld": false},
        "admin": {"grant": [""], "nameable": false, "reason": "held", "withheld": true},
        "operator": {"grant": ["own"], "nameable": true, "withheld": false},
        "support": {"grant": [], "nameable": true, "withheld": false}
      }
    },
    {
      "key": "exempt", "label": {"zh": "免于审核观察与封禁"}, "risk": "critical", "group": "action",
      "defaults": {}, "new": true, "revived": false, "renamed": false,
      "cells": {
        "admin": {"grant": [], "nameable": false, "reason": "critical", "withheld": false},
        "operator": {"grant": [], "nameable": false, "reason": "critical", "withheld": false},
        "support": {"grant": [], "nameable": false, "reason": "critical", "withheld": false}
      }
    }
  ],
  "new_role_grants": [
    {"permission": "plugin.moderation:flow:manage", "role": "admin", "scope": "", "risk": "sensitive", "via": "default", "needs_consent": false, "nameable": false, "withheld": true},
    {"permission": "plugin.moderation:flow:manage", "role": "operator", "scope": "own", "risk": "sensitive", "via": "default", "needs_consent": false, "nameable": true, "withheld": false}
  ],
  "role_grants_need_consent": false,
  "removed_user_permissions": []
}
```

（超管列的 `nameable=false` 表示“不写入”：超管本来就拥有全部权限，前端把这一列固定显示为全勾。首装时 `needs_consent` 恒为 false，见 §3.4.2。）

### 3.3.5 ConsentResult

```go
type ConsentResult struct {
	PluginKey string      `json:"plugin_key"`
	Version   string      `json:"version"`
	Status    string      `json:"status"`
	Upgrade   bool        `json:"upgrade"`
	Grants    []Grant     `json:"grants"`
	Withheld  []RoleGrant `json:"withheld"` // new: grants this consent left for review
}
```

---

## 3.4 设计：同意流程

### 3.4.1 可命名规则只写一处（D2/D3）

```go
// nameable is the rule of whether the consenting actor may name role for a
// new permission of risk (D2, D3): role:define, critical only by a
// superuser, and the catalog's pluginGrantee/editableRole (chapter 2).
func nameable(gs core.PluginGrantees, canDefine bool, role, risk string) (bool, string) {
	switch {
	case !canDefine:
		return false, "role_define"
	case risk == manifest.RiskCritical && !gs.Superuser:
		return false, "critical"
	}
	if reason, denied := gs.Denied[role]; denied {
		return false, reason
	}
	return true, ""
}

// grantees reads the actor's PluginGrantees and role:define; the system
// (isSystem or actor 0) may name every role.
func (s *Service) grantees(ctx context.Context, tx pgx.Tx, actorID int64, roles []string) (core.PluginGrantees, bool, error)
```

`grantees` 是 `markNameable` 和 Review 两处都要用的读取，所以抽成函数。系统身份直接返回 `PluginGrantees{Superuser: true}` 和 `canDefine=true`，否则调用 `s.d.Authz.Can(ctx, actorID, "role:define")` 和 `s.d.Permissions.PluginGrantees(ctx, tx, actorID, roles)`（第 2 章：只读，不 Bump，不报错）。

最终的写入仍由第 2 章会加锁的 `CanGrantToRoles(ctx, tx, actorID, roles, critical)` 把关（§3.4.4），`PluginGrantees` 只用来分类和展示，所以两次调用之间就算有并发改动也不会越权。

### 3.4.2 planRoleGrants：带上范围、风险和升级规则

```go
func planRoleGrants(ctx context.Context, q store.Querier, m *manifest.Manifest, existing map[string]bool,
	roles map[string]map[string][]string, overridden map[string]map[string]bool, upgrade bool) ([]RoleGrant, error)
```

- 只看 `!existing[key]` 的 key（existing 包含 removed 行，见 §3.5.1）。
- `roles[up.Key]` 里的每个 (role, scope) 生成一条：`named[up.Key][role]` 为真时 `Via = override`，否则为 `default`（参数 `overridden` 即 roleGrants 返回的 `named`）。
- inheritFrom：`inheritHolders` 改为同时读两张表，返回 `roleRef{key, builtin, scope, scopeType}`：

```sql
SELECT p.key, r.key, r.builtin, '' AS scope, p.scope_type, p.risk FROM role_permissions rp
  JOIN permissions p ON p.id = rp.permission_id JOIN roles r ON r.id = rp.role_id
 WHERE p.key = ANY($1) AND NOT r.superuser
UNION ALL
SELECT p.key, r.key, r.builtin, rs.scope, p.scope_type, p.risk FROM role_permission_scopes rs
  JOIN permissions p ON p.id = rs.permission_id JOIN roles r ON r.id = rs.role_id
 WHERE p.key = ANY($1) AND NOT r.superuser
ORDER BY 1, 2, 4
```

  继承规则从第 2 章移到这里（第 2 章删掉 `grantInherited` 后，继承只剩这一处实现）：
  - 规则 1：带范围的行只在旧 key 的 `scope_type` 等于新 key 的 `"plugin:"+ResourceName()` 时才继承；不相等就丢弃这一行，无范围行照常继承。
  - 规则 1b：`scope_type` 相同的范围行还要能落到新 key 的声明上（`manifest.PermissionScope.Takes`，与目录 `checkScopes`/`pruneScopes` 同一套判定）：新声明不再允许 own 时丢弃 `own` 行，values 里删掉的值丢弃对应的 `value/<v>` 行。否则这些后缀会进 `GrantRoles`，被目录判为 invalid，整个同意 400。
  - 规则 2（与第 1、2 章约定）：新 key 为 critical 时，只从同样是 critical 的旧 key 继承。旧 key 的风险从查询里多取的 `p.risk` 得到。非 critical 的旧 key 跳过不继承，并记录到计划的 `skipped` 中。第 1 章的清单校验看不到已不再声明的 key 的风险，所以这项检查只能放在同意时做。
  - 被跳过的继承来源以 `[]InheritSkip{Permission, From, Reason: "scope_type"|"scope"|"risk"}` 返回，并写进同意审计（`plugin.consent` 和 `plugin.consent.auto` 的 detail 都加 `inherit_skipped`）。规则 1 只丢弃带范围的行，同样记一条 `scope_type`；规则 1b 记 `scope`。
- `Risk = up.RiskLevel()`。
- **升级规则（autoGrantRoles 对应三级风险）**：

```go
NeedsConsent: upgrade && (risk != manifest.RiskLow || !builtin || !slices.Contains(autoGrantRoles, role))
```

  low → 可自动；sensitive、critical → 一律需要同意。首装（`upgrade=false`）时 NeedsConsent 恒为 false：首装本来就要有人（或系统）同意，不存在“沿用”的问题。
- 去重键从 (permission, role) 改成 (permission, role, scope)。

### 3.4.3 settleRoleGrants：替代 upgradeRoleGrants，首装和升级同一入口

```go
// settleRoleGrants plans the role grants of the consent, marks what the
// actor may name, decides what waits for review and checks the rest:
//   system   (built-in, isSystem): withheld = NeedsConsent (everything is nameable);
//   auto     (upload carry-over):  any NeedsConsent -> errRoleGrantsNeedConsent;
//                                  else withheld = !Nameable;
//   operator (consent page):       withheld = !Nameable for default/inherit,
//                                  overrides were checked already (403);
//   on an overridden key a default to a nameable role the override left
//   out is dropped (the actor decided), to an unnameable one withheld.
// It returns the roles to sync with (withholdGrants), the plan and the
// withheld grants.
func (s *Service) settleRoleGrants(ctx context.Context, tx pgx.Tx, actorID int64, c *consentable,
	existing map[string]bool, roles map[string]map[string][]string, overridden map[string]map[string]bool, auto bool,
) (syncRoles map[string]map[string][]string, plan, pending []RoleGrant, err error)
```

主体（低嵌套，一处循环）：

```go
plan, err := planRoleGrants(ctx, tx, c.manifest, existing, roles, overridden, c.upgrade)
// ...
system := isSystem(ctx)
if auto && !system && needConsent(plan) {
	return nil, nil, nil, errRoleGrantsNeedConsent
}
gs, canDefine, err := s.grantees(ctx, tx, actorID, rolesOf(plan))
// ...
for i := range plan {
	g := &plan[i]
	g.Nameable, _ = nameable(gs, canDefine, g.Role, g.Risk)
	g.Withheld = system && g.NeedsConsent || !system && g.Via != viaOverride && !g.Nameable
}
// an override left these out on purpose
plan = slices.DeleteFunc(plan, func(g RoleGrant) bool {
	local, _ := manifest.LocalPermission(c.manifest.Key, g.Permission)
	return g.Via == viaDefault && g.Nameable && overridden[local] != nil
})
if !system { // grants made now only (!Withheld)
	if err := s.checkGrantable(ctx, tx, actorID, plan, viaDefault, viaInherit); err != nil {
		return nil, nil, nil, err
	}
}
syncRoles, pending = withholdGrants(c.manifest, existing, plan)
return syncRoles, plan, pending, nil
```

和现状比，有三处变化：
1. **首装也要过这一步**（现在 `!c.upgrade` 直接返回）。D2 说的就是首装：管理员安装时，清单默认值里管理员自己的角色、或可分配清单外的角色，都挂起。
2. 升级时，操作者对“不可命名的默认 / 继承授予”从 **403 改为挂起**。D2 是一条全局规则，确认页上有不可命名的格子也不应该卡住整次同意。点名（override）不可命名的角色仍然返回 403，行为不变。覆盖了某一行时，这一行里**不可命名角色的默认值仍然挂起**，不会因为覆盖而被丢掉（与第 5 章约定，前端不需要 dropsPending 提示）。可命名角色里覆盖没写到的，按“操作者决定不给”从计划中删掉。
3. 所有路径都走 `withholdGrants`（现在只有 auto 和系统才走），继承在计划时就解析成显式 (role, scope)，“计划里的就是实际授予的”对所有路径都成立。

`withholdGrants` 简化为 `withholdGrants(m, existing, plan) (map[string]map[string][]string, []RoleGrant)`：判断依据从 `g.NeedsConsent` 改成 `g.Withheld`；**不再拷贝 manifest、清空 InheritFrom**，因为 `PermissionDef` 已没有 `InheritFrom`（第 2 章），`PermissionDefs` 不再映射它，同步直接用 `c.manifest`。原来的 `syncM` 返回值随之去掉（审查时简化）。

内置插件首装时：系统身份 + `upgrade=false`，所以全部可命名、全部不需要同意，`Withheld` 恒为 false，结果和现在一致（默认值全部授予）。

### 3.4.4 检查：点名的和计划要授予的，都过会加锁的 CanGrantToRoles

```go
// checkGrantable checks, with the catalog's locking CanGrantToRoles (role:define
// first), the roles of the grants of plan made now (!Withheld) whose Via is one of vias,
// in a critical batch and a non-critical batch (chapter 2's signature).
// A critical refusal also names the local keys (details.permissions).
func (s *Service) checkGrantable(ctx context.Context, tx pgx.Tx, actorID int64, plan []RoleGrant, vias ...string) error
```

- `checkRoleGrantRights` 改为：先用 `overrideGrants` 的结果，对**非 existing** 的 key 生成 `Via=override` 的临时 RoleGrant（Risk 取自 manifest），再调用 `checkGrantable(…, viaOverride)`。所以点名 critical 的非超管会收到 403 `permission_denied`，details 为 `{reason:"critical", superuser:true, permissions:["exempt"]}`（前两项来自第 2 章，`permissions` 由本层用 `WithDetails` 补上）；点名不可命名的普通角色时 details 为 `{role}`，和现在一样。
- `checkRolesGrantable(ctx, tx, actorID, roles)` 保留作为 `checkGrantable` 的底层（role:define + `CanGrantToRoles`），多一个 `critical bool` 参数透传给第 2 章。`ReviewRoleGrants` 也用 `checkGrantable(…all vias)` 按 critical 分批检查。
- 原先 `upgradeRoleGrants` 里“扣除已点名角色”（`named`）的逻辑去掉：按 Via 分开检查之后自然不会重复。

### 3.4.5 decideAndApply 改后的顺序

```text
decide（宿主权限；new_permission_roles 未知 key 的检查移到 overrideGrants）
overrides, ferrs := overrideGrants(m, req.NewPermissionRoles)     // 合并 400
checkGrantRights（宿主权限，不变）
existing, revived := catalogKeys(...)                               // §3.5.1
checkRoleGrantRights(ctx, tx, actorID, m, overrides, existing)    // 点名 → 403
roles, named := roleGrants(m, overrides, existing)
roles, plan, pending, err := settleRoleGrants(..., roles, named, auto)
grants.Record(...)                                                  // 不变
首装 / 无运行版本：ApplyDefaults(m, roles)
有运行版本（暂存）：SyncPlugin(stageDefs(PermissionDefs(m, roles), PermissionDefs(old, nil)))   // Staged
审计
```

暂存升级时新旧都声明的 key 用运行中版本的定义（old 胜出），新建的 key 仍来自 m（带同意时结算的授予）。否则同意那一刻 `SyncPlugin` 就按新声明 `pruneScopes`，运行中的旧版本提前失去范围授予；激活失败或改升别的版本时这些授予也回不来（范围行是物理删除）。裁剪推迟到激活：`rollout/coordinator.go` 激活时调用 `ApplyDefaults(m, nil)`，`SyncPlugin` 按被激活版本的声明裁剪并标记 removed。

审计：
- 操作者同意：`plugin.consent` 的 detail 删去 `role_keys`，新增 `role_grants: plan`、`pending: pending`（总是数组）。
- auto 或系统：沿用 `auditAutoGrants`（`plugin.consent.auto`），内容不变，元素是新的 RoleGrant。
- `ConsentResult.Withheld = pending`。

### 3.4.6 待复核队列（PendingRoleGrants / ReviewRoleGrants）

`PendingRoleGrants` 的 SQL 改动（新增常量 `AuditConsent = "plugin.consent"`）：

```sql
SELECT a.detail->>'version', g->>'permission', g->>'role', COALESCE(g->>'scope', ''), p.risk, COALESCE(g->>'via', '')
FROM audit_logs a CROSS JOIN LATERAL jsonb_array_elements(a.detail->'pending') g
JOIN permissions p ON p.key = g->>'permission' AND p.status <> 'removed'
WHERE a.action = ANY($3)                         -- {plugin.consent.auto, plugin.consent}
  AND a.target_type = 'plugin' AND a.target_id = $1
  AND jsonb_typeof(a.detail->'pending') = 'array'
  AND a.created_at >= (SELECT installed_at FROM plugins WHERE key = $1)
  AND ($2 = '' OR a.detail->>'version' = $2)
  AND NOT EXISTS (... confirm/reject，与现在相同，reject 仍按 (permission, role) 跨版本清除 ...)
  AND EXISTS (SELECT 1 FROM roles r WHERE r.key = g->>'role' AND NOT r.superuser)
  -- the role already has it: an unscoped row, or the same scoped row
  AND NOT EXISTS (SELECT 1 FROM role_permissions rp JOIN roles r ON r.id = rp.role_id
                  WHERE r.key = g->>'role' AND rp.permission_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM role_permission_scopes rs JOIN roles r ON r.id = rs.role_id
                  WHERE r.key = g->>'role' AND rs.permission_id = p.id AND rs.scope = COALESCE(g->>'scope', ''))
ORDER BY a.id, 2, 3, 4
```

- risk 取目录里的 `permissions.risk`（第 2 章新增的列），不读审计里写的值，所以存量审计行里只有 `sensitive` 也不受影响。
- 被 removed 的权限不再显示（确认了也不会生效，第 2 章的 `GrantPluginPermission` 会跳过 removed）。
- 范围已不合当前声明的项不再显示（读出 `p.scope`，用 `manifest.PermissionScope.Takes` 过滤）：之后的升级去掉了 own 或删掉了某个值时，这些项确认也会被目录判为 invalid，还会连累同一版本的其他项。
- 结果按 (permission, role, scope) 去重。

`ReviewRoleGrants`：继续按整个版本确认或拒绝（接口不变），检查改为 `checkGrantable` 按 critical 分批；`grantReviewed` 按 permission 汇总成 `map[role][]scope`，调用第 2 章的 `GrantPluginPermission(ctx, tx, key, roles map[string][]string)`。

不做“部分确认”：超管可以先全部确认，再到矩阵里取消个别格子；也可以全部拒绝，再在矩阵里单独授予（第 2 章接口）。这样不需要改审计的清除语义。

### 3.4.7 Review 的计算

`resolveDefaultRoles` 改名为 `resolveRoleGrants(ctx, tx, m, actorID, upgrade bool)`，Review 和 Upload 都用 `s.d.DB.Tx` 包一个只读事务来调用它：

1. `existing, revived := catalogKeys(...)`；`roles, _ := roleGrants(m, nil, existing)`；用 `planRoleGrants(…, upgrade)` 得到 plan。
2. 角色列：`SELECT key, name, builtin, superuser FROM roles ORDER BY superuser DESC, builtin DESC, id`；`gs, canDefine := s.grantees(ctx, tx, actorID, 全部非超管角色)`；每列 `nameable(gs, canDefine, role, "")`，超管列固定为 `superuser_role`。
3. `cells[role]`：`Grant` 先取 `roles[local][role]`，再并入 plan 里 Via=inherit 的同角色范围；`Nameable/Reason = nameable(gs, canDefine, role, up.RiskLevel())`；`Withheld = len(Grant) > 0 && !Nameable`（按操作者同意的口径；系统路径不会显示确认页）。plan 的 `Nameable` 和 `Withheld` 按同一规则填写。
4. `Revived`：`revived[key]`，`cells[*].Grant` 改为当前持有（复用 `inheritHolders(revivedKeys)`），前端只读显示“恢复原授权”。`Renamed`：inheritFrom 命中 existing。
5. `RemovedUserPermissions`（仅升级）：

```sql
SELECT key, label FROM permissions
 WHERE plugin_key = $1 AND status <> 'removed' AND key <> ALL($2)   -- $2 = this version's qualified keys
 ORDER BY sort
```

   持有者同样复用 `inheritHolders`，按角色 key 去重。
6. `upload.go:72-81` 改成传 `actorID`；`carryOverConsent` 的条件不变（`r.RoleGrantsNeedConsent && !isSystem`）。auto 同意中不可命名的授予按 §3.4.3 挂起。

---

## 3.5 设计：升级、内置插件、启停卸载

### 3.5.1 “已有”集合与 removed（D6）

- 第 2 章把 `SyncPlugin` 末尾的 DELETE 改成 `UPDATE … SET status='removed'`，重新声明时把行复活为插件当前的状态，且不授 `GrantRoles`、不做继承（行一直在，`xmax=0` 不成立）。
- 本章的 `catalogKeys` SQL **不需要改**就会把 removed 行算进 existing（它不按 status 过滤，`consent.go:576`）。于是 `roleGrants` 对重新声明的 key 不套默认值，`planRoleGrants` 也不为它列授予，满足“撤销过的不会被加回来”。
- 只为 review 新增一个返回值：

```go
// catalogKeys are the keys of m's userPermissions and their inheritFrom the
// catalog has (removed rows included: a key declared again is not new, D6),
// and of those the ones now removed (revived by this version).
func catalogKeys(ctx context.Context, q store.Querier, m *manifest.Manifest) (existing, revived map[string]bool, err error)
```

  SQL：`SELECT key, status = 'removed' FROM permissions WHERE key = ANY($1)`。
- inheritFrom 指向一个 removed 的旧 key：按改名处理，继承 removed 行上保留的持有者（`inheritHolders` 不按 status 过滤）。这和 D6“保留授权、恢复时带回”一致。
- 激活路径 `ApplyDefaults(m, nil)`（`coordinator.go:403`）：`nil` 的语义不变（激活时新建的 key 不授予）。D6 之后，一个“B 同意时登记 → C 激活时 removed → 之后 B 激活”的 key 会带着 B 同意时的授权复活，不再是“新建且为空”。这正是期望的行为，本章不用改代码。

### 3.5.2 autoGrantRoles 与三级风险

| 新 key 的风险 | 授予对象 | 升级时 NeedsConsent | 上传沿用（auto） | 内置随核心升级（系统） |
|---|---|---|---|---|
| low | admin / operator / supplier | false | 可沿用；不可命名的格子挂起 | 直接授予 |
| low | user / 自定义角色（覆盖或继承） | true | 不沿用，留在 awaiting | 挂起复核 |
| sensitive | 任意 | true | 不沿用 | 挂起复核 |
| critical | 只可能来自继承（defaults 禁写，第 1 章） | true | 不沿用 | 挂起复核，确认需超管 |

`autoGrantRoles` 常量不变，注释里的“non-sensitive”改为“low risk”。

### 3.5.3 内置插件随核心升级的系统同意路径

不变的部分：`EnsureBuiltin` → `Upload`（system ctx）→ `carryOverConsent`（系统不受 `RoleGrantsNeedConsent` 限制）→ `consent(auto=true)`；宿主权限有新增时由 `ensureBuiltin` 调用 `Consent`（system ctx, actor 0）。两条路径在 `settleRoleGrants` 里都识别为 system：首装时全部授予；升级时 NeedsConsent 的挂起，其余授予，并写 `plugin.consent.auto`。

需要改的只有 `builtin.go:168` 的注释（“new permissions go to their manifest defaults”）。`ConsentRequest{}` 不带覆盖，本来就按 defaults 走。

内置 9 个声明了 userPermissions 的插件清单改用 `defaults`（第 1 章）之后，存量部署上这些 key 都已存在，**升级时不会对它们重新套用默认值，存量授权不变**。新增的 key 按上表处理。

### 3.5.4 启用、停用、卸载、重装：本方案需不需要改

| 路径 | 本方案是否要改 | 说明 |
|---|---|---|
| `SetPluginActive` | **要改（第 2 章）** | 加上 `AND status <> 'removed'`，否则停用 / 启用会把 removed 改回 disabled/active。本章依赖这一改动 |
| 停用 `Disable` | 不改 | 授权保留，权限 disabled |
| 重新启用 | 不改 | 不弹确认，原样恢复（commit 时 `SetPluginActive(true)`） |
| 首次启用 | 不改 | 权限和授权在同意时已写入；首次启用的 commit 不调 ApplyDefaults |
| 升级激活 | 不改（随 D6 自然生效） | `ApplyDefaults(m, nil)` → `SyncPlugin` 把不再声明的 key 标为 removed |
| 卸载 `DeletePlugin` | 不改 | 删除全部权限行（包括 removed），两张授予表级联删除（D5） |
| 重装 | 不改 | 重新首装、重新确认；旧的待复核项被 `installed_at` 过滤 |

**首次启用与安装确认的关系**：服务端只有一个确认点，就是同意（consent）。上传的插件先同意再启用，`enable` 对 awaiting_consent 返回 409。内置插件由系统在启动时同意（默认值已写入，权限处于 disabled），install-only 的插件之后由管理员启用，启用不弹确认。方案里说的“首次启用确认页”对内置插件而言，就是前端在第一次启用时打开插件的“权限”标签（矩阵数据和保存走第 2 章的按插件接口，第 5 章负责）。服务端的 `Enable` 不加确认步骤，也不新增接口。

---

## 3.6 ①访问权限：补“收窄 / 重新授予”

现有的撤销接口和撤销保持逻辑已经够用（§3.2.5）。缺少的是**收窄**和**重新授予**，用一个接口一起补上（已定采纳；第 5 章插件详情“权限”标签里 ①访问权限的“收窄 / 重新授予”按钮调用它，返回更新后的 `Grant`，与 `GET /plugins/:key/grants` 的元素同形）：

```
PUT /plugins/:key/grants/:permission   body: {"scope": {...}?}
```

```go
// SetGrant sets the active version's grant of one host permission: a scope
// within the current grant narrows it (plugin:manage is enough); anything
// else (a revoked or denied permission, a wider scope) is a grant decision,
// checked like a consent (checkGrantRights, step-up), never wider than the
// active manifest requests. Staged sets get the same narrowing, and a
// staged revocation of the permission is lifted when its version requests
// at least the scope. Audited plugin.grant.narrow / plugin.grant.regrant.
func (s *Service) SetGrant(ctx context.Context, key, permission string, scope map[string]any, actorID int64) (*Grant, error)
```

- 事务顺序和 `RevokeGrant` 一样：先 `UPDATE plugins … row_version+1` 锁住插件行，再写授予行。
- 读活动版本的 manifest（`CurrentVersion` + `LoadManifest`），取得请求范围 `reqScope`。`scope` 省略时为 `reqScope`。`!pkg.ScopeWithin(scope, reqScope)` 时返回 400 `scope_too_wide`；未请求的 permission 返回 404。
- 当前行为 granted 且 `ScopeWithin(scope, cur.Scope)` → **收窄**：只需要 `plugin:manage`（路由级）。
- 其他情况 → **重新授予**：复用 `checkGrantRights(ctx, actorID, []decision{{permission, scope, GrantGranted}}, trust, hasPub)`，按风险要求 `plugin:grant:high` / `critical`，db.schema 的隔离规则也照用。写入复用 `grants.Record(staged=false)`。
- staged 集：在 `grants` 包新增 `Narrow(ctx, tx, key, permission, scope)`，把 staged 里 granted 且比新范围宽的行改窄；重新授予时，staged 里 revoked 的同名行如果对应版本请求的范围 ⊇ 新范围，就改为 granted。否则保持 revoked，那个版本激活时仍然没有这项权限。
- 返回之后调用 `s.Notify(key)`，实例刷新缓存。`grants.Holds` 每次调用都按精确 scope 比对，收窄立即生效。
- 路由：`grant.Perm(http.MethodPut, "/plugins/:key/grants/:permission", PermManage, a.setGrant).Uses(perm.PluginGrantHigh, perm.PluginGrantCritical, perm.PluginGrantDBSchemaUnconfined)`，挂在 `StepUpPluginGrant` 下，和 consent 用同一种 step-up。收窄也会经过 step-up：一个接口一种口径，安全上只严不松。

②（给角色的权限）在插件详情里的读写，完全使用第 2 章的按插件矩阵接口，本章不新增。待复核的显示继续用 `PluginSummary.RoleGrantsToReview`（`plugin/api/plugins.go:49`）和 `role-grants/confirm|reject`。

---

## 3.7 改动清单

| 文件 | 改什么 |
|---|---|
| `plugin/install/consent.go` | `ConsentRequest.NewPermissionRoles` 改为嵌套 `manifest.RoleDefault`；删除 `RoleKeysForNewPermissions` 和 `overrideRoles`；新增 `overrideGrants`；`decide` 去掉 new_permission_roles 的检查（挪进 `overrideGrants`）；`decideAndApply` 按 §3.4.5 调整顺序；`upgradeRoleGrants` 改名为 `settleRoleGrants` 并按 §3.4.3 改写；`checkRoleGrantRights` 改为针对 override 调用 `checkGrantable`；`checkRolesGrantable` 加 `critical bool`；新增 `checkGrantable`、`grantees`、`nameable`；`checkNamedRolesExist` 改为遍历嵌套 map；`catalogKeys` 多返回 revived；`ConsentResult.Withheld`；审计 detail 调整 |
| `plugin/install/grant_set.go`（新） | `SetGrant`（§3.6）。放新文件而不是 `consent.go`：与 `RevokeGrant` 同类但和同意流程无关，也让它能和同意改造并行实现（总览 T2.4） |
| `plugin/install/defaults.go` | `roleGrants` 和 `PermissionDefs` 换成 `map[string]map[string][]string`；`PermissionDefs` 按第 2 章字段填写，InheritFrom 恒为 nil，删除 Sensitive；用 `up.DefaultGrants()` 替代 `up.Roles()`；`ApplyDefaults` 签名里 grants 的类型随之更改（`core.PluginDefaultsApplier` 同步修改） |
| `plugin/install/rolegrants.go` | `RoleGrant` 字段（§3.3.1）；`planRoleGrants` 加 `overridden`、`upgrade` 参数，填 scope/risk/via；`inheritHolders` 改为读两张表并带 scope_type；`withholdGrants` 判断改用 `Withheld`、不再返回拷贝的 manifest；`PendingRoleGrants` 的 SQL 按 §3.4.6 修改；`ReviewRoleGrants` 改用 `checkGrantable`；`grantReviewed` 带上范围；新增常量 `AuditConsent`；注释里的 sensitive 改为 risk |
| `plugin/install/review.go` | `ReviewUserPerm`、`ReviewCell`、`ReviewRoleColumn`、`ReviewRemovedPerm`；`Review` 新字段；`Review(…, actorID)`；`resolveDefaultRoles` 改为 `resolveRoleGrants`（只读事务） |
| `plugin/install/upload.go` | `Upload` 调用 `resolveRoleGrants` 时传 `actorID` |
| `plugin/install/builtin.go` | 只改注释 |
| `plugin/grants/grants.go` | 新增 `Narrow`，以及“解除 staged 撤销”（供 `SetGrant` 使用，和 `Revoke` 对称） |
| `plugin/api/api.go`、`plugins.go` | review 和 getPlugin 把 `actor(c)` 传给 `Review`；新增 `PUT /plugins/:key/grants/:permission` → `setGrant` |
| `core/ports_plugin_infra.go` | `PluginDefaultsApplier.ApplyDefaults` 的 grants 类型改为 `map[string]map[string][]string` |
| `plugin/rollout/coordinator.go` | 不改（仍传 nil） |
| `e2e/plugins.go:107-123` | `ConsentAll` 改为对 review 里 `new=true` 的 `user_permissions` 生成 `new_permission_roles: {local: {role: true}}`，删除 `role_keys_for_new_permissions` |
| `docs/CONTRACTS.md` §4、§5.7 第 227/257 行 | 改写同意请求格式、D2 挂起、`RoleGrant` 字段、review 新字段、`PUT …/grants/:permission` |

依赖其他章节的改动（本章不写代码）：第 1 章的 `RoleDefault`、`DefaultGrants`、`GrantSuffix`、`RiskLevel`、`PermissionGroups`；第 2 章的 `PermissionDef` 新字段、`SyncPlugin` 的 removed 与复活、`SetPluginActive` 排除 removed、`CanGrantToRoles(…, critical)`、`PluginGrantees`、`GrantPluginPermission(map)`、`permissions.risk` 列。

---

## 3.8 测试计划

优先在已有测试文件里扩展；新的不变式集中放进一个新文件 `plugin/install/consent_matrix_test.go`。新写或大改的测试用 testify `require`/`assert`。

**单元测试（无数据库，表驱动）**
- `defaults_test.go` `TestRoleGrantsPriority`（改写）：覆盖的角色替换默认值，`named` 正确；`{}` 时 `named[key]` 存在且为空、默认值仍留在 roles 里（由 settle 决定删掉还是挂起）；改名为 `{}`；existing/revived 不出现在授予里；未覆盖的 key 等于 `DefaultGrants()`（`true` → `[""]`，`["own"]` → `["own"]`）。
- `defaults_test.go` `TestOverrideGrants`（新增）：`true` / `["*","own"]` → `[""]`；`!Own` 时写 `"own"` 返回 `invalid`；枚举 key `"input"` 和后缀 `"value/input"` 都 → `"value/input"`；枚举外的 `"value/x"` 返回 `invalid`；`"flow/3"` → `"flow/3"`；资源不符的 `"proxy/3"` 返回 `invalid`；无 scope 的权限写范围返回 `invalid`；未声明的 key 返回 `unknown`（字段 `new_permission_roles.<local>`）。
- `defaults_test.go` `TestPermissionDefsSort`（扩展断言）：Risk、Group 及其 label/sort、Requires 为完整 key、ScopeType 为 `plugin:flow` 或 `plugin:value`、Defaults、`InheritFrom == nil`。
- `consent_matrix_test.go` `TestNameableRule`：没有 role:define → `role_define`；critical 且非超管 → `critical`；Denied 中的原因原样返回；超管遇到 critical 也可命名。
- `consent_matrix_test.go` `TestPlanNeedsConsentByRisk`：升级时 low+admin → false；low+user → true；sensitive+admin → true；继承 critical → true；首装时全部为 false。

**集成测试（install 包现有的 env：真实 PG + fakePerms；fakePerms 增加 `PluginGrantees` 桩和 `critical` 记录）**
- `consent_roles_test.go` `TestConsentRoleGrantRights`（改写为新格式）：点名角色时缺 role:define 返回 403；点名被拒的角色返回 403（details.role）；点名 critical 的非超管返回 403，`details.reason=="critical"` 且 `details.permissions==["exempt"]`；升级没有新 key 时带点名也不检查。
- `consent_matrix_test.go` `TestInstallWithholdsUnnameableDefaults`（D2）：管理员首装，defaults 为 `{admin:true, operator:["own"]}`，fake 拒绝 admin（held）→ 同步的 GrantRoles 只有 `operator:["own"]`；`ConsentResult.Withheld` 是 admin 那一条且 scope 为 ""；`PendingRoleGrants` 列出它；审计 `plugin.consent` 的 detail.pending 非空；超管 `ReviewRoleGrants(confirm)` 后 fake 记录 `GrantPluginPermission(key, {admin:[""]})`。
- `consent_matrix_test.go` `TestOverrideKeepsUnnameableDefaultsPending`：覆盖 `{operator:["*"]}`，defaults 含 admin（不可命名）和 supplier（可命名）→ admin 挂起，supplier 被删掉，operator 授予 `[""]`。
- `consent_matrix_test.go` `TestInstallWithoutRoleDefineWithholdsAllDefaults`：没有 role:define 的安装者首装 → 全部挂起，`CanGrantToRoles` 没有被调用。
- `rolegrants_test.go` `TestUpgradeRoleGrantsNeedConsent`（改写）：用 risk 替代 sensitive；“操作者同意但不可命名”从期望 403 改为期望成功并挂起；auto 沿用仍只在全部为 low+自动角色时发生。
- `rolegrants_test.go` `TestBuiltinUpgradeWithholdsRoleGrants`（扩展）：挂起项带 scope 和 risk；首装（系统）不挂起任何项。
- `rolegrants_test.go` `TestPendingRoleGrantsScoped`（新增）：角色已有无范围行时隐藏该项；已有同 scope 的行时隐藏；权限被 removed 时隐藏；不同 scope 的两条分别列出。
- `consent_roles_test.go` `TestConsentRenameInheritsOnly`（扩展）：继承带上 scoped 行（scope_type 相同）；scope_type 不同时只继承无范围行。
- `consent_matrix_test.go` `TestRevivedKeyGetsNoDefaults`（D6，install 侧）：目录里预置 `status='removed'` 的 key 并让某角色持有 → 新版本重新声明 → `catalogKeys` 的 existing 和 revived 都为真；SyncPlugin 的 def 里 GrantRoles 为 nil；review 的 `revived=true`，`cells[role].grant` 等于持有的范围。
- `review_test.go` `TestReviewMatrixCells`（新增）：role_columns 的原因；critical 行非超管全部 `nameable=false, reason=critical`；`withheld` 与 nameable 一致；升级时 `removed_user_permissions` 列出持有者。
- `revoked_grants_test.go` `TestSetGrantNarrowsAndRegrants`（新增）：收窄不需要 grant 权限；超出请求范围返回 400；重新授予 high 风险时缺 `plugin:grant:high` 返回 403；重新授予后 `grants.Revoked` 不再包含它；staged 的 revoked 行在版本请求范围足够时解除；收窄同时作用于 staged。
- `consent_roles_test.go` `TestConsentInheritRules`（新增，接手第 2 章删掉的 `TestSyncPluginInheritsOnlyEarlierGrants` 和 `TestSyncPluginGrantRolesAndInheritFrom` 的继承部分）：只继承同意前已有的授予，同一次同意新建的 key 不当作来源；scope_type 不同时只继承无范围行，并记 `scope_type` 跳过；新 critical key 遇到非 critical 旧 key 不继承，审计 `inherit_skipped` 记一条 `risk`；critical → critical 正常继承，但操作者不是超管时挂起。
- 真实 authz 的 removed/复活/SetPluginActive 测试由第 2 章在 `authz/plugin_catalog_test.go` 负责，本章不重复。

**API / e2e**
- `plugin/api/api_test.go`：`PUT /plugins/:key/grants/:permission` 需要 step-up，并声明 `Uses(plugin:grant:*)`（沿用现有路由表测试的写法）。
- `e2e/ac05_upgrade_test.go` `TestAC05_UpgradeAnthropicWithoutDowntime`：配合新的 `ConsentAll` 跑通。
- `e2e/ac06_disable_test.go` `TestAC06_DisableEnableUninstall`：在 enable/disable 后断言插件权限的 status 为 active/disabled，removed 行保持 removed（覆盖第 2 章的 `SetPluginActive` 改动）；卸载后插件权限和角色授予清零；重装后 review 的 `new=true`，`role_grants_to_review` 为空。

---

## 3.9 风险与已定事项

1. **（已定，采纳）操作者升级同意从 403 改为挂起**（§3.4.3 第 2 点）。D2 只剩一条规则；资格不足的人同意时，部分格子等超管复核。生产上目前只有超管安装插件，实际效果不变。
2. **（已定，采纳）继承授予也按可命名规则处理**：改名时，旧持有者在操作者不可命名的角色上挂起，而不是直接继承。和现有代码（继承同样计入 `consentRoles` 检查）同一口径；拆分出更宽的新 key 时“持有者本来就有”的理由不成立，所以不对继承豁免。
3. **（已定，采纳）auto 沿用按上传者判可命名性**：上传者不是超管时，低风险自动授予给他自己持有的角色会挂起。这是 D2 的直接推论。
4. Review 改成按查看者计算：插件详情的 `manifest` 字段也随查看者变化（cells/role_columns），没有缓存问题，但同一个 URL 不同人看到的内容不同，要在 CONTRACTS 里写明。
5. **（已定）`PUT /plugins/:key/grants/:permission` 采纳，首版一条路由**：收窄和重新授予都挂在 `StepUpPluginGrant` 下，收窄也要 step-up，比撤销严格；一个接口一种口径，安全上只严不松。第 5 章 `GrantsTab` 的“收窄 / 重新授予”按钮接这个接口（控制台 HTTP 客户端遇到 step-up 会自动弹出验证框，`web/src/components/stepUp/stepUp.ts:2`）。
6. 不做部分确认（§3.4.6）。如果复核项多到需要逐项决定，后续再给 `role-grants/confirm` 加 `grants[]` 过滤；届时 confirm 的清除也要改成按 (permission, role, scope) 包含匹配。
7. 存量审计里的待复核项没有 `scope`，按 `""`（全部）处理，等同于现有语义（当时没有范围）。reject 的跨版本清除仍按 (permission, role) 匹配，不受影响。
8. 复活的 key 如果同时改了 scope_type，第 2 章 `pruneScopes` 会删除不再适配的范围行；review 的 `cells.grant` 显示的是同意之前的持有，同意后可能变少。首版不在 review 上特别标注（插件改资源名本来就少见，审计里有 `scope_changed` 记录）。
9. 已有 key 的风险升高（如 `moderation:exempt` 改 critical）：确认页首版不列出，已有授权不动（与第 2 章 §2.6 第 1 条同一结论）。

## 3.10 二审修订（0.1.289 候选，发布前）

- **同意模式**：`consentMode`（`modeOperator`/`modeAuto`/`modeSystem`，`consentModeOf`）取代 `auto bool` 与散落的 `isSystem`；同意与预览共用 `markPlan` 判断挂起（code-backend #2）。`checkGrantable` 一次调用 `CanGrantToRoles(roles, 最高风险)`，本地 key 用 `manifest.LocalPermission`（#4、#9）。`planRoleGrants` 不再用 `builtin` 假值区分覆盖（#8）；覆盖值规范化复用 `RoleDefault.Suffixes`（#6）；`cells.grant` 经 `CanonicalSuffixes`（#7）。
- **继承跟随覆盖（C2）**：`new_permission_roles[local]` 出现时，能指定角色上的继承授予以覆盖为准（缺省或 `[]` 不授予）。
- **审查可见性（C4）**：查看者没有 `role:read`（系统同意除外，`seesHolders`）时，`removed_user_permissions[].holders`、revived/renamed 行的 `cells.<角色>.grant` 省略（`omitzero`）、`withheld` 为 false，`new_role_grants` 去掉 `via: inherit`；插件列表、详情的待复核授予经 `RoleGrantsToReview` 同样去掉 `via: inherit`（N4）。
- **暂存同意**：`PermissionDefs` 带 `Staged`，新 key 建成 staged、removed key 不复活，激活时 `ApplyDefaults(m, nil)` 再按被激活版本同步（logic #4）。每次同意先 `DiscardStaged`，`catalogPerms.has` 不把 staged key 算作已有，被取代的暂存版本遗留的 key 按新 key 结算（N3）。激活时 `carryRenamedHolders` 只对本次激活才停用的旧 key（激活前的目录里不是 removed），把同意之后新增的持有者记成 `plugin.role_grants.carry` 待复核（logic #5、N1）；同意计划读 `plugin.consent` 的 `role_grants`，各种同意都写（N2）；`PendingRoleGrants` 一并读取，并只列本插件的权限（security #10）；`grantReviewed` 拒绝非本插件的 key。
- 目录读取全部经第 2 章的只读端口（`loadCatalogPerms`、`PermissionHolders`，调用方先取 `catalog()` 再传下去）；仍直接查 `roles` 表的只有角色列（`noteRoleColumns`）、点名角色存在性与待复核项的角色存在性检查。
