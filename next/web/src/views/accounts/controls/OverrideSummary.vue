<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { SButton } from '@sub2api/ui'

// An override at a glance, with Clear and Edit: the summary line, its first
// three entries and the editor (the default slot). Shared by the parameter
// override (CONTRACTS §84) and the header override (§108); `ns` is their
// i18n namespace (`<ns>.clear`, `<ns>.edit`), `testid` the prefix of the
// summary's and the Edit button's test ids.
defineProps<{ summary: string; items: string[]; ns: string; testid: string; disabled?: boolean }>()
defineEmits<{ (e: 'clear'): void; (e: 'edit'): void }>()
const { t } = useI18n()
</script>

<template>
  <div class="flex w-full flex-col gap-1.5 sm:w-96">
    <div class="flex items-center gap-2">
      <span
        class="min-w-0 flex-1 truncate text-sm"
        :class="items.length ? 'text-fg' : 'text-fg-muted'"
        :data-testid="`${testid}-summary`"
        >{{ summary }}</span
      >
      <SButton v-if="items.length" size="sm" variant="ghost" :disabled="disabled" @click="$emit('clear')">{{ t(`${ns}.clear`) }}</SButton>
      <SButton size="sm" icon="edit" :disabled="disabled" :data-testid="`${testid}-edit`" @click="$emit('edit')">{{
        t(`${ns}.edit`)
      }}</SButton>
    </div>
    <ol v-if="items.length" class="space-y-0.5 text-xs text-fg-muted">
      <li v-for="(text, i) in items.slice(0, 3)" :key="i" class="truncate">{{ i + 1 }}. {{ text }}</li>
      <li v-if="items.length > 3">…</li>
    </ol>
    <slot />
  </div>
</template>
