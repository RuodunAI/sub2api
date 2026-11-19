<script setup lang="ts">
// The guard plugin settings (manifest ui.settings.component), shown in the
// plugin's settings tab: GET / PUT /plugins/guard/settings through
// usePluginSettings.
import { SInput, SSettingRow, SSettingsGroup, SSettingsPage, SSwitch, SUrlPresetInput, usePluginSettings } from '@sub2api/ui'
import { useGuardHost } from './host'
import { COOLDOWN_MAX, SETTINGS_DEFAULTS, SETTINGS_RULES, type GuardSettingsValue } from './settings'

const host = useGuardHost()
const t = host.t
const settings = usePluginSettings<GuardSettingsValue>({ pluginKey: host.plugin.key, api: host.api, rules: SETTINGS_RULES, defaults: SETTINGS_DEFAULTS })
</script>

<template>
  <SSettingsPage :settings="settings">
    <SSettingsGroup>
      <SSettingRow prop="webhook_url" :label="t('settings.webhookUrl.label')" :description="t('settings.webhookUrl.help')">
        <SUrlPresetInput v-bind="settings.bind('webhook_url')" placeholder="https://hooks.example.com/guard" data-testid="guard-webhook-url" />
      </SSettingRow>
      <SSettingRow prop="alert_cooldown_sec" :label="t('settings.cooldown.label')" :description="t('settings.cooldown.help')">
        <SInput
          type="number"
          :model-value="settings.get('alert_cooldown_sec') as number | undefined"
          :min="0"
          :max="COOLDOWN_MAX"
          :step="1"
          :suffix="t('settings.cooldown.unit')"
          data-testid="guard-cooldown"
          @update:model-value="settings.set('alert_cooldown_sec', ($event as number | null) ?? undefined)"
        />
      </SSettingRow>
      <SSettingRow prop="record_snippets" :label="t('settings.snippets.label')" :description="t('settings.snippets.help')">
        <SSwitch :model-value="!!settings.get('record_snippets')" data-testid="guard-snippets" @update:model-value="settings.set('record_snippets', $event)" />
      </SSettingRow>
    </SSettingsGroup>
  </SSettingsPage>
</template>
