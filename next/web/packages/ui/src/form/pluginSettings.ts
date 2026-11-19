// usePluginSettings: a plugin's settings (GET / PUT /plugins/:key/settings,
// CONTRACTS §75) as a form, taken from the console's settings tab
// (views/plugins/detail/SettingsTab.vue): the loaded value with the
// defaults filled is the "unchanged" baseline; saving validates, PUTs the
// whole value, toasts, and on refusal shows the field errors at their
// fields (errors of the whole object as a toast). Secrets come back as
// "******" and go back as is to keep them; a caller with only the plugin's
// settings permission who changes something while keeping a secret is
// asked to type it again (field code "reenter").
import { computed, nextTick, ref, type ComputedRef, type Ref } from 'vue'
import { isApiError, settingsFieldErrors, useHost, VERIFICATION_CANCELLED, type ApiClient } from '@sub2api/host'
import { toast } from '../feedback'
import { useUiT } from '../i18n'
import { useForm, type FormApi } from './useForm'
import type { Rules } from './rules'

type Model = Record<string, unknown>

/** GET /plugins/:key/settings (the fields this composable reads). */
interface SettingsView {
  values: Model | null
  /** RBAC key of manifest ui.settings.permission: its holders may edit too. */
  permission?: string
}

export interface PluginSettingsForm<M extends Model = Model> extends FormApi<M> {
  loading: Ref<boolean>
  saving: Ref<boolean>
  loadError: Ref<unknown>
  /** Differs from the loaded value (after defaults). */
  dirty: ComputedRef<boolean>
  /** plugin:manage, or the plugin's ui.settings.permission. */
  canEdit: ComputedRef<boolean>
  reload(): Promise<void>
  /** validate(), then PUT; true when saved. */
  save(): Promise<boolean>
  /** Back to the loaded value. */
  reset(): void
}

const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v ?? {}))

/** Refusals worth no toast: the session ended, a dismissed verification, an aborted request (the console's notifyError). */
const silent = (e: unknown) => (isApiError(e) && (e.code === 'unauthenticated' || e.code === VERIFICATION_CANCELLED)) || (e instanceof DOMException && e.name === 'AbortError')

export function usePluginSettings<M extends Model = Model>(opts: {
  pluginKey: string
  /** host.api */
  api: ApiClient
  rules: Rules<M> | (() => Rules<M>)
  defaults?: Partial<M>
}): PluginSettingsForm<M> {
  const host = useHost()
  const t = useUiT()
  const path = `/plugins/${encodeURIComponent(opts.pluginKey)}/settings`
  const view = ref<SettingsView | null>(null)
  const values = ref({}) as Ref<M>
  const serverErrors = ref<Record<string, string>>({})
  const baseline = ref('')
  const loading = ref(false)
  const saving = ref(false)
  const loadError = ref<unknown>(null)
  const form = useForm<M>({ model: () => values.value, update: (v) => (values.value = v), rules: opts.rules, serverErrors: () => serverErrors.value, defaults: opts.defaults })

  /** Shows the loaded value; the baseline is taken once the defaults are filled in. */
  async function show() {
    values.value = clone(view.value?.values ?? {}) as M
    serverErrors.value = {}
    form.clearValidate()
    baseline.value = ''
    await nextTick()
    baseline.value = JSON.stringify(values.value)
  }

  async function reload() {
    loading.value = true
    loadError.value = null
    try {
      view.value = await opts.api.get<SettingsView>(path)
      await show()
    } catch (e) {
      loadError.value = e
    } finally {
      loading.value = false
    }
  }

  async function save(): Promise<boolean> {
    if (!(await form.validate())) return false
    saving.value = true
    serverErrors.value = {}
    try {
      await opts.api.put(path, { values: values.value })
      toast(t('ui.form.saved'), 'success')
      baseline.value = JSON.stringify(values.value)
      return true
    } catch (e) {
      const fields = isApiError(e) && e.code === 'invalid_argument' ? Object.fromEntries(Object.entries(e.fields).map(([k, v]) => [k, e.fieldCodes[k] === 'reenter' ? t('ui.form.reenter') : v])) : {}
      const { fields: at, root } = settingsFieldErrors(fields)
      serverErrors.value = at
      // Errors of the whole object have no field to show at.
      if (root.length) toast(root.join('; '), 'error')
      else if (!silent(e)) toast(e instanceof Error ? e.message : String(e), 'error')
      return false
    } finally {
      saving.value = false
    }
  }

  void reload()
  return Object.assign(form, {
    loading,
    saving,
    loadError,
    dirty: computed(() => !!baseline.value && JSON.stringify(values.value) !== baseline.value),
    canEdit: computed(() => host.permissions.has('plugin:manage') || (!!view.value?.permission && host.permissions.has(view.value.permission))),
    reload,
    save,
    reset: () => void show()
  })
}
