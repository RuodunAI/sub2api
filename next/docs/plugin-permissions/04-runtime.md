# 第 4 章 运行时鉴权：路由范围与插件查询接口

> 基于 `sup2api-dev` develop @ dae6ba57（只读）。下文路径都相对 `next/`。核实过的代码引用写作 `path:line`；没有核实的写明“推测”。
>
> 存储为历史：本目录是 0.1.289 时的设计。0.1.290 起角色的授予（含范围授予）与用户的角色只存在 `casbin_rule`（迁移 0100，CONTRACTS §99），下文的 `role_permissions`、`role_permission_scopes`、`user_roles` 及引用它们的语句与行号都是当时的写法，那三张表已删除。

## 4.1 本章范围

负责：
1. 插件控制台路由转发（`ANY /api/v1/p/:key/*path`）在转发前做的权限判断：对带范围的插件权限按“任意范围”放行；路由声明了 `scopeFrom` 时，从请求里取出资源值，判断调用者是否覆盖；把调用者在该动作上的有效范围随请求交给插件（只有 own 时由插件按 `created_by` 判断）。
2. 宿主接口：修正 `AuthzCheck` 对带范围本地 key 的归属判断；`pluginsdk` 读取请求里 `action_scope` 的便捷封装。（`AuthzCheck` 返回范围的扩展审查后推迟，§4.3.4。）
3. 插件 `scope.options` 路由的核心代理：给角色编辑页和插件“权限”标签提供候选值。
4. UI 可见性：带范围的插件权限下，`FilterUI`（菜单、页面、插槽、账号动作）与控制台 `permissions.has/hasAction/scope`、iframe 桥的判定规则。
5. 确认网关请求路径不受影响。

不负责：manifest 字段定义与 `check` 校验、`perm.Parse` 对插件范围的解析（第 1 章，本章只用 `Route.ScopeFrom`、`UserPermission.Scope`、`Manifest.UserPermissionScope`、`perm.Scope{Res,Value}`、`perm.ValidScopeValue`）；authz 目录、表结构与授出检查（第 2 章）；安装与升级流程（第 3 章）；ScopePicker、EntityPicker、`implies.ts` 等前端组件的改造（第 5 章，本章只定规则和接口）。

### 与其他章节约定的接口（已和各章对齐）

| 来源 | 名称 | 本章怎么用 |
|---|---|---|
| 第 1 章 | `Route.ScopeFrom string`（json `scopeFrom`）：单个 `path:<p>` / `query:<n>` / `body:<f>`，用 `manifest.ParseAccountsFrom` 解析 | 转发前取值 |
| 第 1 章 | `PermissionScope{Resource, Options string; Own bool; Values []ScopeValue{Key; Label}}`，`(PermissionScope).ResourceName()`（枚举形式返回 `manifest.ScopeValueResource = "value"`） | 判断路由权限是否带范围、代理 options |
| 第 1 章 | `(*Manifest).UserPermissionScope(local string) *PermissionScope`、`(*Manifest).FindUserPermission(local) (UserPermission, bool)` | 同上 |
| 第 1 章 check | `scopeFrom` 只能用在 admin/user 路由上，且路由的 permission 必须声明 scope；`options` 必须对应一条已声明的 `GET <options>`、scope 为 admin 的路由，路径里不能有参数 | 本章据此假设，不再重复校验 |
| 第 1 章 | `perm.Scope` 新增 `Res, Value string`；`perm.Parse` 按语法解析 `plugin.` 开头的 key，范围是 `*` / `own` / `<res>/<value>`；`perm.ValidScopeValue(v) bool`（`^[A-Za-z0-9_.-]{1,64}$`） | 校验取到的值，读取 `PermissionSet.Scoped` |
| 第 2 章 | 存储：`role_permission_scopes.scope` 存 `<res>/<value>` 或 `own`；`permissions.scope_type = "plugin:<res>"`；status 为 removed/disabled 的 key 已在 `loadPermissionSet` 里过滤掉 | 运行时不再检查状态 |
| 本章提供 | `core.PluginScope`、`PermissionSet.PluginScope`、`PermissionSet.HasSome`、`core.AuthorizePluginScope`（新文件 `core/plugin_scope.go`，不改 `ports_identity.go`） | 本章自用：routes、menus、`plugin/api/ui.go`、scope-options |
| 本章提供 | `GET /api/v1/plugins/:key/scope-options?permission=` | 第 5 章 ScopePicker 的数据源 |

## 4.2 现状

### 4.2.1 路由转发

- 入口 `server/internal/plugin/routes/routes.go:81`，`r.Group().Any("/p/:key/*path", h.serveAPI)`。
- admin/user 路由先认证（`routes.go:178`），再调用 `httpapi.CanAny(c, h.authz, manifest.PermissionKey(key, chosen.Route.Permission))`（`routes.go:182`）。`CanAny` 逐个调用 `authz.Can`（`httpapi/router.go:355`），`Can` 就是 `PermissionSet(...).Has(k)`（`authz/service.go:252`）。
- `Has` 的语义（`core/ports_identity.go:74`）：key 不带范围就表示要全部行，所以只持有 `x:own` 的人 `Has("x")` 为 false。持有任意范围要用 `HasAction`（`ports_identity.go:98`）。**所以插件权限带范围以后，现有的 `CanAny` 会把只持有部分范围的人全部挡在 403 外**，必须修改。
- 读完 body 以后（`routes.go:193`），路由声明了 `Accounts` 时调用 `h.authorizeAccounts`（`routes.go:203`）。实现在 `routes/accounts.go`：
  - `accountIDs(from, params, query, body)`（`accounts.go:40`）对每一项调用 `manifest.ParseAccountsFrom`（`sdk/manifest/route_accounts.go:49`），再由 `sourceIDs`（`:61`）按来源取值：path 参数、query 的所有值、JSON body 顶层字段（名字不区分大小写，`fieldIDs` `:74`）。值可以是数字、数字数组或逗号分隔的字符串（`rawIDs` `:90`、`splitIDs` `:105`）。最后去重，最多 `manifest.MaxAccountBatch` 个。
  - `core.AuthorizeAccounts`（`core/ports_identity.go:531`）：范围是全部就通过；否则一个 id 都没给时 403 `AccountsDenied`，有 id 不在范围内时 404。
