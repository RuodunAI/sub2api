<script setup lang="ts">
// Native account form of the three bedrock account types (manifest
// accountTypes[].form {mode: "native", component: "BedrockAccountForm"},
// CONTRACTS §23.5); accountType.type picks the credential fields:
//   bedrock_apikey: api_key
//   bedrock_aksk:   access_key_id, secret_access_key
//   bedrock_sts:    access_key_id, secret_access_key, session_token, session_expires_at
// then the "Region and models" section every type shares (region,
// inference_profile_prefix). A new account starts with the default region
// and prefix. Secrets are SSecretInput: a stored one stays "******" (the core
// keeps it) until replaced. Every key the form does not show goes back as it
// is.
import { computed } from 'vue'
import { fromLocalInput, SCredentialForm, SField, SHint, SInput, SSecretInput, SSectionTitle, SSelect, toLocalInput, useCredentialForm } from '@sub2api/ui'
import type { AccountTypeView, ProxyTicketSource } from '@sub2api/host'
import { useBedrockHost } from './host'
import { DEFAULTS, PREFIXES, RULES, typeOf } from './fields'

const props = withDefaults(
  defineProps<{
    modelValue: Record<string, unknown>
    mode?: 'create' | 'edit'
    account?: { id: number; type: string; credentials: Record<string, unknown>; [key: string]: unknown } | null
    accountType?: AccountTypeView | null
    errors?: Record<string, string>
    proxyTicket?: ProxyTicketSource
  }>(),
  { mode: 'create', account: null, accountType: null, errors: undefined, proxyTicket: undefined }
)
const emit = defineEmits<{ (e: 'update:modelValue', value: Record<string, unknown>): void }>()
const t = useBedrockHost().t

const type = computed(() => typeOf(props.accountType, props.account))
const signed = computed(() => type.value !== 'bedrock_apikey')
const form = useCredentialForm(props, emit, { rules: () => RULES[type.value], defaults: DEFAULTS })
defineExpose({ validate: form.validate })

const prefixes = computed(() => PREFIXES.map((p) => ({ value: p, label: p === 'none' ? t('prefix.none') : p })))
</script>

<template>
  <SCredentialForm :form="form">
    <SField v-if="!signed" prop="api_key" :label="t('apiKey.label')" :hint="t('apiKey.help')">
      <SSecretInput v-bind="form.bind('api_key')" />
    </SField>
    <template v-else>
      <SField prop="access_key_id" :label="t('accessKeyId.label')" :hint="t(`accessKeyId.help.${type}`)">
        <SInput v-bind="form.bind('access_key_id')" :placeholder="type === 'bedrock_sts' ? 'ASIA...' : 'AKIA...'" mono />
      </SField>
      <SField prop="secret_access_key" :label="t('secretAccessKey.label')" :hint="t(`secretAccessKey.help.${type}`)">
        <SSecretInput v-bind="form.bind('secret_access_key')" />
      </SField>
    </template>
    <template v-if="type === 'bedrock_sts'">
      <SField prop="session_token" :label="t('sessionToken.label')" :hint="t('sessionToken.help')">
        <SSecretInput v-bind="form.bind('session_token')" />
      </SField>
      <SField prop="session_expires_at" :label="t('sessionExpiresAt.label')" :hint="t('sessionExpiresAt.help')">
        <!-- RFC 3339 in the value, local time in the box; cleared: the key is not sent. -->
        <SInput
          type="datetime-local"
          :model-value="toLocalInput(form.get('session_expires_at'))"
          @update:model-value="form.set('session_expires_at', fromLocalInput(String($event ?? '')) || undefined)"
        />
      </SField>
    </template>

    <div class="bedrock-section">
      <SSectionTitle :title="t('regionSection.title')" tag="h4" />
      <SHint size="xs">{{ t('regionSection.description') }}</SHint>
    </div>
    <SField prop="region" :label="t('region.label')" :hint="t('region.help')">
      <SInput v-bind="form.bind('region')" placeholder="us-east-1" mono />
    </SField>
    <SField prop="inference_profile_prefix" :label="t('prefix.label')" :hint="t('prefix.help')">
      <!-- Picking the placeholder again leaves the key out (no null). -->
      <SSelect
        :model-value="form.get('inference_profile_prefix')"
        :options="prefixes"
        @update:model-value="form.set('inference_profile_prefix', $event ?? undefined)"
      />
    </SField>
  </SCredentialForm>
</template>

<style scoped>
.bedrock-section {
  border-top: 1px solid rgb(var(--line));
  padding-top: 1rem;
}
</style>
