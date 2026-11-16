<script setup lang="ts">
import { SHint } from '@sub2api/ui'

// A user a row names (its owner, its creator): the email, with the id on
// hover; the id alone where the email is not known (a deleted user); a dash
// for none.
defineProps<{ id?: number | null; email?: string | null }>()
</script>

<template>
  <span v-if="email" class="text-xs" :title="id ? `#${id}` : undefined">{{ email }}</span>
  <SHint v-else-if="id" inline size="xs">#{{ id }}</SHint>
  <SHint v-else inline>—</SHint>
</template>