- `Route.Accounts` 由注册表在 `registry.go:470` 用 `manifest.EffectiveRouteAccounts`（`route_accounts.go:62`）补全，账号动作的路由不必显式声明。
- 发给插件的 `HTTPRequest` 在 `routes.go:207-221` 组装，只有 `Caller{UserId, RequestId, ClientIp, Locale}`、方法、路径、`RoutePath`、`PathParams`、query、headers（剥掉 `strippedRequestHeaders`，`routes.go:98`）和 body，没有任何权限信息。客户端的请求头会原样转给插件，所以权限信息**不能**放在请求头里（客户端可以伪造）。
- 网关侧 `httpAdapter.HandleHTTP`（`grpcruntime/adapters.go:325`）在插件处理请求期间登记 caller 和它的访问密钥（`callers.enter`），供 `AuthzCheck`/`AccountScopeCheck` 按密钥收窄（`callers.go:59` `narrow`）。

### 4.2.2 宿主接口

- `AuthzCheck`（`grpcruntime/host.go:382`）：先用 `qualify` 判断是不是插件自己的 key（`host.go:435`）。自己的 key 不需要授权，其他 key 需要 `authz.read`。然后调用 `Authorizer.Can(callers.narrow(ctx, uid), uid, key)`，只返回 `allowed bool`（proto `host.proto:624-634`）。
- `qualify` 只在两种情况下认作自己的 key：完整限定的 `plugin.<key>:…`，或者与清单声明的本地 key **完全相等**（`host.go:441-445`）。带范围的本地 key（如 `flow:manage:flow/3`）不相等，会被当成外部 key，要求 `authz.read`。这是需要修复的缺陷。
- `AccountScopeCheck`（`host.go:407`）需要 `accounts.read`，并要求 user_id 是正在服务的请求的调用者（`callers.has`）。返回 `{all, allowed_ids}`（`host.proto:636-654`）。
- SDK：`Host.AuthzCheck(ctx, uid, perm) (bool, error)`（`sdk/pluginsdk/host.go:68`、`:325`）；`RequirePermission`（`sdk/pluginsdk/http.go:266`）；账号范围的封装 `ResolveAccountScope`/`RequireAccounts`（`sdk/pluginsdk/accounts_scope.go:78`、`:116`）。测试替身 `FakeHost.AuthzCheck`（`sdk/pluginsdk/pluginsdktest/fakehost.go:304`）。
- Host API 版本：`sdk/protocol/protocol.go:22` `HostAPIVersion = 9`，`runtime-contract/contract.go:14` 同为 9。新能力用 `requireHostAPI` 拦截旧宿主（`sdk/pluginsdk/hostcore.go:88`）。

### 4.2.3 权限解析

- `perm.Parse`（`server/internal/perm/pattern.go:75`）：最后一段是 `*`、`own` 或含 `/` 时视为范围；动作不在写死的 `scopable`（`pattern.go:32`）里就返回 `ErrInvalidKey`。所以当前 `PermissionSet.Add`（`ports_identity.go:47`）会**丢弃**插件的带范围 key，由第 1 章修改 `perm`。
- 访问密钥：`authz.Service.PermissionSet` 返回 `core.RestrictToAccessKey(ctx, uid, set)`（`authz/service.go:262`、`core/context.go:53`），按模式取交集，丢掉超管标记（`Restrict`，`ports_identity.go:276`）。

### 4.2.4 UI 可见性

- `manifest.FilterUI(ui, allowed, allowedCore)`（`sdk/manifest/visible_ui.go:19`）是侧栏与控制台扩展共用的唯一规则。调用方有两处：`authz/menus.go:234-235`（`allowed = set.Has(PermissionKey)`）和 `plugin/api/ui.go:60-61`（同样用 `ps.Has`）。所以一个只持有 `flow:manage:own` 的用户看不到以 `flow:manage` 为权限的菜单。
- 核心菜单早已按“任意范围”判断（`authz/menus.go:283` `hasAny` 用 `HasAction`，CONTRACTS §21.2“菜单”）。
- 控制台 `HostPermissions`（`web/packages/host/src/index.ts:49-66`）：`has`、`any`，UI 3.15 起有 `hasAction?`、`scope?`；实现见 `web/src/host.ts:52-57`。`parsePermission`（`web/src/authz/implies.ts:53`）只认 `SCOPABLE` 里的动作，插件带范围的 key 返回 null，`permissionIndex` 会忽略它。
- iframe 桥 `permissions.check` 用 `auth.holds`，按 `/me` 列表精确成员判断（`web/src/components/plugin/PluginIframe.vue:79-83`）。

### 4.2.5 网关

- 网关请求（`/v1/*`，API Key）只在 `apikey.Authenticate` 里检查 `gateway:use`（`server/internal/apikey/apikey.go:222`）。插件钩子拿到的是 `RequestMeta`（`common.proto:143`），没有控制台权限。`HTTPRequest` 只在 `routes.go:207` 一处构造，`/api/v1/p` 也只在 `routes.go:81` 注册。

### 4.2.6 可复用的查找协议

- `GET /lookup/{kind}`（CONTRACTS §96）：响应类型 `lookup.Item{ID any; Name; Hint; Status}`、`lookup.Result{Items; More}`（`server/internal/lookup/lookup.go:94`、`:103`）；参数解析 `parse`（`:266`）/`parseIDs`（`:315`），常量 `defaultLimit=20`、`maxLimit=50`、`maxIDs=100`、`maxText=256`（`:26-31`）。
- `plugin/api` 已有 `loadCurrentManifest(ctx, key)`（`plugin/api/ops.go:211`），从 DB 读当前版本的清单。插件未运行时也能读到。

## 4.3 设计

### 4.3.1 核心的范围读取（`server/internal/core/plugin_scope.go`，新文件）

新增一个小类型，不复用 `core.DataScope`。原因是 `DataScope` 的字段是 `Owner/Groups/IDs/Pair*`，全是 int64，SQL 谓词（`ScopeTable`）面向核心表，而插件的值是字符串，核心也不替插件查表。

