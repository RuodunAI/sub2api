<script setup lang="ts" generic="T extends string | number">
// One row of ScopePicker: a label, the chosen values as chips and a picker
// to add more. A holder of every value (`offer` 'any') sees the chosen ones
// as closable chips and picks others (EntityPicker of `kind`, emitting
// `pick` with the picked id); a restricted one toggles the values it may
// offer, with those held beyond them shown to take away only. Attrs (the
// row's data-testid) go to the row.
import { SChip, SHint } from '@sub2api/ui'
import EntityPicker from '@/components/EntityPicker.vue'
import type { EntityKind } from '@/components/entity/entities'
import type { Offer } from '@/authz/scopeChoice'

const props = defineProps<{
  label: string
  offer: Offer<T>
  held: T[]
  labelOf: (id: T) => string
  kind: EntityKind
  /** Every value is chosen (greyed) or the picker is read-only: nothing changes. */
  disabled: boolean
  /** Greyed out: "all" includes the rest. */
  dimmed?: boolean
  /** Ids the picker leaves out (the held ones, as the picker spells them). */
  exclude?: Array<string | number>
  placeholder?: string
  titleOf?: (id: T) => string
  pickerTestid?: string
}>()
const emit = defineEmits<{ (e: 'update', held: T[]): void; (e: 'pick', id: string): void }>()

/** The ids a fixed offer lists, with those granted beyond it (removable only). */
const listed = () => (Array.isArray(props.offer) ? [...new Set([...props.offer, ...props.held])] : props.held)
const offered = (id: T) => props.offer === 'any' || (Array.isArray(props.offer) && props.offer.includes(id))
const toggled = (id: T) => (props.held.includes(id) ? props.held.filter((x) => x !== id) : [...props.held, id])
</script>

<template>
  <div class="flex flex-wrap items-center gap-1.5" :class="dimmed ? 'opacity-60' : ''">
    <SHint inline size="xs" class="w-28 shrink-0">{{ label }}</SHint>
    <SChip
      v-for="id in listed()"
      :key="id"
      size="xs"
      :pressed="offer === 'any' ? undefined : held.includes(id)"
      :tone="offer === 'any' ? 'primary' : 'neutral'"
      :closable="offer === 'any' && !disabled"
      :disabled="disabled || (!offered(id) && !held.includes(id))"
      :title="titleOf?.(id)"
      @close="emit('update', held.filter((x) => x !== id))"
      @update:pressed="emit('update', toggled(id))"
    >{{ labelOf(id) }}</SChip>
    <EntityPicker
      v-if="offer === 'any' && !disabled"
      class="min-w-40 flex-1"
      :kind="kind"
      size="sm"
      :exclude="exclude ?? held"
      :placeholder="placeholder"
      :data-testid="pickerTestid"
      @update:id="emit('pick', $event)"
    />
  </div>
</template>
