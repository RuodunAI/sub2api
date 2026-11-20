import { definePluginHost } from '@sub2api/host'

// Plugin-scoped host handed to register(); shared by all guard components.
export const { setHost, useHost: useGuardHost } = definePluginHost('guard')

/** Human-readable message of a failed call (ApiError is an Error), else `fallback`. */
export function errorMessage(e: unknown, fallback: string): string {
  return e instanceof Error && e.message ? e.message : fallback
}

export type RuleKind = 'keyword' | 'regex'

export interface Rule {
  id?: number
  name: string
  kind: RuleKind
  pattern: string
  enabled: boolean
  updated_at?: string
}

export interface Stats {
  from: string
  to: string
  bucket: 'hour' | 'day'
  blocked_total: number
  requests_total: number
  trend: Array<{ ts: string; blocked: number; total: number }>
  top_rules: Array<{ rule_id: number; name: string; kind: RuleKind; pattern: string; hits: number }>
  recent?: Array<{
    occurred_at: string
    rule_id: number
    rule_name: string
    request_id: string
    user_id: number
    group_id: number
    model: string
    snippet?: string
  }>
}

function timezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

export function fetchStats(query: Record<string, string>): Promise<Stats> {
  return useGuardHost().pluginApi.get<Stats>('/stats', { tz: timezone(), ...query })
}

export async function fetchRules(): Promise<Rule[]> {
  const r = await useGuardHost().pluginApi.get<Rule[] | { rules: Rule[] }>('/rules')
  return Array.isArray(r) ? r : r?.rules || []
}

export async function saveRules(rules: Rule[]): Promise<Rule[]> {
  const r = await useGuardHost().pluginApi.put<Rule[] | { rules: Rule[] }>('/rules', { rules })
  return Array.isArray(r) ? r : r?.rules || rules
}
