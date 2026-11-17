import { ref, toValue, watch, type MaybeRefOrGetter } from 'vue'
import { api } from '@sub2api/host'
import type { Money, UsageSummaryRow } from '@/api/types'
import { browserTZ } from '@/components/stats/stats'

/** A row as answered: the account view's (CONTRACTS §103.5) has no billed cost. */
type SummaryAnswer = Omit<UsageSummaryRow, 'total_cost'> & { total_cost?: Money }
/** The row's cost: billed, or the list price where nothing billed is shown. */
const withCost = ({ total_cost, ...r }: SummaryAnswer): UsageSummaryRow => ({ ...r, total_cost: total_cost ?? r.standard_cost ?? '0' })

/**
 * GET <path>?group_by=<groupBy>&tz=<browser time zone> under the list's
 * filters, reloaded whenever the query they make (with groupBy and `extra`)
 * changes while `enabled` holds (a closed panel costs no query; neither does
 * a filter `extra` overrides, as the model picker's own do for its options).
 * A slow answer to an older filter never overwrites a newer one; a failure is
 * kept in `error` so the page can show it instead of zeros.
 */
export function useUsageSummary(
  path: string,
  filters: Record<string, unknown>,
  groupBy: MaybeRefOrGetter<string>,
  enabled: MaybeRefOrGetter<boolean> = true,
  /** More query parameters (order, limit), watched like the filters. */
  extra: MaybeRefOrGetter<Record<string, unknown>> = {}
) {
  const rows = ref<UsageSummaryRow[]>([])
  const loading = ref(false)
  const error = ref<unknown>(null)
  let seq = 0
  const query = () => ({ ...filters, ...toValue(extra), group_by: toValue(groupBy), tz: browserTZ() })
  /** The query as it is sent (the client leaves an empty parameter out); null while disabled. */
  const sent = () => (toValue(enabled) ? JSON.stringify(Object.entries(query()).filter(([, v]) => v !== '' && v != null)) : null)

  async function reload() {
    if (!toValue(enabled)) return
    const my = ++seq
    loading.value = true
    error.value = null
    try {
      const r = await api.get<SummaryAnswer[] | { items?: SummaryAnswer[] }>(path, query())
      if (my === seq) rows.value = (Array.isArray(r) ? r : r?.items || []).map(withCost)
    } catch (e) {
      if (my === seq) {
        rows.value = []
        error.value = e
      }
    } finally {
      if (my === seq) loading.value = false
    }
  }

  watch(sent, reload, { immediate: true })
  return { rows, loading, error, reload }
}
