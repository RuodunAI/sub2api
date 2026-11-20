<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { SEmpty, SPageHeader, SSpinner } from '@sub2api/ui'
import { usePluginStore, assetURL } from '@/stores/plugins'
import { lt } from '@/i18n'
import PluginComponent from '@/components/plugin/PluginComponent.vue'
import PluginIframe from '@/components/plugin/PluginIframe.vue'

const route = useRoute()
const plugins = usePluginStore()
const { t } = useI18n()

const pluginKey = computed(() => String(route.params.plugin))
const pageId = computed(() => String(route.params.page))
const found = computed(() => plugins.page(pluginKey.value, pageId.value))

const title = computed(() => {
  const f = found.value
  if (!f) return pluginKey.value
  if (f.page.title) return lt(f.page.title)
  const menu = f.plugin.menus?.find((m) => m.page === pageId.value)
  return menu ? lt(menu.label) : lt(f.plugin.name) || f.plugin.key
})

const nativeComponent = computed(() => (found.value?.page.type === 'native' ? plugins.component(pluginKey.value, found.value.page.component) : undefined))
</script>

<template>
  <div>
    <!-- native pages render their own header -->
    <SPageHeader v-if="!nativeComponent" :title="title" :description="found ? `${found.plugin.key} v${found.plugin.version}` : undefined" />
    <!-- A native page waits for its plugin's module in PluginComponent; other plugins still loading do not hold it up. -->
    <div v-if="!plugins.listed" class="flex justify-center py-16"><SSpinner /></div>
    <SEmpty v-else-if="!found" :text="t('pluginHost.notFound', { plugin: pluginKey, page: pageId })" icon="puzzle" />
    <!-- The core requires src on iframe pages (manifest check). -->
    <PluginIframe v-else-if="found.page.type === 'iframe'" :plugin-key="found.plugin.key" :page="pageId" :src="assetURL(found.plugin, found.page.src!)" :min-height="480" />
    <PluginComponent v-else-if="found.page.type === 'native'" v-slot="{ component }" :plugin-key="pluginKey" :name="found.page.component">
      <component :is="component" />
    </PluginComponent>
    <!-- An older package's declarative page (table, form). -->
    <SEmpty v-else :text="t('pluginHost.unsupported', { type: found.page.type })" />
  </div>
</template>
