<script setup lang="ts">
// List view of 模型的协议限定 (DESIGN-2026-11-17-PROTOCOL-BINDING §5.1), laid
// out like ModelLimitsEditor: a card per set of models sharing the same
// protocols, plus an optional one for the other models. The cards are the
// entries that get saved (models with the same protocols form one), so
// giving two cards the same protocols joins them. A model is in one card
// only: adding it to a card takes it out of the one it was in.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SField, SHint, toast } from '@sub2api/ui'
import type { ModelProtocolRule } from '@/api/types'
import ModelTagSelect from '@/components/models/ModelTagSelect.vue'
import { modelProtocolRules, REST, toggleColumn, withEntryModels, withModels, withoutModels, type ProtocolColumn } from './protocolBinding'
import ProtocolChips from './ProtocolChips.vue'
import type { ProtocolEditor } from './useProtocolEdit'

const props = defineProps<{
  editor: ProtocolEditor
  /** Models offered by the model inputs. */
  options: string[]
  /** Messages of the server's errors by saved entry. */
  errors?: Record<number, string[]>
  disabled?: boolean
}>()
const { t } = useI18n()
const { columns, rules, defaults } = props.editor

const entries = computed(() => modelProtocolRules(rules.value, columns.value))
const isRest = (e: ModelProtocolRule) => e.models[0] === REST
const invalidModel = (model: string) => toast(t('accounts.modelInvalid', { model }), 'warning')

const setModels = (e: ModelProtocolRule, models: string[]) => (rules.value = withEntryModels(rules.value, e, models))
const setProtocols = (e: ModelProtocolRule, protocols: string[]) => (rules.value = withModels(rules.value, e.models, protocols))
const toggle = (e: ModelProtocolRule, c: ProtocolColumn) => setProtocols(e, toggleColumn(e.protocols, c))
const remove = (e: ModelProtocolRule) => (rules.value = withoutModels(rules.value, e.models))
/** A new entry starts from the default protocols. */
const add = (models: string[]) => (rules.value = withModels(rules.value, models, defaults.value))
</script>

<template>
  <div class="space-y-3" data-testid="protocol-list">
    <SHint v-if="!entries.length" size="xs" data-testid="protocol-list-empty">{{ t('accounts.protocols.rules.empty') }}</SHint>
    <div v-for="(e, i) in entries" :key="e.models.join(',')" class="space-y-2 rounded-lg border border-line p-3" :data-rest="isRest(e) || undefined" data-testid="protocol-entry">
      <SField :label="t('accounts.protocols.rules.models')">
        <p v-if="isRest(e)" class="text-sm font-medium text-fg">{{ t('accounts.protocols.rules.rest') }}<SHint inline size="xs" class="ml-2">{{ t('accounts.protocols.rules.restHint') }}</SHint></p>
        <ModelTagSelect v-else :model-value="e.models" :options="options" :placeholder="t('accounts.modelLimits.placeholder')" :readonly="disabled" @update:model-value="setModels(e, $event)" @invalid="invalidModel" />
      </SField>
      <SField :label="t('accounts.protocols.rules.protocols')">
        <ProtocolChips :columns="columns" :ids="e.protocols" :disabled="disabled" @toggle="toggle(e, $event)" @remove="setProtocols(e, e.protocols.filter(id => id !== $event))" />
      </SField>
      <SHint v-for="msg in errors?.[i]" :key="msg" tone="danger" size="xs">{{ msg }}</SHint>
      <SButton size="sm" variant="ghost" tone="danger" :disabled="disabled" data-testid="protocol-entry-remove" @click="remove(e)">{{ t('accounts.protocols.rules.removeEntry') }}</SButton>
    </div>
    <div class="space-y-2 rounded-lg border border-dashed border-line p-3" data-testid="protocol-entry-add">
      <SField :label="t('accounts.protocols.rules.add')">
        <ModelTagSelect :model-value="[]" :options="options" :placeholder="t('accounts.protocols.rules.addPlaceholder')" :readonly="disabled" @update:model-value="add" @invalid="invalidModel" />
      </SField>
      <SButton v-if="!(REST in rules)" size="sm" :disabled="disabled" data-testid="protocol-entry-add-rest" @click="add([REST])">{{ t('accounts.protocols.rules.addRest') }}</SButton>
    </div>
  </div>
</template>
