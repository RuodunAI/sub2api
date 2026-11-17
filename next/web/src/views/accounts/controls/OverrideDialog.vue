<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SButton, SModal, SSegmented } from '@sub2api/ui'

// The frame of an override editor: title and hint (`<ns>.editorTitle`,
// `<ns>.editorHint`), the view switch (`<ns>.view.<view>`; the `view` event asks
// for another view, the editor decides), and Cancel / Apply (`<ns>.applyHint`,
// `<ns>.apply`). Shared by the parameter override (CONTRACTS §84) and the
// header override (§108); `testid` prefixes `-editor`, `-view` and `-apply`.
const open = defineModel<boolean>('open', { required: true })
const props = defineProps<{ ns: string; testid: string; views: readonly string[]; view: string }>()
defineEmits<{ (e: 'view', view: string): void; (e: 'apply'): void }>()
const { t } = useI18n()
const options = computed(() => props.views.map(v => ({ value: v, label: t(`${props.ns}.view.${v}`) })))
</script>

<template>
  <SModal v-model:open="open" width="2xl" fixed persistent :data-testid="`${testid}-editor`">
    <template #header>
      <div class="flex w-full flex-wrap items-start gap-3 pr-8">
        <div class="w-full min-w-0 sm:w-auto sm:flex-1">
          <h3 class="text-base font-semibold text-fg">{{ t(`${ns}.editorTitle`) }}</h3>
          <p class="mt-0.5 text-xs text-fg-muted">{{ t(`${ns}.editorHint`) }}</p>
        </div>
        <SSegmented
          :model-value="view"
          size="sm"
          :options="options"
          :data-testid="`${testid}-view`"
          @update:model-value="$emit('view', String($event))"
        />
      </div>
    </template>

    <slot />

    <template #footer>
      <span class="mr-auto hidden text-xs text-fg-muted sm:inline">{{ t(`${ns}.applyHint`) }}</span>
      <SButton @click="open = false">{{ t('common.cancel') }}</SButton>
      <SButton variant="primary" :data-testid="`${testid}-apply`" @click="$emit('apply')">{{ t(`${ns}.apply`) }}</SButton>
    </template>
  </SModal>
</template>
