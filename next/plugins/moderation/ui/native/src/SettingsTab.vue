<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { isApiError } from '@sub2api/host'
import { SAlert, SButton, SCode, SForm, SHint, SInput, SSecretInput, SSegmented, SSettingRow, SSettingsGroup, SSpinner, SSwitch, useForm } from '@sub2api/ui'
import DurationInput from './DurationInput.vue'
import JevQuestionsEditor from './JevQuestionsEditor.vue'
import JevQuestionsView from './JevQuestionsView.vue'
import { copyQuestions } from './questions'
import VerdictCacheRow from './VerdictCacheRow.vue'
import { BLOCK_FORMATS, blockResponse, type BlockFormat } from './blockPreview'
import { enumOptions, joinList } from './format'
import { applyErrorsOrToast, type FieldErrors } from './forms'
import { mergeSettings, type PageValues } from './settingsMerge'
import { CACHE_DAYS_KEY, currentSettings, displayValues, pageKeys, sectionsOf, sentValue, settingDefault, settingRules, settingsKey, specOf, type SettingSection } from './settingsForm'
import type { SectionKey } from './nav'
import { useAsync } from './useAsync'
import { listsImportPending, useStatus } from './status'
import { fetchDefaults, fetchPluginSettings, savePluginSettings, useModHost } from './host'
import type { JevQuestions } from './types'

// One page of the moderation settings (CONTRACTS §72.15; the plugin page is
// where they are edited, ui.settings mode native, §75.5): the sections of
// settingsForm.ts SECTIONS whose page is this one (nav.ts), each key edited
// by the control of its kind, plus the block response preview per API
// format, the verdict cache row and the Jev default question set. useForm
// checks the rules of settingsForm.ts while typing; the plugin's
// ValidateSettings is the final check.
//
// The stored values are brought to the current keys (settingsForm.ts
// currentSettings): retired keys are shown folded into their new keys and
// any save drops the keys the plugin no longer accepts and the 0.1 API key.
// The user lists are the locked entries of the watch list (userLock.ts).
// Until the status says the one-time import of the lists of the stored
// settings is done, the page does not save (a save would drop them).
// Under the cache days, moderation:manage users can empty the verdict cache
// of every node (VerdictCacheRow).
//
// A save re-reads the stored values and writes back only the keys of this
// page edited since it loaded (settingsMerge.ts), so a change made elsewhere
// meanwhile - on another settings page kept open, too - is kept, also to a
// key of this page left alone (the core settings API has no version to
// compare). A key someone else changed that this page changed too is
// confirmed first, then this page's value wins. A number left empty is not
// stored, so its default applies.
const props = defineProps<{ page: SectionKey }>()
const host = useModHost()
const t = host.t

const sections = computed(() => sectionsOf(props.page))
/** The keys of this page (errors on other keys are toasted; the other pages' keys go back as stored). */
const keys = computed(() => pageKeys(props.page))

/** This page's values as shown. */
const values = ref<Record<string, unknown>>({})
const serverErrors = ref<FieldErrors>({})
const form = useForm({ model: () => values.value, update: (v) => (values.value = v), rules: () => settingRules(keys.value, t), serverErrors: () => serverErrors.value })
const saving = ref(false)
/** The status (loaded by the dashboard) says the lists are not imported yet: no save. */
const status = useStatus()
const listsPending = computed(() => listsImportPending(status.data.value))

/** The stored values when the page loaded (currentSettings), and this page's values then (what a save compares with). */
let loaded: Record<string, unknown> = {}
/** The stored settings keep the verdict cache off (verdict_cache_days = 0). */
const cacheOff = ref(false)
const initial = ref<PageValues>({})

const { data, loading, error, reload } = useAsync(() => Promise.all([fetchPluginSettings(), fetchDefaults()]), () => t('settingsTab.loadFailed'))
const defaults = computed(() => data.value?.[1] ?? null)

watch(data, (v) => {
  if (!v) return
  const stored = currentSettings(v[0].values || {})
  values.value = displayValues(stored, keys.value)
  serverErrors.value = {}
  form.clearValidate()
  loaded = stored
  cacheOff.value = stored[CACHE_DAYS_KEY] === 0
  initial.value = pageValues()
})
onMounted(reload)

/** The keys of this page as sent; undefined = removed (its default applies). */
const pageValues = (): PageValues => Object.fromEntries(keys.value.map((k) => [k, sentValue(k, values.value[k])]))
const dirty = computed(() => !!data.value && JSON.stringify(pageValues()) !== JSON.stringify(initial.value))
/** reload for the header button; dirty for leaving the page (ModerationDashboard). */
defineExpose({ reload, dirty })

const label = (k: string) => t(`settingsTab.field.${k}.label`)
const help = (k: string) => (specOf(k).help ? t(`settingsTab.field.${k}.help`) : undefined)
const sectionDescription = (g: SettingSection) => (g.description ? t(`settingsTab.section.${g.id}.description`) : undefined)
const options = (k: string) => enumOptions(`settingsTab.option.${k}`, specOf(k).options ?? [])
const unit = (k: string) => {
  const u = specOf(k).unit
  return u ? t(`settingsTab.unit.${u}`) : undefined
}

