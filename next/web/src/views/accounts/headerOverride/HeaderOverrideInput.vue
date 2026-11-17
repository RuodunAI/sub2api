<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import OverrideSummary from '../controls/OverrideSummary.vue'
import HeaderOverrideEditor from './HeaderOverrideEditor.vue'
import { describeRow, readRows, type HeaderOverride } from './model'

// The request header override (CONTRACTS §108) at a glance, with the editor
// behind "Edit"; the v-model is the account's `header_override` (null clears
// it). Plugins get it as `host.components.HeaderOverrideEditor`.
const model = defineModel<HeaderOverride | null>({ required: true })
defineProps<{ disabled?: boolean }>()
const { t } = useI18n()
const P = 'accounts.headerOverride'
const open = ref(false)

const items = computed(() => readRows(model.value).map(r => describeRow(r, t)))
const summary = computed(() => (items.value.length ? t(`${P}.count`, { n: items.value.length }) : t(`${P}.none`)))
</script>

<template>
  <OverrideSummary
    :summary="summary"
    :items="items"
    :ns="P"
    testid="account-header-override"
    :disabled="disabled"
    @clear="model = null"
    @edit="open = true"
  >
    <HeaderOverrideEditor v-model:open="open" :value="model ?? null" @apply="model = $event" />
  </OverrideSummary>
</template>
