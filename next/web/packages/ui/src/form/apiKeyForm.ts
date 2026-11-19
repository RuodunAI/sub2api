// createApiKeyForm: the native account form of an API key type (api_key,
// an optional base_url, a few extra fields) as one call:
//   host.registerComponent('AnthropicKeyForm', createApiKeyForm({
//     baseUrl: { presets: ['https://api.anthropic.com'], placeholder: () => host.t('baseUrl.placeholder') }
//   }))
// The component takes the §23.5 props and exposes validate(). base_url
// follows useGuardedSettings: without account:settings:custom it offers the
// type's official values only.
import { defineComponent, h, type Component, type PropType, type VNode } from 'vue'
import { useGuardedSettings, type AccountTypeView, type GuardedSettings, type ProxyTicketSource } from '@sub2api/host'
import SField from '../SField.vue'
import SInput from '../SInput.vue'
import SSecretInput from '../SSecretInput.vue'
import SSectionTitle from '../SSectionTitle.vue'
import SSelect from '../SSelect.vue'
import SSwitch from '../SSwitch.vue'
import STextarea from '../STextarea.vue'
import SUrlPresetInput from '../SUrlPresetInput.vue'
import { useUiT } from '../i18n'
import { fromLocalInput, toLocalInput } from '../timeRange'
import SCredentialForm from './SCredentialForm.vue'
import { useCredentialForm, type NativeAccountFormProps } from './credential'
import { toList, type Rule, type Rules } from './rules'
import type { FormApi } from './useForm'

type Model = Record<string, unknown>
type Text = () => string

export interface ApiKeyFormSpec {
  /** Defaults: 8–512 visible ASCII characters (/^[!-~]+$/). */
  apiKey?: { min?: number; max?: number; pattern?: RegExp; placeholder?: Text; help?: Text }
  /** false: the type has no base_url. */
  baseUrl?:
    | false
    | {
        required?: boolean
        presets?: string[]
        /** Default /^(https?:\/\/[^\s?#]+)?$/, required: /^https?:\/\/[^\s?#]+$/. */
        pattern?: RegExp
        /** The message of a value the pattern refuses (default: the generic "invalid format"). */
        message?: Text
        /** Default: this package's "Base URL". */
        label?: Text
        placeholder?: Text
        help?: Text
        /** Shown above the API key (a site's address before its key). */
        first?: boolean
      }
  /** Further fields, in order; each is one SField. */
  extra?: ExtraField[]
  /** Filled into a new account's absent keys. */
  defaults?: Model
}

export interface ExtraField {
  key: string
  kind: 'text' | 'secret' | 'url' | 'select' | 'textarea' | 'date-time' | 'switch'
  label: Text
  help?: Text
  placeholder?: Text
  /** select: the choices. */
  options?: { value: string; label: Text }[]
  /** url: suggestions; a field in the type's guarded_settings offers its official values only, as base_url does. */
  presets?: string[]
  rules?: Rule | Rule[]
  /** SSectionTitle above the first field of a section. */
  section?: Text
  /** Shown and validated only while true. */
  when?: (m: Model) => boolean
}

