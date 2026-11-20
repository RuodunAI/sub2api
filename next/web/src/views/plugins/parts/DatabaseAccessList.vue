<script setup lang="ts">
// The tables outside its own a plugin asks for (manifest database.access):
// table, columns (all of them when none is listed) and privileges, one entry
// each, and what writing or referencing core tables means when asked for.
// Shown by the consent page under db.core_read and by the plugin's database
// tab. `granted`, the entries of the db.core_read grant in force (an
// upgrade's current_scope.access), marks what the request adds to it.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SAlert, SBadge, SHint } from '@sub2api/ui'
import type { DatabaseAccess, DatabasePrivilege } from '@/api/types'
import { useOptionalT } from '@/composables/optionalText'
import { accessScopeEntries, asArray } from '../pluginUtil'
import { useConsentLabels } from '../consent/hostPermission'

const props = defineProps<{ access: DatabaseAccess[]; granted?: string[] }>()
const { t } = useI18n()
const tr = useOptionalT()
const { colon } = useConsentLabels()

const privilegeLabel = (p: string) => tr(`plugins.database.privilege.${p}`, undefined, p)
const WRITES = new Set<DatabasePrivilege>(['insert', 'update', 'delete'])
const asked = computed(() => new Set(props.access.flatMap((a) => a.privileges)))
const warnings = computed(() => [
  [...asked.value].some((p) => WRITES.has(p)) ? t('plugins.database.writeWarn') : '',
  asked.value.has('references') ? t('plugins.database.referencesWarn') : ''
].filter(Boolean))

const granted = computed(() => (props.granted ? new Set(props.granted) : null))
/** The table is not in the grant at all: the whole entry is new. */
const tableAdded = (a: DatabaseAccess) => !!granted.value && ![...granted.value].some((e) => e.split(':')[1]?.startsWith(a.table + '.'))
/** Some of what the privilege asks for on a table already granted is not granted yet. */
const privilegeAdded = (a: DatabaseAccess, p: DatabasePrivilege) =>
  !!granted.value && !tableAdded(a) && accessScopeEntries({ ...a, privileges: [p] }).some((e) => !granted.value!.has(e))
</script>

<template>
  <div data-testid="database-access">
    <ul class="divide-y divide-line rounded-lg border border-line">
      <li v-for="a in access" :key="a.table" class="px-3 py-2" :data-table="a.table">
        <div class="flex flex-wrap items-center gap-1.5">
          <code class="mr-1 break-all font-mono text-sm font-medium">{{ a.table }}</code>
          <SBadge v-if="tableAdded(a)" tone="danger" data-testid="access-added">{{ t('plugins.database.added') }}</SBadge>
          <SBadge v-for="p in a.privileges" :key="p" :tone="p === 'select' ? 'gray' : 'warning'" :data-privilege="p">
            {{ privilegeLabel(p) }}<template v-if="privilegeAdded(a, p)"> · {{ t('plugins.database.added') }}</template>
          </SBadge>
        </div>
        <SHint size="xs" class="mt-0.5 break-words">
          {{ t('plugins.database.columns') }}{{ colon }}<span v-if="asArray(a.columns).length" class="font-mono">{{ asArray(a.columns).join(', ') }}</span
          ><template v-else>{{ t('plugins.database.allColumns') }}</template>
        </SHint>
      </li>
    </ul>
    <SAlert v-if="warnings.length" tone="warning" dense class="mt-2" data-testid="database-access-warn">
      <p v-for="w in warnings" :key="w">{{ w }}</p>
    </SAlert>
  </div>
</template>
