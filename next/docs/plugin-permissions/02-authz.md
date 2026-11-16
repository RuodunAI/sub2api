# 第 2 章 权限目录与授权（authz）

> 基于 `/home/claude/sup2api-dev` develop @ dae6ba57。下文 `path:line` 以 `next/server/internal/` 为根（另注明的除外），都是在代码里核对过的；标“推测”的是没有核实的判断。
>
> 存储为历史：本目录是 0.1.289 时的设计。0.1.290 起角色的授予（含范围授予）与用户的角色只存在 `casbin_rule`（迁移 0100，CONTRACTS §99），下文的 `role_permissions`、`role_permission_scopes`、`user_roles` 及引用它们的语句与行号都是当时的写法，那三张表已删除。版本号也是当时的推测：这部分随 0.1.290 与 Casbin、会话一起发布，0.1.289 没有单独发布，`PluginPermissionsFirstCore`、UI 3.16 的核心下限和内置插件的 `hostCompat` 都是 0.1.290；`0.1.289-plugin-perm-down` 已删除，停在 0099 的开发库重建或恢复到 0098 的备份；线上的升级见 CHANGELOG 0.1.290（发布手册已删除，CONTRACTS §101）。

## 2.1 本章范围

负责：

- `permissions` 表的迁移：加列、`sensitive` 改为 `risk`、`scope_type` 加宽，以及回滚脚本。
- `core.PermissionDef`、`core.PermissionCatalog` 的改动（`core/ports_identity.go`）。
- `authz` 包：
  - `SyncPlugin`：写入新列；不再声明的 key 标为 `removed`，重新声明时复活；范围变化时清理不再适用的范围授权；去掉继承步骤，继承改由第 3 章在同意时解析。
  - `SetPluginActive` 不碰 `removed` 行。
  - `CanGrantToRoles` 加上 critical 规则；新增只读的 `PluginGrantees`；`GrantPluginPermission` 支持范围。
  - 角色写入：插件范围 key 的校验；已移除权限可以取消勾选。
  - 权限目录 API 的新字段。
  - 按插件读写“权限 × 角色”矩阵的接口（`GET`/`PUT /permissions/plugins/:plugin`）。“恢复插件建议”由前端用目录里的 `defaults` 完成，后端 reset 接口首版不做（已定）。
- 与第 1 章 `perm` 之间的接口约定，以及其他章节要调用的 authz 函数签名。

不负责：manifest 结构与校验（第 1 章）、`perm` 包实现（第 1 章，本章只写依赖的签名）、安装/升级确认、待复核与 `install.PermissionDefs` 的映射（第 3 章）、运行时判范围与 `PermissionSet.PluginScope/HasSome`（第 4 章）、前端（第 5 章）。

## 2.2 现状

### 表结构

- `permissions` 表见 `migrations/0001_core.sql:164-178`：`key, module, label, description, source, plugin_key (ON DELETE CASCADE), sensitive bool, status varchar(10), sort`。status 的注释写的是 “active | disabled | removed (core permission dropped from code)”。
- `scope_type varchar(20)` 和 `role_permission_scopes(role_id, permission_id, scope varchar(200))` 由 `migrations/0097_authz_hardening.sql:109-118` 加入。
- 迁移目录是 `migrations/*.sql`，由 `migrations/embed.go:6` 嵌入，文件名格式 `NNNN_name.sql`，编号不能重复（`migrations/migrations_test.go:39`）。当前最新编号是 `0098_plugin_staged_grants.sql`。
- 迁移只有 up：每个文件在一个事务里执行（`store/migrate.go:80-110`）；从 `0026` 起必须能重复执行（`migrations/embed.go:15`）。回滚用 `next/deploy/rollback/<版本>-*-down.sql` 加一份 `.md` 说明，参考 `0.1.286-authz-down.sql`（0.1.290 后已删除，`git show 91e0c26bc:next/deploy/rollback/0.1.286-authz-down.sql`）。
- 数据库只有 PostgreSQL（pgx）。

### 核心目录

`corePerm` 在 `authz/catalog.go:27-51` 定义，有 `requires`、`sensitive`（只用于展示）、`superOnly`（不进任何内置角色默认值，`accepts`，`catalog.go:340`）、`stepUp` 几个字段。

- `CorePermissions()`（`catalog.go:364`）只把 `Sensitive`、`ScopeType` 写进 `PermissionDef`。
- `requires` 不入库，`ListPermissions` 用 `corePermByKey` 现场去代码里查（`roles.go:766-768`），插件权限因此拿不到 requires。

### 插件目录

`SyncPlugin`（`authz/plugin_catalog.go:26-102`）：

- upsert 不写 `status`，所以已有行的状态不变；`ON CONFLICT ... WHERE permissions.plugin_key = EXCLUDED.plugin_key` 是归属检查，`RETURNING (xmax = 0)` 区分新建。
- 只给新建的 key 授 `GrantRoles`，并通过 `grantInherited`（`sync.go:308`）继承旧 key 的持有者。`grantInherited` 只复制 `role_permissions`，不复制范围行。
- 最后执行 `DELETE FROM permissions WHERE plugin_key=$1 AND key <> ALL($2)`（`plugin_catalog.go:98`），授权随之级联删除。

`SetPluginActive`（`plugin_catalog.go:201-215`）执行 `UPDATE permissions SET status=$2 WHERE plugin_key=$1`，**会把 `removed` 改回 active/disabled**。只要插件权限开始使用 `removed` 状态，这就是 bug。

`CanGrantToRoles`（`plugin_catalog.go:115-145`）对每个角色先查 `editableRole`，再查 `pluginGrantee`（`plugin_catalog.go:161`：必须在可分配清单内，非超管不能是自己持有的角色）。它不区分风险等级。

`grantPlugin`（`plugin_catalog.go:188`）只写 `role_permissions`。

### 已移除状态（核心已在使用）

- `SyncCore` 把代码里删掉的核心权限标为 removed（`sync.go:168`）。
- `resolvePermissions`（`grants.go:29`）丢弃 removed 的 key。
- `writeGrants`（`grants.go:89`）删除时带 `p.status <> 'removed'`，所以已移除权限的授权永远保留，等 key 回来时复活。
- `TestSetRolePermissionsKeepsInactive`（`authz/roles_test.go:382`）锁定了“保存时列出的 removed key 被忽略，授权行保留”这一行为：现在**角色编辑页无法取消勾选已移除权限**。

### 缓存与视图

| 视图 | 位置 | 规则 |
|---|---|---|
| 生效集 | `loadPermissionSet`，`service.go:320` | `roleGrantsJoin("p.status = 'active'")`，disabled 和 removed 都不生效 |
| 授予视图 | `RolesPermissions`，`service.go:370` | `p.status <> 'removed'`，disabled 计入，removed 不计入 |
| 目录快照 | `ensureCatalog`，`service.go:433` | 只缓存 key → status；`ActivePermissionKeys` 只返回 active |

- `roleSelect`（`roles.go:52-63`）**不按状态过滤**，所以 `Role.permission_keys` 含 removed 的 key。
- `ListPermissions`（`roles.go:744`）列出所有状态的权限。

### 访问密钥

- `grantablePermissions`（`iam/access_key.go:68-104`）要求 key 的动作是 active，并且被用户的生效集蕴含。
- 选择器 `AccessKeyPermissions`（`iam/access_key.go:254-275`）只列 active 的模块和权限，复用 `authz.PermissionModule/PermissionItem`。

### 角色写入

