<script setup lang="ts">
// A plugin's user permissions × roles, shared by the install / upgrade
// consent page and the plugin detail "Permissions" tab. It emits nothing: the
// state lives in `matrix` (useGrantMatrix), which the page owns and reads
// (`matrix.value`, `matrix.changes`, `matrix.dirty`) to save or build its
// payload. `lockOf` says which cells may change (CellLock); `scopeLimit` (the
// editor's scopes of an action, auth.scopes; [ALL_SCOPE] for the super admin)
// what a scoped cell may offer.
//
// Rows follow `perms` under their `groups` (a heading row per group whose
// checkbox ticks the group for one role as concrete keys, D1); permissions of
// no listed group come last. An unscoped cell is a checkbox; a scoped one a
// select (none / all / own / chosen…) whose "chosen…" opens ScopePicker
// below the table, outside the scroll area, so phones do not cut it off. A
// dot marks a cell that differs from the plugin's suggestion.
//
// Slots: #toolbar (end of the toolbar), #trailing-head and #trailing="{ perm }"
// (one more column, e.g. the upgrade's "needs confirmation").
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SBadge, SButton, SCheckbox, SHint, SIcon, SSelect } from '@sub2api/ui'
import type { PermissionGroup } from '@/api/types'
import { lt } from '@/i18n'
import type { Scope } from '@/authz/implies'
import { offerOf } from '@/authz/scopeChoice'
import { cellKeys, cellOfScopes, scopesOfCell } from '@/authz/pluginGrants'
import ScopePicker from '@/views/roles/ScopePicker.vue'
import PermRiskBadge from './PermRiskBadge.vue'
import type { CellLock, GrantMatrix, MatrixPerm, MatrixRole } from './useGrantMatrix'

const props = withDefaults(
  defineProps<{
    matrix: GrantMatrix
    perms: MatrixPerm[]
    groups?: PermissionGroup[]
    roles: MatrixRole[]
    pluginKey: string
    lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
    scopeLimit: (action: string) => Scope[]
    /** Custom roles fold under "more roles" (consent page). */
    collapseCustom?: boolean
    /** Rows shown before "N more" (0: all). */
    foldAfter?: number
    readOnly?: boolean
  }>(),
  { groups: () => [], collapseCustom: false, foldAfter: 0, readOnly: false }
)
const { t } = useI18n()

/** Locks that freeze a cell outright; the others (not_held, removed) only take away. */
const FIXED = new Set<CellLock>(['superuser', 'pending_review', 'critical', 'role_readonly'])
const LOCK_HINT: Record<Exclude<CellLock, null>, string> = {
  superuser: 'plugins.matrix.superFixed',
  pending_review: 'plugins.matrix.pendingReviewHint',
  critical: 'plugins.matrix.criticalOnly',
  not_held: 'roles.notHeld',
  role_readonly: 'plugins.matrix.roleReadonly',
  removed: 'roles.removed'
}

const showCustom = ref(false)
const customCount = computed(() => props.roles.filter((r) => !r.builtin).length)
const shownRoles = computed(() => (props.collapseCustom && !showCustom.value ? props.roles.filter((r) => r.builtin) : props.roles))

const unfolded = ref(false)
const folded = computed(() => props.foldAfter > 0 && !unfolded.value && props.perms.length > props.foldAfter)
const visible = computed(() => (folded.value ? props.perms.slice(0, props.foldAfter) : props.perms))

/** The rows by group, in the groups' order; rows of no listed group last, without a heading. */
const sections = computed(() => {
  const known = new Set(props.groups.map((g) => g.key))
  const out = props.groups.map((g) => ({ key: g.key, label: lt(g.label) || g.key, perms: visible.value.filter((p) => p.group === g.key) }))
  out.push({ key: '', label: '', perms: visible.value.filter((p) => !p.group || !known.has(p.group)) })
  return out.filter((s) => s.perms.length)
})

const offerFor = (action: string) => offerOf(props.scopeLimit(action), action)
const fixed = (p: MatrixPerm, r: MatrixRole) => props.readOnly || FIXED.has(props.lockOf(p, r))
const lockHint = (p: MatrixPerm, r: MatrixRole) => {
  const lock = props.lockOf(p, r)
  return lock ? t(LOCK_HINT[lock]) : ''
}
/** What a cell shows: everything for the super admin, the suggestion while pending review, else its keys. */
function shown(p: MatrixPerm, r: MatrixRole): string[] {
  const lock = props.lockOf(p, r)
  if (lock === 'superuser') return [p.action]
  if (lock === 'pending_review') return cellKeys(props.matrix.suggested.value[r.key] ?? [], p.action)
  return props.matrix.cell(p, r)
}
/** An empty cell under a take-away-only lock stays empty. */
const disabled = (p: MatrixPerm, r: MatrixRole) => fixed(p, r) || (props.lockOf(p, r) !== null && !props.matrix.cell(p, r).length)