```go
// PluginScope is what a console user reaches with one scoped plugin action
// (CONTRACTS §21.0, plugin resources): every row, the rows they created
// (Own: only the plugin knows its creator), and/or the listed values
// (the <value> of "<res>/<value>" grants, sorted, deduplicated).
type PluginScope struct {
	All, Own bool
	Values   []string
}

// Empty reports that the user holds the action at no scope.
func (s PluginScope) Empty() bool { return !s.All && !s.Own && len(s.Values) == 0 }

// Allows reports whether value is in reach without asking the plugin:
// All, or a listed value. An Own scope is the plugin's to decide.
func (s PluginScope) Allows(value string) bool { return s.All || slices.Contains(s.Values, value) }

// Proto is s as the plugin sees it for action (qualified, without scope).
func (s PluginScope) Proto(action string) *pluginv1.ActionScope {
	return &pluginv1.ActionScope{Permission: action, All: s.All, Own: s.Own, Values: s.Values}
}

// PluginScope is the holder's scope of a plugin action: All for a superuser
// or an unscoped grant, else the own flag and the values of its scoped grants.
func (s PermissionSet) PluginScope(action perm.Key) PluginScope {
	if _, ok := s.Keys[string(action)]; ok || s.Superuser {
		return PluginScope{All: true}
	}
	var out PluginScope
	for _, sc := range s.Scoped[action] {
		out.Own = out.Own || sc.Own
		if sc.Value != "" {
			out.Values = append(out.Values, sc.Value)
		}
	}
	slices.Sort(out.Values)
	out.Values = slices.Compact(out.Values)
	return out
}

// HasSome reports whether the set holds k at some scope when k names none
// (HasAction), else whether it implies k (Has). What a plugin route, menu or
// slot naming a plugin key asks: a holder of x:own reaches part of x's rows.
func (s PermissionSet) HasSome(k string) bool {
	p, err := perm.Parse(k)
	if err != nil {
		return false
	}
	if p.Scope.All {
		return s.HasAction(p.Action)
	}
	return s.Has(k)
}

// AuthorizePluginScope is the scopeFrom rule, mirroring AuthorizeAccounts:
// All passes; otherwise the request must name a value (else
// ErrPermissionDenied naming key) and every value must be listed, unless the
// holder has Own, when values outside the list are the plugin's to check
// against its created_by (else ErrNotFound: out of scope looks like missing).
func AuthorizePluginScope(ctx context.Context, s PluginScope, key string, values []string) error {
	switch {
	case s.All:
		return nil
	case len(values) == 0:
		return ErrPermissionDenied.WithDetails(map[string]any{"permission": key})
	case s.Own:
		return nil
	case slices.ContainsFunc(values, func(v string) bool { return !s.Allows(v) }):
		return ErrNotFound.WithMessage(Text(ctx, "not found", "不存在"))
	}
	return nil
}
```

说明：
- `PluginScope` 不区分 `Res`。一个动作只有一个资源，第 2 章在写入时已经拒绝资源对不上的范围，所以 `Values` 只放 `Scope.Value`，与第 2 章的约定一致。
- `HasSome` 对不带范围的 key 与 `Has` 等价：`HasAction(k)` 等于 `Keys[k]` 或 `Scoped[k]` 非空，而不带范围的动作 `Scoped` 永远为空。所以把它用在不带范围的插件权限上，行为不变。
- 访问密钥不需要特殊处理：`Authorizer.PermissionSet` 返回的已经是收窄后的集合（`RestrictToAccessKey`）。见 4.3.6。

### 4.3.2 路由转发：权限检查与 `scopeFrom`

`serveAPI` 的 admin/user 分支改成两步：读 body 之前检查权限，读 body 之后检查范围。`CanAny` 换成一次 `PermissionSet`，结果有缓存，与 `Can` 内部是同一次调用。

```go
// routes.go, serveAPI
case "admin", "user":
	if !httpapi.Authenticate(c, h.tokens) {
		return
	}
	httpapi.StampAuthzVersion(c, h.authz)
	if set, ok = h.authorizeRoute(c, key, chosen.Route.Permission); !ok {
		return
	}
...
// 读 body、authorizeAccounts（不变）之后：
actionScope, ok := h.authorizeScope(c, chosen, set, params, body)
if !ok {
	return
}
req := &pluginv1.HTTPRequest{Caller: CallerOf(c), ..., ActionScope: actionScope}
```

新文件 `routes/scope.go`：

```go
// authorizeRoute checks the route's plugin permission at some scope
// (PermissionSet.HasSome) and returns the caller's set for authorizeScope. A
// route without a permission names "" and is denied, as before. A key held
// for every row is recorded in Granted, as CanAny did.
func (h *Handler) authorizeRoute(c *gin.Context, pluginKey, local string) (core.PermissionSet, bool)

// authorizeScope applies the route permission's declared scope: nil and
// true for a permission without one; otherwise the caller's PluginScope,
// checked against the values the route's scopeFrom names
// (core.AuthorizePluginScope), as the ActionScope forwarded to the plugin.
func (h *Handler) authorizeScope(c *gin.Context, rb *core.RouteBinding, set core.PermissionSet,
	params map[string]string, body []byte) (*pluginv1.ActionScope, bool) {
	m := rb.Plugin.Manifest
	if m == nil || m.UserPermissionScope(rb.Route.Permission) == nil {
		return nil, true
	}
	key := manifest.PermissionKey(rb.Plugin.Key, rb.Route.Permission)
	scope := set.PluginScope(perm.Key(key))
	if rb.Route.ScopeFrom != "" {
		values, err := scopeValues(rb.Route.ScopeFrom, params, c.Request.URL.Query(), body)
		if err == nil {
			err = core.AuthorizePluginScope(c.Request.Context(), scope, key, values)
		}
		if err != nil {
			httpapi.Fail(c, err)
			return nil, false
		}
	}
	return scope.Proto(key), true
}
```

- 不改注册表：`RouteBinding.Plugin` 是 `core.PluginInfo`，带 `Manifest`（`routes.go:429` `iconAllowed` 已在用）。直接在请求时查 `UserPermissionScope`。这是对清单里少量权限的线性查找，开销可以忽略。
- 只要路由权限声明了 scope，就会带上 `ActionScope`，不论有没有 `scopeFrom`。列表类路由不写 `scopeFrom`，插件按 `ActionScope` 过滤。
- 没有值时只放行 All，规则与 `accounts` 相同。理由：`scopeFrom` 声明的是“本路由作用于请求点名的那一行”，没点名时插件可能理解成“全部”，宽松处理有越权风险。列表接口不应声明 `scopeFrom`，第 1 章的文档里写明这一点。
- 返回码与账号路由一致：一个范围都不持有时 403（`authorizeRoute`）；请求没点名值、又不是 All 时 403，`details.permission` 为完整 key；有值不在范围内时 404。
- 判定矩阵（`v` 为 `scopeFrom` 取到的值）：

| 调用者持有（`plugin.p:flow:manage…`） | 无 scopeFrom 的路由 | `PUT /flows/3`（`scopeFrom: path:id`） | 转给插件的 `action_scope` |
|---|---|---|---|
| 不带范围 / 超管 | 通过 | 通过 | `{all:true}` |
| `:flow/3` | 通过 | 通过 | `{values:["3"]}` |
| `:flow/5` | 通过 | 404 | — |
| `:own` | 通过 | 转发，插件判 created_by | `{own:true}` |
| `:own` + `:flow/5` | 通过 | 转发 | `{own:true, values:["5"]}` |
| 都没有 | 403 | 403 | — |

#### 取值代码复用：把 `accountIDs` 拆成通用取值 + 账号解析

`routes/accounts.go` 现在把“按来源取值”和“解析成 int64”写在一起。改成先取字符串，账号再解析；`scopeFrom` 复用同一段取值代码。不新增解析器：