- `SetRolePermissions`（`roles.go:371`）：`editableRole`（`roles.go:129`），然后 `resolvePermissions`，然后 `added := notImplied(live, r.PermissionKeys)`，然后 `covers(added)`，最后 `writeGrants(coreOnly=false)`。
- `ResetRole`/`resetDefaults`（`roles.go:328`）以 `coreOnly=true` 调用 `writeGrants`，不碰插件权限。

### 范围解析

- `perm.Parse` 只认写死在 `perm/pattern.go:32` 的 `scopable` 表（8 个核心动作）。插件 key 末尾带 `:own`、`:x/3` 会返回 `ErrInvalidKey`，于是 `resolvePermissions` 报 400，`PermissionSet.Add` 静默丢弃。
- `canonicalKeys`、`impliedByOther`、`notImplied`（`grants.go:135-170`）都只经过 `perm.Parse/Implies` 和 `PermissionSet.Has`，本身不认识资源。

### 安装侧（第 3 章）

- `install.PermissionDefs`（`plugin/install/defaults.go:58`）把 manifest 映射成 `PermissionDef`。
- `catalogKeys`（`plugin/install/consent.go:567`）按 key 是否存在于 `permissions` 判断“已有”，不看状态，所以 removed 的行也算已有，`planRoleGrants` 不会再给它套默认值。D6 需要的正是这个行为，不用改。

## 2.3 设计

### 2.3.1 与第 1 章 perm 的接口（已与 sdk-perm、runtime-scope 对齐）

本章依赖以下签名，由第 1 章实现：

```go
// perm/pattern.go
const PluginPrefix = "plugin."

// Scope 增加两个可比较字段；插件范围 "<res>/<value>" 设置 Res、Value，
// own 设置 Own，"*" 设置 All。只有动作以 PluginPrefix 开头的 key 才按此语法解析。
type Scope struct {
    All, Own   bool
    Group, ID  int64
    Res, Value string
}

func Parse(k string) (Pattern, error)                // 插件 key：末段为 * / own / 含一个 "/" 时视为范围
func (p Pattern) Suffix() string                     // 插件范围返回 Res+"/"+Value
func Implies(have, want Pattern) bool                // h.Group==0 分支改为 h.ID==w.ID && h.Res==w.Res && h.Value==w.Value
func ValidScopeValue(v string) bool                  // ^[A-Za-z0-9_.-]{1,64}$
```

资源名直接读 `p.Scope.Res`（第 1 章不导出资源正则，也不提供 `PluginScopeResource`）。

依赖的 manifest 函数（第 1 章）：

- `up.RiskLevel()`
- `up.DefaultGrants() map[string][]string`：角色 → 规范化后缀，`""` 表示全部；critical 返回 nil。
- `PermissionScope.ResourceName()`：枚举形式返回 `"value"`，即 `manifest.ScopeValueResource`。
- 常量 `manifest.RiskLow/RiskSensitive/RiskCritical`。core 已经 import `sdk/manifest`（`core/ports_identity.go`），可以直接用。

分工：perm 只校验语法；资源是否对得上、是否允许 own、值是否在枚举内，由 authz 对照 `permissions.scope_type/scope` 校验（2.3.5）。这样 perm 仍然是叶子包，不需要运行时注册表；和原型的说法（“scopable 改为核心表 + 目录 scope_type”）相比，入侵更小，效果相同。

有了这些，`canonicalKeys`、`impliedByOther`、`notImplied`、`PermissionSet.Has/Missing`、`within/covers`、`roleGrantsJoin` 拼出的 `p.key || ':' || rs.scope` 都**不用改**。插件资源范围自然遵循“授出的 ⊆ 自己的授予集”：持有 `flow:manage` 的人能授 `flow:manage:flow/3`，反过来不行。

D1：保存时 `plugin.m:flow:*` 被解析成动作 `plugin.m:flow` 加 All，这个 key 不存在，于是报 400 `unknown`。数据库里永远不会出现通配，展开由前端做。

### 2.3.2 迁移 `0099_plugin_permission_meta.sql`

```sql
-- Plugin permission metadata (docs: plugin-perm-design ch.2): risk levels
-- replace the sensitive flag, plugin permission groups, soft dependencies,
-- scopes and the manifest's suggested grants, kept per permission. A plugin
-- permission no longer declared becomes status 'removed' (its grants stay and
-- grant nothing) instead of being deleted. Reversed by
-- deploy/rollback/0.1.289-plugin-perm-down.sql.
--
-- Safe to run again: every change is IF NOT EXISTS or converges, and the
-- risk backfill only touches rows still at the default.

SET LOCAL lock_timeout = '10s';

-- low | sensitive | critical. Core: superOnly -> critical, sensitive ->
-- sensitive (the core sync writes it); plugins: manifest risk.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS risk varchar(10) NOT NULL DEFAULT 'low';
-- Soft dependencies (qualified keys), editor hints only, core and plugin alike.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS requires text[] NOT NULL DEFAULT '{}';
-- Plugin permission group: key, label and order, copied onto each permission
-- of the group (the manifest's permissionGroups[]); '' for core.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_key varchar(50) NOT NULL DEFAULT '';
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_label jsonb NOT NULL DEFAULT '{}';
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_sort integer NOT NULL DEFAULT 0;
-- A plugin permission's scope as the manifest declares it (manifest
-- PermissionScope, resource set to its ResourceName(), so "value" for an
-- enum): {"resource", "own", "options", "values": [{"key", "label"}]}.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS scope jsonb NOT NULL DEFAULT '{}';
-- The manifest's suggested grants: {"<role>": ["", "own", "flow/3"]},
-- canonical suffixes, "" for every row. Shown by GET /permissions (the
-- console's "restore plugin suggestion"); never granted by itself.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS defaults jsonb NOT NULL DEFAULT '{}';
-- "plugin:" + a resource of up to 30 characters.
ALTER TABLE permissions ALTER COLUMN scope_type TYPE varchar(40);

DO $$ BEGIN
    ALTER TABLE permissions ADD CONSTRAINT permissions_risk_check CHECK (risk IN ('low', 'sensitive', 'critical'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Existing rows keep what sensitive said until their next sync.
UPDATE permissions SET risk = 'sensitive' WHERE sensitive AND risk = 'low';

COMMENT ON COLUMN permissions.status IS
    'active | disabled (owning plugin disabled) | removed (dropped from code or no longer declared by its plugin; grants kept, granting nothing)';
```

几点说明：

- **`sensitive` 列先保留一版**。新代码只写不读，写入值是 SQL 表达式 `risk <> 'low'`，Go 里没有对应字段。这样回滚到上一版核心时，旧代码的 `INSERT ... sensitive` 和 `SyncCore` 的比较照常工作。下一个迁移再 `DROP COLUMN sensitive`，此后 `risk` 是唯一来源。
- 分组标签和排序**跟着权限行冗余存储**，不新建 `permission_groups` 表。理由：分组只服务于展示；插件卸载时权限行 `ON DELETE CASCADE`，分组跟着走；升级时 `SyncPlugin` 逐行 upsert 就能顺带更新，不需要额外的同步和删除逻辑。代价是每行多一份几十字节的 label。
- 不新建 `scope_type` 以外的范围列：资源名放在 `scope_type`（`plugin:<res>`，授权判定用），清单的 scope 声明整体放进 `scope jsonb`（展示与 `fits` 用）。
- `ALTER COLUMN ... TYPE varchar(40)` 在 PostgreSQL 里加宽 varchar 只改元数据，重复执行也安全。
- 不需要推进 authz 版本：新列都只用于展示，下次 `SyncCore` 或 `SyncPlugin` 写入时会 Bump。

