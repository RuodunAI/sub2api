<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SAlert, SBadge, SButton, SHint, SInput, SSegmented, SSettingRow, SSettingsGroup, SSpinner, SSwitch } from '@sub2api/ui'
import SettingsSaveBar from '@/components/SettingsSaveBar.vue'
import { useSettingsForm } from '@/composables/useSettingsForm'
import { useAuthStore } from '@/stores/auth'
import { lineStatus, UPDATE_LINES, useUpdatesStore } from '@/stores/updates'

// Update source of both version lines (GET/PUT /system/update-source, served
// by the gateway). The secret is write-only: the server only says whether one
// is stored, and an empty field leaves it unchanged.
type Kind = 'github' | 'url'
interface Source { enabled: boolean; kind: Kind; repository: string; url: string; has_secret: boolean; default_repository: string }
interface Body { enabled: boolean; kind: Kind; repository: string; url: string; secret?: string }
interface Form { enabled: boolean; kind: Kind; repository: string; url: string; secret: string; clearSecret: boolean; hasSecret: boolean; defaultRepository: string }

const { t } = useI18n()
const auth = useAuthStore()
const updates = useUpdatesStore()
const canManage = computed(() => auth.has('settings:manage'))
const canCheck = computed(() => auth.has('system:update:read'))
/** The stored source, to tell when an edit drops its secret. */
const stored = ref<Partial<Source>>({})
const { form, loading, saving, dirty, errors, loadError, isLoaded, load, save, reset } = useSettingsForm<Form, Body>({
  path: '/system/update-source',
  defaults: { enabled: true, kind: 'github', repository: '', url: '', secret: '', clearSecret: false, hasSecret: false, defaultRepository: '' },
  inlineLoadError: true,
  fromServer: (r) => {
    const s = (stored.value = r as Partial<Source>)
    return { enabled: !!s.enabled, kind: s.kind === 'url' ? 'url' : 'github', repository: s.repository ?? '', url: s.url ?? '', secret: '', clearSecret: false, hasSecret: !!s.has_secret, defaultRepository: s.default_repository ?? '' }
  },
  toBody: (f) => {
    const body: Body = { enabled: f.enabled, kind: f.kind, repository: f.repository.trim(), url: f.url.trim() }
    if (f.secret) body.secret = f.secret
    else if (f.clearSecret) body.secret = ''
    return body
  }
})

const disabled = computed(() => !isLoaded.value || !canManage.value || saving.value)
const kinds = computed(() => (['github', 'url'] as const).map(value => ({ value, label: t(`coreUpdates.kinds.${value}`) })))
const sourceChanged = computed(() => form.kind !== stored.value.kind || form.repository.trim() !== stored.value.repository || form.url.trim() !== stored.value.url)
const secretDropped = computed(() => form.hasSecret && !form.secret && !form.clearSecret && sourceChanged.value)
/** The latest check in one line of text per outcome; null while there is none to show. */
const result = computed(() => {
  const check = updates.result
  if (updates.error) return { tone: 'danger' as const, lines: [updates.error] }
  if (!check) return null
  const statuses = UPDATE_LINES.map(line => lineStatus(check, line))
  if (!check.enabled || statuses.includes('unchecked')) return { tone: 'muted' as const, lines: [t(`coreUpdates.status.${check.enabled ? 'unchecked' : 'disabled'}`)] }
  const failed = UPDATE_LINES.filter((_, i) => statuses[i] === 'failed')
  if (failed.length) return { tone: 'danger' as const, lines: failed.map(line => `${t(`coreUpdates.lines.${line}`)}: ${check[line].error || t('coreUpdates.status.failed')}`) }
  return { tone: 'success' as const, lines: [t('coreUpdates.connected', { core: check.core.latest_version, gateway: check.gateway.latest_version })] }
})

function toggleClear() {
  form.clearSecret = !form.clearSecret
  form.secret = ''
}

async function submit() {
  if (!(await save())) return
  updates.invalidate()
  if (canCheck.value) void updates.check(true)
}

onMounted(() => {
  void load()
  if (canCheck.value && !updates.result) void updates.check()
})
</script>