```go
// requestValues collects the distinct values the request names under from:
// path parameters, query parameters (every value) and top-level JSON body
// fields, matched case-insensitively like encoding/json. A value is a JSON
// string (comma-separated parts), a number, or an array of them; empty
// parts are skipped. A body that is no JSON object names none.
func requestValues(from []string, params map[string]string, query url.Values, body []byte) ([]string, error)

// accountIDs = requestValues + strconv.ParseInt for each value (error message unchanged),
// dedupe, and the MaxAccountBatch cap. Behavior unchanged.
func accountIDs(from []string, params map[string]string, query url.Values, body []byte) ([]int64, error)

// scopeValues reads the values of a scopeFrom entry: requestValues, each
// perm.ValidScopeValue (else 400 fields[{<name>, invalid}]), at most
// maxScopeValues (= manifest.MaxAccountBatch, the same bound as a batch of
// account ids).
func scopeValues(from string, params map[string]string, query url.Values, body []byte) ([]string, error)
```

`sourceIDs`/`fieldIDs`/`rawIDs`/`splitIDs` 相应改成 `sourceValues`/`fieldValues`/`rawValues`/`splitValues`，返回 `[]string`。`rawValues` 用 `json.Decoder.UseNumber()` 解出 `any`，处理 `string`、`json.Number`、`[]any`（元素是 string 或 json.Number）三种情况。账号 id 的行为（含小数、非法值报 400）保持不变，由 `TestPluginRouteAccounts` 保证。

body 字段是数组时，每个值都必须在范围内，这是 `AuthorizePluginScope` 的“全部值”语义，与第 1 章约定一致。

### 4.3.3 告诉插件“仅 own”：proto 变化

选择在 proto 里加字段，不用请求头。理由：`routes.go:225-231` 会把客户端请求头转给插件，权限信息放在头里可以被伪造；如果改为专门剥掉某个头，就又多一条安全约束要维护。

`sdk/proto/sub2api/plugin/v1/common.proto` 新增：

```proto
// ActionScope is a console user's scope of one scoped plugin permission
// (CONTRACTS §21.0, plugin resources): what the core decided, for the plugin
// to filter by. Unset fields mean nothing is reached.
message ActionScope {
  // Fully qualified action key without a scope: "plugin.moderation:flow:manage".
  string permission = 1;
  // Every row of the resource.
  bool all = 2;
  // The rows the user created. Only the plugin knows its rows' creator: it
  // MUST check its own created_by (Caller.user_id) for any value not in values.
  bool own = 3;
  // The <value> of the user's "<resource>/<value>" grants, sorted, once each.
  repeated string values = 4;
}
```

`app.proto` 的 `HTTPRequest`：

```proto
message HTTPRequest {
  ...
  bytes body = 8;
  // Set when the route's permission declares a scope (manifest
  // userPermissions[].scope): the caller's scope of that permission, already
  // narrowed by an access key. With scopeFrom the core has checked the named
  // values, except those only own may reach (own: the plugin checks). Absent
  // on routes whose permission has no scope (a manifest declaring one needs
  // core >= PluginPermissionsFirstCore, which always sets it).
  ActionScope action_scope = 9;
}
```

`host.proto` 只改 `AuthzCheckRequest.permission` 的注释（带范围的本地 key 也算自己的 key，见 4.3.4），消息结构不变。

**不升 Host API**（审查时简化）：`action_scope` 只在路由权限声明了 scope 时出现，而声明 scope 的清单已被第 1 章 `coreFloors` 限定在核心 ≥ `PluginPermissionsFirstCore`，那一版一定会填它；旧插件忽略未知字段。所以 `sdk/protocol/protocol.go:22`、`runtime-contract/contract.go:14` 的 `HostAPIVersion = 9` 都不动。SDK 仍按失败关闭处理（4.3.5）：拿不到 `action_scope` 就是零值，什么也放不过。

### 4.3.4 `AuthzCheck`：修正自己 key 的判断；返回范围推迟

本轮只修一个缺陷：`qualify` 把带范围的本地 key 当成外部 key（4.2.2）。修法：先把输入限定成 `manifest.PermissionKey(h.key(), p)`，用 `perm.Parse` 取出动作，动作的本地部分是已声明的 key 时就算自己的 key。这样 `flow:manage:flow/3` 和 `flow:manage:own` 都按自己的 key 处理，`allowed` 仍是 `Authorizer.Can` 的蕴含判断。参数名 `perm` 与包名冲突，改名为 `p`。`AuthzCheck` 其余代码不变。

**`AuthzCheckResponse.scope` 与 SDK `ResolveScope` 首版不做**（审查时推迟）：唯一用途是“在 A 动作的路由里判断 B 动作的范围”（例如列表路由权限是 `flow:read`，要给每行标能否 `flow:manage`）。本轮内置插件都不声明 scope（第 1 章 §8.1），e2e 也不需要，没有调用方。真有插件需要时再加，届时按下表扩展 `AuthzCheck`（响应加一个可选 `ActionScope scope = 2`，宿主填 `set.PluginScope(action).Proto(...)`，SDK 用 `h.Client().AuthzCheck` 直接调，和 `ResolveAccountScope` 一样不给 `Host` 接口加方法），不新开 RPC：

| | 扩展 `AuthzCheck`（以后采用） | 新增 `ScopeCheck` RPC |
|---|---|---|
| proto | 响应加 1 个可选字段 | 新 RPC + 请求/响应消息 |
| 宿主 | `AuthzCheck` 里多 3 行 | 新 handler、鉴权、参数校验 |
| SDK / 测试替身 | `FakeHost.AuthzCheck` 多填一个字段 | 新接口方法、`FakeHost` 新方法、`pluginsdktest` 新的 corecalls |
| 权限模型 | 沿用“自己的 key 免授权，其他需要 authz.read” | 要另定规则 |

### 4.3.5 pluginsdk 便捷封装（`sdk/pluginsdk/action_scope.go`，新文件）

与 `accounts_scope.go` 同一种风格，只给插件写带范围路由所必需的几样：

```go
// Scope is a console user's scope of one scoped plugin permission
// (HTTPRequest.action_scope). The zero value reaches nothing.
type Scope struct {
	Permission string // qualified action, no scope
	All, Own   bool
	Values     []string
	userID     int64 // the caller, for Own
}

// RequestScope is the scope the core attached to req for the route's own
// permission. A route whose permission declares no scope gets the zero
// Scope: fail closed.
func RequestScope(req *pluginv1.HTTPRequest) Scope

// Allows reports whether the user may act on the row value, created by
// createdBy (0 when the row records none, which only All and Values reach).
func (s Scope) Allows(value string, createdBy int64) bool {
	return s.All || slices.Contains(s.Values, value) || s.Own && createdBy > 0 && createdBy == s.userID
}

// RequireScope is nil when req's caller may act on value (created by
// createdBy); otherwise the answer to send: 401 without a user, 404 out of
// scope (as the core answers on its own routes).
func RequireScope(req *pluginv1.HTTPRequest, value string, createdBy int64) *pluginv1.HTTPResponse
```

