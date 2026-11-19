<script setup lang="ts">
// The "Doubao video" account form (type relay): the relay's own base URL and
// endpoints, the optional asset library, and the legacy prefixes behind a
// section that opens by itself while one of its fields holds a value or an
// error. The relay type has no guarded settings.
import { computed, ref } from 'vue'
import { SCredentialForm, SDisclosure, SField, SHint, SInput, SSecretInput, SSectionTitle, useCredentialForm } from '@sub2api/ui'
import type { AccountTypeView, ProxyTicketSource } from '@sub2api/host'
import { useVolcHost } from './host'
import { ASSET_KEY, ASSET_REGION, ENDPOINT, OPTIONAL_URL, PATH_PREFIX } from './rules'

// The §23.5 props (NativeAccountFormProps), written out: the SFC compiler
// cannot resolve a props type imported from another package.
const props = defineProps<{
  modelValue: Record<string, unknown>
  mode: 'create' | 'edit'
  account?: { id: number; type: string; credentials: Record<string, unknown>; [key: string]: unknown } | null
  accountType?: AccountTypeView | null
  errors?: Record<string, string>
  proxyTicket?: ProxyTicketSource
}>()
const emit = defineEmits<{ 'update:modelValue': [Record<string, unknown>] }>()
const host = useVolcHost()
const t = host.t

const form = useCredentialForm(props, emit, {
  rules: {
    api_key: { required: true, min: 8, max: 512, pattern: /^[!-~]+$/ },
    base_url: { required: true, min: 8, pattern: /^https?:\/\/[^\s?#]+$/ },
    video_endpoint: ENDPOINT,
    asset_endpoint: ENDPOINT,
    access_key: ASSET_KEY,
    secret_key: ASSET_KEY,
    asset_region: ASSET_REGION,
    api_prefix: PATH_PREFIX,
    video_api_prefix: PATH_PREFIX,
    asset_base_url: OPTIONAL_URL
  }
})
defineExpose({ validate: form.validate })

const LEGACY = ['api_prefix', 'video_api_prefix', 'asset_base_url']
const toggled = ref<boolean>()
const legacyOpen = computed(() => toggled.value ?? LEGACY.some((k) => !!form.get(k) || !!form.error(k)))
</script>

<template>
  <SCredentialForm :form="form">
    <SField prop="api_key" :label="host.i18n.t('ui.form.apiKey')" :hint="t('relay.keyHelp')">
      <SSecretInput v-bind="form.bind('api_key')" placeholder="sk-..." />
    </SField>
    <SField prop="base_url" :label="host.i18n.t('ui.form.baseUrl')" :hint="t('relay.baseUrlHelp')">
      <SInput v-bind="form.bind('base_url')" placeholder="https://relay.example.com" />
    </SField>
    <SField prop="video_endpoint" :label="t('relay.videoEndpoint')" :hint="t('relay.videoEndpointHelp')">
      <SInput v-bind="form.bind('video_endpoint')" placeholder="/api/v3/contents/generations/tasks" />
    </SField>

    <div>
      <SSectionTitle :title="t('asset.section')" tag="h4" />
      <SHint size="xs">{{ t('asset.sectionHelp') }}</SHint>
    </div>
    <SField prop="asset_endpoint" :label="t('relay.assetEndpoint')" :hint="t('relay.assetEndpointHelp')">
      <SInput v-bind="form.bind('asset_endpoint')" placeholder="/api/support/v1/asset" />
    </SField>
    <SField prop="access_key" :label="t('asset.accessKey')" :hint="t('relay.accessKeyHelp')">
      <SInput v-bind="form.bind('access_key')" :placeholder="t('asset.accessKeyPlaceholder')" />
    </SField>
    <SField prop="secret_key" :label="t('asset.secretKey')" :hint="t('relay.secretKeyHelp')">
      <SSecretInput v-bind="form.bind('secret_key')" :placeholder="t('asset.secretKeyPlaceholder')" clearable />
    </SField>
    <SField prop="asset_region" :label="t('asset.region')" :hint="t('relay.regionHelp')">
      <SInput v-bind="form.bind('asset_region')" :placeholder="t('asset.regionPlaceholder')" />
    </SField>

    <SDisclosure variant="section" :label="t('relay.legacy')" :description="t('relay.legacyHelp')" :open="legacyOpen" data-testid="volcengine-legacy" @update:open="toggled = $event">
      <div class="volcengine-fields">
        <SField prop="api_prefix" :label="t('relay.apiPrefix')" :hint="t('relay.apiPrefixHelp')">
          <SInput v-bind="form.bind('api_prefix')" :placeholder="t('relay.apiPrefixPlaceholder')" />
        </SField>
        <SField prop="video_api_prefix" :label="t('relay.videoApiPrefix')" :hint="t('relay.videoApiPrefixHelp')">
          <SInput v-bind="form.bind('video_api_prefix')" :placeholder="t('relay.videoApiPrefixPlaceholder')" />
        </SField>
        <SField prop="asset_base_url" :label="t('relay.assetBaseUrl')" :hint="t('relay.assetBaseUrlHelp')">
          <SInput v-bind="form.bind('asset_base_url')" placeholder="https://relay.example.com/api/support/v1/asset" />
        </SField>
      </div>
    </SDisclosure>
  </SCredentialForm>
</template>

