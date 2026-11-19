<script setup lang="ts">
// Secret box (API keys, passwords): a password input with show / hide and
// autocomplete="new-password". A stored secret comes back as SECRET_MASK
// ("******"): the box stays empty with "******" as placeholder and a note
// that leaving it empty keeps the value. Typing replaces the secret;
// clearing the box again emits SECRET_MASK, so the core keeps the stored
// value. Other attrs (data-testid, ...) go to the input.
import { computed, ref, watch } from 'vue'
import SHint from './SHint.vue'
import SInput from './SInput.vue'
import { SECRET_MASK } from './form/rules'
import { useUiT } from './i18n'
import type { ControlSize } from './field'

defineOptions({ inheritAttrs: false })
const props = withDefaults(defineProps<{ modelValue?: string | null; placeholder?: string; disabled?: boolean; size?: ControlSize; revealable?: boolean }>(), {
  modelValue: undefined,
  placeholder: undefined,
  size: undefined,
  revealable: true
})
const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()
const t = useUiT()
const kept = computed(() => props.modelValue === SECRET_MASK)
/** A secret is stored: set by a masked value, forgotten when the value is reset (another account). */
const stored = ref(kept.value)
watch(
  () => props.modelValue,
  (v) => {
    if (v === SECRET_MASK) stored.value = true
    else if (v === undefined || v === null) stored.value = false
  }
)

function update(v: unknown) {
  const text = String(v ?? '')
  emit('update:modelValue', text === '' && stored.value ? SECRET_MASK : text)
}
</script>

<template>
  <div class="s-secret-input">
    <SInput
      type="password"
      autocomplete="new-password"
      :revealable="revealable"
      :model-value="kept ? '' : (modelValue ?? '')"
      :placeholder="kept ? SECRET_MASK : placeholder"
      :disabled="disabled"
      :size="size"
      v-bind="$attrs"
      @update:model-value="update"
    />
    <SHint v-if="kept" size="xs">{{ t('ui.form.secretKeep') }}</SHint>
  </div>
</template>

<style scoped>
.s-secret-input {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 0.25rem;
}
</style>
