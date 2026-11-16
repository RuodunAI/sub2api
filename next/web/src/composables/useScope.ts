import { useAuthStore } from '@/stores/auth'
import { scopeAllowsRow, type ScopedRow } from '@/authz/implies'
import { P, type ScopableKey, type ScopedPermKey } from '@/generated/permissions'

// Resource scopes (CONTRACTS §21, §102.2): an action key reaches every row
// (account:update), or the rows of its scope: own (the caller's: an account
// it owns, a proxy it created), team (the caller's and those of the users
// below it), group/<g> (accounts bound to the group), account/<id>,
// group/<g>/account/<id>, proxy/<id>. The server filters in SQL; the console
// uses the same rule (scopeAllowsRow, core.DataScope.AllowsRow) to show or
// hide row actions and columns. Only the server knows a team (can).

/**
 * 'all' reaches every row; 'team' the caller's and its team's rows (and
 * maybe more by other scopes); 'own' is the restricted tier, any narrower
 * scope (own, groups, ids). The own-level limits apply below 'all' (capped
 * fields, the administrator's lock); null reaches nothing.
 */
export type ScopeLevel = 'all' | 'team' | 'own' | null

/** The scoped account actions. */
export const ACCOUNT_ACTIONS = {
  read: P.AccountRead,
  create: P.AccountCreate,
  update: P.AccountUpdate,
  delete: P.AccountDelete,
  test: P.AccountTest,
  reveal: P.AccountCredentialView
} as const

/** The scoped proxy actions. */
export const PROXY_ACTIONS = {
  read: P.ProxyRead,
  manage: P.ProxyManage
} as const

/**
 * Creating a proxy (also pasting a proxy URL, which may create one): the own
 * scope of proxy:manage or more; proxy/<id> reaches only existing proxies.
 */
export const PROXY_CREATE_KEY: ScopedPermKey = `${PROXY_ACTIONS.manage}:own`

/** Single keys that widen what a restricted caller may set on an account (CONTRACTS §21). */
export const ACCOUNT_FIELD_KEYS = {
  relay: P.AccountRelay,
  groupBind: P.AccountGroupBind,
  groupManage: P.GroupManage,
  settingsCustom: P.AccountSettingsCustom
} as const

export function useScope() {
  const auth = useAuthStore()

  /** How far action reaches for the caller. */
  function level(action: ScopableKey): ScopeLevel {
    if (auth.has(action)) return 'all'
    if (auth.has(`${action}:team`)) return 'team'
    return auth.hasAction(action) ? 'own' : null
  }

  /**
   * Whether one of the caller's scopes of action reaches row, a row the
   * server listed to the caller. Only the server knows a team (who is below
   * the caller), so a team scope takes any such row: the server still
   * refuses the action on a row outside the team.
   */
  function can(row: ScopedRow | null | undefined, action: ScopableKey): boolean {
    return auth.scopes(action).some((s) => (s.team && !!row) || scopeAllowsRow(s, row, auth.me?.id))
  }

  return { level, can }
}
