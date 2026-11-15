<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { flip } from './flip'
import { SBadge, SButton, SCheckbox, SEmpty, SHint, SIcon, SSectionTitle, toast } from '@sub2api/ui'
import type { PermissionItem, PermissionModule } from '@/api/types'
import { lt } from '@/i18n'
import { ALL_SCOPE, parsePermission, reduceScopes, type Scope, type ScopeResource } from '@/authz/implies'
import { defaultScopes, offerOf, withScopes } from '@/authz/scopeChoice'
import { cellKeys, isSuggestedRole, localOf, restoreCell, suggestionState, withCell, type SuggestedRole } from '@/authz/pluginGrants'
import PermRiskBadge from '@/components/permission/PermRiskBadge.vue'
import { groupSections, permHint } from '@/components/permission/permRows'
import ScopePicker, { type ScopeTarget } from './ScopePicker.vue'

// Permission checkboxes of a role, one collapsible block per module (core
// modules first, then plugin modules). `allGranted` (superuser) ticks
// everything; changes are emitted as a new set. Permissions marked step_up
// carry a shield: using them needs a step-up verification each time.
// A scoped action (scope_type, CONTRACTS §21) is one row; ticked, its scopes
// open below it (ScopePicker), offered as far as `scopeLimit` (the editor's
// own scopes of the action) implies; without it everything is offered.
// `grantable` (the editor's own keys, in the grant view: a disabled plugin's
// count, useEditorGrants) keeps an unticked permission the editor does not
// hold from being ticked: the server refuses grants beyond the editor. A
// plugin module lists its permissions under their groups, a group ticked as
// a whole for concrete keys (D1); a module's or group's checkbox leaves the
// critical ones out, which are ticked one by one. With `roleKey` a built-in
// role compares each plugin permission with the plugin's suggestion for it
// (defaults) and can put a module back to it, locally: the restore is saved
// with the rest of the page. Comparing and restoring follow the matrix's
// rules (pluginGrants.suggestionState / restoreCell): a cell the editor
// cannot put back whole is neither marked nor touched.
const props = defineProps<{
  modules: PermissionModule[]
  granted: Set<string>
  allGranted?: boolean
  readOnly?: boolean
  scopeLimit?: (action: string) => Scope[]
  /** The role being edited: a built-in one shows and restores the plugins' suggestions. */
  roleKey?: string
  /** Whether the editor may grant a key (useEditorGrants.mayGrant). */
  grantable?: (key: string) => boolean
}>()
const emit = defineEmits<{ (e: 'update:granted', v: Set<string>): void }>()
const { t } = useI18n()

const anyStepUp = computed(() => props.modules.some((m) => m.permissions.some((p) => p.step_up)))
const groups = computed(() => [props.modules.filter((m) => m.source !== 'plugin'), props.modules.filter((m) => m.source === 'plugin')])
const collapsed = ref(new Set<string>())
/** Scoped actions whose scopes stay open while none is chosen (the editor is choosing). */
const opened = ref(new Set<string>())

const scoped = (p: PermissionItem) => !!p.scope_type
/** The granted scopes of each action. */
const grantedScopes = computed(() => {
  const out = new Map<string, Scope[]>()
  for (const k of props.granted) {
    const p = parsePermission(k)
    if (p) out.set(p.action, [...(out.get(p.action) ?? []), p.scope])
  }
  // A scope another granted one implies is not a choice of its own.
  for (const [action, list] of out) out.set(action, reduceScopes(list))
  return out
})
const scopesOf = (key: string) => grantedScopes.value.get(key) ?? []
const offerFor = (key: string) => offerOf(props.scopeLimit ? props.scopeLimit(key) : [ALL_SCOPE], key)
const isChecked = (key: string) => !!props.allGranted || props.granted.has(key) || scopesOf(key).length > 0
const showScopes = (p: PermissionItem) => scoped(p) && !props.allGranted && (isChecked(p.key) || opened.value.has(p.key))
/** Whose scopes a scoped action picks: a plugin action's own resource (its manifest scope), else a core resource's rows. */
const pickerTarget = (m: PermissionModule, p: PermissionItem): ScopeTarget =>
  m.plugin_key && p.scope_type?.startsWith('plugin:')
    ? { kind: 'plugin', key: m.plugin_key, local: localOf(p.key), scope: p.scope, label: p.label }
    : { kind: 'core', resource: p.scope_type as ScopeResource }

