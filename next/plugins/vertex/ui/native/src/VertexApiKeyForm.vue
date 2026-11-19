<script setup lang="ts">
// Native account form of vertex_apikey (manifest accountTypes[].form
// {mode: "native", component: "VertexApiKeyForm"}, CONTRACTS §23.5): the API
// key, then the optional project address (project ID and location, both or
// neither; neither is the express address).
import { SCredentialForm, SField, SHint, SSecretInput, SSectionTitle, useCredentialForm } from '@sub2api/ui'
import { useVertexHost } from './host'
import { API_KEY_RULES } from './fields'
import { accountFormProps } from './props'
import TargetFields from './TargetFields.vue'

const props = defineProps(accountFormProps)
const emit = defineEmits<{ (e: 'update:modelValue', value: Record<string, unknown>): void }>()
const t = useVertexHost().t

const form = useCredentialForm(props, emit, { rules: API_KEY_RULES })
defineExpose({ validate: form.validate })
</script>

<template>
  <SCredentialForm :form="form">
    <SField prop="api_key" :label="t('apiKey.label')" :hint="t('apiKey.help')">
      <SSecretInput v-bind="form.bind('api_key')" />
    </SField>
    <div class="vertex-section">
      <SSectionTitle :title="t('express.title')" tag="h4" />
      <SHint size="xs">{{ t('express.description') }}</SHint>
    </div>
    <TargetFields
      :form="form"
      :project="{ label: t('express.project'), placeholder: t('express.placeholder') }"
      :location="{ label: t('express.location'), hint: t('location.help'), placeholder: t('express.placeholder') }"
    />
  </SCredentialForm>
</template>

<style scoped>
.vertex-section {
  border-top: 1px solid rgb(var(--line));
  padding-top: 1rem;
}
</style>
