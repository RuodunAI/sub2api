<script setup lang="ts">
// What an upgrade does to the user permissions it does not ask about: the
// ones already in the catalog keep their grants; those removed before and
// declared again come back with theirs (D6); those this version no longer
// declares stay on their holders, not in force. Shown only: the matrix above
// lists just the permissions the consent creates.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SBadge, SCard, SHint } from '@sub2api/ui'
import type { PluginReview, ReviewUserPermission } from '@/api/types'
import { lt } from '@/i18n'
import { asArray } from '../pluginUtil'

const props = defineProps<{ review: PluginReview }>()
const { t } = useI18n()

const perms = computed(() => asArray<ReviewUserPermission>(props.review.user_permissions))
const kept = computed(() => perms.value.filter((p) => !p.new && !p.revived).length)
const revived = computed(() => perms.value.filter((p) => p.revived))
const removed = computed(() => props.review.removed_user_permissions ?? [])
const holders = computed(() => new Set(removed.value.flatMap((p) => p.holders)).size)
</script>

<template>
  <SCard v-if="kept || revived.length || removed.length" class="mt-4" data-testid="upgrade-permission-changes">
    <div class="space-y-3 text-sm">
      <SHint v-if="kept">{{ t('plugins.matrix.upgradeKept', { n: kept }) }}</SHint>
      <div v-if="revived.length" data-testid="upgrade-revived">
        <div class="font-medium">{{ t('plugins.matrix.revivedTitle', { n: revived.length }) }}</div>
        <SHint size="xs" class="mt-0.5">{{ t('plugins.matrix.revivedHint') }}</SHint>
        <div class="mt-1 flex flex-wrap gap-1.5">
          <SBadge v-for="p in revived" :key="p.key" tone="primary" :title="p.key">{{ lt(p.label) || p.key }}</SBadge>
        </div>
      </div>
      <div v-if="removed.length" data-testid="upgrade-removed">
        <div class="font-medium">{{ t('plugins.matrix.removedTitle', { n: removed.length }) }}</div>
        <SHint size="xs" class="mt-0.5">{{ t('plugins.matrix.removedHint', { m: holders }) }}</SHint>
        <div class="mt-1 flex flex-wrap gap-1.5">
          <SBadge v-for="p in removed" :key="p.key" tone="gray" :title="p.key"><span class="line-through">{{ lt(p.label) || p.key }}</span></SBadge>
        </div>
      </div>
    </div>
  </SCard>
</template>