**回滚**：新增 `next/deploy/rollback/0.1.289-plugin-perm-down.sql` 及 `.md`（版本号随第 1 章 `PluginPermissionsFirstCore`，推测值，发布时定）。和 `0.1.286-authz-down`（已删除）一样，只用于不经网关的环境：网关不允许跨 schema 回退（`deploy/gateway/ovh/README.md:31`），线上出问题按“修复前进或从备份恢复”处理。

1. `DELETE FROM permissions WHERE source='plugin' AND status='removed'`，并写审计 `via "rollback 0.1.289"`。旧核心的 `SetPluginActive` 会把 removed 复活，所以必须先删，这也和旧语义“删除”一致。
2. `DELETE FROM role_permission_scopes rs USING permissions p WHERE p.id = rs.permission_id AND p.source='plugin'`：旧 perm 解析不了这些行，它们本来就不起作用。
3. 删除新增的 7 列和约束，`scope_type` 改回 varchar(20)（插件行先置 `''`）。
4. 删除 `schema_migrations` 里的 0099 行，并推进 authz 版本。

### 2.3.3 核心目录的风险映射

`corePerm` 字段不动，`sensitive`、`superOnly` 继续表达核心语义（低侵入）。只加一个方法：

```go
// risk is p's level in the catalog: superOnly ones are critical (only a
// holder grants them, first the super admin), sensitive ones sensitive.
func (p corePerm) risk() string {
    switch {
    case p.superOnly:
        return manifest.RiskCritical
    case p.sensitive:
        return manifest.RiskSensitive
    }
    return manifest.RiskLow
}
```

- `CorePermissions()`（`catalog.go:364`）改为写 `Risk: p.risk(), Requires: p.requiredKeys()`，去掉 `Sensitive`。
- `SyncCore`（`sync.go:28-50, 118-125`）的 unnest 数组把 `sensitive []bool` 换成 `risks []string, requires []string`（requires 用 `text[]` 的字面量 JSON 编码，或改成按 `jsonb` 传；两种做法二选一）。
- 写入时 `sensitive = (risk <> 'low')`。`IS DISTINCT FROM` 比较里加上 `risk`、`requires`。
- `SelfPermissionModule`（`self.go:43`）也改用 `Risk: p.risk()`。

这样“核心 superOnly = critical = 插件 critical”成了同一个概念：

- 角色编辑时，只有持有者能授（`covers`），初始只有超管持有。
- 插件安装时，只有超管能授（2.3.6）。
- 都不进默认值：核心靠 `builtinRole.accepts`，插件靠 manifest check 和 `DefaultGrants()` 返回 nil，`SyncPlugin` 再兜底（2.3.4）。

CONTRACTS §4 里“`GET /permissions` 不返回这个标记”的说法要同步改成“以 `risk: critical` 返回”。

### 2.3.4 `core.PermissionDef` 与 `PermissionCatalog`

```go
// core/ports_identity.go

// PermissionDef describes one permission in the catalog.
type PermissionDef struct {
    Key    string
    Module string
    // ScopeType: core "account"/"proxy" (perm.ScopeType); plugin
    // "plugin:"+manifest scope ResourceName(); "" for none.
    ScopeType   string
    // Scope is a plugin permission's manifest scope (Resource set to
    // ResourceName()); nil for core and unscoped. core already imports
    // sdk/manifest, so the manifest type is reused as is.
    Scope       *manifest.PermissionScope
    Label       LocalizedText
    Description LocalizedText
    Risk        string // manifest.RiskLow | RiskSensitive | RiskCritical
    Sort        int
    // Group of a plugin permission (manifest permissionGroups[]).
    Group      string
    GroupLabel LocalizedText
    GroupSort  int
    // Requires are soft dependencies, qualified keys; never authorization.
    Requires []string
    // Defaults is the manifest's suggestion, role -> canonical scope
    // suffixes ("" every row): stored for GET /permissions (the console's
    // "restore plugin suggestion"), never granted by itself. Nil for critical.
    Defaults map[string][]string
    // GrantRoles are what the catalog grants when it first creates the
    // permission, role -> suffixes as Defaults (super admin roles skipped):
    // the consent's decision, inherited holders already resolved into it
    // (install planRoleGrants), not the manifest's.
    GrantRoles map[string][]string
    // InheritFrom is gone: the consent resolves inheritance (ch.3).
}

// PluginGrantees is which roles an actor may hand new plugin permissions
// (PermissionCatalog.PluginGrantees): Denied maps a role to why not.
type PluginGrantees struct {
    Superuser bool              `json:"superuser"` // may grant critical ones
    Denied    map[string]string `json:"denied"`    // superuser_role | not_editable | not_assignable | held
}

type PermissionCatalog interface {
    // SyncPlugin upserts the plugin's permissions, revives removed ones it
    // declares again (grants as they were, no GrantRoles), grants newly
    // created ones their GrantRoles (scopes included), drops scope grants
    // the declared scope no longer allows, and marks keys no longer
    // declared removed. It never inherits: the consent resolved that (ch.3).
    SyncPlugin(ctx context.Context, tx pgx.Tx, pluginKey string, defs []PermissionDef) error
    // CanGrantToRoles: as before; risk critical also needs a superuser actor.
    CanGrantToRoles(ctx context.Context, tx pgx.Tx, actorID int64, roleKeys []string, risk string) error
    // PluginGrantees is CanGrantToRoles read-only: no Bump, no locks, a
    // refusal is a Denied entry rather than an error.
    PluginGrantees(ctx context.Context, tx pgx.Tx, actorID int64, roleKeys []string) (PluginGrantees, error)
    // GrantPluginPermission grants key to roles (role -> suffixes); removed
    // keys and super admin roles are skipped. Callers check the rights.
    GrantPluginPermission(ctx context.Context, tx pgx.Tx, key string, roles map[string][]string) error
    SetPluginActive(ctx context.Context, tx pgx.Tx, pluginKey string, active bool) error // never touches removed rows
    DeletePlugin(ctx context.Context, tx pgx.Tx, pluginKey string) error                  // unchanged (D5)
}
```

- `Sensitive bool`、`InheritFrom []string` 删除。
- `GrantRoles` 从 `[]string` 改成 `map[string][]string`。
- 不新增 `core.PermissionScope` / `PermissionScopeValue`：`Scope` 直接用第 1 章的 `manifest.PermissionScope`（审查时合并，少两个只做搬运的类型）。
- `PluginGrantees` 用 `pgx.Tx` 而不用 `store.Querier`，是为了和接口里其他方法一致；core 也不 import store。
- 第 3 章的 `install.PermissionDefs` 负责映射，字段对应关系：

| PermissionDef 字段 | 来源 |
|---|---|
| `Risk` | `up.RiskLevel()` |
| `Group` / `GroupLabel` / `GroupSort` | `permissionGroups` 里的 key、label 和下标×10 |
| `Requires` | `PermissionKey(m.Key, r)` |
| `ScopeType` | `"plugin:"+up.Scope.ResourceName()` |
| `Scope` | `up.Scope` 的拷贝，`Resource` 置为 `up.Scope.ResourceName()` |
| `Defaults` | `up.DefaultGrants()` |
| `GrantRoles` | 同意页的决定（第 3 章） |

### 2.3.5 插件范围的校验：一个判定，三处复用

`authz/grants.go` 新增：

