<script setup lang="ts">
// The scopes of one scoped action (CONTRACTS §21), stacked: every row, the
// rows the holder created, chosen groups, chosen accounts or proxies, and
// accounts within a chosen group. "All" includes the rest (greyed out). Only
// what the editor's own scopes imply is offered (`offer`); granted scopes
// beyond it can only be removed. Ids show by name through the lookup, a
// deleted one as "#12 (deleted)".
// `target` says whose: a core resource's (account, proxy), or a plugin
// action's, which has none of the core rows: its scopes are own (when the
// manifest allows it) and "<resource>/<value>" refs, a static enum's values
// as toggles, a plugin resource's picked from its scope candidates
// (EntityPicker of the pscope kind), shown by name; the permission's label
// says what is picked.
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SCheckbox, SChip, SHint } from '@sub2api/ui'
import EntityPicker from '@/components/EntityPicker.vue'
import { pluginScopeKind } from '@/components/entity/entities'
import { useEntityNames } from '@/components/entity/lookup'
import type { LText, PluginScopeDecl } from '@/api/types'
import { lt } from '@/i18n'
import { scopeSuffix, takesRowIds, type Scope, type ScopeResource } from '@/authz/implies'
import { choiceOf, scopesOf, type RefOffer, type ScopeChoice, type ScopeOffer } from '@/authz/scopeChoice'
import { useScopeLabels } from './scopeLabels'
import ScopeChipRow from './ScopeChipRow.vue'

/** Whose scopes: a core resource's, or a plugin action's (the plugin, its local key, the manifest's scope and the permission's label). */
export type ScopeTarget =
  | { kind: 'core'; resource: ScopeResource }
  | { kind: 'plugin'; key: string; local: string; scope?: PluginScopeDecl; label?: LText }

const props = defineProps<{
  action: string
  target: ScopeTarget
  scopes: Scope[]
  offer: ScopeOffer
  disabled?: boolean
}>()
const emit = defineEmits<{ (e: 'update:scopes', v: Scope[]): void }>()
const { t } = useI18n()
const { rowLabel: label } = useScopeLabels()
const names = useEntityNames()

const choice = computed(() => choiceOf(props.scopes))
/** Everything but "all" waits while every row is chosen. */
const narrowOff = computed(() => props.disabled || choice.value.all)
const core = computed(() => (props.target.kind === 'core' ? props.target.resource : null))
const plugin = computed(() => (props.target.kind === 'plugin' ? props.target : null))
const grouped = computed(() => core.value === 'account')
/** Rows by id (account/X, proxy/X, group/G/account/X); not for account:create. */
const rows = computed(() => takesRowIds(props.action))
/** A plugin action offers own only where its manifest allows it. */
const ownOffered = computed(() => props.offer.own && (!plugin.value || !!plugin.value.scope?.own))

function set(patch: Partial<ScopeChoice>) {
  emit('update:scopes', scopesOf({ ...choice.value, ...patch }))
}
const added = (list: number[], id: string) => (id && !list.includes(Number(id)) ? [...list, Number(id)] : list)

// <resource>/<value> of a plugin action.
const resourceName = computed(() => plugin.value?.scope?.resource ?? '')
const enumValues = computed(() => plugin.value?.scope?.values)
const refKind = computed(() => pluginScopeKind(plugin.value?.key ?? '', plugin.value?.local ?? ''))
const refOf = (value: string) => `${resourceName.value}/${value}`
const valueOf = (ref: string) => ref.slice(resourceName.value.length + 1)
/** The permission the refs belong to, by name: the row and the picker say what is chosen. */
const permName = computed(() => lt(plugin.value?.label) || plugin.value?.local || '')
/** What the refs row offers: an enum's values are a fixed list (all of them to a holder of every value). */
const refOffer = computed<RefOffer>(() => {
  const values = enumValues.value?.map((v) => refOf(v.key))
  return values && props.offer.refs === 'any' ? values : props.offer.refs
})
function refLabel(ref: string): string {
  const value = valueOf(ref)
  const item = enumValues.value?.find((v) => v.key === value)
  return item ? lt(item.label) || value : names.label(refKind.value, value)
}
function addRef(value: string) {
  if (value && !choice.value.refs.includes(refOf(value))) set({ refs: [...choice.value.refs, refOf(value)] })
}

// group/<g>/account/<id>: the group is picked first, then an account preferring it.
const pairGroup = ref('')
function addPair(account: string) {
  const g = Number(pairGroup.value)
  if (!g || !account || choice.value.pairs.some(([pg, id]) => pg === g && id === Number(account))) return
  set({ pairs: [...choice.value.pairs, [g, Number(account)]] })
}
</script>

