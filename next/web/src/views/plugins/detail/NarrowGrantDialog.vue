<script setup lang="ts">
// Narrows a host permission grant: the items of each list in its scope
// ({domains:[…]}, {fields:[…]}) as toggles, at least one kept per list; the
// other keys go back as they are. It only emits the narrowed scope: the
// grants tab sends it (PUT /plugins/:key/grants/:permission) and closes it.
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { SChip, SFormModal, SHint } from '@sub2api/ui'
import { display, type PluginGrant } from '../pluginUtil'

const props = defineProps<{ grant: PluginGrant | null; label: string; loading?: boolean }>()
const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ (e: 'submit', scope: Record<string, unknown>): void }>()
const { t } = useI18n()

/** The lists of the grant's scope: [key, items]. */
const lists = computed(() => Object.entries(props.grant?.scope ?? {}).filter((e): e is [string, unknown[]] => Array.isArray(e[1])))
/** The items taken out, by list; every dialog starts with none. */
const removed = ref<Record<string, unknown[]>>({})
watch(open, (v) => v && (removed.value = {}))

const kept = (key: string, items: unknown[]) => items.filter((x) => !removed.value[key]?.includes(x))
const changed = computed(() => Object.values(removed.value).some((l) => l.length))
function toggle(key: string, item: unknown, keep: boolean) {
  const cur = removed.value[key] ?? []
  removed.value = { ...removed.value, [key]: keep ? cur.filter((x) => x !== item) : [...cur, item] }
}
function submit() {
  emit('submit', { ...props.grant?.scope, ...Object.fromEntries(lists.value.map(([key, items]) => [key, kept(key, items)])) })
}
</script>

<template>
  <SFormModal
    v-model:open="open"
    :title="t('plugins.grants.narrowTitle', { perm: label })"
    width="sm"
    :loading="loading"
    :submit-disabled="!changed"
    :submit-text="t('plugins.grants.narrow')"
    :submit-attrs="{ 'data-testid': 'narrow-submit' }"
    :enter-submit="false"
    @submit="submit"
  >
    <SHint class="mb-3">{{ t('plugins.grants.narrowHint') }}</SHint>
    <div v-for="[key, items] in lists" :key="key" class="mb-3 space-y-1.5" :data-list="key">
      <div class="font-mono text-xs text-fg-subtle">{{ key }}</div>
      <div class="flex flex-wrap gap-1.5">
        <SChip
          v-for="(item, i) in items"
          :key="i"
          :pressed="kept(key, items).includes(item)"
          :disabled="kept(key, items).length === 1 && kept(key, items).includes(item)"
          @update:pressed="toggle(key, item, $event)"
        >{{ display(item) }}</SChip>
      </div>
    </div>
  </SFormModal>
</template>