```go
// pluginScope is a plugin permission's scope as the catalog stores it
// (scope_type "plugin:<resource>", scope): the scopes a grant may name.
type pluginScope struct {
    resource string   // "" when the permission takes no scope
    own      bool
    values   []string // enum keys; nil for an options or id resource
}

// readPluginScope reads scope_type and scope; ok is false for a core key.
func readPluginScope(scopeType string, scope *manifest.PermissionScope) (pluginScope, bool)

// fits reports whether p's scope is one the permission takes: every row
// always; own when allowed; <resource>/<value> of its resource, and of its
// values when it lists them. A core-style scope never fits.
func (ps pluginScope) fits(p perm.Pattern) bool {
    switch s := p.Scope; {
    case s.All:
        return true
    case s.Own:
        return ps.own
    case s.Res == "" || s.Res != ps.resource:
        return false
    }
    return ps.values == nil || slices.Contains(ps.values, p.Scope.Value)
}
```

三处复用：

1. `resolvePermissions`（`grants.go:29`）：查询改为 `SELECT key, id, status, scope_type, scope FROM permissions WHERE key = ANY($1)`。插件 key（`strings.HasPrefix(action, perm.PluginPrefix)`）如果 `!fits`，加入 `invalid`，报 400 `invalid`，消息里带 key。核心 key 照旧由 `perm.Parse` 校验。
2. `SyncPlugin`：校验 `GrantRoles` 和 `Defaults` 的每个后缀；`Defaults` 不合法时报 400，挡住 manifest check 漏过的情况。范围变化后用同一个判定挑出要删除的范围行（下节）。
3. `GrantPluginPermission`：按库里存的 scope 校验 `roles` 的后缀。

`readPluginScope` 和 `fits` 有多个调用方，表达的是稳定的业务概念（“插件权限接受哪些范围”），符合“可复用行为才单独成函数”的规范。

**限制（推测，交给第 4 章）**：authz 只校验值的语法和资源名，不校验 options 资源下的 id 是否真的存在。插件数据被删后，`flow/3` 这样的授权会残留，但不会造成越权，因为插件里已经没有 flow 3。可以留作以后由插件通知核心清理的接口。

### 2.3.6 SyncPlugin 改写（D6）

```go
func (s *Service) SyncPlugin(ctx context.Context, tx pgx.Tx, pluginKey string, defs []core.PermissionDef) error
```

签名不变，流程如下（低嵌套，每步一个已有或新增的命名函数）：

1. 校验 `pluginKey`，然后 `Bump`（同现状）。
2. 逐个 def 处理：
   1. 先跑校验：risk 必须在三级之内；`GrantRoles` 和 `Defaults` 的每个后缀都要满足 `fits`；critical 的 `Defaults` 必须为空（兜底）。
   2. upsert：

      ```sql
      INSERT INTO permissions (key, module, label, description, source, plugin_key, risk, sensitive, status, sort,
          scope_type, scope, group_key, group_label, group_sort, requires, defaults)
      VALUES ($1, $2, $3, $4, 'plugin', $5::text, $6::varchar, $6::varchar <> 'low',
          CASE WHEN EXISTS (SELECT 1 FROM plugins WHERE key = $5::text AND status IN (`+runningPluginStatuses+`))
               THEN 'active' ELSE 'disabled' END,
          $7, $8, $9, $10, $11, $12, $13, $14)
      ON CONFLICT (key) DO UPDATE SET module = EXCLUDED.module, label = EXCLUDED.label,
          description = EXCLUDED.description, risk = EXCLUDED.risk, sensitive = EXCLUDED.sensitive,
          sort = EXCLUDED.sort, scope_type = EXCLUDED.scope_type, scope = EXCLUDED.scope,
          group_key = EXCLUDED.group_key, group_label = EXCLUDED.group_label, group_sort = EXCLUDED.group_sort,
          requires = EXCLUDED.requires, defaults = EXCLUDED.defaults,
          -- a removed key declared again revives with the plugin's state; others keep theirs
          status = CASE WHEN permissions.status = 'removed' THEN EXCLUDED.status ELSE permissions.status END
          WHERE permissions.plugin_key = EXCLUDED.plugin_key
      RETURNING (xmax = 0)
      ```

      复活时直接复用 `EXCLUDED.status`，也就是插入时算出的运行状态，不用再写一遍 CASE。没有返回行仍然表示 key 属于别的插件，报 `ErrConflict`（同现状）。
   3. `inserted` 的 key 进入 `created`。
   4. 没有新建的 key 一律执行 `pruneScopes(ctx, tx, key, def)`：读出该权限的范围行，用 `fits`（按 def 的新声明；实现为 `manifest.PermissionScope.Takes`，目录、同意的 `scopeSuffix` 和继承共用这一套判定）挑出不再适用的，`DELETE ... WHERE permission_id=$1 AND scope = ANY($2) RETURNING role_id, scope`，每行写审计 `authz.sync.revoke {via:"scope_changed", key}`。不带范围的行（全部）始终有效，保留。范围没变时它什么也不删，所以不需要先读旧行比对（审查时去掉了原来的 `pluginRows`；插件权限每个插件只有几项，多一次按 `permission_id` 的索引查询可以忽略）。必须删而不能只靠“蕴含不上”：`own` 和枚举值即使被清单收回，运行时仍会命中（第 4 章 `PluginScope.Own/Allows`）。暂存升级（另一版本仍在运行）的同意不会触发这里的裁剪：第 3 章 §3.4.5 让新旧都声明的 key 沿用运行版本的定义，裁剪在激活时的 `ApplyDefaults(m, nil)` 里按被激活版本的声明进行。
3. 对 `created`：`grantPlugin(ctx, tx, "default", key, d.GrantRoles)`。**继承这一步删除**：第 3 章已经在同意时按“当前持有者”把继承解析成显式的 `GrantRoles`（含范围），`PermissionDef.InheritFrom` 字段删除，所以 `SyncPlugin` 的继承分支和 `grantInherited`（`sync.go:308`）不再有调用方，一并删掉。`isNew` 和 `from` 的计算（`plugin_catalog.go:79-96`）也随之删除。
4. 把删除改成标记：

   ```sql
   UPDATE permissions SET status = 'removed'
   WHERE plugin_key = $1 AND status <> 'removed' AND key <> ALL($2)
   ```

   授权行不动。复活靠第 2 步的 upsert。因为复活的行不进 `created`，它不会被授 `GrantRoles`，也不会做继承，原授权原样生效，满足“不重套默认”。第 3 章的 `catalogKeys` 把 removed 的行算作已有，前后一致。

   标记和复活都改变了持有者的授予是否生效，却没有角色写入，所以各写一条系统审计（目标是插件）：`UPDATE … RETURNING key` 标记的 key 写 `authz.sync.remove`，upsert 时原来是 removed 的 key（upsert 前用 CTE 读出旧状态）写 `authz.sync.revive`，detail 都是 `{keys, holders: {key: [角色]}}`。没有 key 时不写。
5. `committedLater(v)`（同现状）。

`grantPlugin` 支持范围：

```go
// grantPlugin grants the plugin permission key to roles (role -> suffixes,
// "" every row) but super admin ones, audited with via; a removed key
// grants nothing.
func (s *Service) grantPlugin(ctx context.Context, tx pgx.Tx, via, key string, roles map[string][]string) error
```

- Go 侧先把每个角色的 `key[:suffix]` 过一遍 `canonicalKeys`：有 `""` 就丢掉同角色的其他后缀，保证不会同时写入全部和 own 两行。
- 然后拆成两组 `unnest($roles::text[], $scopes::text[])`：一组插入 `role_permissions`（scope=""），一组插入 `role_permission_scopes`。两组都 `JOIN permissions p ON p.key=$3 AND p.source='plugin' AND p.status <> 'removed'`、`JOIN roles r ... AND NOT r.superuser`。
- 结果交给现有的 `syncGrant` 写审计，返回的 key 是完整 key（含范围）。