const API_KEY_PATTERN = /^[!-~]+$/
const BASE_URL = /^(https?:\/\/[^\s?#]+)?$/
const BASE_URL_REQUIRED = /^https?:\/\/[^\s?#]+$/

/** The rules of the spec: api_key, base_url, then each extra field's (gated by its `when`). */
function specRules(spec: ApiKeyFormSpec): Rules<Model> {
  const k = spec.apiKey ?? {}
  const rules: Rules<Model> = { api_key: { required: true, min: k.min ?? 8, max: k.max ?? 512, pattern: k.pattern ?? API_KEY_PATTERN } }
  const b = spec.baseUrl ?? {}
  if (b) rules.base_url = [{ required: b.required }, { pattern: b.pattern ?? (b.required ? BASE_URL_REQUIRED : BASE_URL), message: b.message }]
  for (const f of spec.extra ?? []) {
    const when = f.when
    rules[f.key] = toList(f.rules).map((r) => (when ? { ...r, when: (m: Model) => when(m) && (!r.when || r.when(m)) } : r))
  }
  return rules
}

/** The control of an extra field, bound to the form. */
function control(f: ExtraField, form: FormApi<Model>, guard: GuardedSettings): VNode {
  const bound = form.bind(f.key)
  const set = (v: unknown) => form.set(f.key, v)
  const placeholder = f.placeholder?.()
  switch (f.kind) {
    case 'secret':
      // An optional secret can be cleared; a required one only replaced.
      return h(SSecretInput, { ...bound, placeholder, clearable: !toList(f.rules).some((r) => r.required) })
    case 'url':
      return h(SUrlPresetInput, { ...bound, presets: f.presets, allowed: guard.allowed(f.key), placeholder })
    case 'select':
      return h(SSelect, {
        modelValue: bound.modelValue,
        options: (f.options ?? []).map((o) => ({ value: o.value, label: o.label() })),
        placeholder,
        'onUpdate:modelValue': (v: unknown) => set(v ?? undefined)
      })
    case 'textarea':
      return h(STextarea, { ...bound, placeholder })
    case 'date-time':
      // RFC 3339 in the value, local time in the box.
      return h(SInput, { type: 'datetime-local', modelValue: toLocalInput(bound.modelValue), 'onUpdate:modelValue': (v: unknown) => set(fromLocalInput(String(v ?? '')) || undefined) })
    case 'switch':
      return h(SSwitch, { ...bound, modelValue: !!bound.modelValue })
    default:
      return h(SInput, { ...bound, placeholder })
  }
}

export function createApiKeyForm(spec: ApiKeyFormSpec): Component {
  return defineComponent({
    name: 'ApiKeyForm',
    props: {
      modelValue: { type: Object as PropType<Model>, default: () => ({}) },
      mode: { type: String as PropType<'create' | 'edit'>, default: 'create' },
      account: { type: Object as PropType<NativeAccountFormProps['account']>, default: null },
      accountType: { type: Object as PropType<AccountTypeView | null>, default: null },
      errors: { type: Object as PropType<Record<string, string>>, default: undefined },
      proxyTicket: { type: Function as PropType<ProxyTicketSource>, default: undefined }
    },
    emits: { 'update:modelValue': (_v: Model) => true },
    setup(props, { emit, expose }) {
      const t = useUiT()
      const form = useCredentialForm(props, (_e, v) => emit('update:modelValue', v), { rules: specRules(spec), defaults: spec.defaults })
      const guard = useGuardedSettings(() => props.accountType)
      expose({ validate: form.validate })

      const field = (key: string, label: string, help: Text | undefined, child: VNode) => h(SField, { key, prop: key, label, hint: help?.() }, () => child)
      return () => {
        const k = spec.apiKey ?? {}
        const b = spec.baseUrl ?? {}
        const nodes: VNode[] = [
          field('api_key', t('ui.form.apiKey'), k.help, h(SSecretInput, { ...form.bind('api_key'), placeholder: k.placeholder?.() }))
        ]
        if (b) {
          const url = field(
            'base_url',
            b.label?.() ?? t('ui.form.baseUrl'),
            b.help,
            h(SUrlPresetInput, { ...form.bind('base_url'), presets: b.presets, allowed: guard.allowed('base_url'), placeholder: b.placeholder?.() })
          )
          if (b.first) nodes.unshift(url)
          else nodes.push(url)
        }
        let section: string | undefined
        for (const f of spec.extra ?? []) {
          if (f.when && !f.when(form.value())) continue
          const title = f.section?.()
          if (title && title !== section) nodes.push(h(SSectionTitle, { key: `section-${f.key}`, title, tag: 'h4' }))
          section = title ?? section
          nodes.push(field(f.key, f.label(), f.help, control(f, form, guard)))
        }
        return h(SCredentialForm, { form }, () => nodes)
      }
    }
  })
}