- 不给 `Host` 接口加方法（加方法会破坏实现了 `Host` 的测试替身），不提供 `Filter`（没有调用方，列表过滤写在 SQL 里更自然）。
- 列表的 SQL 过滤由插件自己写，SDK 不给 SQL 生成器。插件表结构各不相同，给了也用不上。文档示例：`WHERE ($all OR id = ANY($values) OR ($own AND created_by = $uid))`。
- `RequirePermission`（`http.go:266`）保持“全量”语义，注释补一句：带范围的权限请用 `RequestScope`/`RequireScope`，因为只持有 own 的人用 `RequirePermission("flow:manage")` 会得到 403。
- options 路由的封装见 4.3.7。

### 4.3.6 访问密钥收窄后的语义

不需要新增代码：`authorizeRoute`、options 代理用的都是 `Authorizer.PermissionSet`，已经过 `RestrictToAccessKey`（`authz/service.go:262`）；`AuthzCheck` 用的 `Authorizer.Can` 也一样。第 1 章让 `perm.Parse`、`Implies` 认识插件范围之后，`Restrict` 的逐模式取交集（`ports_identity.go:276`）就能直接用于插件 key。

| 用户持有 | 访问密钥写的 | 收窄结果（`PluginScope`） |
|---|---|---|
| `flow:manage`（全部） | `flow:manage:own` | `{own}` |
| `flow:manage:flow/2` | `flow:manage`（存量不带后缀） | `{values:[2]}`（`Restrict` 保留用户的较窄范围） |
| `flow:manage:own` | `flow:manage:flow/2` | 不可比，丢弃，结果为空，路由 403 |
| 超管 | `flow:manage:flow/2` | `{values:[2]}`（密钥永远不按超管处理） |

`HTTPRequest.action_scope` 和插件随后调用 `AuthzCheck` 得到的结果出自同一个收窄集合，所以二者一致。几个请求同时在途时，`callers.narrow` 取几个密钥的交集（`callers.go:59`），这是现有的失败关闭语义。插件不会被告知调用来自访问密钥（`routes.go:173-177` 的现有约定不变）。

### 4.3.7 `scope.options` 路由的核心代理

#### 接口

```
GET /api/v1/plugins/:key/scope-options?permission=<本地 key>&q=&ids=a,b&limit=20
权限：PermAny(role:read, plugin:read)
200 {"data":{"items":[{"id":"3","name":"客服线路","hint":"已发布","status":"active"}],"more":false}}
```

- 响应与 `GET /lookup/{kind}` 同形（`lookup.Result`/`lookup.Item`），只是 `id` 永远是字符串。插件的值是字符串，key 写作 `<res>/<id>`。第 5 章的 EntityPicker 可以直接复用 lookup 的缓存与渲染。
- 参数规则沿用 §96.1：`q` 最长 256 字节，`limit` 1..50（默认 20），`ids` 最多 100 个；给了 `ids` 时忽略 `q` 和 `limit`，`more` 为 false。复用方式是从 lookup 包导出一个函数，不复制代码：

```go
// lookup.go: ParseText reads q, ids (as text) and limit as GET /lookup does
// for a kind without numeric ids; the scope options proxy shares it.
func ParseText(c *gin.Context) (q string, ids []string, limit int, err error) {
	p, err := parse(c, &Entity{})
	...
}
```

- 按 `permission` 查，而不是按 `resource`：候选值要按调用者**在该动作上的范围**过滤，只列他能授出的值。这与 lookup“看不到的不返回、已选的显示原值”一致，而防提权最终仍在保存时检查（第 2 章）。共用同一个资源和 options 路由的多个权限会打到同一条插件路由。
- 权限选 `role:read` 或 `plugin:read`，不选 `role:define`/`plugin:install`：
  1. 候选值和名称不只是编辑时要用，角色页（`GET /roles/:id` 需要 `role:read`，`authz/http.go:18`）和插件详情“权限”标签（`plugin:read`，`plugin/api/api.go:69`）在只读查看时也要把 `flow/3` 显示成名称；
  2. 返回内容已经按调用者自己的范围过滤，只读者能看到的不会超过他本来就能访问的数据；
  3. 写权限在保存接口上检查。内置角色中持有 `role:define`/`plugin:install` 的都同时持有对应的读权限，推测自定义角色也是如此；不是的话，给前端加一个 403 退化路径即可。
  访问密钥带了其中一个 key 就能调用。

#### 处理流程（`plugin/api/scope_options.go`，新文件）

放在 `plugin/api`，不放在 `routes`：枚举范围要读未运行插件的清单（`loadCurrentManifest`，`ops.go:211`），`routes.Handler` 没有 DB。转发只需要“找到绑定、调用 `Client.HandleHTTP`”两步，不必复用 `serveAPI`。

```go
// RegisterRoutes:
r.PermAny(http.MethodGet, "/plugins/:key/scope-options", a.scopeOptions, perm.RoleRead, perm.PluginRead)

// scopeOptions answers the candidates of a scoped plugin permission
// (userPermissions[].scope) the caller could grant: the manifest's values,
// or the plugin's options route asked with the caller's scope.
func (a *API) scopeOptions(c *gin.Context)
```

1. `m, _, err := a.loadCurrentManifest(ctx, key)` 找不到插件时 404。`up, ok := m.FindUserPermission(local)` 找不到权限时 404。`up.Scope == nil` 时返回 400 `invalid_argument`。
2. `q, ids, limit, err := lookup.ParseText(c)`；`set := a.d.Authz.PermissionSet(ctx, uid)`；`ps := set.PluginScope(perm.Key(PermissionKey(key, local)))`。
3. **枚举范围**（`len(Scope.Values) > 0`）：核心直接从清单作答，不调用插件。插件停用或尚未运行（如首次启用的确认页）时也可用。`id = Key`，`name = Label` 取请求语言（`core.Locale`），按 `q` 不区分大小写匹配 key 或 label，按 `ps.Allows` 过滤，再截取 `limit`。
4. **插件资源**（`Scope.Options != ""`）：
   - `ps.Empty()` 时直接返回空列表，不调用插件；
   - `ids` 在 `!ps.All && !ps.Own` 时先与 `ps.Values` 取交集，交集为空时直接返回空列表；
   - 在 `a.d.Registry.Current().Routes(key)` 里找 `Method == GET && Path == Scope.Options` 的绑定，找不到（插件停用或未运行，或节点上的版本还没有这条路由）时返回 503 `plugin_unavailable`（`core.ErrPluginUnavailable`，`core/errors.go:79`）；
   - 组装请求并调用 `rb.Client.HandleHTTP`。超时、并发分级、caller 登记都由 `httpAdapter` 完成（`adapters.go:325`），插件在处理期间可以照常调用 `AuthzCheck`：

