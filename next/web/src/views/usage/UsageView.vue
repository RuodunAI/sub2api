<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SPageHeader } from '@sub2api/ui'
import UsageRecords from './UsageRecords.vue'

// Every request (`all`, /usage), or those the accounts in the caller's reach
// served (`account`, /account-usage, CONTRACTS §103.5).
const props = withDefaults(defineProps<{ view?: 'all' | 'account' }>(), { view: 'all' })
const { t } = useI18n()
const base = props.view === 'account' ? '/account-usage' : '/usage'

// The page owns the header and the refresh button, which sits in the search
// bar (#actions) rather than on a row of its own; the records component
// follows `revision` and reports its loading state, so no ref reaches into
// its methods.
const revision = ref(0)
const loading = ref(false)
function refresh() {
  revision.value++
}
</script>

<template>
  <div>
    <SPageHeader :title="t(view === 'account' ? 'usage.accountTitle' : 'usage.title')" :description="t(view === 'account' ? 'usage.accountDescription' : 'usage.description')" />
    <UsageRecords v-model:loading="loading" :revision="revision" :list-path="base" :summary-path="`${base}/summary`" :view="view">
      <template #actions>
        <SButton size="sm" :loading="loading" icon="refresh" @click="refresh">{{ t('common.refresh') }}</SButton>
      </template>
    </UsageRecords>
  </div>
</template>
