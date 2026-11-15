<script setup lang="ts">
// A plugin's user permissions × roles, shared by the install / upgrade
// consent page and the plugin detail "Permissions" tab. It emits nothing: the
// state lives in `matrix` (useGrantMatrix), which the page owns and reads
// (`matrix.value`, `matrix.changes`, `matrix.dirty`) to save or build its
// payload. `lockOf` says which cells may change (CellLock); `scopeLimit` what
// a scoped cell may offer: the editor's scopes of an action (auth.scopes) on
// the detail page; left out, everything (the consent page: the permissions it
// creates need not be held, the server's cells.nameable decides).
//
// Rows follow `perms` under their `groups` (a heading row per group whose
// checkbox ticks the group's shown rows for one role as concrete keys, D1);
// permissions of no listed group come last. An unscoped cell is a checkbox; a
// scoped one MatrixScopeCell, whose "chosen…" opens ScopePicker below the
// table, outside the scroll area, so phones do not cut it off. A dot marks a
// cell that differs from the plugin's suggestion; a locked cell says why in
// its control's label as well as its title.
//
// Slots: #toolbar (end of the toolbar), #trailing-head and #trailing="{ perm }"
// (one more column, e.g. the upgrade's "needs confirmation").
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SBadge, SButton, SCheckbox, SHint, SIcon } from '@sub2api/ui'
import type { PermissionGroup } from '@/api/types'
import { lt } from '@/i18n'
import { ALL_SCOPE, type Scope } from '@/authz/implies'
import { offerOf } from '@/authz/scopeChoice'
import { cellOfScopes, scopesOfCell } from '@/authz/pluginGrants'
import ScopePicker from '@/views/roles/ScopePicker.vue'
import PermRiskBadge from './PermRiskBadge.vue'
import MatrixScopeCell from './MatrixScopeCell.vue'
import { groupSections, permHint } from './permRows'
import { isFixed, isShrinkOnly, type CellLock, type GrantMatrix, type MatrixPerm, type MatrixRole } from './useGrantMatrix'

const props = withDefaults(
  defineProps<{
    matrix: GrantMatrix
    perms: MatrixPerm[]
    groups?: PermissionGroup[]
    roles: MatrixRole[]
    pluginKey: string
    lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
    scopeLimit?: (action: string) => Scope[]
    /** Custom roles fold under "more roles" (consent page). */
    collapseCustom?: boolean
    /** Rows shown before "N more" (0: all). */
    foldAfter?: number
    readOnly?: boolean
  }>(),
  { groups: () => [], scopeLimit: () => [ALL_SCOPE], collapseCustom: false, foldAfter: 0, readOnly: false }
)
const { t } = useI18n()

const LOCK_HINT: Record<Exclude<CellLock, null>, string> = {
  superuser: 'plugins.matrix.superFixed',
  pending_review: 'plugins.matrix.pendingReviewHint',
  critical: 'plugins.matrix.criticalOnly',
  inherited: 'plugins.matrix.inheritedHint',
  not_held: 'roles.notHeld',
  role_readonly: 'plugins.matrix.roleReadonly',
  removed: 'roles.removed'
}

const showCustom = ref(false)
const customCount = computed(() => props.roles.filter((r) => !r.builtin).length)
const shownRoles = computed(() => (props.collapseCustom && !showCustom.value ? props.roles.filter((r) => r.builtin) : props.roles))

const unfolded = ref(false)
const folded = computed(() => props.foldAfter > 0 && !unfolded.value && props.perms.length > props.foldAfter)
const sections = computed(() => groupSections(folded.value ? props.perms.slice(0, props.foldAfter) : props.perms, props.groups))

const offerFor = (action: string) => offerOf(props.scopeLimit(action), action)
const roleName = (r: MatrixRole) => lt(r.name) || r.key
const label = (p: MatrixPerm) => lt(p.label) || p.local