const label = (p: MatrixPerm) => lt(p.label) || p.local
const cellLabel = (p: MatrixPerm, r: MatrixRole) => `${label(p)} · ${lt(r.name) || r.key}`
const hint = (p: MatrixPerm) => [p.action, p.description && lt(p.description)].filter(Boolean).join('\n')
const requiresText = (p: MatrixPerm) => (p.requires ?? []).map((k) => {
  const req = props.perms.find((x) => x.action === k)
  return req ? label(req) : k
}).join(t('roles.listSep'))
const criticalRow = (p: MatrixPerm) => props.roles.some((r) => props.lockOf(p, r) === 'critical')
const rowDiffers = (p: MatrixPerm) => props.roles.some((r) => props.matrix.differsFromSuggested(p, r))
const differing = computed(() => props.perms.reduce((n, p) => n + props.roles.filter((r) => props.matrix.differsFromSuggested(p, r)).length, 0))
const columnReadonly = (r: MatrixRole) => props.perms.length > 0 && props.perms.every((p) => props.lockOf(p, r) === 'role_readonly')
const anyPending = computed(() => props.perms.some((p) => props.roles.some((r) => props.lockOf(p, r) === 'pending_review')))

// A heading row's checkbox: the rows of the group the role's cell may take.
function groupState(key: string, perms: MatrixPerm[], r: MatrixRole): 'all' | 'some' | 'none' {
  const rows = perms.filter((p) => props.lockOf(p, r) === null && p.status !== 'removed')
  const n = rows.filter((p) => props.matrix.cell(p, r).length).length
  if (n === 0) return 'none'
  return n === rows.length ? 'all' : 'some'
}
const groupEditable = (perms: MatrixPerm[], r: MatrixRole) => !props.readOnly && perms.some((p) => props.lockOf(p, r) === null && p.status !== 'removed')
function toggleGroup(key: string, perms: MatrixPerm[], r: MatrixRole) {
  props.matrix.setGroup(key, r, groupState(key, perms, r) !== 'all', offerFor)
}

// Scoped cells: the select names the choice; "custom" opens the picker below.
type Choice = 'none' | 'all' | 'own' | 'custom'
function choiceOf(p: MatrixPerm, r: MatrixRole): Choice {
  const scopes = scopesOfCell(shown(p, r), p.action)
  if (!scopes.length) return 'none'
  if (scopes.some((s) => s.all)) return 'all'
  return scopes.every((s) => s.own) ? 'own' : 'custom'
}
/** The chosen plugin values ("<resource>/<value>") of a cell. */
const refCount = (p: MatrixPerm, r: MatrixRole) => scopesOfCell(shown(p, r), p.action).filter((s) => s.ref).length
function options(p: MatrixPerm, r: MatrixRole) {
  const offer = offerFor(p.action)
  const current = choiceOf(p, r)
  // Under a take-away-only lock nothing but the current choice and none is open.
  const closed = (c: Choice) => props.lockOf(p, r) !== null && c !== current && c !== 'none'
  const refs = refCount(p, r)
  const all: Array<[Choice, boolean, string]> = [
    ['none', true, t('plugins.matrix.notGranted')],
    ['all', offer.all, t('plugins.matrix.scopeAll')],
    ['own', !!p.scope?.own && offer.own, t('plugins.matrix.scopeOwn')],
    ['custom', !!offer.refs, refs ? t('plugins.matrix.scopeCount', { n: refs }) : t('plugins.matrix.scopeCustom')]
  ]
  return all.filter(([c, offered]) => offered || c === current).map(([c, , text]) => ({ value: c, label: text, disabled: closed(c) }))
}

const expanded = ref<{ action: string; role: string } | null>(null)
const expandedCell = computed(() => {
  const e = expanded.value
  const perm = e && props.perms.find((p) => p.action === e.action)
  const role = e && props.roles.find((r) => r.key === e.role)
  return perm && role ? { perm, role } : null
})
function choose(p: MatrixPerm, r: MatrixRole, c: Choice) {
  if (c === 'custom') {
    expanded.value = { action: p.action, role: r.key }
    return
  }
  props.matrix.setCell(p, r, c === 'none' ? [] : [c === 'all' ? p.action : `${p.action}:own`])
}
</script>