原先打算放进 `grantInherited` 的两条继承规则，已和第 3 章约定改由 `install.planRoleGrants` 实现：

1. 只有 `scope_type` 相同才带上范围行。
2. critical 的新 key 只从同为 critical 的旧 key 继承（与第 1 章约定：manifest check 看不到已经不再声明的旧 key 的风险），旧 key 的风险读 `permissions.risk`；不满足就跳过，并写进同意审计。

`SetPluginActive`：

```sql
UPDATE permissions SET status = $2 WHERE plugin_key = $1 AND status <> 'removed'
```

`DeletePlugin` 不变：卸载时删除全部行，包括 removed，授权级联删除（D5）。

### 2.3.7 critical 与授权对象检查

现有的 `editableRole`、`pluginGrantee` 先抽出两个**纯判定**函数，供“报错版”和“只读版”共用：

```go
// editDenial is why actor (its grantSet) may not change role r, whose
// grants are cur: "" when it may (editableRole's rule; superuser roles are
// frozen).
func editDenial(actor, cur core.PermissionSet, r *Role) string // "" | superuser_role | not_editable | not_assignable

// granteeDenial is why actor may not hand new plugin permissions to role
// key (pluginGrantee's rule, T59).
func granteeDenial(actor core.PermissionSet, held []string, key string) string // "" | not_assignable | held
```

- `editableRole`（`roles.go:129`）改成：锁行，取 `grantSet` 和 `cur`，调用 `editDenial`，再把原因映射回原来的错误（`errSuperuserRoleFrozen`、`cannotActOnRole`、`notAssignable`）。行为和错误文案都不变，actor 0 照旧放行。
- `pluginGrantee` 改成 `granteeDenial` 的报错包装。

**`CanGrantToRoles(ctx, tx, actorID, roleKeys, risk)`**（二审改为传风险，调用方传本次授予里最高的风险）：在现有流程的最前面加一条：

```go
if critical && actorID != 0 && !actor.Superuser {
    return core.ErrPermissionDenied.WithMessage(t(ctx,
        "only the super admin may grant critical plugin permissions",
        "只有超级管理员能授予插件的关键权限")).WithDetails(map[string]any{"reason": "critical", "superuser": true})
}
```

- `actor` 来自 `pluginGrantor`，访问密钥会经 `RestrictToAccessKey` 去掉超管标记，所以通过访问密钥不能授 critical。
- 第 3 章每次只调用一次，传本次授予的最高风险（二审 code-backend #4，此前按 critical 分两批调用）。

**`PluginGrantees`**：

- 不 Bump、不加锁，用 `queryRoles` 取角色。
- 逐个角色调用 `editDenial` 和 `granteeDenial`，把非空原因填进 `Denied`；`Superuser` 填 `actor.Superuser`。
- 第 3 章的同意页用它给格子置灰；critical 行在 `!Superuser` 时整行置灰。

**角色编辑页（`SetRolePermissions` 等）对 critical 不加额外规则**。`covers` 要求新增的 key 被操作者的授予集蕴含，而 critical 插件权限不进任何默认值，初始只有超管持有（超管的 `Has` 恒为真）。所以“只有持有者能授”自然成立，和核心 superOnly 的委派规则（CONTRACTS §4、D2）完全一样：超管把它授给某个角色后，该角色的持有者如果有 `role:define`，也能继续授出，全程有审计。这正是 D3 写的“角色编辑时只有持有者能授”。如果以后要收紧成“角色编辑时也只有超管能授 critical”，在 2.3.8 的 `setRolePart` 里对 `added` 加一条 `risk=critical && !actor.Superuser` 即可，不需要改别处。本方案不加。

### 2.3.8 角色写入：抽出 `setRolePart`，三个入口共用

```go
// grantPart is the part of a role's grants a write replaces: all of them,
// the core ones (a reset), or one plugin's.
type grantPart struct {
    coreOnly bool
    plugin   string // plugin key; "" for none
}

// setRolePart makes keys the grants of r (locked by editableRole) within
// part: actorID must cover what keys add beyond what r grants; a removed
// permission r holds stays when listed and goes when not, one it does not
// hold is ignored (it can only be unticked). Audited as role.permissions
// with detail.
func (s *Service) setRolePart(ctx context.Context, tx pgx.Tx, actorID int64, r *Role, keys []string, part grantPart, detail map[string]any) error
```

流程：

1. `resolvePermissions`：`grant` 增加 `removed bool`，removed 的 key 也返回，但不进 `live`。
2. `grants` 里丢掉 `removed && !held` 的项，`held` 用 `r.PermissionKeys`。
3. `added := notImplied(live, r.PermissionKeys)`，然后 `covers`。
4. `writeGrants(ctx, tx, r.ID, grants, part)`。
5. 更新 `updated_at`，写审计。

`writeGrants` 的签名从 `coreOnly bool` 改成 `part grantPart`。删除条件变成：

```sql
... AND (NOT $3 OR p.source = 'core') AND ($4 = '' OR p.plugin_key = $4)
    AND (p.status <> 'removed' OR NOT $3)   -- a reset (coreOnly) keeps removed grants
```

插入时只插 `!removed` 的 id，所以一个没持有的 removed key 永远不会被写进来。

已移除权限的处理变化如下：

- **角色编辑页可以取消勾选已移除的权限**（原型 C 和第 5 节表格的要求）。
- 列出的 removed 授权照样保留，P2 的“按原样回存”依然成立，因为 `roleSelect` 返回的 `permission_keys` 本来就含 removed 的 key。
- 内置角色的“重置为默认”（`coreOnly`）保持现状，保留已移除核心权限的授权。

三个入口：

| 入口 | 改法 |
|---|---|
| `SetRolePermissions`（`roles.go:371`） | `editableRole` 之后调用 `setRolePart(part{})`，函数体缩短 |
| `SetPluginRoleGrants`（新） | 见 2.3.10 |

`CreateRole` 和 `resetDefaults` 也改用 `grantPart{}` / `grantPart{coreOnly: true}` 调用 `writeGrants`。它们的 `grants` 先过滤掉 removed：新建角色和默认值都不会授出 removed。

### 2.3.9 权限目录 API（`GET /permissions`）

`PermissionItem`（`roles.go:715`）：

```go
type PermissionItem struct {
    Key         string             `json:"key"`
    Label       core.LocalizedText `json:"label"`
    Description core.LocalizedText `json:"description"`
    Risk        string             `json:"risk"`              // low | sensitive | critical (replaces sensitive)
    StepUp      bool               `json:"step_up"`
    Requires    []string           `json:"requires,omitempty"` // core and plugin, from the column
    ScopeType   string             `json:"scope_type"`         // "account" | "proxy" | "plugin:<res>" | ""
    Scope       *manifest.PermissionScope `json:"scope,omitempty"` // plugin scoped keys only; resource = ResourceName() ("value" for an enum)
    Group       string             `json:"group,omitempty"`
    // Defaults: the plugin's suggestion as full canonical keys per built-in
    // role, comparable with Role.permission_keys as is.
    Defaults map[string][]string `json:"defaults,omitempty"`
    Status   string              `json:"status"`               // active | disabled | removed
}

type PermissionModule struct {
    // ... existing fields ...
    Groups []PermissionGroup `json:"groups,omitempty"` // plugin modules, by group_sort
}

type PermissionGroup struct {
    Key   string             `json:"key"`
    Label core.LocalizedText `json:"label"`
    Sort  int                `json:"sort"`
}
```

