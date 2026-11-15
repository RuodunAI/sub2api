<script setup lang="ts">
// A plugin's user permissions × roles, shared by the install / upgrade
// consent page and the plugin detail "Permissions" tab. It emits nothing and
// takes its rows, columns, locks and the editor's scopes from `matrix`
// (useGrantMatrix), which the page owns and reads (`matrix.value`,
// `matrix.changes`, `matrix.dirty`) to save or build its payload.
//
// Rows follow the matrix's perms under their `groups` (a heading row per
// group whose checkbox ticks the group's ordinary rows for one role as
// concrete keys, D1; a critical row is ticked on its own); permissions of no
// listed group come last. An unscoped cell is a checkbox; a scoped one
// MatrixScopeCell, whose "chosen…" opens ScopePicker below the table,
// outside the scroll area, so phones do not cut it off; it scrolls into view
// and takes the focus. A mark on a cell's corner says it differs from the
// plugin's suggestion; a locked cell says why in its control's label and
// title, and the legend explains the lock for touch screens. On phones the
// super admin column (fixed at everything) gives way to a line saying so.
//
// Slots: #toolbar (end of the toolbar), #trailing-head and #trailing="{ perm }"
// (one more column, e.g. the upgrade's "needs confirmation").
import { computed, nextTick, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SBadge, SButton, SCheckbox, SHint, SIcon, toast, useScrollClip } from '@sub2api/ui'
import type { PermissionGroup } from '@/api/types'
import { lt } from '@/i18n'
import { cellOfScopes, scopesOfCell } from '@/authz/pluginGrants'
import ScopePicker from './ScopePicker.vue'
import PermRiskBadge from './PermRiskBadge.vue'
import MatrixScopeCell from './MatrixScopeCell.vue'
import { groupSections, permHint } from './permRows'
import { isFixed, type CellLock, type CellState, type GrantMatrix, type MatrixPerm, type MatrixRole } from './useGrantMatrix'

const props = withDefaults(
  defineProps<{
    matrix: GrantMatrix
    groups?: PermissionGroup[]
    pluginKey: string
    /** Custom roles fold under "more roles" (consent page), unless one of their cells grants something. */
    collapseCustom?: boolean
    /** Rows shown before "N more" (0: all). */
    foldAfter?: number
  }>(),
  { groups: () => [], collapseCustom: false, foldAfter: 0 }
)
const { t } = useI18n()

const LOCK_HINT: Record<Exclude<CellLock, null>, string> = {
  superuser: 'plugins.matrix.superFixed',
  pending_review: 'plugins.matrix.pendingReviewHint',
  critical: 'plugins.matrix.criticalOnly',
  not_held: 'roles.notHeld',
  role_readonly: 'plugins.matrix.roleReadonly',
  removed: 'roles.removedHint',
  holders_hidden: 'plugins.matrix.holdersHidden'
}

const perms = computed(() => props.matrix.perms.value)
const roles = computed(() => props.matrix.roles.value)
const readOnly = computed(() => props.matrix.readOnly.value)
const state = (p: MatrixPerm, r: MatrixRole) => props.matrix.state(p, r)

const roleName = (r: MatrixRole) => lt(r.name) || r.key
const label = (p: MatrixPerm) => lt(p.label) || p.local
const hint = (v: CellState) => (v.lock ? t(LOCK_HINT[v.lock]) : '')
/** The control's label, with the lock's reason for screen readers. */
const aria = (p: MatrixPerm, r: MatrixRole) => [label(p), roleName(r), hint(state(p, r))].filter(Boolean).join(' · ')

// Custom roles fold away unless a cell of theirs grants something or waits
// for review: a grant must not hide in a column nobody opened.
const customRoles = computed(() => roles.value.filter((r) => !r.builtin))
const customGrants = computed(() => customRoles.value.reduce((n, r) => n + perms.value.filter((p) => state(p, r).keys.length || state(p, r).lock === 'pending_review').length, 0))
const showCustom = ref<boolean | null>(null)
const customShown = computed(() => showCustom.value ?? customGrants.value > 0)
const shownRoles = computed(() => (props.collapseCustom && !customShown.value ? roles.value.filter((r) => r.builtin) : roles.value))
/** The super admin column is fixed at everything: phones drop it for a line saying so. */
const columnClass = (r: MatrixRole) => (r.superuser ? 'hidden md:table-cell' : '')
const hasSuper = computed(() => roles.value.some((r) => r.superuser))