```go
req := &pluginv1.HTTPRequest{
	Caller:      routes.CallerOf(c), // 从 routes.go:208-213 提取的导出函数，serveAPI 也改用它
	Method:      http.MethodGet,
	Path:        up.Scope.Options,
	RoutePath:   rb.Route.Path,
	Query:       map[string]*pluginv1.HeaderValues{"q": {Values: []string{q}}, "ids": {Values: []string{strings.Join(ids, ",")}}, "limit": {Values: []string{strconv.Itoa(limit)}}},
	Headers:     map[string]*pluginv1.HeaderValues{"accept-language": ...},
	ActionScope: ps.Proto(PermissionKey(key, local)), // 被配置的那个权限的范围，不是 options 路由自己的
}
```

5. 校验插件的响应：状态必须是 200，否则返回 503 `plugin_unavailable` 并记日志。body 解成 `{"data":{"items":[…],"more":bool}}`。丢弃 `id` 不满足 `perm.ValidScopeValue`、`name` 为空的行；`name`/`hint` 截到 256 字节，`status` 截到 32 字节。`!ps.All && !ps.Own` 时再按 `ps.Values` 过滤一遍（纵深防御，不信任插件）。截取到 `limit` 条，用 `httpapi.OK` 输出 `lookup.Result`。

插件路由本身的 `permission`（第 1 章建议写同资源的读 key）只约束经 `/api/v1/p/<key>/…` 的直接调用。代理调用跳过它，因为调用者是配置角色的人，可能不持有该权限；代理已经用 `role:read`/`plugin:read` 检查过，并用被配置权限的范围过滤。插件区分两种调用的依据是 `action_scope.permission`：代理调用时，它是**被配置的那个**权限。

#### 插件侧封装

```go
// ScopeOptionsQuery is one call of a scope options route (manifest
// userPermissions[].scope.options), from the core's proxy or the console.
type ScopeOptionsQuery struct {
	Text  string   // q, trimmed
	IDs   []string // ids=; when given, Text and Limit do not apply
	Limit int      // 1..50
	Scope Scope    // the caller's scope of the permission being configured
}

type ScopeOption struct{ ID, Name, Hint, Status string }

// ScopeOptions wraps fn as the options route: parses the query, answers
// DataResponse({items, more}) with fn's page (items cut to Limit, more set
// when fn returned more), and keeps fn from seeing a request without a user.
// fn filters by q.Scope (All; Values; Own = rows created by the caller).
func ScopeOptions(fn func(ctx context.Context, q ScopeOptionsQuery) ([]ScopeOption, bool, error)) HandlerFunc
```

文档示例（moderation 的 `/scopes/flow`）：`SELECT id::text, name, status FROM flows WHERE (name ILIKE $q OR id::text = $q) AND ($all OR id::text = ANY($values) OR ($own AND created_by = $uid)) ORDER BY name LIMIT $limit+1`。

#### 前端退化（规则，组件由第 5 章实现）

- “全部”“自己创建的”不来自 options：ScopePicker 根据清单里的 `scope.own` 固定显示这两项。
- 503 `plugin_unavailable`（插件未运行），或 403：只显示“全部 / 自己创建的”加手输值，已选值显示原值。首次安装的确认页属于这种情况；清单默认值只能是 `["*"]`、`["own"]` 或枚举值（第 1 章校验），所以确认页本来就不需要具体 id。

### 4.3.8 UI 可见性规则

一条规则：**界面元素（菜单、页面的 Create 链接、PageNav 项、插槽、账号动作）引用的插件权限，按“任意范围持有”判断显示**。这与核心的账号、代理菜单一致（`HasAction`），由路由与插件在数据层面收窄。

服务端：
- `authz/menus.go:234` 和 `plugin/api/ui.go:60-61` 的 `allowed` 回调从 `set.Has(manifest.PermissionKey(p.Key, perm))` 改成 `set.HasSome(...)`。`allowedCore`（核心 key，PageNav 的 `corePermission`）保持 `set.Has` 不变，本章不改核心 key 的显示口径。
- `FilterUI` 本身（`visible_ui.go`）不改，它只接收回调，签名也不变。
- 账号动作（`AccountAction.Permission` 必须等于路由的 permission，`manifest.go:1098`）：按 D4，插件 key 不带账号范围。第 1 章 check 规则 U1、U2 已规定：账号动作和 `ui.settings.permission` 引用的权限不能声明 scope，否则报 `unsupported`。前者的范围由 `accounts` 与核心 `account:*` 取交集，后者由 `CanAny`（全量）判断。这两处不受本章影响。

控制台（规则，具体实现在第 5 章）：
- `parsePermission`（`implies.ts:53`）要与第 1 章的 `perm.Parse` 同步：`plugin.` 开头的 key 按语法解析出 `Res/Value`，用共享向量 `sdk/permvectors/vectors.json` 加用例锁定。之后 `permissionIndex.hasAction/scopes` 自然覆盖插件动作。
- `HostPermissions` 形状不变（3.15 已有 `hasAction?`、`scope?`），语义扩展到插件 key：
  - `has("plugin.p:flow:manage")`：持有全部才为 true（保持全量语义）；
  - `hasAction("plugin.p:flow:manage")`：任意范围，用来决定按钮“编辑”是否出现；
  - `scope("plugin.p:flow:manage")`：例如 `["plugin.p:flow:manage:own","plugin.p:flow:manage:flow/2"]`。
  依赖这些语义的插件页面声明 `hostUICompat: "^3.16"`，因为 3.15 的控制台不解析插件范围。用 scope 的清单反正必须 3.16（第 1 章）。
- 行级按钮（某一行能否编辑）：**推荐由插件后端在列表行里返回 `can_edit`**，后端用 `Scope.Allows(id, created_by)` 计算。不推荐前端自己用 `scope()` 加 `created_by` 去算：own 的判定依赖插件数据，放在后端只有一份逻辑。
- iframe 桥 `permissions.check`（`PluginIframe.vue:79`）：本地 key 不带范围时，改为“`/me` 列表里有该 key，或者有该 key 加一段范围后缀”（任意范围，同服务端 `HasSome`）；带范围时按蕴含判断（持有全部或蕴含该范围的授予即为 true，同服务端 `Has`；二审 logic #6，此前精确匹配，持有全部反而为 false）。仍然不用 `auth.has`，避免超管捷径泄露身份，这一点原有注释已经写明。