`ListPermissions` 的改动：

- SELECT 增加 `p.risk, p.requires, p.group_key, p.group_label, p.group_sort, p.scope, p.defaults`。
- `ORDER BY p.source, p.module, p.group_sort, p.sort, p.key`。
- 删掉 `corePermByKey` 里的 `Requires` 赋值，改从列里读；`StepUp` 仍然从代码取。
- 遇到新的 `group_key` 时追加到 `m.Groups`。
- `defaults` 的后缀展开成完整 key：`""` 展开为 `key`，其他展开为 `key+":"+suffix`。展开只在这里做一次，前端不用再懂后缀语法。

示例：

```json
{
  "module": "plugin.moderation",
  "label": {"en": "Moderation", "zh": "提示词审核"},
  "source": "plugin",
  "plugin_key": "moderation",
  "status": "active",
  "groups": [
    {"key": "flow", "label": {"en": "Flows", "zh": "审核流程"}, "sort": 10},
    {"key": "action", "label": {"en": "Actions", "zh": "处置"}, "sort": 30}
  ],
  "permissions": [
    {"key": "plugin.moderation:flow:manage", "label": {"zh": "编辑审核流程"}, "description": {},
     "risk": "sensitive", "step_up": false,
     "requires": ["plugin.moderation:flow:read"],
     "scope_type": "plugin:flow",
     "scope": {"resource": "flow", "own": true, "options": "/scopes/flow"},
     "group": "flow",
     "defaults": {"admin": ["plugin.moderation:flow:manage"],
                  "operator": ["plugin.moderation:flow:manage:own"]},
     "status": "active"},
    {"key": "plugin.moderation:exempt", "label": {"zh": "免于审核观察与封禁"}, "description": {},
     "risk": "critical", "step_up": false, "scope_type": "", "group": "action", "status": "active"},
    {"key": "plugin.moderation:record:raw_view", "label": {"zh": "查看未脱敏原文"}, "description": {},
     "risk": "sensitive", "step_up": false, "scope_type": "", "group": "record", "status": "removed"}
  ]
}
```

核心项示例：

```json
{"key": "balance:adjust:any", "risk": "critical", "requires": ["balance:adjust"], "scope_type": "", "step_up": false, "status": "active"}
```

- `GET /roles` 和 `GET /roles/:id` 结构不变。插件范围 key 以 `plugin.m:flow:manage:flow/3` 的形式出现在 `permission_keys` 里，removed 的 key 也在其中，前端按目录里的 status 标“已移除”。
- `tsgen.go`：`PermMeta.sensitive` 改成 `risk: 'low' | 'sensitive' | 'critical'`，重新生成 `next/web/src/generated/permissions.ts`，golden 测试是 `TestGeneratedPermissionsTS`。
- 访问密钥选择器复用 `PermissionItem`，自动带上 `risk`，`iam` 不用改代码（它只按 status 过滤）。

### 2.3.10 按插件的矩阵读写

路由，加在 `authz/http.go` 的 `RegisterRoutes` 里：

```go
r.Perm(http.MethodGet, "/permissions/plugins/:plugin", "role:read", s.handlePluginRoleGrants)
r.Perm(http.MethodPut, "/permissions/plugins/:plugin", "role:define", s.handleSetPluginRoleGrants)
```

服务函数：

```go
// PluginRoleMatrix is GET/PUT /permissions/plugins/:plugin: the plugin's
// catalog module and every role's grants of it.
type PluginRoleMatrix struct {
    PluginKey string            `json:"plugin_key"`
    Module    *PermissionModule `json:"module"`
    Roles     []PluginRoleRow   `json:"roles"`
}

type PluginRoleRow struct {
    ID        int64              `json:"id"`
    Key       string             `json:"key"`
    Name      core.LocalizedText `json:"name"`
    Builtin   bool               `json:"builtin"`
    Superuser bool               `json:"superuser"`
    Editable  bool               `json:"editable"`
    Denied    string             `json:"denied,omitempty"` // editDenial
    // PermissionKeys: the role's grants of this plugin, removed ones included.
    PermissionKeys []string `json:"permission_keys"`
}

func (s *Service) PluginRoleGrants(ctx context.Context, actorID int64, pluginKey string) (*PluginRoleMatrix, error)
func (s *Service) SetPluginRoleGrants(ctx context.Context, actorID int64, pluginKey string, roles map[string][]string) (*PluginRoleMatrix, error)
```

**读（GET）**：

- 插件不存在时返回 404。
- `module` 取 `ListPermissions` 结果中 `PluginKey == pluginKey` 的那一项。直接复用，不另写 SQL。
- `roles` 来自 `ListRoles`，`permission_keys` 只保留以 `manifest.PermissionModule(pluginKey)+":"` 开头的 key。
- `editable` / `denied` 来自 `editDenial`：`grantSet` 只算一次，每个角色调一次 `RolesPermissions`。角色数量很少，这个开销可以接受。
- 超管角色 `superuser: true, editable: false`，前端整列固定全勾。

**写（PUT）**：

```json
{"roles": {
  "operator": ["plugin.moderation:flow:read", "plugin.moderation:flow:manage:flow/2", "plugin.moderation:flow:manage:flow/5"],
  "cs": []
}}
```

- 只替换列出的角色在这个插件上的授权；没列出的角色和插件以外的授权都不动。
- 在一个 `mutate` 事务里，按角色 key 排序逐个处理：
  1. `resolveRoles`，未知角色报 400 `roles.<role>`。
  2. `editableRole`，锁行并检查。
  3. 校验每个 key 都属于这个插件，否则报 400，`Field: "roles.<role>", Code: "foreign"`。
  4. 调用 `setRolePart(part{plugin: pluginKey}, detail{"plugin": pluginKey, "via": "plugin_matrix"})`。
- 任何一个角色失败，整个事务回滚。403 的错误在原 details 上补一个 `"role": "<key>"`（`*core.Error.WithDetails` 合并），前端据此定位到列。
- 成功后返回最新的 GET 结构。

**恢复插件建议：不设后端接口（已定）**。角色编辑页和插件详情页都在前端用 `GET /permissions` 里的 `defaults`（完整 key）改好勾选，再走已有的保存路径（`PUT /roles/:id/permissions` 或矩阵 PUT），授出部分照样过 `editableRole` + `covers`。按“没有调用方就不加函数和路由”，不新增 `ResetRolePluginGrants`、`POST /roles/:id/plugins/:plugin/reset` 及其测试；以后出现 API 级调用方（脚本、CLI）时，用 `setRolePart(part{plugin})` 加一条 `SELECT key, defaults->$role` 即可实现。

复用了 `ListPermissions`、`ListRoles`、`resolveRoles`、`editableRole`、`setRolePart`（也就是 `resolvePermissions`、`notImplied`、`covers`、`writeGrants`、`audit`）和 `mutate`。新增的只有两个薄编排函数（`PluginRoleGrants`、`SetPluginRoleGrants`），没有新的授权规则。

### 2.3.11 插件 removed 之后各处的表现