const unfolded = ref(false)
const folded = computed(() => props.foldAfter > 0 && !unfolded.value && perms.value.length > props.foldAfter)
const sections = computed(() => groupSections(folded.value ? perms.value.slice(0, props.foldAfter) : perms.value, props.groups))

const readonlyColumns = computed(() => new Set(roles.value.filter((r) => perms.value.length > 0 && perms.value.every((p) => state(p, r).lock === 'role_readonly')).map((r) => r.key)))
const anyPending = computed(() => perms.value.some((p) => roles.value.some((r) => state(p, r).lock === 'pending_review')))
/** A lock icon in the cell: frozen by the cell itself, not by its column (whose heading shows it) nor marked otherwise. */
const lockIcon = (r: MatrixRole, v: CellState) => isFixed(v.lock) && v.lock !== 'superuser' && v.lock !== 'pending_review' && v.lock !== 'holders_hidden' && !readonlyColumns.value.has(r.key)
/** An inherited cell whose holders the viewer may not see: a badge in place of a control that would read "not granted". */
const anyHoldersHidden = computed(() => perms.value.some((p) => shownRoles.value.some((r) => state(p, r).lock === 'holders_hidden')))
const hasCritical = (rows: readonly MatrixPerm[]) => rows.some((p) => p.risk === 'critical')
const anyLock = computed(() => readonlyColumns.value.size > 0 || perms.value.some((p) => shownRoles.value.some((r) => lockIcon(r, state(p, r)))))
const rowDiffers = (p: MatrixPerm) => roles.value.some((r) => props.matrix.differs(p, r))

function restore(p?: MatrixPerm) {
  const skipped = props.matrix.restore(p)
  if (skipped) toast(t('roles.restoreSkipped', { n: skipped }, skipped), 'info')
}

// A heading row's checkbox: the group's ordinary rows the role's cell may take (useGrantMatrix.groupRows).
const groupOpen = (rows: MatrixPerm[], r: MatrixRole) => !readOnly.value && props.matrix.groupRows(rows, r).length > 0

// More roles than fit scroll inside the card under the fixed first column; a
// fade on the right edge says so while columns are hidden past it.
const scroller = ref<HTMLElement>()
const table = ref<HTMLElement>()
const clipped = useScrollClip(scroller, table)

const expanded = ref<{ action: string; role: string } | null>(null)
const expandedCell = computed(() => {
  const e = expanded.value
  const perm = e && perms.value.find((p) => p.action === e.action)
  const role = e && roles.value.find((r) => r.key === e.role)
  return perm && role ? { perm, role } : null
})
const editing = (p: MatrixPerm, r: MatrixRole) => expanded.value?.action === p.action && expanded.value.role === r.key
const panel = ref<HTMLElement>()
/** Opens a cell's scope picker below the table and brings it into view: the select alone would not show it changed. */
async function openScope(p: MatrixPerm, r: MatrixRole) {
  expanded.value = { action: p.action, role: r.key }
  await nextTick()
  panel.value?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  panel.value?.focus({ preventScroll: true })
}
</script>