<template>
  <div class="space-y-2 rounded-lg bg-surface-2 px-3 py-2 text-sm" :data-testid="`scope-${action}`">
    <div class="flex flex-wrap items-center gap-x-4 gap-y-1">
      <SCheckbox
        v-if="offer.all || choice.all"
        size="xs"
        :model-value="choice.all"
        :disabled="disabled"
        :label="t('roles.scope.all')"
        data-testid="scope-all"
        @update:model-value="set({ all: !!$event })"
      />
      <SCheckbox
        v-if="ownOffered || choice.own"
        size="xs"
        :model-value="choice.own || choice.all"
        :disabled="narrowOff"
        :label="t('roles.scope.own')"
        data-testid="scope-own"
        @update:model-value="set({ own: !!$event })"
      />
      <SHint v-if="choice.all" inline size="xs">{{ t('roles.scope.allHint') }}</SHint>
      <SHint v-else-if="!scopes.length" inline size="xs" class="text-warning-600 dark:text-warning-400">{{ t('roles.scope.none') }}</SHint>
    </div>

    <!-- group/<g> -->
    <ScopeChipRow
      v-if="grouped && (offer.groups || choice.groups.length)"
      :label="t('roles.scope.groups')"
      :offer="offer.groups"
      :held="choice.groups"
      :label-of="(g) => label('group', g)"
      kind="group"
      :disabled="narrowOff"
      :dimmed="choice.all"
      data-testid="scope-groups"
      @update="set({ groups: $event })"
      @pick="set({ groups: added(choice.groups, $event) })"
    />

    <!-- account/<id>, proxy/<id> -->
    <ScopeChipRow
      v-if="core && rows && (offer.ids || choice.ids.length)"
      :label="t(grouped ? 'roles.scope.accounts' : 'roles.scope.proxies')"
      :offer="offer.ids"
      :held="choice.ids"
      :label-of="(id) => label(core!, id)"
      :kind="core"
      :disabled="narrowOff"
      :dimmed="choice.all"
      data-testid="scope-ids"
      @update="set({ ids: $event })"
      @pick="set({ ids: added(choice.ids, $event) })"
    />

    <!-- group/<g>/account/<id> -->
    <div v-if="grouped && rows && (offer.groups || choice.pairs.length)" class="flex flex-wrap items-center gap-1.5" :class="choice.all ? 'opacity-60' : ''" data-testid="scope-pairs">
      <SHint inline size="xs" class="w-28 shrink-0">{{ t('roles.scope.pairs') }}</SHint>
      <SChip
        v-for="[g, id] in choice.pairs"
        :key="`${g}/${id}`"
        size="xs"
        tone="primary"
        :closable="!narrowOff"
        @close="set({ pairs: choice.pairs.filter(([pg, pid]) => pg !== g || pid !== id) })"
      >{{ label('group', g) }} / {{ label('account', id) }}</SChip>
      <template v-if="offer.groups && !narrowOff">
        <EntityPicker
          v-if="offer.groups === 'any'"
          v-model:id="pairGroup"
          class="min-w-36"
          kind="group"
          size="sm"
          :placeholder="t('roles.scope.pairGroup')"
          data-testid="scope-pair-group"
        />
        <template v-else>
          <SChip v-for="g in offer.groups" :key="g" size="xs" :pressed="pairGroup === String(g)" @update:pressed="pairGroup = $event ? String(g) : ''">{{ label('group', g) }}</SChip>
        </template>
        <EntityPicker
          v-if="pairGroup"
          class="min-w-36 flex-1"
          kind="account"
          size="sm"
          :placeholder="t('roles.scope.pairAccount')"
          :scope="{ prefer_group_id: pairGroup }"
          data-testid="scope-pair-account"
          @update:id="addPair"
        />
      </template>
    </div>

    <!-- <resource>/<value>: a plugin resource or a static enum -->
    <ScopeChipRow
      v-if="plugin && (refOffer || choice.refs.length)"
      :label="t(enumValues ? 'roles.scope.values' : 'roles.scope.items')"
      :offer="refOffer"
      :held="choice.refs"
      :label-of="refLabel"
      :title-of="(r) => r"
      :kind="refKind"
      :disabled="narrowOff"
      :dimmed="choice.all"
      :exclude="choice.refs.map(valueOf)"
      :placeholder="t('roles.scope.itemsOf', { perm: permName })"
      picker-testid="scope-ref-picker"
      data-testid="scope-refs"
      @update="set({ refs: $event })"
      @pick="addRef"
    />

    <!-- held, not offered: group/*, group/*/account/<id> -->
    <div v-if="choice.other.length" class="flex flex-wrap items-center gap-1.5" data-testid="scope-other">
      <SHint inline size="xs" class="w-28 shrink-0">{{ t('roles.scope.other') }}</SHint>
      <SChip
        v-for="s in choice.other"
        :key="scopeSuffix(action, s)"
        size="xs"
        :closable="!narrowOff"
        class="font-mono"
        @close="set({ other: choice.other.filter((o) => o !== s) })"
      >{{ scopeSuffix(action, s) }}</SChip>
    </div>
  </div>
</template>
