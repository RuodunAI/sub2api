import { ref, type Ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from '@sub2api/ui'
import type { JsonError, JsonParse } from './jsonObject'

// The views of an override editor (OverrideDialog) around its JSON text:
// valid JSON is loaded into the draft as it is typed; while the text is
// invalid the editor neither leaves the JSON view nor applies. Messages are
// `<ns>.json.<error>` (named by the error's members; a syntax error shows
// the parser's message) and `<ns>.json.invalid`.
export interface JsonViewOptions<V extends string, T> {
  ns: string
  /** The views; the editor opens on the first. */
  views: readonly V[]
  parse: (text: string) => JsonParse<T>
  /** Loads a value parsed from the JSON text into the draft. */
  load: (value: T | null) => void
  /** The draft as JSON text, shown on switching to JSON. */
  format: () => string
  /** Why the draft cannot be shown as JSON now ('' when it can). */
  blockJson?: () => string
}

export function useJsonView<V extends string, T>(o: JsonViewOptions<V, T>) {
  const { t } = useI18n()
  const view = ref(o.views[0]) as Ref<V>
  const jsonText = ref('')
  const jsonError = ref('')

  const explain = (e: JsonError) => (e.error === 'syntax' ? (e.detail ?? '') : t(`${o.ns}.json.${e.error}`, { ...e }))

  function editJson(text: string) {
    jsonText.value = text
    const parsed = o.parse(text)
    jsonError.value = 'error' in parsed ? explain(parsed) : ''
    if ('value' in parsed) o.load(parsed.value)
  }

  /** Whether the JSON view holds invalid text, which keeps the editor on it (told with a toast). */
  function jsonInvalid(): boolean {
    if (view.value !== 'json' || !jsonError.value) return false
    toast(t(`${o.ns}.json.invalid`), 'error')
    return true
  }

  function setView(next: V) {
    if (next === view.value || jsonInvalid()) return
    const blocked = next === 'json' ? o.blockJson?.() : ''
    if (blocked) return toast(blocked, 'error')
    if (next === 'json') {
      jsonText.value = o.format()
      jsonError.value = ''
    }
    view.value = next
  }

  /** Back to the first view, for an editor that opens again. */
  const reset = () => (view.value = o.views[0])

  return { view, jsonText, jsonError, editJson, setView, jsonInvalid, reset }
}