/** Called by the form's submit once its rules passed (the plugin checks again). */
async function save() {
  if (listsPending.value) return
  serverErrors.value = {}
  const current = pageValues()
  saving.value = true
  try {
    const latest = await fetchPluginSettings()
    const stored = latest.values || {}
    const m = mergeSettings(keys.value, loaded, currentSettings(stored), initial.value, current)
    // nothing edited still saves what currentSettings drops or folds
    if (JSON.stringify(m.values) === JSON.stringify(stored)) {
      host.toast(t('settingsTab.noChanges'), 'info')
      return
    }
    if (m.conflicts.length) {
      const names = joinList(m.conflicts.map(label))
      if (!(await host.confirm({ message: t('settingsTab.conflictConfirm', { keys: names }), confirmText: t('settingsTab.overwrite') }))) return
    }
    await savePluginSettings(m.values)
    host.toast(t('saved'), 'success')
    await reload()
  } catch (e) {
    // The first error of a key is shown at its row; others are toasted. A
    // kept secret the core asks to re-enter (§75.1) says so in the plugin's
    // words.
    const errs: FieldErrors = {}
    applyErrorsOrToast(errs, e, keys.value, settingsKey)
    if (isApiError(e)) {
      for (const [path, code] of Object.entries(e.fieldCodes)) if (code === 'reenter') errs[settingsKey(path)] = t('settingsTab.reenter')
    }
    serverErrors.value = errs
  } finally {
    saving.value = false
  }
}

const format = ref<BlockFormat>('openai')
const formatOptions = computed(() => enumOptions('settingsTab.format', BLOCK_FORMATS))
/** The block response as sent, empty inputs taking their defaults. */
const preview = computed(() =>
  blockResponse(format.value, Number(values.value.block_status || settingDefault('block_status')), String(values.value.block_message || settingDefault('block_message')))
)

const jev = computed(() => (values.value.jev_default_set as JevQuestions | null | undefined) ?? null)
const jevMode = computed({
  get: () => (jev.value ? 'custom' : 'builtin'),
  set: (m: string) => form.set('jev_default_set', m === 'custom' && defaults.value ? copyQuestions(defaults.value.builtin) : null)
})
const jevOptions = computed(() => [
  { value: 'builtin', label: t('settingsTab.jevBuiltin', { v: defaults.value?.builtin_version || '' }) },
  { value: 'custom', label: t('settingsTab.jevCustom') }
])
</script>

<template>
  <div>
    <SSpinner v-if="loading && !data" block />
    <SAlert v-else-if="error" tone="danger">{{ error }}</SAlert>
    <SForm v-else :form="form" @submit="save">
      <div class="mod-form mod-form-page">
        <SAlert v-if="listsPending" tone="warning">{{ t('settingsTab.listsPending') }}</SAlert>
        <SSettingsGroup v-for="g in sections" :key="g.id" :title="t(`settingsTab.section.${g.id}.title`)" :description="sectionDescription(g)">
          <template v-for="k in g.keys" :key="k">
            <SSettingRow :prop="k" :label="label(k)" :description="help(k)" :stacked="['url', 'secret', 'jev'].includes(specOf(k).kind)" :data-testid="`mod-setting-${k}`">
              <template v-if="specOf(k).details" #details>{{ t(`settingsTab.field.${k}.details`) }}</template>
              <SSegmented v-if="specOf(k).kind === 'enum'" v-bind="form.bind(k)" :options="options(k)" size="sm" />
              <SSwitch v-else-if="specOf(k).kind === 'switch'" :model-value="!!values[k]" @update:model-value="form.set(k, $event)" />
              <DurationInput v-else-if="specOf(k).kind === 'duration'" v-bind="form.bind(k)" />
              <SInput
                v-else-if="specOf(k).kind === 'integer'"
                v-bind="form.bind(k)"
                class="mod-setting-control"
                type="number"
                :min="specOf(k).min"
                :max="specOf(k).max"
                :step="1"
                :suffix="unit(k)"
              />
              <SInput v-else-if="specOf(k).kind === 'text'" v-bind="form.bind(k)" class="mod-setting-control" :maxlength="specOf(k).maxLength" />
              <SSecretInput
                v-else-if="specOf(k).kind === 'url' || specOf(k).kind === 'secret'"
                v-bind="form.bind(k)"
                class="mod-grow"
                :plain="specOf(k).kind === 'url'"
                :maxlength="specOf(k).maxLength"
                :placeholder="specOf(k).kind === 'url' ? 'https://' : ''"
                clearable
              />
              <div v-else-if="specOf(k).kind === 'jev'" class="mod-form mod-setting-wide">
                <SSegmented v-model="jevMode" :options="jevOptions" size="sm" />
                <JevQuestionsEditor v-if="jev" :model-value="jev" @update:model-value="form.set(k, $event)">
                  <template #toolbar>
                    <SButton size="sm" @click="jevMode = 'custom'">{{ t('settingsTab.jevCopyBuiltin') }}</SButton>
                  </template>
                </JevQuestionsEditor>
                <JevQuestionsView v-else-if="defaults" :questions="defaults.builtin" />
              </div>
            </SSettingRow>
            <VerdictCacheRow v-if="k === CACHE_DAYS_KEY" :off="cacheOff" />
            <SSettingRow v-if="k === 'block_message'" :label="t('settingsTab.preview')" stacked>
              <div class="mod-form mod-setting-wide">
                <SSegmented v-model="format" :options="formatOptions" size="sm" />
                <SCode :text="`HTTP ${preview.status}\n${preview.body}`" class="mod-text-block" />
                <SHint size="xs">{{ t('settingsTab.streamNote') }}</SHint>
              </div>
            </SSettingRow>
          </template>
        </SSettingsGroup>

        <div class="mod-editor-footer">
          <SHint v-if="dirty" size="xs" tone="warning" class="mod-mr-auto">{{ t('settingsTab.unsaved') }}</SHint>
          <SButton type="submit" variant="primary" :loading="saving" :disabled="listsPending">{{ t('save') }}</SButton>
        </div>
      </div>
    </SForm>
  </div>
</template>