| 位置 | 表现 | 依据 |
|---|---|---|
| 生效集、网关、插件路由 | 不生效 | `loadPermissionSet` 只取 active |
| 授予视图（防提权比较、`editableRole`、分配角色） | 不计入 | `RolesPermissions` 过滤 removed |
| `GET /roles` | 仍在 `permission_keys` 里 | `roleSelect` 不过滤 |
| `GET /permissions` | 列出，`status: "removed"` | `ListPermissions` |
| 角色编辑页、插件矩阵 | 显示“已移除”，只能取消勾选，勾不上新的 | `setRolePart` |
| 重新声明 | 状态随插件；授权原样恢复，不套默认、不继承 | 2.3.6 |
| 访问密钥里的这个 key | 保留字符串，不生效（和用户生效集取交集）；不能新选；复活后重新生效 | `grantablePermissions` 只允许 active，选择器只列 active |
| 插件停用（disabled） | 授权保留不生效；授予视图仍计入，持有者仍可授出（二审 logic #3 确认保留） | 现状 |
| 卸载 | 行和授权一起删除 | `DeletePlugin`，D5 |

## 2.4 改动清单

| 文件 | 改什么 |
|---|---|
| `migrations/0099_plugin_permission_meta.sql`（新） | 2.3.2 的 SQL |
| `next/deploy/rollback/0.1.289-plugin-perm-down.sql` / `.md`（新，版本号推测） | 回滚：删除 removed 插件行和插件范围行、删列、删 schema_migrations 行、推进版本 |
| `core/ports_identity.go` | `PermissionDef` 换字段（`Scope *manifest.PermissionScope`，删 `Sensitive`、`InheritFrom`）；新增 `PluginGrantees`；`PermissionCatalog` 的 `CanGrantToRoles` 加 `critical`，新增 `PluginGrantees`，`GrantPluginPermission` 改用 map |
| `authz/catalog.go` | 新增 `corePerm.risk()`；`CorePermissions` 写 Risk 和 Requires |
| `authz/sync.go` | `SyncCore` 的数组换成 risk、requires 并比较它们；删除 `grantInherited` |
| `authz/plugin_catalog.go` | 改写 `SyncPlugin`；新增 `pruneScopes`；`grantPlugin` 支持范围；`SetPluginActive` 跳过 removed；`CanGrantToRoles` 加 critical；新增 `PluginGrantees`；`granteeDenial` 和 `pluginGrantee` 包装；`GrantPluginPermission` 支持范围 |
| `authz/grants.go` | 新增 `pluginScope`、`readPluginScope`、`fits`；`resolvePermissions` 读 scope 列并校验插件范围，返回 removed 标记；`writeGrants(part grantPart)` |
| `authz/roles.go` | 新增 `editDenial`，`editableRole` 用它；新增 `grantPart`、`setRolePart`；`SetRolePermissions`、`CreateRole`、`resetDefaults` 改调用方式；`PermissionItem`、`PermissionModule` 加字段；`ListPermissions` 读新列；新增 `PluginRoleGrants`、`SetPluginRoleGrants` |
| `authz/http.go` | 两条新路由（`GET`/`PUT /permissions/plugins/:plugin`）和 handler |
| `authz/self.go` | `Sensitive` 改为 `Risk` |
| `authz/tsgen.go` 与 `next/web/src/generated/permissions.ts` | `PermMeta.risk` |
| `next/docs/CONTRACTS.md` | §4：risk 与 superOnly → critical、插件 removed、新目录字段；§5.2：两条新接口 |
| `plugin/install/*`（第 3 章） | 适配新签名：`PermissionDefs` 的映射、`CanGrantToRoles(..., critical)`、`GrantPluginPermission(map)`、`PluginGrantees` |

不改：`perm` 包（第 1 章）、`service.go`（缓存和视图已经按状态过滤，正确）、`iam/access_key.go`、`DeletePlugin`、`roleGrantsJoin`、`canonicalKeys`、`impliedByOther`、`notImplied`。

## 2.5 测试计划

都扩展现有测试文件，不新建测试文件；使用 `testutil.DB`。

`authz/plugin_catalog_test.go`：

- `TestSyncPluginRemovesAndRevives`
  - v1 声明 a、b，b 授给 admin 和 operator。
  - v2 只声明 a：断言 b 的 `status='removed'`、`role_permissions` 行还在、admin 的 `PermissionSet` 没有 b、`RolesPermissions` 没有 b。
  - 在 removed 状态下撤销 operator 的 b。
  - v3 再声明 b，`GrantRoles{"user":[""]}`：断言 b 恢复为 active、只有 admin 持有（operator 没被加回、user 没被授予）、没有新的 `authz.sync.grant` 审计。
  - 插件停用时复活：断言状态为 disabled。
- `TestSetPluginActiveKeepsRemoved`：先 removed，再 `SetPluginActive(true)`，断言仍是 removed。
- `TestSyncPluginScopedGrants`
  - `GrantRoles{"admin":[""], "operator":["own","flow/3"]}`：断言 operator 写进两条 `role_permission_scopes`。
  - 断言 `{"operator":["", "own"]}` 只写一条 `role_permissions`。
  - 断言后缀 `proxy/3`、不允许的 `own`、枚举外的 `value/x` 都报 400。
  - 断言审计里的 key 带范围。
- `TestSyncPluginPrunesScopesOnScopeChange`
  - scope 从允许 own 改为不允许：断言 own 行被删、写了 `authz.sync.revoke via scope_changed`、全部范围行保留。
  - 资源从 flow 改成 ticket：断言 `flow/*` 行全部删除。
- `TestSyncPluginGrantRolesAndInheritFrom` 改名为 `TestSyncPluginGrantRoles`，只保留 GrantRoles 部分。`TestSyncPluginInheritsOnlyEarlierGrants` 删除，它对应的契约（继承只看同意前已有的持有者，包括范围和 critical 规则）移到第 3 章 `planRoleGrants` 的测试。
- `TestSyncPluginSplitsGrowthAndModeration`（`plugin_catalog_test.go:250`）：改成通过显式 `GrantRoles` 构造场景，断言拆分后旧 key 是 removed、授权保留。
- `TestCanGrantToRoles`（扩展）：断言 admin 以 `critical=true` 调用时返回 403，`details.reason="critical"`；超管和 actor 0 通过；通过访问密钥的超管返回 403。
- `TestPluginGrantees`：一张表覆盖 superuser_role、not_editable、not_assignable、held、可命名五种情况；断言不 Bump（version 不变）、不报错。
- `TestGrantPluginPermission`（扩展）：带范围的 map；removed 的 key 不授予。

`authz/roles_test.go`：

- `TestSetRolePermissionsKeepsInactive`（改断言）
  - 列出的 removed 授权保留。
  - 不列出时删除，这是新行为，替换原来的“the removed one stays”。
  - 列出一个没持有的 removed key，断言不新增行。
  - `ResetRole` 仍保留 removed 的核心授权。
- `TestPluginRoleGrantsMatrix`
  - admin 视角的 GET：断言 admin 列 `editable=false, denied="not_assignable"`、operator 列可编辑、超管列 `superuser=true`。
  - PUT 两个角色、第二个缺权限：断言返回 403 带 `details.role` 和 `missing`，第一个角色没被写入（回滚）。
  - 断言别的插件的 key 返回 400 `foreign`。
  - 断言没列出的角色和核心授权不变。
  - 断言审计里 `via=plugin_matrix`。

`authz/scoped_grants_test.go`：

- `TestPluginScopedKeysResolveAndCover`
  - `plugin.m:flow:manage:flow/3` 能保存。
  - 以下各自返回 400 `invalid`：权限不允许 own 时写 `:own`；资源写错 `:ticket/3`；枚举外的值；scope_type 为空的权限带了范围。
  - 断言 `plugin.m:flow:*` 返回 400 `unknown`（D1）。
  - `canonicalKeys` 在有不带范围的 key 时去掉 `flow/3`。
  - 持有 `flow:manage` 的人能授 `flow/3`；只持有 `flow/3` 的人授不带范围的 key 返回 403 `missing`。

`authz/catalog_test.go`：

