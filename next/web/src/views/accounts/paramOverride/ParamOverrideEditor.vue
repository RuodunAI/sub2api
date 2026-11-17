<script setup lang="ts">
import { computed, nextTick, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SDropdown, SField, SIcon, SInput, STextarea, toast } from '@sub2api/ui'
import OverrideDialog from '../controls/OverrideDialog.vue'
import { useJsonView } from '../controls/useJsonView'
import OperationPane from './OperationPane.vue'
import OverridePreview from './OverridePreview.vue'
import { describeOperation, describeProblem, operationName } from './describe'
import {
  CONTEXT_KEYS,
  duplicateDraft,
  MAX_DESCRIPTION,
  MAX_OPERATIONS,
  MODES,
  moveDraft,
  newOperation,
  parseOverride,
  readOverride,
  removeDraft,
  TEMPLATES,
  validateOperation,
  writeOverride,
  type OperationDraft,
  type Override
} from './model'

// The request parameter override editor (CONTRACTS §84), in the layout of
// new-api's and of the dynamic weight items editor: the operations on the
// left (a dot per kind, a red dot on errors), the selected one on the right,
// the whole override as JSON behind the Visual / JSON switch, and a dry run
// against the server. It edits a copy: Apply hands the override back to the
// account form, which saves it with the account; Cancel drops the copy.
const open = defineModel<boolean>('open', { required: true })
const props = defineProps<{ value: Override | null; models: string[]; headerOverride?: Record<string, string> | null }>()
const emit = defineEmits<{ (e: 'apply', value: Override | null): void }>()
const { t } = useI18n()
const P = 'accounts.paramOverride'
const JSON_PLACEHOLDER = JSON.stringify({ operations: [{ mode: 'set', path: 'temperature', value: 0.7 }] })
const JSON_SHAPE = { shape: '{"operations": [...]}' }

const VIEWS = ['visual', 'json', 'preview'] as const
type View = (typeof VIEWS)[number]
const drafts = ref<OperationDraft[]>([])
const selectedId = ref(0)
const search = ref('')
const contextListId = `po-context-${useId()}`

const selectedIndex = computed(() => drafts.value.findIndex(d => d.id === selectedId.value))
const selected = computed(() => drafts.value[selectedIndex.value] ?? null)
const problems = computed(() => drafts.value.map(validateOperation))
const current = computed(() => writeOverride(drafts.value))

function load(value: Override | null, select = 0) {
  drafts.value = readOverride(value)
  selectedId.value = drafts.value[Math.min(select, drafts.value.length - 1)]?.id ?? 0
}

const { view, jsonText, jsonError, editJson, setView, jsonInvalid, reset } = useJsonView({
  ns: P,
  views: VIEWS,
  parse: parseOverride,
  load: value => load(value, Math.max(0, i())),
  format: () => (current.value ? JSON.stringify(current.value, null, 2) : '')
})

watch(open, now => {
  if (!now) return
  reset()
  search.value = ''
  load(props.value)
})

const nav = ref<HTMLElement>()
watch(selectedId, () =>
  nextTick(() => nav.value?.querySelector('[aria-current="true"]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' }))
)

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  return drafts.value
    .map((d, i) => ({ d, i }))
    .filter(({ d }) => !q || `${d.description} ${describeOperation(d, t)}`.toLowerCase().includes(q))
})
const dotClass = (d: OperationDraft) =>
  ({ body: 'bg-primary-500', string: 'bg-primary-300', header: 'bg-sky-500', control: 'bg-amber-500' })[MODES[d.mode]?.group ?? ''] ??
  'bg-fg-muted'

function setDrafts(next: OperationDraft[], select: number) {
  drafts.value = next
  selectedId.value = next[Math.max(0, Math.min(select, next.length - 1))]?.id ?? 0
}
function add(seed?: Record<string, unknown>) {
  if (drafts.value.length >= MAX_OPERATIONS) return toast(t(`${P}.max`, { n: MAX_OPERATIONS }), 'warning')
  setDrafts([...drafts.value, newOperation(seed)], drafts.value.length)
}
const templateActions = computed(() => TEMPLATES.map(x => ({ key: x.key, label: t(`${P}.templates.${x.key}`) })))
const addTemplate = (key: string) => {
  const tpl = TEMPLATES.find(x => x.key === key)
  if (tpl) add({ description: t(`${P}.templates.${key}`), ...tpl.op })
}
const i = () => selectedIndex.value
const move = (by: -1 | 1) => setDrafts(moveDraft(drafts.value, i(), by), i() + by)
const duplicate = () => setDrafts(duplicateDraft(drafts.value, i()), i() + 1)
const remove = () => setDrafts(removeDraft(drafts.value, i()), i())

function apply() {
  if (jsonInvalid()) return
  const bad = problems.value.findIndex(p => p.length)
  if (bad >= 0) {
    selectedId.value = drafts.value[bad].id
    view.value = 'visual'
    return toast(t(`${P}.invalid`, { n: bad + 1, problem: describeProblem(problems.value[bad][0], t) }), 'error')
  }
  emit('apply', current.value)
  open.value = false
}
</script>

