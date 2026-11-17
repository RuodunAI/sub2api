<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { SSettingRow } from '@sub2api/ui'
import HeaderOverrideInput from './HeaderOverrideInput.vue'
import type { HeaderOverride } from './model'

// The account editor's "request header override" row (CONTRACTS §108).
// `reason` (a fixed common control) replaces the hint.
const model = defineModel<HeaderOverride | null>({ required: true })
defineProps<{ error?: string; disabled?: boolean; reason?: string }>()
const { t } = useI18n()
const P = 'accounts.headerOverride'
</script>

<template>
  <SSettingRow :label="t(`${P}.title`)" :description="reason || t(`${P}.hint`)" :error="error" data-testid="account-header-override">
    <HeaderOverrideInput v-model="model" :disabled="disabled" />
  </SSettingRow>
</template>