/** What one cell renders, worked out once per cell. */
interface CellView {
  lock: CellLock
  /** The keys it shows: everything for the super admin, else its keys. */
  keys: string[]
  /** Frozen outright (or the whole matrix read-only). */
  fixed: boolean
  /** Fixed, or empty under a take-away-only lock. */
  disabled: boolean
  differs: boolean
  /** Why it is locked; '' when it is not. */
  hint: string
  /** The control's label, with the lock's reason for screen readers. */
  aria: string
}
const cellId = (p: MatrixPerm, r: MatrixRole) => `${p.action}\u0000${r.key}`
const views = computed(() => {
  const out = new Map<string, CellView>()
  for (const p of props.perms) {
    for (const r of props.roles) {
      const lock = props.lockOf(p, r)
      const keys = lock === 'superuser' ? [p.action] : props.matrix.cell(p, r)
      const fixed = props.readOnly || isFixed(lock)
      const hint = lock ? t(LOCK_HINT[lock]) : ''
      out.set(cellId(p, r), {
        lock,
        keys,
        fixed,
        disabled: fixed || (lock !== null && !keys.length),
        differs: props.matrix.differsFromSuggested(p, r),
        hint,
        aria: [label(p), roleName(r), hint].filter(Boolean).join(' · ')
      })
    }
  }
  return out
})
const view = (p: MatrixPerm, r: MatrixRole) => views.value.get(cellId(p, r))!
const cellsOf = (p: MatrixPerm) => props.roles.map((r) => view(p, r))

const requiresText = (p: MatrixPerm) => (p.requires ?? []).map((k) => {
  const req = props.perms.find((x) => x.action === k)
  return req ? label(req) : k
}).join(t('roles.listSep'))
const differing = computed(() => [...views.value.values()].filter((v) => v.differs).length)
const readonlyColumns = computed(() => new Set(props.roles.filter((r) => props.perms.length > 0 && props.perms.every((p) => view(p, r).lock === 'role_readonly')).map((r) => r.key)))
const anyPending = computed(() => [...views.value.values()].some((v) => v.lock === 'pending_review'))
/** A lock icon in the cell: frozen by the cell itself, not by its column (whose heading shows it) nor marked otherwise. */
const lockIcon = (r: MatrixRole, v: CellView) => isFixed(v.lock) && v.lock !== 'superuser' && v.lock !== 'pending_review' && !readonlyColumns.value.has(r.key)

// A heading row's checkbox: the group's shown rows the role's cell may take (useGrantMatrix.groupRows).
const groupState = (perms: MatrixPerm[], r: MatrixRole) => props.matrix.groupState(perms, r, offerFor)
const groupOpen = (perms: MatrixPerm[], r: MatrixRole) => !props.readOnly && props.matrix.groupRows(perms, r, offerFor).length > 0

// More roles than fit scroll inside the card under the fixed first column; a
// fade on the right edge says so while columns are hidden past it.
const scroller = ref<HTMLElement>()
const table = ref<HTMLElement>()
const clipped = ref(false)
const measure = () => {
  const e = scroller.value
  clipped.value = !!e && e.scrollLeft + e.clientWidth < e.scrollWidth - 1
}
const resized = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(measure)
onMounted(() => {
  for (const el of [scroller.value, table.value]) if (el) resized?.observe(el)
  measure()
})
onBeforeUnmount(() => resized?.disconnect())

