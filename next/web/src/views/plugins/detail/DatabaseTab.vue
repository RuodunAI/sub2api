<script setup lang="ts">
// GET /plugins/:key/database (CONTRACTS §130): the plugin's table prefix and
// database identity, the tables outside its own it was granted, and the
// migration scripts of its installed version with when each ran.
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@sub2api/host'
import { SBadge, SButton, SCard, SDescItem, SDescriptions, SHint, SSpinner } from '@sub2api/ui'
import type { PluginDatabase } from '@/api/types'
import { formatDateTime } from '@/utils/format'
import { notifyError } from '@/utils/errors'
import type { PluginDetail } from '../pluginUtil'
import DatabaseAccessList from '../parts/DatabaseAccessList.vue'
import MigrationScript from '../parts/MigrationScript.vue'

const props = defineProps<{ detail: PluginDetail }>()
const { t } = useI18n()

const data = ref<PluginDatabase | null>(null)
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    data.value = await api.get<PluginDatabase>(`/plugins/${encodeURIComponent(props.detail.key)}/database`)
  } catch (e) {
    notifyError(e)
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div v-if="!data" class="flex justify-center py-12">
    <SSpinner v-if="loading" />
    <SButton v-else size="sm" icon="refresh" @click="load">{{ t('common.refresh') }}</SButton>
  </div>
  <div v-else class="space-y-4" data-testid="plugin-database">
    <SCard :title="t('plugins.detail.tabs.database')">
      <template #actions>
        <SButton size="sm" variant="ghost" :loading="loading" icon="refresh" @click="load">{{ t('common.refresh') }}</SButton>
      </template>
      <SDescriptions>
        <SDescItem :label="t('plugins.database.prefix')" :value="data.prefix" mono copyable>
          <code class="break-all font-mono text-sm">{{ data.prefix }}</code>
          <SHint size="xs">{{ t('plugins.database.prefixHint') }}</SHint>
        </SDescItem>
        <SDescItem :label="t('plugins.database.role')" :value="data.role" mono copyable>
          <code class="break-all font-mono text-sm">{{ data.role }}</code>
          <SHint size="xs">{{ t('plugins.database.roleHint') }}</SHint>
        </SDescItem>
      </SDescriptions>
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
