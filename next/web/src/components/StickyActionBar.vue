<script setup lang="ts">
// A bar pinned to the bottom of a long form while it scrolls: what is about
// to happen on the left (default slot), the buttons on the right (#actions,
// e.g. SettingsSaveBar). The plugin settings, the plugin's role grants and
// the consent page share it, in the same tokens.
</script>

<template>
  <div class="sticky bottom-0 z-10 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-surface/95 p-3 shadow-lg backdrop-blur">
    <div class="min-w-0 flex-1 text-sm text-fg"><slot /></div>
    <div class="flex shrink-0 flex-wrap gap-2"><slot name="actions" /></div>
  </div>
</template>