/** granted with p on or off; a scoped action goes on at its widest offered scope. */
function withItem(granted: Set<string>, p: PermissionItem, on: boolean): Set<string> {
  if (!scoped(p)) return flip(granted, p.key, on)
  if (on && isChecked(p.key)) return granted
  // Ticked, the scopes stay open while being chosen; unticked, they close.
  opened.value = flip(opened.value, p.key, on)
  return withScopes(granted, p.key, on ? defaultScopes(offerFor(p.key)) : [])
}
const grantedCount = (perms: PermissionItem[]) => perms.filter((p) => isChecked(p.key)).length
const inactive = (status: string) => status !== 'active' && status !== ''
const moduleInactive = (m: PermissionModule) => inactive(m.status)
const permInactive = (m: PermissionModule, p: PermissionItem) => moduleInactive(m) || inactive(p.status)
/** The editor may grant p: holds it, or (a scoped action) holds some scope of it, which ScopePicker offers. */
const mayGrant = (p: PermissionItem) => !props.grantable || props.grantable(p.key) || (scoped(p) && !!props.scopeLimit?.(p.key).length)
// A ticked permission can always be unticked. A removed one can only be
// unticked, and so can one the editor may not grant (a disabled plugin's
// stays selectable to its holders: grantable is the grant view).
const selectable = (p: PermissionItem) => isChecked(p.key) || (p.status !== 'removed' && mayGrant(p))
const notHeld = (p: PermissionItem) => !props.allGranted && !props.readOnly && p.status !== 'removed' && !selectable(p)

/** What a module's or group's checkbox ticks: the selectable permissions but the critical ones (D1), ticked one by one. */
const bulk = (perms: PermissionItem[]) => perms.filter((p) => selectable(p) && p.risk !== 'critical')
// "All" means every one the checkbox ticks.
function stateOf(perms: PermissionItem[]): 'all' | 'some' | 'none' {
  const rows = bulk(perms)
  const n = rows.filter((p) => isChecked(p.key)).length
  if (n === 0) return 'none'
  return n === rows.length ? 'all' : 'some'
}

function toggle(p: PermissionItem) {
  if (props.readOnly || !selectable(p)) return
  emit('update:granted', withItem(props.granted, p, !isChecked(p.key)))
}

/** Ticks or clears the permissions of a module or group its checkbox covers (bulk), as concrete keys (D1). */
function toggleAll(perms: PermissionItem[]) {
  if (props.readOnly) return
  const on = stateOf(perms) !== 'all'
  emit('update:granted', bulk(perms).reduce((s, p) => withItem(s, p, on), props.granted))
}

function setScopes(p: PermissionItem, scopes: Scope[]) {
  // Narrowing (all off, the last chip removed) keeps the scopes open to choose from.
  opened.value = flip(opened.value, p.key, true)
  emit('update:granted', withScopes(props.granted, p.key, scopes))
}

/** Permission names by key, for the requires hints. */
const labels = computed(() => new Map(props.modules.flatMap((m) => m.permissions.map((p) => [p.key, lt(p.label) || p.key]))))
/** Ticked, the keys it needs that are not ticked, by name. */
const unmet = (p: PermissionItem) => (isChecked(p.key) ? (p.requires ?? []).filter((k) => !isChecked(k)).map((k) => labels.value.get(k) ?? k) : [])

// The plugins' suggestions for the edited built-in role.
const suggestedRole = computed<SuggestedRole | null>(() => (!props.allGranted && props.roleKey && isSuggestedRole(props.roleKey) ? props.roleKey : null))
const grantedKeys = computed(() => [...props.granted])
const mayGrantKey = (key: string) => !props.grantable || props.grantable(key)
/** Rows with a suggestion: not critical (none suggested) nor removed (no new grants). */
const suggests = (p: PermissionItem) => !!p.defaults && p.status !== 'removed'
const suggestion = (p: PermissionItem, role: SuggestedRole) => p.defaults?.[role] ?? []
/** The permission against the plugin's suggestion for the role (pluginGrants.suggestionState); null without one. */
function suggestionOf(p: PermissionItem) {
  const role = suggestedRole.value
  return role && suggests(p) ? suggestionState(cellKeys(grantedKeys.value, p.key), suggestion(p, role), mayGrantKey) : null
}
/** "suggested" while granted as the plugin suggests, "differs" while a restore can put it back; null otherwise. */
function mark(p: PermissionItem): 'suggested' | 'differs' | null {
  const st = suggestionOf(p)
  if (st === 'differs') return 'differs'
  return st === 'same' && suggestion(p, suggestedRole.value!).length ? 'suggested' : null
}
const differing = (m: PermissionModule) => m.permissions.filter((p) => mark(p) === 'differs').length

