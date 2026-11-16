<script setup lang="ts">
// Changes the owner (所属) of an account: PUT /accounts/:id/owner
// (account:owner:update, CONTRACTS §103.4). The owner decides who reaches the
// account by an own or team scope; its creator stays as it was.
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@sub2api/host'
import { SField, SFormModal, toast } from '@sub2api/ui'
import type { Account } from '@/api/types'
import EntityPicker from '@/components/EntityPicker.vue'
import { applyFormError } from '@/utils/formErrors'

/** The answer of PUT /accounts/:id/owner. */
export interface AccountOwner {
  id: number
  owner_id: number
  owner_email: string
}

const props = defineProps<{ open: boolean; account: Account | null }>()
const emit = defineEmits<{ (e: 'update:open', v: boolean): void; (e: 'changed', owner: AccountOwner): void }>()
const { t } = useI18n()
const owner = ref('')
const saving = ref(false)
const errors = ref<Record<string, string>>({})

watch(
  () => props.open,
  (v) => {
    if (!v) return
    owner.value = props.account?.owner_id ? String(props.account.owner_id) : ''
    errors.value = {}
  }
)

async function submit() {
  if (!props.account) return
  if (!owner.value) {
    errors.value = { owner_id: t('common.required') }
    return
  }
  saving.value = true
  try {
    emit('changed', await api.put<AccountOwner>(`/accounts/${props.account.id}/owner`, { owner_id: Number(owner.value) }))
    toast(t('accounts.ownerChange.done'), 'success')
    emit('update:open', false)
  } catch (e) {
    applyFormError(e, errors, { not_found: 'accounts.ownerChange.notFound' })
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <SFormModal
    :open="open"
    :title="t('accounts.ownerChange.title', { name: account?.name || '' })"
    :loading="saving"
    data-testid="account-owner-dialog"
    @update:open="emit('update:open', $event)"
    @submit="submit"
  >
    <SField :label="t('accounts.owner')" required :hint="t('accounts.ownerChange.hint')" :error="errors.owner_id">
      <EntityPicker v-model:id="owner" kind="user" data-testid="account-owner-picker" />
    </SField>
  </SFormModal>
</template>
