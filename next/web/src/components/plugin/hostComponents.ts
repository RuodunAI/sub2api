import { defineAsyncComponent } from 'vue'
import type { HostComponents } from '@sub2api/host'
import ProxyPicker from '@/components/ProxyPicker.vue'

/**
 * The core account editors (UI 3.7) and the entity pickers (UI 3.11, the
 * multiple one 3.12) handed to plugins as `host.components`
 * (docs/COMMON-CONTROLS.md §3; props in HOST_COMPONENT_NAMES). The editors
 * load on first use, so the console's startup bundle does not carry them. The
 * proxy picker is in it already as a schema widget, and with it the entity
 * pickers it is made of.
 */
export const hostComponents: HostComponents = Object.freeze({
  ParamOverrideEditor: defineAsyncComponent(() => import('@/views/accounts/paramOverride/ParamOverrideInput.vue')),
  RateLimitEditor: defineAsyncComponent(() => import('@/views/accounts/controls/RateLimitEditor.vue')),
  ModelLimitsEditor: defineAsyncComponent(() => import('@/views/accounts/controls/ModelLimitsInput.vue')),
  HeaderPassthroughEditor: defineAsyncComponent(() => import('@/views/accounts/controls/HeaderPassthroughEditor.vue')),
  ProxyPicker,
  ModelMappingEditor: defineAsyncComponent(() => import('@/views/accounts/ModelMappingEditor.vue')),
  EntityPicker: defineAsyncComponent(() => import('@/components/EntityPicker.vue')),
  EntityMultiPicker: defineAsyncComponent(() => import('@/components/EntityMultiPicker.vue')),
  HeaderOverrideEditor: defineAsyncComponent(() => import('@/views/accounts/headerOverride/HeaderOverrideInput.vue'))
})