<template>
  <SSettingsGroup :title="t('coreUpdates.sourceTitle')" :description="t('coreUpdates.sourceDescription')" data-testid="update-source-settings">
    <template #actions>
      <SButton v-if="!isLoaded && !loading" size="sm" icon="refresh" @click="load">{{ t('coreUpdates.retry') }}</SButton>
      <SettingsSaveBar v-else :dirty="dirty" :saving="saving" :can-manage="canManage" @save="submit" @reset="reset" />
    </template>
    <SSpinner v-if="loading && !isLoaded" block />
    <template v-else>
      <SAlert v-if="loadError" tone="danger" class="mt-4">{{ loadError }}</SAlert>
      <SSettingRow :label="t('coreUpdates.enabled')" :description="t('coreUpdates.enabledHint')">
        <SSwitch v-model="form.enabled" :disabled="disabled" :aria-label="t('coreUpdates.enabled')" data-testid="update-source-enabled" />
      </SSettingRow>
      <SSettingRow :label="t('coreUpdates.kind')" :description="t('coreUpdates.kindHint')">
        <SSegmented v-model="form.kind" :options="kinds" :disabled="disabled" :aria-label="t('coreUpdates.kind')" data-testid="update-source-kind" />
      </SSettingRow>
      <SSettingRow v-if="form.kind === 'github'" :label="t('coreUpdates.repository')" :description="t('coreUpdates.repositoryHint')" :error="errors.repository">
        <div class="flex w-full gap-2 sm:w-96">
          <SInput v-model="form.repository" mono name="repository" placeholder="owner/repository" class="min-w-0 flex-1" :disabled="disabled" />
          <SButton size="sm" :disabled="disabled || !form.defaultRepository || form.repository.trim() === form.defaultRepository" data-testid="update-source-default" @click="form.repository = form.defaultRepository">{{ t('coreUpdates.restoreDefault') }}</SButton>
        </div>
      </SSettingRow>
      <SSettingRow v-else :label="t('coreUpdates.url')" :description="t('coreUpdates.urlHint')" :error="errors.url">
        <SInput v-model="form.url" mono type="url" name="url" placeholder="https://releases.example.com/sub2api/" class="w-full sm:w-96" :disabled="disabled" />
      </SSettingRow>
      <SSettingRow :label="t(`coreUpdates.secret.${form.kind}`)" :description="t(`coreUpdates.secretHint.${form.kind}`)" :error="errors.secret">
        <template #details>{{ t('coreUpdates.secretDetails') }}</template>
        <div class="flex w-full flex-col gap-2 sm:w-96">
          <div v-if="form.hasSecret" class="flex flex-wrap items-center gap-2" data-testid="update-source-secret-state">
            <SBadge :tone="form.clearSecret ? 'warning' : 'success'">{{ t(form.clearSecret ? 'coreUpdates.secretWillClear' : 'coreUpdates.secretSet') }}</SBadge>
            <SButton v-if="canManage" size="sm" :tone="form.clearSecret ? 'default' : 'danger'" :disabled="disabled" data-testid="update-source-clear-secret" @click="toggleClear">
              {{ t(form.clearSecret ? 'coreUpdates.undo' : 'coreUpdates.clear') }}
            </SButton>
          </div>
          <SInput
            v-if="!form.clearSecret"
            v-model="form.secret"
            type="password"
            name="secret"
            autocomplete="new-password"
            revealable
            mono
            :placeholder="t(form.hasSecret ? 'coreUpdates.secretKeep' : 'coreUpdates.secretNone')"
            :disabled="disabled"
          />
        </div>
      </SSettingRow>
      <SAlert v-if="secretDropped" tone="warning" class="my-3" data-testid="update-source-secret-dropped">{{ t('coreUpdates.secretDropped') }}</SAlert>
      <SSettingRow v-if="canCheck && (updates.busy || result)" :label="t('coreUpdates.checkResult')" data-testid="update-source-result">
        <SHint v-if="updates.busy">{{ t('coreUpdates.checking') }}</SHint>
        <div v-else class="space-y-1">
          <SHint v-for="line in result!.lines" :key="line" :tone="result!.tone" class="break-words">{{ line }}</SHint>
        </div>
      </SSettingRow>
    </template>
  </SSettingsGroup>
</template>