<template>
  <div class="space-y-3" data-testid="perm-matrix">
    <div class="flex flex-wrap items-center gap-2">
      <SHint v-if="matrix.differing.value" inline size="xs" data-testid="matrix-differs">{{ t('roles.differsCount', { n: matrix.differing.value }, matrix.differing.value) }}</SHint>
      <SButton v-if="matrix.differing.value && !readOnly" size="xs" variant="secondary" icon="refresh" data-testid="matrix-restore" @click="restore()">{{ t('roles.restoreSuggested') }}</SButton>
      <SButton v-if="collapseCustom && customRoles.length" size="xs" variant="ghost" data-testid="matrix-more-roles" @click="showCustom = !customShown">
        {{ customShown ? t('plugins.matrix.fewerRoles') : customGrants ? t('plugins.matrix.moreRolesGrants', { n: customGrants }, customGrants) : t('plugins.matrix.moreRoles') }}
        <SIcon name="chevron-down" class="h-3.5 w-3.5 transition-transform" :class="customShown ? 'rotate-180' : ''" />
      </SButton>
      <slot name="toolbar" />
    </div>
    <SHint v-if="hasSuper" size="xs" class="md:hidden" data-testid="matrix-super-note">{{ t('plugins.matrix.superFixedNote') }}</SHint>

    <div class="relative">
      <div ref="scroller" class="overflow-x-auto rounded-xl border border-line">
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
                :class="columnClass(r)"
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
              <td v-for="r in shownRoles" :key="r.key" class="px-1.5 py-1.5 text-center" :class="columnClass(r)">
                <SCheckbox
                  v-if="!r.superuser"
                  bare
                  size="xs"
                  :model-value="matrix.groupState(s.perms, r) === 'all'"
                  :indeterminate="matrix.groupState(s.perms, r) === 'some'"
                  :disabled="!groupOpen(s.perms, r)"
                  :aria-label="`${t('roles.groupSelectAll')} · ${s.label} · ${roleName(r)}`"
                  :title="hasCritical(s.perms) ? t('roles.groupSkipsCritical') : undefined"
                  :data-group-role="r.key"
                  @update:model-value="matrix.toggleGroup(s.perms, r)"
                />
              </td>
              <td v-if="$slots['trailing-head']" />
            </tr>
            <tr v-for="p in s.perms" :key="p.action" class="group border-t border-line first:border-t-0" :data-permission="p.action">
              <th scope="row" class="sticky left-0 z-[1] min-w-[9rem] max-w-[11rem] bg-surface px-3 py-2 text-left font-normal group-hover:bg-surface-2 md:min-w-[13rem] md:max-w-[18rem]" :title="permHint(p.action, p.description)">
                <div class="flex min-w-0 flex-wrap items-center gap-1.5">
                  <span class="line-clamp-2 text-fg" :class="p.status === 'removed' ? 'text-fg-subtle line-through' : ''">{{ label(p) }}</span>
                  <PermRiskBadge :risk="p.risk" />
                  <SBadge v-if="p.status === 'removed'" size="xs" tone="gray" :title="t('roles.removedHint')" data-testid="matrix-removed">{{ t('roles.removed') }}</SBadge>
                  <SButton
                    v-if="!readOnly && rowDiffers(p)"
                    size="xs"
                    variant="ghost"
                    icon="refresh"
                    icon-only
                    class="ml-auto hidden shrink-0 md:inline-flex"
                    :aria-label="t('roles.restoreSuggested')"
                    :title="t('roles.restoreSuggested')"
                    data-testid="matrix-restore-row"
                    @click="restore(p)"
                  />
                </div>
                <SHint v-if="roles.some((r) => state(p, r).lock === 'critical')" size="xs" class="mt-0.5 text-danger-600 dark:text-danger-400">{{ t('plugins.matrix.criticalOnly') }}</SHint>
                <SHint v-if="p.inheritFrom?.length" size="xs" class="mt-0.5" data-testid="matrix-inherited">
                  {{ t('plugins.matrix.inheritedFrom', { keys: p.inheritFrom.join(t('roles.listSep')) }) }}
                </SHint>
                <SHint v-if="p.requires?.length" size="xs" class="mt-0.5">{{ t('roles.requiresHint', { names: matrix.requiresLabels(p).join(t('roles.listSep')) }) }}</SHint>
                <SHint size="xs" class="mt-0.5 hidden truncate font-mono md:block">{{ p.action }}</SHint>
              </th>
              <td
                v-for="r in shownRoles"
                :key="r.key"
                class="px-1.5 py-2 text-center"
                :class="[columnClass(r), state(p, r).lock === 'pending_review' ? 'bg-warning-50 dark:bg-warning-900/20' : '', editing(p, r) ? 'bg-primary-50 dark:bg-primary-900/20' : '']"
                :title="hint(state(p, r)) || undefined"
                :data-role="r.key"
                :data-lock="state(p, r).lock ?? undefined"
              >
                <SBadge v-if="state(p, r).lock === 'holders_hidden'" size="xs" tone="gray" data-testid="cell-by-inheritance">{{ t('plugins.matrix.byInheritance') }}</SBadge>
                <!-- The mark sits on the control's corner, so it reads as this cell's whatever the row's height. -->
                <span v-else class="relative inline-flex max-w-full" :class="p.scope ? 'w-full' : ''">
                  <SCheckbox
                    v-if="!p.scope"
                    bare
                    size="xs"
                    :model-value="state(p, r).keys.length > 0"
                    :disabled="state(p, r).disabled"
                    :aria-label="aria(p, r)"
                    @update:model-value="matrix.setCell(p, r, $event ? [p.action] : [])"
                  />
                  <MatrixScopeCell
                    v-else
                    class="w-full"
                    :perm="p"
                    :cell="state(p, r)"
                    :offer="matrix.offerFor(p.action)"
                    :editing="editing(p, r)"
                    :label="aria(p, r)"
                    @update="matrix.setCell(p, r, $event)"
                    @open="openScope(p, r)"
                  />
                  <span
                    v-if="matrix.differs(p, r)"
                    class="absolute -right-1.5 -top-1 h-1.5 w-1.5 rounded-full bg-gray-400 dark:bg-gray-500"
                    role="img"
                    :title="t('roles.differs')"
                    :aria-label="t('roles.differs')"
                    data-testid="cell-differs"
                  />
                </span>
                <SBadge v-if="state(p, r).lock === 'pending_review'" size="xs" tone="warning" class="mt-1">{{ t('plugins.matrix.pendingReview') }}</SBadge>
                <SIcon v-else-if="lockIcon(r, state(p, r))" name="lock" class="ml-1 inline h-3 w-3 text-fg-subtle" />
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

    <div v-if="expandedCell" ref="panel" tabindex="-1" class="w-full scroll-mb-24 space-y-2 rounded-xl border border-primary-300 p-3 outline-none dark:border-primary-700" data-testid="matrix-scope-panel">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span class="text-sm font-medium text-fg">{{ t('plugins.matrix.scopeOf', { role: roleName(expandedCell.role), perm: label(expandedCell.perm) }) }}</span>
        <SButton size="xs" variant="secondary" data-testid="matrix-scope-done" @click="expanded = null">{{ t('plugins.matrix.done') }}</SButton>
      </div>
      <ScopePicker
        :action="expandedCell.perm.action"
        :target="{ kind: 'plugin', key: pluginKey, local: expandedCell.perm.local, scope: expandedCell.perm.scope, label: expandedCell.perm.label }"
        :scopes="scopesOfCell(matrix.cell(expandedCell.perm, expandedCell.role), expandedCell.perm.action)"
        :offer="matrix.offerFor(expandedCell.perm.action)"
        :disabled="state(expandedCell.perm, expandedCell.role).disabled"
        @update:scopes="matrix.setCell(expandedCell.perm, expandedCell.role, cellOfScopes(expandedCell.perm.action, $event))"
      />
    </div>

    <SHint size="xs" class="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span>{{ t('roles.riskLegend') }}</span>
      <span class="inline-flex items-center gap-1"><span class="h-1.5 w-1.5 rounded-full bg-gray-400 dark:bg-gray-500" />{{ t('roles.differs') }}</span>
      <span v-if="anyPending" class="inline-flex items-center gap-1"><SBadge size="xs" tone="warning">{{ t('plugins.matrix.pendingReview') }}</SBadge>{{ t('plugins.matrix.pendingReviewHint') }}</span>
      <span v-if="anyLock" class="inline-flex items-center gap-1" data-testid="matrix-lock-legend"><SIcon name="lock" class="h-3 w-3" />{{ t('plugins.matrix.lockLegend') }}</span>
      <span v-if="anyHoldersHidden" class="inline-flex items-center gap-1" data-testid="matrix-inherit-legend"><SBadge size="xs" tone="gray">{{ t('plugins.matrix.byInheritance') }}</SBadge>{{ t('plugins.matrix.holdersHidden') }}</span>
    </SHint>
  </div>
</template>
