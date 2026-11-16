// The entities the console picks by name (EntityPicker, GET /lookup/{kind}).
// Each is one description: where it is looked up, how it shows, who may look
// it up. A new entity is a new entry here; the picker does not change. A
// plugin's scope candidates (GET /plugins/:key/scope-options) are kinds made
// up per plugin permission, so they are described by entityOf, not listed.

import { P, type PermRef, type ScopableKey } from '@/generated/permissions'

export type CoreEntityKind = 'user' | 'group' | 'api_key' | 'account' | 'model' | 'proxy'
/** The values a plugin permission's scope takes: pscope:<plugin>/<local permission key>. */
export type PluginScopeKind = `pscope:${string}/${string}`
export type EntityKind = CoreEntityKind | PluginScopeKind

export interface EntityDescriptor {
  /** The {kind} of GET /lookup/{kind}. */
  path: string
  /** SIcon name. */
  icon: string
  /** i18n key of the entity's name (placeholder, "No matching …"). */
  label: string
  /** False when the id is the name itself (models): no "#id" and nothing to look up by id. */
  numeric: boolean
  /** Any one of these lets the caller look the entity up; none = every signed-in user. */
  permissions: PermRef[]
  /** Instead of permissions: this action held at any scope (CONTRACTS §21); the lookup lists the rows in reach. */
  action?: ScopableKey
  /** The same for a `mine` lookup; left out where `mine` asks nothing less. */
  minePermissions?: PermRef[]
  /**
   * Lookup parameters that name another entity, with the i18n key ({name})
   * of the line that tells the user what the parameter did to the list.
   */
  scopes?: Record<string, { kind: EntityKind; note: string }>
  /** The whole path looked up, instead of /lookup/{path}. */
  url?: string
  /** Parameters every lookup of the kind sends. */
  query?: Record<string, string>
  /** The id is an opaque string (a plugin's value): named through ids=, shown as it is while unknown, never as "#id". */
  opaqueIds?: boolean
}

export const ENTITIES: Record<CoreEntityKind, EntityDescriptor> = {
  // user:read takes the team scope (CONTRACTS §102.3): the lookup lists the users in reach.
  user: { path: 'users', icon: 'user', label: 'entity.kind.user', numeric: true, permissions: [], action: P.UserRead },
  group: {
    path: 'groups',
    icon: 'group',
    label: 'entity.kind.group',
    numeric: true,
    // account:group:bind alone finds the groups the caller can use: those an own account may be bound to.
    permissions: ['group:read', 'account:group:bind'],
    minePermissions: []
  },
  api_key: {
    path: 'api-keys',
    icon: 'key',
    label: 'entity.kind.api_key',
    numeric: true,
    // The usage records already show every key's id, name and owner to this permission.
    permissions: ['usage:all:read'],
    minePermissions: ['apikey:self:manage'],
    scopes: { user_id: { kind: 'user', note: 'entity.scope.userKeys' } }
  },
  account: {
    path: 'accounts',
    icon: 'account',
    label: 'entity.kind.account',
    numeric: true,
    permissions: [],
    action: P.AccountRead,
    scopes: { prefer_group_id: { kind: 'group', note: 'entity.scope.groupAccountsFirst' } }
  },
  model: { path: 'models', icon: 'cpu', label: 'entity.kind.model', numeric: false, permissions: ['price:read'], minePermissions: ['apikey:self:manage'] },
  proxy: { path: 'proxies', icon: 'proxy', label: 'entity.kind.proxy', numeric: true, permissions: [], action: P.ProxyRead }
}

/**
 * An id of a numeric kind as the lookup takes one (typed as q, or in ids=): a
 * positive integer of at most 18 digits (any fits the core's int64). "0",
 * "007" and longer digit strings are not: typed, they are text, and their
 * name is never asked for (the core refuses ids= with a 0 or with a number
 * past int64, and the other ids of the request with it).
 */
export const isEntityId = (v: string): boolean => /^[1-9]\d{0,17}$/.test(v)

/** A plugin scope value as a key takes one (perm.scopeValueRe); the only ids an opaque kind looks up or takes typed. */
export const isScopeValue = (v: string): boolean => /^[A-Za-z0-9_.-]{1,64}$/.test(v)

/** Whether v can be an id of the entity as it stands: a numeric id, a plugin value, any text where the id is the name. */
export const takesId = (entity: EntityDescriptor, v: string): boolean => (entity.opaqueIds ? isScopeValue(v) : !entity.numeric || isEntityId(v))

const PLUGIN_SCOPE = 'pscope:'

/** The kind of the values a plugin permission's scope takes. */
export const pluginScopeKind = (plugin: string, local: string): PluginScopeKind => `${PLUGIN_SCOPE}${plugin}/${local}`

/** Whether kind is a plugin scope's (pluginScopeKind) rather than a core entity. */
export const isPluginScopeKind = (kind: EntityKind): kind is PluginScopeKind => kind.startsWith(PLUGIN_SCOPE)

/** The descriptors of the plugin scope kinds seen so far: a kind gets the same object every time. */
const pluginScopes = new Map<PluginScopeKind, EntityDescriptor>()

/** The description of a kind. */
export function entityOf(kind: EntityKind): EntityDescriptor {
  if (!isPluginScopeKind(kind)) return ENTITIES[kind]
  let entity = pluginScopes.get(kind)
  if (!entity) {
    const rest = kind.slice(PLUGIN_SCOPE.length)
    const i = rest.indexOf('/')
    entity = {
      path: '',
      url: `/plugins/${encodeURIComponent(rest.slice(0, i))}/scope-options`,
      query: { permission: rest.slice(i + 1) },
      icon: 'plugin',
      label: 'entity.kind.pluginScope',
      numeric: false,
      opaqueIds: true,
      permissions: ['role:read', 'plugin:read']
    }
    pluginScopes.set(kind, entity)
  }
  return entity
}

/** Whether a caller holding what `auth` answers may look `kind` up (`mine`: only its own rows). */
export function mayLookUp(kind: EntityKind, mine: boolean, auth: { has(perm: PermRef[]): boolean; hasAction(action: string): boolean }): boolean {
  const entity = entityOf(kind)
  if (entity.action) return auth.hasAction(entity.action)
  return auth.has((mine && entity.minePermissions) || entity.permissions)
}