- `TestCatalogRisk`：断言所有 superOnly 都是 critical、sensitive 映射为 sensitive、其余 low。
- `TestListPermissionsPluginMeta`：同步一个带分组、范围、defaults、requires 的插件后，断言 `groups` 的顺序、`scope.resource` 不带前缀、`defaults` 展开成完整 key、核心项的 `requires` 来自列。

其他：

- `authz/tsgen_test.go`：更新 golden。
- `migrations/migrations_db_test.go`：`TestPluginPermissionMetaMigration`，断言 `sensitive=true` 的行回填为 `risk='sensitive'`、scope_type 能写 40 个字符、约束拒绝 `risk='high'`。重复执行由现有的 `TestMigrationsFromFirstIdempotentRunTwice` 覆盖。
- 回滚 DB 测试，仿照 `TestAuthzHardeningRollback`（已删除）：先迁移 0099，造一个 removed 插件行和一条插件范围行，跑回滚脚本，断言行被删、列被删、0098 及以前的结构完整。
- e2e `next/e2e/ac02_rbac_test.go` 加一个用例：通过 API 安装测试插件，然后 PUT 矩阵，再 GET `/me`，断言权限可见；升级到去掉某个 key 的版本后断言该 key 不生效；再升级回来，断言授权原样恢复。

## 2.6 风险与未决问题

1. **已有 key 的风险升级**（比如 low → critical，本轮的 `moderation:exempt`）：已定只更新 `risk` 列，已有持有者保留授权，确认页首版不单独列出“风险升高的已有权限”，也不自动撤销（与“不覆盖已撤销”对称，同样不覆盖已授予）。角色编辑页按新风险显示徽标。
2. **取消勾选 removed 是行为变化**：通过 API 回存角色、又不带 removed key 的脚本会删掉这些授权。这是原型要求的语义，CONTRACTS §4 要写明，并在 CHANGELOG 里提示。
3. **`sensitive` 列多保留一版**：新旧两份数据短期并存，只写不读。下一个迁移必须记得删掉，建议在 0099 的注释和 CHANGELOG 里都写上。
4. **options 资源的 id 不校验是否存在**（2.3.5）：插件数据删除后会残留授权。不会越权，但角色页可能显示一个找不到名字的 id。前端用 `EntityPicker` 显示“已删除 #3”，属于第 5 章。
5. **矩阵 PUT 的锁**：一次请求锁住多个角色。按角色 key 排序加锁可以避免死锁；所有 authz 写操作本来就串行在 `Bump` 的行锁上，所以实际上没有并发问题。
6. **`PluginGrantees` 用 `pgx.Tx`**：第 3 章在 review 里开一个只读事务调用它（§3.4.7），与接口其他方法一致，已定。
7. **迁移与回滚的版本号**：迁移编号 0099（当前最新 0098）；回滚脚本按 `PluginPermissionsFirstCore` 命名为 `0.1.289-plugin-perm-down`（推测值，发布时改成真实版本）。迁移需要用户点头才能发布。

## 2.7 与其他章节的接口约定

- **第 1 章（perm、manifest）**：
  - `perm.Scope{Res, Value}`、`perm.PluginPrefix`、`perm.ValidScopeValue`（perm 实现归第 1 章）。
  - `manifest`：`up.RiskLevel()`、`up.DefaultGrants()`、`PermissionScope.ResourceName()`、`ScopeValueResource="value"`；check 保留资源名 account、proxy、group、value。
  - `scope_type = "plugin:"+ResourceName()`，存储的范围是 `"<res>/<value>"` 或 `"own"`。
- **第 3 章（install）**：
  - `core.PermissionDef{Risk, Group, GroupLabel, GroupSort, Requires, ScopeType, Scope *manifest.PermissionScope, Defaults, GrantRoles map[string][]string}`。`InheritFrom` 删除，继承由 `planRoleGrants` 解析，包括范围同 scope_type 才继承、critical 只从 critical 继承这两条。
  - `CanGrantToRoles(ctx, tx, actorID, roleKeys, risk string)`。
  - `PluginGrantees(ctx, tx, actorID, roleKeys) (core.PluginGrantees, error)`，原因码 `superuser_role|not_editable|not_assignable|held`。
  - `GrantPluginPermission(ctx, tx, key, map[string][]string)`。
  - `SetPluginActive` 不碰 removed（本章实现）。
  - `catalogKeys` 把 removed 算作已有（不改）。
  - `PendingRoleGrants` 需要带范围（第 3 章）。
- **第 4 章（运行时）**：removed 和 disabled 已经被生效集过滤；范围存储格式同上；`PermissionSet.PluginScope` 和 `HasSome` 由第 4 章在 core 里实现。
- **第 5 章（前端）**：
  - `GET /permissions` 的新字段：`risk`、`group`、`groups`、`requires`、`scope_type`、`scope{resource, own?, options?, values?}`（`manifest.PermissionScope`，resource 总是填，枚举为 `value`）、`defaults`（完整 key）。`sensitive` 字段去掉。
  - `GET`/`PUT /api/v1/permissions/plugins/:plugin`；403 的 `details.role`。“恢复插件建议”由前端用 `defaults` 预填后保存，后端不提供 reset 接口。
  - `generated/permissions.ts` 里的 `PermMeta.risk`。

## 二审修订（0.1.289 候选，发布前）

- **并发写入（C1）**：`Role.Version` 与 `PluginRoleMatrix.Version` 返回 `authz_meta.version`；矩阵 GET 在可重复读快照里读 `version` 与角色（`readSnapshot`）。`SetRolePermissions`、`SetPluginRoleGrants` 多一个 `version *int64`，经 `mutateAt` 在 Bump 之后比较 `v-1`，不一致 409 `version_conflict`（`details.version`），nil 不检查。
- **关键权限二次验证（C3）**：`resolvePermissions` 标出 critical 的插件 key，`setRolePart`、`CreateRole` 对**新增**的 critical key 在写入事务里 `ConsumeStepUp(StepUpPluginGrant)`；路由不变，被拒时事务回滚、版本不变。
- **审计**：`role.permissions` 记 `added`、`removed`；矩阵写入另记该插件上的 `before`/`after`。
- **只读端口**：`PermissionCatalog` 加 `PluginPermissions`（一个插件的 key、状态、风险、范围）与 `PermissionHolders`（key → 角色与范围，超管角色除外），install 不再直接读授权表（code-backend #1）。范围的存取统一在 `core.StoredPluginScope`/`PluginScopeType`/`ReadPluginScope`/`PluginScopeSuffix`（#5）。
- **暂存同意**：`PermissionDef.Staged` 时 `SyncPlugin` 新建的 key 为 `staged`（`SetPluginActive` 不碰），不复活 removed key、不裁剪其范围授予；激活的同步把声明的 staged key 随插件状态启用，不声明的不动、也不标 removed（logic #4、N3）。staged key 不进 `listPermissions`、`roleSelect`、`readRoleGrants`，`resolvePermissions` 视其为未知，`grantsInPart` 不删它的授予。新增端口 `DiscardStaged`：删除插件的 staged key 及授予，审计 `authz.sync.discard`，每次同意先调用。
- `RolesPermissions` 与 `eachRolePermissions` 共用 `readRoleGrants`（#3）；`editDenial` 同时返回缺的权限，`editableRole` 不再重算（#10）。`setRolePart` 的审计按写入的授予算出 after，不再重读角色（N5）。
- **停用插件**（logic #3 取舍）：生效集不计、授予视图计入的现状保留，持有者停用期间仍能授出；见 CONTRACTS §4。