<template>
  <OverrideDialog v-model:open="open" :ns="P" testid="po" :views="VIEWS" :view="view" @view="setView($event as View)" @apply="apply">
    <datalist :id="contextListId">
      <option v-for="k in CONTEXT_KEYS" :key="k" :value="k" />
    </datalist>

    <OverridePreview
      v-if="view === 'preview'"
      :override="current"
      :header-override="headerOverride ?? null"
      :model="models[0] || 'claude-sonnet-4-5'"
    />

    <SField v-else-if="view === 'json'" :hint="t(`${P}.json.hint`, JSON_SHAPE)" :error="jsonError">
      <STextarea
        :model-value="jsonText"
        :rows="18"
        mono
        size="sm"
        :placeholder="JSON_PLACEHOLDER"
        data-testid="po-json"
        @update:model-value="editJson"
      />
    </SField>

    <div v-else class="grid grid-cols-1 gap-4 md:grid-cols-[15rem_minmax(0,1fr)]">
      <nav ref="nav" class="flex min-w-0 flex-col gap-2 md:border-r md:border-line md:pr-3" :aria-label="t(`${P}.list`)">
        <SInput v-model="search" size="sm" icon="search" :placeholder="t(`${P}.search`)" :aria-label="t(`${P}.search`)" />
        <div class="flex gap-1 overflow-x-auto md:max-h-[24rem] md:flex-col md:overflow-y-auto" data-testid="po-nav">
          <button
            v-for="{ d, i: n } in filtered"
            :key="d.id"
            type="button"
            class="flex min-w-[10rem] shrink-0 items-center gap-2 rounded-lg border px-2 py-1.5 text-left text-sm md:min-w-0"
            :class="
              d.id === selectedId ? 'border-primary-400 bg-primary-50 dark:bg-primary-900/20' : 'border-transparent hover:bg-surface-2'
            "
            :aria-current="d.id === selectedId || undefined"
            @click="selectedId = d.id"
          >
            <span class="w-4 shrink-0 text-right text-xs text-fg-muted">{{ n + 1 }}</span>
            <span class="h-2 w-2 shrink-0 rounded-full" :class="dotClass(d)" aria-hidden="true" />
            <span class="min-w-0 flex-1 truncate">{{ operationName(d, t) }}</span>
            <SIcon v-if="d.conditions.length" name="filter" class="h-3 w-3 shrink-0 text-fg-muted" :title="t(`${P}.hasConditions`)" />
            <span
              v-if="problems[n].length"
              class="h-2 w-2 shrink-0 rounded-full bg-danger-500"
              role="img"
              :aria-label="t(`${P}.hasErrors`)"
            />
          </button>
          <p v-if="!filtered.length" class="p-2 text-xs text-fg-muted">{{ drafts.length ? t(`${P}.noMatch`) : t(`${P}.empty`) }}</p>
        </div>
        <div class="flex flex-wrap items-center gap-1">
          <SButton size="xs" variant="ghost" icon="plus" :disabled="drafts.length >= MAX_OPERATIONS" data-testid="po-add" @click="add()">{{
            t(`${P}.add`)
          }}</SButton>
          <SDropdown :actions="templateActions" align="start" width="16rem" @select="addTemplate">
            <SButton size="xs" variant="ghost" icon="list" :disabled="drafts.length >= MAX_OPERATIONS" data-testid="po-templates">{{
              t(`${P}.fromTemplate`)
            }}</SButton>
          </SDropdown>
        </div>
      </nav>

      <div v-if="selected" :key="selectedId" class="min-w-0 space-y-3">
        <div class="flex flex-wrap items-center gap-2">
          <SInput
            v-model="selected.description"
            class="min-w-0 flex-1"
            size="sm"
            :maxlength="MAX_DESCRIPTION"
            :placeholder="t(`${P}.description`)"
            :aria-label="t(`${P}.description`)"
            data-testid="po-description"
          />
          <SButton size="xs" variant="ghost" :disabled="selectedIndex === 0" @click="move(-1)">
            {{ t(`${P}.up`) }}
          </SButton>
          <SButton size="xs" variant="ghost" :disabled="selectedIndex >= drafts.length - 1" @click="move(1)">
            {{ t(`${P}.down`) }}
          </SButton>
          <SButton size="xs" variant="ghost" :disabled="drafts.length >= MAX_OPERATIONS" @click="duplicate">
            {{ t(`${P}.duplicate`) }}
          </SButton>
          <SButton size="xs" variant="ghost" tone="danger" data-testid="po-remove" @click="remove">
            {{ t(`${P}.remove`) }}
          </SButton>
        </div>
        <OperationPane v-model="drafts[selectedIndex]" :problems="problems[selectedIndex]" :context-list-id="contextListId" />
      </div>
      <div
        v-else
        class="flex flex-col items-center justify-center gap-3 rounded-xl border border-dashed border-line p-8 text-center text-sm text-fg-muted"
      >
        <p>{{ t(`${P}.emptyHint`) }}</p>
        <SButton size="sm" icon="plus" @click="add()">{{ t(`${P}.add`) }}</SButton>
      </div>
    </div>
  </OverrideDialog>
</template>
