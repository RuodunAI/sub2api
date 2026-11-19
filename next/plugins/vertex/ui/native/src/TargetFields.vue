<script setup lang="ts">
// project_id and location of an account (where its requests go), shared by
// the vertex account forms; the form decides their texts.
import { SField, SInput, type FormApi } from '@sub2api/ui'

interface Texts {
  label: string
  hint?: string
  placeholder: string
}

defineProps<{ form: FormApi; project: Texts; location: Texts }>()
</script>

<template>
  <SField prop="project_id" :label="project.label" :hint="project.hint">
    <SInput v-bind="form.bind('project_id')" :placeholder="project.placeholder" mono />
  </SField>
  <SField prop="location" :label="location.label" :hint="location.hint">
    <SInput v-bind="form.bind('location')" :placeholder="location.placeholder" mono />
  </SField>
</template>
