<script setup lang="ts">
// Deleting an account. The dialog stays open while the request runs.
import { ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { SFormModal } from '@sub2api/ui'
import type { Account } from '@/api/types'
import { deleteResource } from '@/composables/crud'

const props = defineProps<{ open: boolean; account: Account | null }>()
const emit = defineEmits<{ (e: 'update:open', v: boolean): void; (e: 'deleted'): void }>()
const { t } = useI18n()
const messageId = useId()

const busy = ref(false)

async function remove() {
  if (!props.account || busy.value) return
  busy.value = true
  try {
    // The dialog stays busy until it is closed: clearing busy first would make
    // it closable again for a tick while it is still open.
    if (!(await deleteResource(`/accounts/${props.account.id}`))) return
    emit('update:open', false)
    emit('deleted')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <SFormModal
    :open="open"
    :title="t('common.delete')"
    width="sm"
    :describedby="messageId"
    :loading="busy"
    submit-variant="danger"
    :submit-text="t('common.delete')"
    :submit-attrs="{ 'data-testid': 'account-delete-confirm' }"
    :enter-submit="false"
    @update:open="v => emit('update:open', v)"
    @submit="remove"
  >
    <p :id="messageId">{{ t('common.confirmDelete', { name: account?.name ?? '' }) }}</p>
  </SFormModal>
</template>