/**
 * Puts a module's permissions back to the plugin's suggestion, locally (saved
 * with the page); a permission the editor cannot put back whole stays as it
 * is and is counted as skipped.
 */
function restoreSuggested(m: PermissionModule) {
  const role = suggestedRole.value
  if (!role || props.readOnly) return
  let skipped = 0
  const keys = m.permissions.filter(suggests).reduce((acc, p) => {
    const next = restoreCell(cellKeys(acc, p.key), suggestion(p, role), mayGrantKey)
    if (!next) skipped++
    return next ? withCell(acc, p.key, next) : acc
  }, grantedKeys.value)
  emit('update:granted', new Set(keys))
  if (skipped) toast(t('roles.restoreSkipped', { n: skipped }, skipped), 'info')
}
</script>

<template>
  <div>
    <SSectionTitle :title="t('roles.permissions')" />
    <SEmpty v-if="!modules.length" :text="t('roles.noPermissions')" />
    <div class="space-y-2">
      <template v-for="(group, gi) in groups" :key="gi">
        <div v-if="gi === 1 && group.length" class="flex items-center gap-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-subtle">
          <span class="h-px flex-1 bg-line" />
          {{ t('roles.pluginPermissions') }}
          <span class="h-px flex-1 bg-line" />
        </div>
        <div v-for="m in group" :key="m.module" class="rounded-xl border border-line" :class="moduleInactive(m) ? 'opacity-60' : ''" :data-module="m.module">
          <div class="flex items-center gap-3 px-3 py-2">
            <button type="button" class="flex min-w-0 flex-1 items-center gap-2 text-left" :aria-expanded="!collapsed.has(m.module)" @click="collapsed = flip(collapsed, m.module)">
              <SIcon :name="collapsed.has(m.module) ? 'chevron-right' : 'chevron-down'" class="h-4 w-4 shrink-0 text-fg-subtle" />
              <span class="truncate text-sm font-medium text-fg">{{ lt(m.label) || m.module }}</span>
              <SHint inline size="xs" class="hidden font-mono sm:inline">{{ m.module }}</SHint>
              <SBadge v-if="m.source === 'plugin' && m.status === 'disabled'" tone="warning">{{ t('roles.pluginDisabledKept') }}</SBadge>
              <SBadge v-else-if="m.status === 'removed'" tone="gray">{{ t('roles.removed') }}</SBadge>
              <SHint inline size="xs" class="ml-auto shrink-0">{{ grantedCount(m.permissions) }}/{{ m.permissions.length }}</SHint>
            </button>
            <SCheckbox
              size="xs"
              class="shrink-0"
              :model-value="stateOf(m.permissions) === 'all'"
              :indeterminate="stateOf(m.permissions) === 'some'"
              :disabled="readOnly || !bulk(m.permissions).length"
              :label="t('roles.selectAll')"
              :title="t('roles.groupSkipsCritical')"
              @update:model-value="toggleAll(m.permissions)"
            />
          </div>
          <div v-if="differing(m)" class="flex flex-wrap items-center gap-2 border-t border-line px-3 py-1.5" data-testid="module-differs">
            <SHint inline size="xs">{{ t('roles.differsCount', { n: differing(m) }, differing(m)) }}</SHint>
            <SButton v-if="!readOnly" size="xs" variant="secondary" icon="refresh" data-testid="module-restore" @click="restoreSuggested(m)">{{ t('roles.restoreSuggested') }}</SButton>
          </div>
          <template v-if="!collapsed.has(m.module)">
            <div v-for="s in groupSections(m.permissions, m.groups)" :key="s.key" class="border-t border-line px-3 py-3" :data-group="s.key || undefined" data-testid="permission-section">
              <div v-if="s.label" class="mb-2 flex items-center gap-3">
                <span class="min-w-0 truncate text-xs font-medium text-fg-subtle">{{ s.label }}</span>
                <span class="h-px flex-1 bg-line" />
                <SCheckbox
                  size="xs"
                  class="shrink-0"
                  :model-value="stateOf(s.perms) === 'all'"
                  :indeterminate="stateOf(s.perms) === 'some'"
                  :disabled="readOnly || !bulk(s.perms).length"
                  :label="t('roles.groupSelectAll')"
                  :title="t('roles.groupSkipsCritical')"
                  data-testid="group-select-all"
                  @update:model-value="toggleAll(s.perms)"
                />
              </div>
              <!-- grid-cols-1 (minmax(0, 1fr)) and min-w-0 keep a scope picker from widening the
                   single phone column past the screen. -->
              <div class="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-2 xl:grid-cols-3">
                <!-- SCheckbox's own text span fixes the colour, so the inactive tone goes on the
                     slot content; [&>span]:min-w-0 lets the truncate inside shrink. -->
                <template v-for="p in s.perms" :key="p.key">
                  <SCheckbox
                    class="min-w-0 [&>span]:min-w-0"
                    :model-value="isChecked(p.key)"
                    :disabled="readOnly || !selectable(p)"
                    :data-permission="p.key"
                    @update:model-value="toggle(p)"
                  >
                    <span class="block min-w-0" :class="permInactive(m, p) ? 'text-fg-subtle' : ''" :title="permHint(p.key, p.description)">
                      <span class="flex min-w-0 items-center gap-2">
                        <span class="truncate">{{ lt(p.label) || p.key }}</span>
                        <PermRiskBadge v-if="p.risk !== 'low' || m.source === 'plugin'" :risk="p.risk" class="shrink-0" />
                        <span v-if="p.step_up" class="shrink-0 text-primary-500 dark:text-primary-400" :title="t('roles.stepUp')" :aria-label="t('roles.stepUp')" role="img" data-testid="permission-step-up"><SIcon name="shield" class="h-3.5 w-3.5" /></span>
                        <SBadge v-if="mark(p)" size="xs" class="shrink-0" tone="gray" :dot="mark(p) === 'differs'" :data-mark="mark(p)" data-testid="permission-mark">{{ t(`roles.${mark(p)}`) }}</SBadge>
                        <template v-if="!moduleInactive(m)">
                          <SBadge v-if="p.status === 'disabled'" tone="warning">{{ t('roles.pluginDisabled') }}</SBadge>
                          <SBadge v-else-if="p.status === 'removed'" tone="gray" :title="t('roles.removedHint')" data-testid="permission-removed">{{ t('roles.removed') }}</SBadge>
                        </template>
                      </span>
                      <SHint v-if="notHeld(p)" inline size="xs" class="block truncate" data-testid="permission-not-held">{{ t('roles.notHeld') }}</SHint>
                      <SHint v-else-if="unmet(p).length" inline size="xs" tone="warning" class="block truncate" data-testid="permission-requires">
                        {{ t('roles.requiresHint', { names: unmet(p).join(t('roles.listSep')) }) }}
                      </SHint>
                    </span>
                  </SCheckbox>
                  <ScopePicker
                    v-if="showScopes(p)"
                    class="min-w-0 sm:col-span-2 xl:col-span-3"
                    :action="p.key"
                    :target="pickerTarget(m, p)"
                    :scopes="scopesOf(p.key)"
                    :offer="offerFor(p.key)"
                    :disabled="readOnly"
                    @update:scopes="setScopes(p, $event)"
                  />
                </template>
              </div>
            </div>
          </template>
        </div>
      </template>
    </div>
    <SHint size="xs" class="mt-3">{{ t('roles.riskLegend') }}</SHint>
    <SHint v-if="anyStepUp" size="xs" class="mt-1 flex items-center gap-1">
      <SIcon name="shield" class="h-3.5 w-3.5 text-primary-500 dark:text-primary-400" />{{ t('roles.stepUpLegend') }}
    </SHint>
  </div>
</template>
