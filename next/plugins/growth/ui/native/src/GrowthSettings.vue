<script setup lang="ts">
// The growth plugin settings (manifest ui.settings.component), shown in the
// plugin's settings tab: GET / PUT /plugins/growth/settings through
// usePluginSettings. The fields of a switched-off section are hidden; a
// cleared number box sends no key, as the JSON Schema form did.
import { SInput, SSettingRow, SSettingsGroup, SSettingsPage, SSwitch, usePluginSettings } from '@sub2api/ui'
import { useGrowthHost } from './host'
import { SETTINGS_DEFAULTS, SETTINGS_RULES, type GrowthSettingsValue } from './settings'

const host = useGrowthHost()
const t = host.t
const settings = usePluginSettings<GrowthSettingsValue>({ pluginKey: host.plugin.key, api: host.api, rules: SETTINGS_RULES, defaults: SETTINGS_DEFAULTS })
</script>

<template>
  <SSettingsPage :settings="settings">
    <SSettingsGroup :title="t('settings.referral.title')" :description="t('settings.referral.description')">
      <SSettingRow prop="referral_enabled" :label="t('settings.referralEnabled')">
        <SSwitch :model-value="!!settings.get('referral_enabled')" data-testid="growth-referral-enabled" @update:model-value="settings.set('referral_enabled', $event)" />
      </SSettingRow>
      <template v-if="settings.get('referral_enabled')">
        <SSettingRow prop="referral_rate_percent" :label="t('settings.referralRate.label')" :description="t('settings.referralRate.help')">
          <SInput type="number" v-bind="settings.bindNumber('referral_rate_percent')" :min="0" :max="100" step="any" :suffix="t('settings.unit.percent')" data-testid="growth-referral-rate" />
        </SSettingRow>
        <SSettingRow prop="referral_max_per_invitee" :label="t('settings.referralMax.label')" :description="t('settings.referralMax.help')">
          <SInput type="number" v-bind="settings.bindNumber('referral_max_per_invitee')" :min="0" step="any" :suffix="t('settings.unit.usd')" data-testid="growth-referral-max" />
        </SSettingRow>
        <SSettingRow prop="referral_duration_days" :label="t('settings.referralDuration.label')" :description="t('settings.referralDuration.help')">
          <SInput type="number" v-bind="settings.bindNumber('referral_duration_days')" :min="0" :step="1" :suffix="t('settings.unit.days')" data-testid="growth-referral-days" />
        </SSettingRow>
      </template>
    </SSettingsGroup>
    <SSettingsGroup :title="t('settings.checkin.title')" :description="t('settings.checkin.description')">
      <SSettingRow prop="checkin_enabled" :label="t('settings.checkinEnabled')">
        <SSwitch :model-value="!!settings.get('checkin_enabled')" data-testid="growth-checkin-enabled" @update:model-value="settings.set('checkin_enabled', $event)" />
      </SSettingRow>
      <template v-if="settings.get('checkin_enabled')">
        <SSettingRow prop="checkin_min_quota" :label="t('settings.checkinMin')">
          <SInput type="number" v-bind="settings.bindNumber('checkin_min_quota')" :min="0" step="any" :suffix="t('settings.unit.usd')" data-testid="growth-checkin-min" />
        </SSettingRow>
        <SSettingRow prop="checkin_max_quota" :label="t('settings.checkinMax')">
          <SInput type="number" v-bind="settings.bindNumber('checkin_max_quota')" :min="0" step="any" :suffix="t('settings.unit.usd')" data-testid="growth-checkin-max" />
        </SSettingRow>
        <SSettingRow prop="checkin_timezone" :label="t('settings.checkinTimezone.label')" :description="t('settings.checkinTimezone.help')">
          <SInput v-bind="settings.bind('checkin_timezone')" mono placeholder="UTC" data-testid="growth-checkin-timezone" />
        </SSettingRow>
      </template>
    </SSettingsGroup>
  </SSettingsPage>
</template>
