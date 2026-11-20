<script setup lang="ts">
// One migration script of a plugin: its id, what the default slot says about
// it (will run, ran at …) and its SQL behind a toggle, monospaced and
// scrolling within its box.
import { useI18n } from 'vue-i18n'
import { SDisclosure } from '@sub2api/ui'

defineProps<{ id: string; sql: string }>()
const { t } = useI18n()
</script>

<template>
  <li class="min-w-0 py-2" :data-migration="id">
    <div class="flex flex-wrap items-center gap-2">
      <code class="break-all font-mono text-sm">{{ id }}</code>
      <slot />
    </div>
    <SDisclosure class="mt-1" :label="t('plugins.database.showSql')" :open-label="t('plugins.database.hideSql')">
      <pre class="max-h-96 overflow-auto font-mono text-fg" data-testid="migration-sql">{{ sql }}</pre>
    </SDisclosure>
  </li>
</template>