<template>
  <div class="space-y-3" data-testid="perm-matrix">
    <div class="flex flex-wrap items-center gap-2">
      <SHint v-if="differing" inline size="xs" data-testid="matrix-differs">{{ t('roles.differsCount', { n: differing }) }}</SHint>
      <SButton v-if="differing && !readOnly" size="xs" variant="secondary" data-testid="matrix-restore" @click="matrix.restore()">{{ t('roles.restoreSuggested') }}</SButton>
      <SButton v-if="collapseCustom && customCount" size="xs" variant="ghost" data-testid="matrix-more-roles" @click="showCustom = !showCustom">
        {{ showCustom ? t('plugins.matrix.fewerRoles') : t('plugins.matrix.moreRoles') }}
        <SIcon name="chevron-down" class="h-3.5 w-3.5 transition-transform" :class="showCustom ? 'rotate-180' : ''" />
      </SButton>
      <slot name="toolbar" />
    </div>

    <div class="overflow-x-auto rounded-xl border border-line">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-line">
            <th scope="col" class="sticky left-0 z-[1] min-w-[9rem] max-w-[11rem] bg-surface px-3 py-2 text-left font-medium text-fg-subtle md:min-w-[16rem] md:max-w-none">
              {{ t('plugins.matrix.permission') }}
            </th>
            <th
              v-for="r in shownRoles"
              :key="r.key"
              scope="col"
              class="min-w-[5.5rem] max-w-[8rem] px-2 py-2 text-center font-medium text-fg-subtle"
              :title="r.superuser ? t('plugins.matrix.superFixed') : columnReadonly(r) ? t('plugins.matrix.roleReadonly') : lt(r.name) || r.key"
              :data-role="r.key"
            >
              <span class="inline-flex max-w-full items-center gap-1">
                <span class="truncate">{{ lt(r.name) || r.key }}</span>
                <SIcon v-if="columnReadonly(r)" name="lock" class="h-3 w-3 shrink-0" />
              </span>
            </th>
            <th v-if="$slots['trailing-head']" scope="col" class="px-2 py-2 text-left font-medium text-fg-subtle"><slot name="trailing-head" /></th>
          </tr>
        </thead>
        <tbody v-for="s in sections" :key="s.key" class="border-b border-line last:border-b-0">
          <tr v-if="s.label" class="group bg-surface-2" :data-group="s.key">
            <th scope="rowgroup" class="sticky left-0 z-[1] bg-surface-2 px-3 py-1.5 text-left text-xs font-medium text-fg-subtle">{{ s.label }}</th>
            <td v-for="r in shownRoles" :key="r.key" class="px-2 py-1.5 text-center">
              <SCheckbox
                v-if="!r.superuser"
                bare
                size="xs"
                :model-value="groupState(s.key, s.perms, r) === 'all'"
                :indeterminate="groupState(s.key, s.perms, r) === 'some'"
                :disabled="!groupEditable(s.perms, r)"
                :aria-label="`${t('roles.groupSelectAll')} · ${s.label} · ${lt(r.name) || r.key}`"
                :data-group-role="r.key"
                @update:model-value="toggleGroup(s.key, s.perms, r)"
              />
            </td>
            <td v-if="$slots['trailing-head']" />
          </tr>
          <tr v-for="p in s.perms" :key="p.action" class="group border-t border-line first:border-t-0" :data-permission="p.action">
            <th scope="row" class="sticky left-0 z-[1] min-w-[9rem] max-w-[11rem] bg-surface px-3 py-2 text-left font-normal group-hover:bg-surface-2 md:min-w-[16rem] md:max-w-none" :title="hint(p)">
              <div class="flex min-w-0 flex-wrap items-center gap-1.5">
                <span class="line-clamp-2 text-fg" :class="p.status === 'removed' ? 'text-fg-subtle line-through' : ''">{{ label(p) }}</span>
                <PermRiskBadge :risk="p.risk" />
                <SBadge v-if="p.status === 'removed'" size="xs" tone="gray">{{ t('roles.removed') }}</SBadge>
                <button
                  v-if="!readOnly && rowDiffers(p)"
                  type="button"
                  class="ml-auto shrink-0 rounded p-0.5 text-fg-subtle hover:text-fg"
                  :title="t('roles.restoreSuggested')"
                  :aria-label="t('roles.restoreSuggested')"
                  data-testid="matrix-restore-row"
                  @click="matrix.restore(p)"
                ><SIcon name="refresh" class="h-3.5 w-3.5" /></button>
              </div>
              <SHint v-if="criticalRow(p)" size="xs" class="mt-0.5 text-danger-600 dark:text-danger-400">{{ t('plugins.matrix.criticalOnly') }}</SHint>
              <SHint v-if="p.requires?.length" size="xs" class="mt-0.5">{{ t('roles.requiresHint', { names: requiresText(p) }) }}</SHint>
              <SHint size="xs" class="mt-0.5 hidden truncate font-mono md:block">{{ p.action }}</SHint>
            </th>
            <td
              v-for="r in shownRoles"
              :key="r.key"
              class="relative px-2 py-2 text-center"
              :class="lockOf(p, r) === 'pending_review' ? 'bg-warning-50 dark:bg-warning-900/20' : ''"
              :title="lockHint(p, r) || undefined"
              :data-role="r.key"
              :data-lock="lockOf(p, r) ?? undefined"
            >
              <SCheckbox
                v-if="!p.scope"
                bare
                size="xs"
                :model-value="shown(p, r).length > 0"
                :disabled="disabled(p, r)"
                :aria-label="cellLabel(p, r)"
                @update:model-value="matrix.setCell(p, r, $event ? [p.action] : [])"
              />
              <div v-else class="flex flex-col items-center gap-1">
                <SSelect
                  size="sm"
                  class="min-w-[6rem]"
                  :model-value="choiceOf(p, r)"
                  :options="options(p, r)"
                  :disabled="disabled(p, r)"
                  :aria-label="cellLabel(p, r)"
                  @update:model-value="choose(p, r, $event as Choice)"
                />
                <button
                  v-if="choiceOf(p, r) === 'custom' && !fixed(p, r)"
                  type="button"
                  class="text-xs text-primary-600 hover:underline dark:text-primary-400"
                  data-testid="matrix-open-scope"
                  @click="expanded = { action: p.action, role: r.key }"
                >{{ t('plugins.matrix.scopeCount', { n: refCount(p, r) }) }}</button>
              </div>
              <SBadge v-if="lockOf(p, r) === 'pending_review' && shown(p, r).length" size="xs" tone="warning" class="mt-1">{{ t('plugins.matrix.pendingReview') }}</SBadge>
              <SIcon v-else-if="lockOf(p, r) === 'critical' || (lockOf(p, r) === 'pending_review' && !shown(p, r).length)" name="lock" class="ml-1 inline h-3 w-3 text-fg-subtle" />
              <span
                v-if="matrix.differsFromSuggested(p, r)"
                class="absolute right-1 top-1 h-1.5 w-1.5 rounded-full bg-primary-500 dark:bg-primary-400"
                :title="t('plugins.matrix.differs')"
                data-testid="cell-differs"
              />
            </td>
            <td v-if="$slots['trailing-head']" class="px-2 py-2"><slot name="trailing" :perm="p" /></td>
          </tr>
        </tbody>
      </table>
    </div>

    <SButton v-if="foldAfter > 0 && perms.length > foldAfter" size="xs" variant="ghost" data-testid="matrix-fold" @click="unfolded = !unfolded">
      {{ folded ? t('plugins.matrix.foldMore', { n: perms.length - foldAfter }) : t('plugins.matrix.foldLess') }}
    </SButton>

    <div v-if="expandedCell" class="w-full space-y-2 rounded-xl border border-line p-3" data-testid="matrix-scope-panel">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span class="text-sm font-medium text-fg">{{ t('plugins.matrix.scopeOf', { role: lt(expandedCell.role.name) || expandedCell.role.key, perm: label(expandedCell.perm) }) }}</span>
        <SButton size="xs" variant="secondary" data-testid="matrix-scope-done" @click="expanded = null">{{ t('plugins.matrix.done') }}</SButton>
      </div>
      <ScopePicker
        :action="expandedCell.perm.action"
        resource=""
        :plugin="{ key: pluginKey, local: expandedCell.perm.local, scope: expandedCell.perm.scope }"
        :scopes="scopesOfCell(matrix.cell(expandedCell.perm, expandedCell.role), expandedCell.perm.action)"
        :offer="offerFor(expandedCell.perm.action)"
        :disabled="disabled(expandedCell.perm, expandedCell.role)"
        @update:scopes="matrix.setCell(expandedCell.perm, expandedCell.role, cellOfScopes(expandedCell.perm.action, $event))"
      />
    </div>

    <SHint size="xs" class="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span>{{ t('roles.riskLegend') }}</span>
      <span class="inline-flex items-center gap-1"><span class="h-1.5 w-1.5 rounded-full bg-primary-500 dark:bg-primary-400" />{{ t('plugins.matrix.differs') }}</span>
      <span v-if="anyPending" class="inline-flex items-center gap-1"><SBadge size="xs" tone="warning">{{ t('plugins.matrix.pendingReview') }}</SBadge>{{ t('plugins.matrix.pendingReviewHint') }}</span>
    </SHint>
  </div>
</template>