### 4.3.9 网关请求路径不受影响

- 网关（`/v1/*`）的鉴权只有 `gateway:use`（`apikey.go:222`），不涉及插件用户权限。
- 本章修改的只有：`routes.serveAPI`（控制台 `/api/v1/p`）、`plugin/api` 新接口、`AuthzCheck`、`FilterUI` 的两个调用方。钩子的 `RequestMeta`、调度、`OnEvents` 都不经过这些代码。
- `AuthzCheck` 在钩子或后台调用时行为不变（只修正了带范围本地 key 的归属）。moderation 的 `isStaff` 调用 `AuthzCheck(uid, "moderation:exempt")`（`plugins/moderation/internal/moderation/staff.go:59`），该 key 不带范围，所以结果不变。
- 测试 `TestGatewayPathIgnoresPluginScopes`（见 4.5）锁定这一点。

## 4.4 改动清单

| 文件 | 改什么 |
|---|---|
| `sdk/proto/sub2api/plugin/v1/common.proto` | 新增 `message ActionScope` |
| `sdk/proto/sub2api/plugin/v1/app.proto` | `HTTPRequest.action_scope = 9` |
| `sdk/proto/sub2api/plugin/v1/host.proto` | 只改 `AuthzCheckRequest.permission` 注释（带范围的本地 key 算自己的 key），消息不变 |
| `sdk/gen/pluginv1/*` | `buf generate` 重新生成 |
| `sdk/protocol/protocol.go`、`runtime-contract/contract.go` | **不改**（不升 Host API，§4.3.3） |
| `server/internal/core/plugin_scope.go`（新） | `PluginScope`、`Empty/Allows/Proto`、`PermissionSet.PluginScope`、`PermissionSet.HasSome`、`AuthorizePluginScope` |
| `server/internal/plugin/routes/routes.go` | admin/user 分支改调 `authorizeRoute`，读 body 后调 `authorizeScope`，请求带 `ActionScope`；提取导出函数 `CallerOf(c)` |
| `server/internal/plugin/routes/scope.go`（新） | `authorizeRoute`、`authorizeScope`、`scopeValues` |
| `server/internal/plugin/routes/accounts.go` | 取值拆成 `requestValues`/`sourceValues`/`fieldValues`/`rawValues`/`splitValues`（字符串），`accountIDs` 改为在其上解析 int64，行为不变 |
| `server/internal/plugin/grpcruntime/host.go` | `qualify` 认带范围的本地 key（参数改名 `p`）；`AuthzCheck` 其余不变 |
| `server/internal/plugin/api/scope_options.go`（新） | `scopeOptions` handler 及响应校验 |
| `server/internal/plugin/api/api.go` | 注册 `GET /plugins/:key/scope-options`（`PermAny(role:read, plugin:read)`） |
| `server/internal/lookup/lookup.go` | 导出 `ParseText(c)`（包一层 `parse(c, &Entity{})`） |
| `server/internal/authz/menus.go:234`、`server/internal/plugin/api/ui.go:60` | `allowed` 回调改用 `set.HasSome` |
| `sdk/pluginsdk/action_scope.go`（新） | `Scope`、`RequestScope`、`Allows`、`RequireScope`、`ScopeOptions`、`ScopeOptionsQuery`、`ScopeOption` |
| `sdk/pluginsdk/http.go:260` | `RequirePermission` 注释补带范围权限的说明 |
| `docs/CONTRACTS.md` | §21.0“插件与 lookup”段补插件资源范围的运行时规则；新增小节写清 scopeFrom 判定矩阵、`action_scope`、`/plugins/:key/scope-options`（编号由主文档定） |
| 第 5 章（只提要求） | `implies.ts` 解析插件范围；`PluginIframe.vue` `localAllowed` 改为任意范围；ScopePicker 接 options 接口 |

不改：`manifest.FilterUI`、`EffectiveRouteAccounts`、`core.AuthorizeAccounts`、注册表 `addRoutes`、网关、`pluginsdktest`（`Harness.DoAs`，`harness.go:181`，本轮没有插件用范围；SDK 单测直接构造 `HTTPRequest`）、Host API 版本。

## 4.5 测试计划

### 单元测试

`server/internal/core/plugin_scope_test.go`
- `TestPluginScope`：表驱动。不带范围或超管时为 All；`own` 加 `flow/3`、`flow/2` 得到 `{Own, Values:[2,3]}`（已排序）；没有持有时 `Empty()`；持有其他动作的范围不串到本动作。
- `TestHasSome`：不带范围的插件 key，持有 own 时为 true；对不带范围的 key 与 `Has` 等价（随机抽核心 key 验证）；带范围的 key 按蕴含判断；非法 key 为 false。
- `TestAuthorizePluginScope`：覆盖 4.3.2 的矩阵。All 通过；无值且非 All 返回 403，`details.permission` 为完整 key；有 own 时，不在 Values 里的值也通过；值不在范围内返回 404。

`server/internal/plugin/routes/scope_test.go`（复用 `routes_test.go` 的假注册表和授权器）
- `TestPluginRouteScopedPermissionAnyScope`：只持有 `flow:manage:own` 时，无 `scopeFrom` 的路由返回 200，插件收到 `action_scope{permission:"plugin.p:flow:manage", own:true}`。
- `TestPluginRouteScopeFrom`：表驱动，path、query、body（数字、字符串、数组、逗号串、字段名大小写不同）× 4.3.2 矩阵的每一行，断言状态码，以及插件是否被调用、收到的 `action_scope`。
- `TestPluginRouteScopeFromInvalidValue`：值不满足 `ValidScopeValue` 时 400 `fields[{id, invalid}]`；超过 500 个时 400。
- `TestPluginRouteUnscopedPermissionUnchanged`：不带范围的权限仍按全量判断，请求里没有 `action_scope`，`Granted` 有记录。
- `TestPluginRouteScopeAccessKey`：访问密钥 `flow:manage:own` 加上用户持有全部时，收到 `{own}`；`flow:manage:flow/2` 加上用户持有 own 时返回 403。
- `TestPluginRouteScopeHeaderNotTrusted`：客户端带任意 `X-…-Scope` 头，插件收到的 `action_scope` 不受影响。
- 现有的 `TestPluginRouteAccounts`、`TestPluginAPIRouteWithoutPermissionDenied` 原样通过（取值重构与权限判断改写的回归）。

`server/internal/plugin/routes/accounts_test.go`
- `TestRequestValues`：string、number、array、逗号串、空片段、非对象 body、字段名大小写不同。`accountIDs` 对 `1.5`、`"a"` 仍返回原来的错误。

`server/internal/plugin/grpcruntime/host_test.go`