const expanded = ref<{ action: string; role: string } | null>(null)
const expandedCell = computed(() => {
  const e = expanded.value
  const perm = e && props.perms.find((p) => p.action === e.action)
  const role = e && props.roles.find((r) => r.key === e.role)
  return perm && role ? { perm, role } : null
})
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

    <div class="relative">
      <div ref="scroller" class="overflow-x-auto rounded-xl border border-line" @scroll.passive="measure">
        <table ref="table" class="w-full border-collapse text-sm">
          <thead>
            <tr class="border-b border-line">
              <th scope="col" class="sticky left-0 z-[1] min-w-[9rem] max-w-[11rem] bg-surface px-3 py-2 text-left font-medium text-fg-subtle md:min-w-[13rem] md:max-w-[18rem]">
                {{ t('plugins.matrix.permission') }}
              </th>
              <th
                v-for="r in shownRoles"
                :key="r.key"
                scope="col"
                class="min-w-[5.5rem] max-w-[8rem] px-1.5 py-2 text-center font-medium text-fg-subtle"
                :title="r.superuser ? t('plugins.matrix.superFixed') : readonlyColumns.has(r.key) ? t('plugins.matrix.roleReadonly') : roleName(r)"
                :data-role="r.key"
              >
                <span class="inline-flex max-w-full items-center gap-1">
                  <span class="truncate">{{ roleName(r) }}</span>
                  <SIcon v-if="readonlyColumns.has(r.key)" name="lock" class="h-3 w-3 shrink-0" />
                </span>
              </th>
              <th v-if="$slots['trailing-head']" scope="col" class="px-2 py-2 text-left font-medium text-fg-subtle"><slot name="trailing-head" /></th>
            </tr>
          </thead>
          <tbody v-for="s in sections" :key="s.key" class="border-b border-line last:border-b-0">
            <tr v-if="s.label" class="group bg-surface-2" :data-group="s.key">
              <th scope="rowgroup" class="sticky left-0 z-[1] bg-surface-2 px-3 py-1.5 text-left text-xs font-medium text-fg-subtle">{{ s.label }}</th>
              <td v-for="r in shownRoles" :key="r.key" class="px-1.5 py-1.5 text-center">
                <SCheckbox
                  v-if="!r.superuser"
                  bare
                  size="xs"
                  :model-value="groupState(s.perms, r) === 'all'"
                  :indeterminate="groupState(s.perms, r) === 'some'"
                  :disabled="!groupOpen(s.perms, r)"
                  :aria-label="`${t('roles.groupSelectAll')} · ${s.label} · ${roleName(r)}`"
                  :data-group-role="r.key"
                  @update:model-value="matrix.toggleGroup(s.perms, r, offerFor)"
                />
              </td>
              <td v-if="$slots['trailing-head']" />
            </tr>
            <tr v-for="p in s.perms" :key="p.action" class="group border-t border-line first:border-t-0" :data-permission="p.action">
              <th scope="row" class="sticky left-0 z-[1] min-w-[9rem] max-w-[11rem] bg-surface px-3 py-2 text-left font-normal group-hover:bg-surface-2 md:min-w-[13rem] md:max-w-[18rem]" :title="permHint(p.action, p.description)">
                <div class="flex min-w-0 flex-wrap items-center gap-1.5">
                  <span class="line-clamp-2 text-fg" :class="p.status === 'removed' ? 'text-fg-subtle line-through' : ''">{{ label(p) }}</span>
                  <PermRiskBadge :risk="p.risk" />
                  <SBadge v-if="p.status === 'removed'" size="xs" tone="gray">{{ t('roles.removed') }}</SBadge>
                  <button
                    v-if="!readOnly && cellsOf(p).some((v) => v.differs)"
                    type="button"
                    class="ml-auto shrink-0 rounded p-0.5 text-fg-subtle hover:text-fg"
                    :title="t('roles.restoreSuggested')"
                    :aria-label="t('roles.restoreSuggested')"
                    data-testid="matrix-restore-row"
                    @click="matrix.restore(p)"
                  ><SIcon name="refresh" class="h-3.5 w-3.5" /></button>
                </div>
                <SHint v-if="cellsOf(p).some((v) => v.lock === 'critical')" size="xs" class="mt-0.5 text-danger-600 dark:text-danger-400">{{ t('plugins.matrix.criticalOnly') }}</SHint>
                <SHint v-if="p.inheritFrom?.length" size="xs" class="mt-0.5" data-testid="matrix-inherited">
                  {{ t('plugins.matrix.inheritedFrom', { keys: p.inheritFrom.join(t('roles.listSep')) }) }}
                </SHint>
                <SHint v-if="p.requires?.length" size="xs" class="mt-0.5">{{ t('roles.requiresHint', { names: requiresText(p) }) }}</SHint>
                <SHint size="xs" class="mt-0.5 hidden truncate font-mono md:block">{{ p.action }}</SHint>
              </th>
              <td
                v-for="r in shownRoles"
                :key="r.key"
                class="px-1.5 py-2 text-center"
                :class="view(p, r).lock === 'pending_review' ? 'bg-warning-50 dark:bg-warning-900/20' : ''"
                :title="view(p, r).hint || undefined"
                :data-role="r.key"
                :data-lock="view(p, r).lock ?? undefined"
              >
                <!-- The dot sits on the control's corner, so it reads as this cell's whatever the row's height. -->
                <span class="relative inline-flex max-w-full" :class="p.scope ? 'w-full' : ''">
                  <SCheckbox
                    v-if="!p.scope"
                    bare
                    size="xs"
                    :model-value="view(p, r).keys.length > 0"
                    :disabled="view(p, r).disabled"
                    :aria-label="view(p, r).aria"
                    @update:model-value="matrix.setCell(p, r, $event ? [p.action] : [])"
                  />
                  <MatrixScopeCell
                    v-else
                    class="w-full"
                    :perm="p"
                    :keys="view(p, r).keys"
                    :offer="offerFor(p.action)"
                    :shrink-only="isShrinkOnly(view(p, r).lock)"
                    :disabled="view(p, r).disabled"
                    :fixed="view(p, r).fixed"
                    :label="view(p, r).aria"
                    @update="matrix.setCell(p, r, $event)"
                    @open="expanded = { action: p.action, role: r.key }"
                  />
                  <span
                    v-if="view(p, r).differs"
                    class="absolute -right-1.5 -top-1 h-1.5 w-1.5 rounded-full bg-primary-500 dark:bg-primary-400"
                    role="img"
                    :title="t('plugins.matrix.differs')"
                    :aria-label="t('plugins.matrix.differs')"
                    data-testid="cell-differs"
                  />
                </span>
                <SBadge v-if="view(p, r).lock === 'pending_review'" size="xs" tone="warning" class="mt-1">{{ t('plugins.matrix.pendingReview') }}</SBadge>
                <SIcon v-else-if="lockIcon(r, view(p, r))" name="lock" class="ml-1 inline h-3 w-3 text-fg-subtle" />
              </td>
              <td v-if="$slots['trailing-head']" class="px-2 py-2"><slot name="trailing" :perm="p" /></td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="clipped" class="pointer-events-none absolute inset-y-px right-px w-10 rounded-r-xl bg-gradient-to-l from-surface to-transparent" data-testid="matrix-clipped" />
    </div>

    <SButton v-if="foldAfter > 0 && perms.length > foldAfter" size="xs" variant="ghost" data-testid="matrix-fold" @click="unfolded = !unfolded">
      {{ folded ? t('plugins.matrix.foldMore', { n: perms.length - foldAfter }) : t('plugins.matrix.foldLess') }}
    </SButton>

    <div v-if="expandedCell" class="w-full space-y-2 rounded-xl border border-line p-3" data-testid="matrix-scope-panel">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span class="text-sm font-medium text-fg">{{ t('plugins.matrix.scopeOf', { role: roleName(expandedCell.role), perm: label(expandedCell.perm) }) }}</span>
        <SButton size="xs" variant="secondary" data-testid="matrix-scope-done" @click="expanded = null">{{ t('plugins.matrix.done') }}</SButton>
      </div>
      <ScopePicker
        :action="expandedCell.perm.action"
        resource=""
        :plugin="{ key: pluginKey, local: expandedCell.perm.local, scope: expandedCell.perm.scope }"
        :scopes="scopesOfCell(matrix.cell(expandedCell.perm, expandedCell.role), expandedCell.perm.action)"
        :offer="offerFor(expandedCell.perm.action)"
        :disabled="view(expandedCell.perm, expandedCell.role).disabled"
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
