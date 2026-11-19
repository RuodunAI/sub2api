import { createApiKeyForm } from '@sub2api/ui'
import { text } from './host'
import { ASSET_KEY, ASSET_REGION, OPTIONAL_URL } from './rules'

// The "Volcengine Ark · Official general" account form (type apikey): the API
// key, the official base URL, and the optional asset library fields. base_url
// and asset_base_url are guarded settings: without account:settings:custom
// only the official values are offered.
export const ApiKeyForm = createApiKeyForm({
  apiKey: { placeholder: () => 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx', help: text('apikey.keyHelp') },
  baseUrl: {
    presets: ['https://ark.cn-beijing.volces.com', 'https://ark.ap-southeast.bytepluses.com'],
    placeholder: text('apikey.baseUrlPlaceholder'),
    help: text('apikey.baseUrlHelp')
  },
  extra: [
    {
      key: 'access_key',
      kind: 'text',
      section: text('asset.section'),
      label: text('asset.accessKey'),
      placeholder: text('asset.accessKeyPlaceholder'),
      help: text('apikey.accessKeyHelp'),
      rules: ASSET_KEY
    },
    {
      key: 'secret_key',
      kind: 'secret',
      section: text('asset.section'),
      label: text('asset.secretKey'),
      placeholder: text('asset.secretKeyPlaceholder'),
      help: text('apikey.secretKeyHelp'),
      rules: ASSET_KEY
    },
    {
      key: 'asset_base_url',
      kind: 'url',
      section: text('asset.section'),
      presets: ['https://ark.cn-beijing.volcengineapi.com'],
      label: text('asset.baseUrl'),
      placeholder: text('apikey.assetBaseUrlPlaceholder'),
      help: text('apikey.assetBaseUrlHelp'),
      rules: OPTIONAL_URL
    },
    {
      key: 'asset_region',
      kind: 'text',
      section: text('asset.section'),
      label: text('asset.region'),
      placeholder: text('asset.regionPlaceholder'),
      help: text('apikey.regionHelp'),
      rules: ASSET_REGION
    }
  ]
})
