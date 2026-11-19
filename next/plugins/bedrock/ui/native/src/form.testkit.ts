// Test kit of the account form specs: a plugin host speaking the English
// messages, the account editor around a native form, and a driver that
// types a case's inputs into the form's controls.
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h, ref, type Component } from 'vue'
import type { PluginHost } from '@sub2api/host'
import messages from './messages'

type M = Record<string, unknown>

/** host.t over the English messages ({name} params filled in). */
export function fakeHost(): PluginHost {
  const t = (key: string, params?: Record<string, unknown>) => {
    const v = key.split('.').reduce<unknown>((o, k) => (o && typeof o === 'object' ? (o as M)[k] : undefined), messages.en)
    const s = typeof v === 'string' ? v : key
    return params ? s.replace(/\{(\w+)\}/g, (m, k) => (params[k] === undefined ? m : String(params[k]))) : s
  }
  return { t, addMessages: () => {}, registerComponent: () => {} } as unknown as PluginHost
}

/** Mounts a native account form the way the account editor does (v-model credentials, the §23.5 props). */
export function editor(form: Component, props: { type: string; mode?: 'create' | 'edit'; modelValue?: M; errors?: Record<string, string> }) {
  const credentials = ref<M>(props.modelValue ?? {})
  const inner = ref<{ validate: () => boolean | Promise<boolean> }>()
  const mode = props.mode ?? 'create'
  const w = mount(
    defineComponent({
      setup: () => () =>
        h('form', [
          h(form, {
            ref: inner,
            modelValue: credentials.value,
            'onUpdate:modelValue': (v: M) => (credentials.value = v),
            mode,
            account: mode === 'edit' ? { id: 1, type: props.type, credentials: props.modelValue ?? {} } : null,
            accountType: { plugin_key: 'bedrock', type: props.type },
            errors: props.errors,
            proxyTicket: async () => 'ticket'
          })
        ])
    }),
    { attachTo: document.body }
  )
  return { w, credentials, validate: async () => inner.value!.validate() }
}

/**
 * Types `input` into the controls: `order` names the field of each
 * input / select in document order. A select takes the label of the option
 * to pick, an input the text (a datetime-local box its local value).
 */
export async function fill(w: VueWrapper<any>, order: string[], input: Record<string, string>) {
  const controls = w.findAll('input, select')
  for (const [field, value] of Object.entries(input)) {
    const el = controls[order.indexOf(field)]
    if (!el) throw new Error(`no control for ${field}`)
    if (el.element.tagName === 'SELECT') {
      const option = el.findAll('option').find((o) => o.text() === value)
      if (!option) throw new Error(`${field}: no option ${value}`)
      await el.setValue((option.element as HTMLOptionElement).value)
    } else {
      await el.setValue(value)
    }
    await el.trigger('blur')
    await flushPromises()
  }
}

/** What the account editor would save (JSON, as sent). */
export const saved = (v: M) => JSON.parse(JSON.stringify(v)) as M