- `TestHostAuthzCheckScopedLocalKeyIsOwn`：`flow:manage:flow/3` 不需要 `authz.read`，`allowed` 按蕴含判断（持有 `flow/3` 或全部时为 true）。

- 现有的 `TestHostAuthzCheckOwnKeysOnly`、`TestHostAuthzCheckArguments` 保持通过。

`server/internal/plugin/api/scope_options_test.go`
- `TestScopeOptionsValues`：枚举范围，插件未运行时也返回；`q` 匹配 key 和中英文 label；按调用者范围过滤；`limit` 与 `more`；`ids`。
- `TestScopeOptionsForwards`：插件资源时，插件收到的 path、query 和 `action_scope.permission` 是被配置的权限；对响应做截断、丢掉非法行、按 Values 再过滤（插件多返回的行被丢掉）。
- `TestScopeOptionsEmptyScopeSkipsPlugin`：调用者不持有该动作时返回 `items:[]`，插件没被调用；`ids` 与 Values 无交集时也不调用插件。
- `TestScopeOptionsErrors`：未知插件或权限 404；权限无 scope 400；插件未运行 503 `plugin_unavailable`；插件返回非 200 或坏 JSON 时 503；没有 `role:read`/`plugin:read` 时 403；参数非法（与 lookup 同）400。
- `TestScopeOptionsAccessKey`：密钥带 `plugin:read` 可调用，候选值按密钥收窄后的范围过滤。

`server/internal/authz/menus_test.go`、`server/internal/plugin/api/api_test.go`（`/ui/plugins` 的现有用例在这里，没有 `ui_test.go`）
- `TestPluginMenusScopedPermission`：只持有 own 时能看到以该权限为 permission 的菜单、Create 链接和插槽；什么都不持有时看不到。
- `TestUIPluginsScopedPermission`：`/ui/plugins` 同样如此。

`sdk/pluginsdk/action_scope_test.go`
- `TestRequestScopeZeroFailsClosed`：没有 `action_scope` 时 `Allows` 永远为 false。
- `TestScopeAllows`：All；Values；Own 时 created_by 等于、不等于、为 0 的情况。
- `TestRequireScope`：401、404、nil。

- `TestScopeOptionsHandler`：解析 q、ids、limit；`more` 的计算；DataResponse 的形状；没有 user 时 401。

### 集成与 e2e

- `e2e/ac27_plugin_scope_test.go`（新）：不新写测试插件二进制。取市场里的 guard 包，用 `Package.SetManifest` 改清单后用测试发布者重签上传（`e2e/pkg.go:85,127`、`e2e/publishers.go:16`；推测沿用 `ac15_16_trust_ui_test.go` 的重签方式，若同 key 换发布者被拒，就换一个插件 key 以 community 信任上传）。清单改动：`rules:read` 加 `scope: {values: [{key:"a"}, {key:"b"}]}`，`GET /rules` 加 `scopeFrom: "query:set"`；`stats:read` 加同样的枚举 scope（guard 菜单的权限，`plugins/guard/manifest.json`）。核心在转发前判定，guard 本身忽略 `set` 参数，所以不需要改插件代码。
  - `TestPluginScopeRouteE2E`：角色授予 `rules:read:value/a` 后，`GET /rules?set=a` 返回 200，`?set=b` 返回 404，不带 `set` 返回 403；授予不带范围的 `rules:read` 后三种都 200。
  - `TestPluginScopeOptionsE2E`：持有 `role:read` 的用户调用 `/plugins/<k>/scope-options?permission=rules:read`，核心直接从清单回答枚举值（插件停用后仍可用），只返回调用者范围内的值；没有 `role:read`/`plugin:read` 时 403。插件资源（options 路由转发）由单测 `TestScopeOptionsForwards` 覆盖。
  - `TestPluginScopeMenuE2E`：只持有 `stats:read:value/a` 的用户 `/me/menus` 包含 guard 菜单。
  - `TestGatewayPathIgnoresPluginScopes`：带插件范围授权的用户，网关请求的结果与对照组相同（只看 `gateway:use`）。
- 内置插件本轮不声明 `scope`/`scopeFrom`（第 1 章 §8.1），所以不改 moderation 路由；`ac22_moderation_test.go` 只回归 exempt 改 critical 后 `isStaff` 的行为不变。

## 4.6 风险与未决问题

1. **依赖第 1 章的 `perm.Parse`**：插件范围解析不进 `perm`，`PermissionSet.Add` 会丢掉带范围的插件 key（`pattern.go:84-87`），本章所有判断都会退化成“没有范围”。实现顺序必须先做第 1 章的 perm 部分（总览 P0）。
2. **own 的正确性依赖插件**：核心不知道创建者。插件忘了检查 `own` 就会越权（只持有 own 的人能改别人的行）。缓解措施：SDK 的零值失败关闭；`RequireScope` 一行接入；CONTRACTS 和 SDK 注释写明“必须检查”；内置插件的 e2e 覆盖。官方插件以外的插件只能靠审核。
3. **`scopeFrom` 无值时只放行 All**：与 accounts 一致，但插件作者可能把列表路由也写上 `scopeFrom`，结果受限用户全部 403。第 1 章已在文档中写明“`scopeFrom` 只用于作用于单个点名行的路由，列表路由按 `action_scope` 过滤”。check 无法判断哪些路由是列表，所以不做 lint，靠文档和 e2e 覆盖。
4. **options 候选值只读暴露**：持有 `role:read` 的人可以看到自己范围内插件资源的名称，范围之外的看不到。如果日后要求只读者看不到任何名称，把权限改成 `role:define` 或 `plugin:install` 即可，前端已有 403 退化。
5. **滚动升级的版本错位**：scope-options 用 DB 当前版本的清单，转发用节点上运行的那一代。新版本刚批准、节点还没切换时，options 路由可能不存在，返回 503，前端退化，可以接受。
6. **iframe 桥语义变化**：`permissions.check("flow:manage")` 原来对只持有 own 的人返回 false（因为根本没有带范围的插件 key），改为 true。没有存量插件受影响，因为存量没有带范围的插件 key，但要在 CONTRACTS 的桥一节写明。
7. **`AuthzCheck.allowed` 保持全量语义**：插件用 `RequirePermission("flow:manage")` 会挡住只持有 own 的人。这是有意保持兼容，SDK 注释引导改用 `RequestScope`/`RequireScope`。`sub2api-plugin check` 首版不做静态提示。
8. **账号动作和 `ui.settings.permission` 不允许带范围**：第 1 章已采纳，规则 U1、U2：`ui.accountActions[i].permission`、`ui.settings.permission` 引用声明了 scope 的权限时报 `unsupported`。所以 4.3.8 对这两处不需要运行时处理。
