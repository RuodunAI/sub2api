<script setup lang="ts">
// GET /plugins/:key/database (CONTRACTS §130): the plugin's table prefix and
// database identity, the tables outside its own it asks for, and the
// migration scripts of the version it runs with when each ran. Loaded again
// when that version changes; only the latest load is shown.
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@sub2api/host'
import { SBadge, SButton, SCard, SDescItem, SDescriptions, SEmpty, SHint, SSpinner } from '@sub2api/ui'
import type { PluginDatabase } from '@/api/types'
import { formatDateTime } from '@/utils/format'
import { errorMessage } from '@/utils/errors'
import { latestOnly } from '@/composables/useLatestRequest'
import type { PluginDetail } from '../pluginUtil'
import DatabaseAccessList from '../parts/DatabaseAccessList.vue'
import MigrationScript from '../parts/MigrationScript.vue'

const props = defineProps<{ detail: PluginDetail }>()
const { t } = useI18n()

const data = ref<PluginDatabase | null>(null)
const loading = ref(false)
const loadError = ref('')
const begin = latestOnly()

async function load() {
  const latest = begin()
  loading.value = true
  loadError.value = ''
  try {
    const r = await api.get<PluginDatabase>(`/plugins/${encodeURIComponent(props.detail.key)}/database`)
    if (latest()) data.value = r
  } catch (e) {
    if (!latest()) return
    data.value = null
    loadError.value = errorMessage(e)
  } finally {
    if (latest()) loading.value = false
  }
}

// The version the plugin runs (the core's CurrentVersion): its scripts and access.
watch([() => props.detail.active_version, () => props.detail.desired_version], load, { immediate: true })
</script>

<template>
  <div v-if="!data && loading" class="flex justify-center py-12"><SSpinner /></div>
  <SCard v-else-if="!data">
    <SEmpty icon="warning" :text="t('plugins.database.loadFailed')" data-testid="plugin-database-error">
      <SHint v-if="loadError" size="xs" tone="danger">{{ loadError }}</SHint>
      <SButton size="sm" :loading="loading" @click="load">{{ t('common.refresh') }}</SButton>
    </SEmpty>
  </SCard>
  <div v-else class="space-y-4" data-testid="plugin-database">
    <SCard :title="t('plugins.detail.tabs.database')">
      <template #actions>
        <SButton size="sm" variant="ghost" :loading="loading" icon="refresh" @click="load">{{ t('common.refresh') }}</SButton>
      </template>
      <SDescriptions>
        <SDescItem :label="t('plugins.database.prefix')" :value="data.prefix" mono copyable />
        <SDescItem :label="t('plugins.database.role')" :value="data.role" mono copyable />
      </SDescriptions>
      <SHint size="xs" class="mt-2">{{ t('plugins.database.prefixHint') }}</SHint>
      <SHint size="xs" class="mt-1">{{ t('plugins.database.roleHint') }}</SHint>
    </SCard>

    <SCard :title="t('plugins.database.access')" :subtitle="data.access.length ? t('plugins.database.accessHint') : undefined">
      <DatabaseAccessList v-if="data.access.length" :access="data.access" />
      <SHint v-else>{{ t('plugins.database.noAccess') }}</SHint>
    </SCard>

    <SCard :title="t('plugins.database.migrations')" :subtitle="t('plugins.database.migrationsHint', { prefix: data.prefix })">
      <ul v-if="data.migrations.length" class="divide-y divide-line">
        <MigrationScript v-for="m in data.migrations" :id="m.id" :key="m.id" :sql="m.sql">
          <SHint v-if="m.applied_at" inline size="xs">{{ t('plugins.database.appliedAt', { at: formatDateTime(m.applied_at) }) }}</SHint>
          <SBadge v-else tone="warning">{{ t('plugins.database.notApplied') }}</SBadge>
        </MigrationScript>
      </ul>
      <SHint v-else>{{ t('plugins.database.noMigrations') }}</SHint>
    </SCard>
  </div>
</template>
