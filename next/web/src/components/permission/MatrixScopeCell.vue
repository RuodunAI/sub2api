<script setup lang="ts">
// One scoped cell of PermissionMatrix: a select naming the cell's choice
// (none / all / own / chosen…) as far as `offer` allows, and, while values
// are chosen, a "N chosen" link. "Chosen…" opens the matrix's ScopePicker
// (`open`) and stays shown while it is open for this cell (`editing`), so the
// select does not snap back before a value is picked; the other choices emit
// the cell's new keys (`update`). Under a shrink-only lock nothing but the
// current choice and none is open; a frozen cell opens nothing.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SLink, SSelect } from '@sub2api/ui'
import { OWN_SCOPE, ALL_SCOPE } from '@/authz/implies'
import type { ScopeOffer } from '@/authz/scopeChoice'
import { cellOfScopes, cellSummary } from '@/authz/pluginGrants'
import { isShrinkOnly, type CellState, type MatrixPerm } from './useGrantMatrix'

const props = defineProps<{
  perm: MatrixPerm
  cell: CellState
  offer: ScopeOffer
  /** The matrix's scope picker is open for this cell. */
  editing: boolean
  label: string
}>()
const emit = defineEmits<{ (e: 'update', keys: string[]): void; (e: 'open'): void }>()
const { t } = useI18n()

type Choice = 'none' | 'all' | 'own' | 'custom'
const summary = computed(() => cellSummary(props.cell.keys, props.perm.action))
const choice = computed<Choice>(() => (props.editing ? 'custom' : summary.value.choice))
const options = computed(() => {
  const refs = summary.value.refs
  const all: Array<[Choice, boolean, string]> = [
    ['none', true, t('plugins.matrix.notGranted')],
    ['all', props.offer.all, t('plugins.matrix.scopeAll')],
    ['own', !!props.perm.scope?.own && props.offer.own, t('plugins.matrix.scopeOwn')],
    ['custom', !!props.offer.refs, refs ? t('plugins.matrix.scopeCount', { n: refs }) : t('plugins.matrix.scopeCustom')]
  ]
  const closed = (c: Choice) => isShrinkOnly(props.cell.lock) && c !== choice.value && c !== 'none'
  return all.filter(([c, offered]) => offered || c === choice.value).map(([c, , text]) => ({ value: c, label: text, disabled: closed(c) }))
})

function choose(c: Choice) {
  if (c === 'custom') emit('open')
  else emit('update', c === 'none' ? [] : cellOfScopes(props.perm.action, [c === 'all' ? ALL_SCOPE : OWN_SCOPE]))
}
</script>

<template>
  <div class="flex flex-col items-center gap-1">
    <SSelect size="sm" class="w-full min-w-[5.5rem]" :model-value="choice" :options="options" :disabled="cell.disabled" :aria-label="label" @update:model-value="choose($event as Choice)" />
    <SLink v-if="summary.choice === 'custom' && !cell.fixed" as="button" class="text-xs" data-testid="matrix-open-scope" @click="emit('open')">{{ t('plugins.matrix.scopeCount', { n: summary.refs }) }}</SLink>
  </div>
</template>
