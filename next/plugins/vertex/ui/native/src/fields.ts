import { SECRET_MASK, type Rule, type Rules } from '@sub2api/ui'

// The fields of the account types and their rules: the bounds the plugin's
// ValidateCredentials holds them to (internal/vertex/credentials.go), for
// immediate feedback; the save is checked there.

export type KeyFileType = 'vertex_sa' | 'vertex_authorized_user'

/** The credentials field holding the JSON text of each key file type (vertex.keyKinds). */
export const KEY_FIELD: Record<KeyFileType, string> = {
  vertex_sa: 'service_account_json',
  vertex_authorized_user: 'authorized_user_json'
}

type T = (key: string) => string

/** Project and location, empty allowed: the key file's project / the default location, or the express address. */
const PROJECT: Rule = { pattern: /^([a-z][a-z0-9-]{4,28}[a-z0-9])?$/ }
const LOCATION: Rule = { pattern: /^([a-z0-9-]{2,32})?$/ }

/** Empty once trimmed, as the plugin reads the fields (accountform.Object.Str). */
const blank = (v: unknown) => typeof v !== 'string' || !v.trim()

/** The key of the project in each key file (keyFile.project: project_id of a service account, quota_project_id of user credentials). */
const FILE_PROJECT: Record<KeyFileType, string> = { vertex_sa: 'project_id', vertex_authorized_user: 'quota_project_id' }

/** Whether the key file names no project; false when the form cannot tell (a stored file, masked, or one the plugin refuses itself). */
function namesNoProject(text: unknown, type: KeyFileType): boolean {
  if (typeof text !== 'string' || text === SECRET_MASK) return false
  try {
    const file: unknown = JSON.parse(text)
    return typeof file === 'object' && file !== null && blank((file as Record<string, unknown>)[FILE_PROJECT[type]])
  } catch {
    return false
  }
}

/** The rules of a key file type: without a project the plugin takes the file's, and refuses the account when the file names none (validateKeyAccount). */
export const keyFileRules = (type: KeyFileType, t: T): Rules => ({
  [KEY_FIELD[type]]: { required: true, min: 2, max: 32768 },
  project_id: [PROJECT, { validator: (v, { model }) => (blank(v) && namesNoProject(model[KEY_FIELD[type]], type) ? t('project.required') : undefined) }],
  location: LOCATION
})

/** Project and location of an API key account: both (the project address) or neither (the express address), target.check. */
export const apiKeyTarget = (t: T) => {
  const pairedWith = (other: string): Rule => ({ validator: (v, { model }) => (blank(v) && !blank(model[other]) ? t('express.both') : undefined) })
  return { project_id: [PROJECT, pairedWith('location')], location: [LOCATION, pairedWith('project_id')] }
}

/** Filled into a new key file account: vertex.LocationGlobal. An API key account has no default (the express address, apiKeyForm). */
export const KEY_FILE_DEFAULTS = { location: 'global' }
