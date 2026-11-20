import type { Rules } from '@sub2api/ui'
import { useBedrockHost } from './host'

// The fields of the three account types and their rules: the bounds the
// plugin's ValidateCredentials holds them to (internal/bedrock/credentials.go),
// for immediate feedback; the save is checked there.

export type BedrockType = 'bedrock_apikey' | 'bedrock_aksk' | 'bedrock_sts'

/** inference_profile_prefix values: none, or the prefix put before model ids (bedrock.ProfilePrefixes). */
export const PREFIXES = ['none', 'us', 'eu', 'apac', 'jp', 'au', 'in', 'global'] as const

/** Filled into a new account: bedrock.DefaultRegion and bedrock.DefaultProfilePrefix. */
export const DEFAULTS = { region: 'us-east-1', inference_profile_prefix: 'global' }

const VISIBLE = /^[!-~]+$/

const ACCESS_KEY: Rules = {
  access_key_id: { required: true, pattern: /^[A-Z0-9]{16,128}$/ },
  secret_access_key: { required: true, min: 1, max: 1024, pattern: VISIBLE }
}

/** A region outside amazonaws.com: China (cn-*) or isolated (us-iso-*, eu-isoe-*, ...), bedrock.unsupportedPartition. */
const unsupportedPartition = (region: string) => {
  const [geography, ...rest] = region.split('-')
  return geography === 'cn' || rest.join('-').startsWith('iso')
}

/** Region and prefix, shared by every type. The empty region means the default. */
const SETTINGS: Rules = {
  region: [
    { pattern: /^([a-z]{2}(-[a-z]+)+-\d)?$/ },
    { validator: (v) => (typeof v === 'string' && unsupportedPartition(v) ? useBedrockHost().t('region.unsupported') : undefined) }
  ],
  inference_profile_prefix: { enum: PREFIXES }
}

export const RULES: Record<BedrockType, Rules> = {
  bedrock_apikey: { api_key: { required: true, whitespace: true }, ...SETTINGS },
  bedrock_aksk: { ...ACCESS_KEY, ...SETTINGS },
  bedrock_sts: {
    ...ACCESS_KEY,
    session_token: { required: true, min: 1, max: 8192, pattern: VISIBLE },
    session_expires_at: { type: 'date-time' },
    ...SETTINGS
  }
}

