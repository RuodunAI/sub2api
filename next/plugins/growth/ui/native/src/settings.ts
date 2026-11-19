import type { Rules } from '@sub2api/ui'

// The growth settings (Go: growth.Settings, checked again by parseSettings,
// which is final).
export interface GrowthSettingsValue extends Record<string, unknown> {
  referral_enabled?: boolean
  referral_rate_percent?: number
  referral_max_per_invitee?: number
  referral_duration_days?: number
  checkin_enabled?: boolean
  checkin_min_quota?: number
  checkin_max_quota?: number
  checkin_timezone?: string
}

/** Filled into absent keys (as the JSON Schema form did). */
export const SETTINGS_DEFAULTS: GrowthSettingsValue = {
  referral_enabled: true,
  referral_rate_percent: 10,
  referral_max_per_invitee: 0,
  referral_duration_days: 0,
  checkin_enabled: true,
  checkin_min_quota: 0.01,
  checkin_max_quota: 0.05,
  checkin_timezone: 'UTC'
}

const referral = (m: GrowthSettingsValue) => !!m.referral_enabled
const checkin = (m: GrowthSettingsValue) => !!m.checkin_enabled

/** The fields of a switched-off section are hidden and not checked here. */
export const SETTINGS_RULES: Rules<GrowthSettingsValue> = {
  referral_enabled: { type: 'boolean' },
  referral_rate_percent: { type: 'number', min: 0, max: 100, when: referral },
  referral_max_per_invitee: { type: 'number', min: 0, when: referral },
  referral_duration_days: { type: 'integer', min: 0, when: referral },
  checkin_enabled: { type: 'boolean' },
  checkin_min_quota: { type: 'number', min: 0, when: checkin },
  checkin_max_quota: { type: 'number', min: 0, when: checkin },
  checkin_timezone: { type: 'string', when: checkin }
}

/** The number settings: a cleared box sends no key, as the JSON Schema form did. */
export type NumberSetting = 'referral_rate_percent' | 'referral_max_per_invitee' | 'referral_duration_days' | 'checkin_min_quota' | 'checkin_max_quota'
