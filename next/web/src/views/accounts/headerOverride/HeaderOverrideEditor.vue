<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SDropdown, SField, SInput, STextarea, toast } from '@sub2api/ui'
import OverrideDialog from '../controls/OverrideDialog.vue'
import { useJsonView } from '../controls/useJsonView'
import {
  describeRow,
  formatOverride,
  MAX_ENTRIES,
  newRow,
  parseOverride,
  QUICK_INSERTS,
  readRows,
  rowProblems,
  writeRows,
  type HeaderOverride,
  type HeaderRow
} from './model'

// The request header override editor (CONTRACTS §108): one row per header
// with a quick insert of the placeholders beside the value and the meaning
// of the row under it, or the whole object as JSON (new-api's channel
// header_override, pasted as is). It edits a copy: Apply hands the override
// (null when empty) back to the account form; Cancel drops the copy.
const open = defineModel<boolean>('open', { required: true })
const props = defineProps<{ value: HeaderOverride | null }>()
const emit = defineEmits<{ (e: 'apply', value: HeaderOverride | null): void }>()
const { t } = useI18n()
const P = 'accounts.headerOverride'
const JSON_PLACEHOLDER = JSON.stringify({ 'User-Agent': 'my-client/1.0', 'x-trace-id': '{client_header:x-request-id}' }, null, 2)

const VIEWS = ['visual', 'json'] as const
type View = (typeof VIEWS)[number]
const rows = ref<HeaderRow[]>([])

const problems = computed(() => rowProblems(rows.value))
const insertActions = computed(() => QUICK_INSERTS.map(q => ({ key: q.key, label: t(`${P}.insert.${q.key}`) })))
/** A row's name in labels: its header, else its number. */
const rowName = (r: HeaderRow, i: number) => r.key.trim() || i + 1

function load(value: HeaderOverride | null) {
  rows.value = readRows(value)
  if (!rows.value.length) rows.value = [newRow()]
}

/** "Row n has an error: …" for the first row that cannot be saved, '' when every row can. */
function rowsProblem(): string {
  const bad = problems.value.findIndex(Boolean)
  return bad < 0 ? '' : t(`${P}.invalid`, { n: bad + 1, problem: t(`${P}.problems.${problems.value[bad]}`) })
}

// JSON cannot hold the rows while they have problems (a duplicate name would be dropped).
const { view, jsonText, jsonError, editJson, setView, jsonInvalid, reset } = useJsonView({
  ns: P,
  views: VIEWS,
  parse: parseOverride,
  load,
  format: () => formatOverride(writeRows(rows.value)),
  blockJson: rowsProblem
})

watch(open, now => {
  if (!now) return
  reset()
  load(props.value)
})

const insert = (r: HeaderRow, key: string) => Object.assign(r, QUICK_INSERTS.find(q => q.key === key)?.apply(r))
const remove = (i: number) => rows.value.splice(i, 1)
function add() {
  if (rows.value.length >= MAX_ENTRIES) return toast(t(`${P}.max`, { n: MAX_ENTRIES }), 'warning')
  rows.value.push(newRow())
}

function apply() {
  if (jsonInvalid()) return
  const problem = rowsProblem()
  if (problem) {
    view.value = 'visual'
    return toast(problem, 'error')
  }
  emit('apply', writeRows(rows.value))
  open.value = false
}
</script>

<template>
  <OverrideDialog v-model:open="open" :ns="P" testid="ho" :views="VIEWS" :view="view" @view="setView($event as View)" @apply="apply">
    <SField v-if="view === 'json'" :hint="t(`${P}.json.hint`)" :error="jsonError">
      <STextarea
        :model-value="jsonText"
        :rows="16"
        mono
        size="sm"
        :placeholder="JSON_PLACEHOLDER"
        data-testid="ho-json"
        @update:model-value="editJson"
      />
    </SField>

    <div v-else class="space-y-2" data-testid="ho-rows">
      <div class="grid grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_4.5rem] gap-2 text-xs text-fg-muted max-md:hidden" aria-hidden="true">
        <span>{{ t(`${P}.key`) }}</span>
        <span>{{ t(`${P}.value`) }}</span>
      </div>
      <div v-for="(r, i) in rows" :key="r.id" data-testid="ho-row">
        <div
          class="grid grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_4.5rem] items-center gap-2 max-md:grid-cols-[minmax(0,1fr)_4.5rem] max-md:rounded-control max-md:border max-md:border-line max-md:p-2"
        >
          <SInput
            v-model="r.key"
            size="sm"
            mono
            :error="!!problems[i]"
            :placeholder="t(`${P}.keyPlaceholder`)"
            :aria-label="`${t(`${P}.key`)} ${i + 1}`"
            data-testid="ho-key"
          />
          <SInput
            v-model="r.value"
            size="sm"
            mono
            class="max-md:col-start-1"
            :placeholder="t(`${P}.valuePlaceholder`)"
            :aria-label="`${t(`${P}.value`)} ${i + 1}`"
            data-testid="ho-value"
          />
          <div class="flex items-center max-md:col-start-2 max-md:row-start-1">
            <SDropdown :actions="insertActions" width="14rem" @select="insert(r, $event)">
              <template #header>{{ t(`${P}.insert.title`) }}</template>
              <template #trigger="{ toggle, attrs }">
                <SButton
                  size="sm"
                  variant="ghost"
                  icon="code"
                  icon-only
                  :aria-label="t(`${P}.insert.titleOf`, { name: rowName(r, i) })"
                  :title="t(`${P}.insert.title`)"
                  data-testid="ho-insert"
                  v-bind="attrs"
                  @click.stop="toggle"
                />
              </template>
            </SDropdown>
            <SButton
              size="sm"
              variant="ghost"
              icon="x"
              icon-only
              :aria-label="t(`${P}.removeOf`, { name: rowName(r, i) })"
              :title="t(`${P}.remove`)"
              data-testid="ho-remove"
              @click="remove(i)"
            />
          </div>
        </div>
        <p
          v-if="r.key.trim() || r.value"
          class="mt-1 truncate text-xs"
          :class="problems[i] ? 'text-danger-600 dark:text-danger-400' : 'text-fg-muted'"
          data-testid="ho-row-hint"
        >
          {{ problems[i] ? t(`${P}.problems.${problems[i]}`) : describeRow(r, t) }}
        </p>
      </div>
      <SButton size="sm" icon="plus" :disabled="rows.length >= MAX_ENTRIES" data-testid="ho-add" @click="add">{{ t(`${P}.add`) }}</SButton>
    </div>
  </OverrideDialog>
</template>
