<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SPageHeader } from '@sub2api/ui'
import UsageOverview from './UsageOverview.vue'

// The overview menu items: all users' statistics (`admin`), those of the
// accounts in the caller's reach (`accountView`, CONTRACTS §102.5), or the caller's.
const props = withDefaults(defineProps<{ admin?: boolean; accountView?: boolean }>(), { admin: false, accountView: false })
const { t } = useI18n()
const revision = ref(0)
const loading = ref(false)
const view = computed(() => {
  if (props.accountView) return { summary: '/account-usage/summary', title: 'usage.accountOverviewTitle', description: 'usage.accountOverviewDescription' }
  return props.admin
    ? { summary: '/usage/summary', title: 'usage.overviewTitle', description: 'usage.overviewDescription' }
    : { summary: '/me/usage/summary', title: 'usage.myOverviewTitle', description: 'usage.myOverviewDescription' }
})
</script>

<template>
  <div>
    <SPageHeader :title="t(view.title)" :description="t(view.description)">
      <template #actions>
        <SButton :loading="loading" icon="refresh" @click="revision++">{{ t('common.refresh') }}</SButton>
      </template>
    </SPageHeader>
    <UsageOverview
      v-model:loading="loading"
      :revision="revision"
      :summary-path="view.summary"
      :default-range="admin || accountView ? 'today' : '7d'"
      :admin="admin"
      :account-view="accountView"
    />
  </div>
</template>
