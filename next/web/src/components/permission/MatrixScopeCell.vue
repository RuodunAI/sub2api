<script setup lang="ts">
// One scoped cell of PermissionMatrix: a select naming the cell's choice
// (none / all / own / chosen…) as far as `offer` allows, and, while values
// are chosen, a "N chosen" button. "Chosen…" opens the matrix's ScopePicker
// (`open`); the other choices emit the cell's new keys (`update`). Under a
// shrink-only lock nothing but the current choice and none is open.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SSelect } from '@sub2api/ui'
import type { ScopeOffer } from '@/authz/scopeChoice'
import { scopesOfCell } from '@/authz/pluginGrants'
import type { MatrixPerm } from './useGrantMatrix'

const props = defineProps<{
  perm: MatrixPerm
  /** The keys the cell shows. */
  keys: string[]
  offer: ScopeOffer
  /** The cell may only take away (isShrinkOnly). */
  shrinkOnly: boolean
  disabled: boolean
  /** Frozen outright: the "N chosen" button does not open the picker. */
  fixed: boolean
  label: string
}>()
const emit = defineEmits<{ (e: 'update', keys: string[]): void; (e: 'open'): void }>()
const { t } = useI18n()

type Choice = 'none' | 'all' | 'own' | 'custom'
const scopes = computed(() => scopesOfCell(props.keys, props.perm.action))
/** The chosen plugin values ("<resource>/<value>"). */
const refs = computed(() => scopes.value.filter((s) => s.ref).length)
const choice = computed<Choice>(() => {
  if (!scopes.value.length) return 'none'
  if (scopes.value.some((s) => s.all)) return 'all'
  return scopes.value.every((s) => s.own) ? 'own' : 'custom'
})
const options = computed(() => {
  const all: Array<[Choice, boolean, string]> = [
    ['none', true, t('plugins.matrix.notGranted')],
    ['all', props.offer.all, t('plugins.matrix.scopeAll')],
    ['own', !!props.perm.scope?.own && props.offer.own, t('plugins.matrix.scopeOwn')],
    ['custom', !!props.offer.refs, refs.value ? t('plugins.matrix.scopeCount', { n: refs.value }) : t('plugins.matrix.scopeCustom')]
  ]
  const closed = (c: Choice) => props.shrinkOnly && c !== choice.value && c !== 'none'
  return all.filter(([c, offered]) => offered || c === choice.value).map(([c, , text]) => ({ value: c, label: text, disabled: closed(c) }))
})

function choose(c: Choice) {
  const a = props.perm.action
  if (c === 'custom') emit('open')
  else emit('update', c === 'none' ? [] : [c === 'all' ? a : `${a}:own`])
}
</script>

<template>
  <div class="flex flex-col items-center gap-1">
    <SSelect size="sm" class="w-full min-w-[5.5rem]" :model-value="choice" :options="options" :disabled="disabled" :aria-label="label" @update:model-value="choose($event as Choice)" />
    <button
      v-if="choice === 'custom' && !fixed"
      type="button"
      class="text-xs text-primary-600 hover:underline dark:text-primary-400"
      data-testid="matrix-open-scope"
      @click="emit('open')"
    >{{ t('plugins.matrix.scopeCount', { n: refs }) }}</button>
  </div>
</template>
