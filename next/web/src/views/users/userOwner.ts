import type { FieldCodeMessages } from '@/utils/errors'

/** The owner_id field errors of POST /users and PATCH /users/:id (CONTRACTS §103.3). */
export const OWNER_ERRORS: FieldCodeMessages = {
  self: 'users.ownerErrors.self',
  cycle: 'users.ownerErrors.cycle',
  too_deep: 'users.ownerErrors.too_deep',
  not_found: 'users.ownerErrors.not_found',
  owner_cannot_manage: 'users.ownerErrors.owner_cannot_manage'
}
