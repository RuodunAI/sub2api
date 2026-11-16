/**
 * Whose usage a page shows: every user's (`all`, /usage, an operator's), the
 * caller's own (`self`, /me/usage), or that of the accounts in the caller's
 * reach (`account`, /account-usage, CONTRACTS §103.5: the account's facts
 * only). The dev mock's export takes it too.
 */
export type UsageView = 'all' | 'self' | 'account'
