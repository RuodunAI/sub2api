<script setup lang="ts">
import { computed } from 'vue'
import { usePlatforms } from '@/composables/platforms'
import PlatformEndpointList from './PlatformEndpointList.vue'

// Endpoints of the given platforms, grouped by platform (e.g. what an API
// key of a group can call). `max` limits endpoints per platform. Whether an
// account serves another platform through protocol conversion is the
// account's choice per model (DESIGN-2026-11-17-PROTOCOL-BINDING §3.2), so
// nothing is derived here; a group's detail lists what its accounts really
// open (GroupServedProtocols).
const props = defineProps<{ ids: string[]; max?: number }>()
const platforms = usePlatforms()
platforms.load()

const own = computed(() => platforms.endpointsOf(props.ids))
</script>

<template>
  <div class="space-y-2" data-testid="platform-endpoints">
    <PlatformEndpointList v-for="g in own" :key="g.id" v-bind="g" :max="max" />
  </div>
</template>
