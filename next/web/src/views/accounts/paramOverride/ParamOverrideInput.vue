<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import OverrideSummary from '../controls/OverrideSummary.vue'
import ParamOverrideEditor from './ParamOverrideEditor.vue'
import { describeOperation } from './describe'
import { MODES, readOverride, type Override } from './model'

// The request parameter override (CONTRACTS §84) at a glance, with the editor
// behind "Edit"; the v-model is the account's `param_override` (null clears it).
// `headerOverride` (the account's `header_override`, §108) is the starting
// header state of the dry run. Plugins get it as `host.components.ParamOverrideEditor`.
const model = defineModel<Override | null>({ required: true })
withDefaults(defineProps<{ models?: string[]; disabled?: boolean; headerOverride?: Record<string, string> | null }>(), {
  models: () => [],
  headerOverride: null
})
const { t } = useI18n()
const P = 'accounts.paramOverride'
const open = ref(false)

const drafts = computed(() => readOverride(model.value ?? null))
const headerOps = computed(() => drafts.value.filter(d => MODES[d.mode]?.group === 'header').length)
const summary = computed(() =>
  drafts.value.length
    ? [t(`${P}.count`, { n: drafts.value.length }), headerOps.value && t(`${P}.headerCount`, { n: headerOps.value })]
        .filter(Boolean)
        .join(' · ')
    : t(`${P}.none`)
)
const items = computed(() => drafts.value.map(d => d.description || describeOperation(d, t)))
</script>

<template>
  <OverrideSummary
    :summary="summary"
    :items="items"
    :ns="P"
    testid="account-param-override"
    :disabled="disabled"
    @clear="model = null"
    @edit="open = true"
  >
    <ParamOverrideEditor
      v-model:open="open"
      :value="model ?? null"
      :models="models"
      :header-override="headerOverride"
      @apply="model = $event"
    />
  </OverrideSummary>
</template>
