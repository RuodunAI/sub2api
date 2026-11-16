// What the editor may hand out (the server's grantSet, CONTRACTS §43.6): its
// keys in the grant view. /me lists the keys in force only, so a disabled
// plugin's keys are missing from it, while the server checks a grant against
// the keys of the editor's roles with a disabled plugin's included and a
// removed permission's left out (authz RolesPermissions). The editor's roles'
// saved keys (`roles`: the Roles page's list, the matrix's rows) are added to
// /me for that: a holder of a disabled plugin's permission may still grant it.
import { computed } from 'vue'
import { ALL_SCOPE, permissionIndex, type Scope } from '@/authz/implies'
import { actionOf } from '@/authz/pluginGrants'
import { useAuthStore } from '@/stores/auth'

export interface EditorGrants {
  /** The editor may grant key. */
  mayGrant: (key: string) => boolean
  /** The editor's scopes of action ([] when not held). */
  scopes: (action: string) => Scope[]
  /** The editor holds action at some scope. */
  holdsAction: (action: string) => boolean
}

export function useEditorGrants(
  roles: () => ReadonlyArray<{ key: string; permission_keys?: string[] | null }>,
  removed: (action: string) => boolean = () => false
): EditorGrants {
  const auth = useAuthStore()
  const index = computed(() => {
    const mine = new Set(auth.me?.roles ?? [])
    const saved = roles()
      .filter((r) => mine.has(r.key))
      .flatMap((r) => r.permission_keys ?? [])
      .filter((k) => !removed(actionOf(k)))
    return permissionIndex([...(auth.me?.permissions ?? []), ...saved])
  })
  const superuser = () => !!auth.me?.superuser
  return {
    mayGrant: (key) => superuser() || index.value.has(key),
    scopes: (action) => (superuser() ? [ALL_SCOPE] : index.value.scopes(action)),
    holdsAction: (action) => superuser() || index.value.hasAction(action)
  }
}
