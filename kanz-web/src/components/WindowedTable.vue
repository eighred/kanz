<script setup lang="ts" generic="T">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
const props = defineProps<{ items: readonly T[]; rowKey: (item: T) => string; columns: number; label: string; empty: string }>()
const viewport = ref<HTMLElement>()
const scrollTop = ref(0)
const height = ref(384)
const rowHeight = 64
const overscan = 4
// Bound each scroll surface below browser layout-height limits without dropping served rows.
const pageSize = 10000
const page = ref(0)
const pageOffset = computed(() => page.value * pageSize)
const pageItems = computed(() => props.items.slice(pageOffset.value, pageOffset.value + pageSize))
const pageCount = computed(() => Math.ceil(props.items.length / pageSize))
const start = computed(() => Math.max(0, Math.min(pageItems.value.length, Math.floor(scrollTop.value / rowHeight) - overscan)))
const end = computed(() => Math.min(pageItems.value.length, start.value + Math.ceil(height.value / rowHeight) + overscan * 2))
const visible = computed(() => pageItems.value.slice(start.value, end.value))
let observer: ResizeObserver | undefined
onMounted(() => {
  observer = new ResizeObserver(() => { height.value = viewport.value?.clientHeight || 384 })
  if (viewport.value) observer.observe(viewport.value)
})
onBeforeUnmount(() => observer?.disconnect())
function resetScroll() { scrollTop.value = 0; if (viewport.value) viewport.value.scrollTop = 0 }
watch(() => props.items, () => { page.value = 0; resetScroll() })
function changePage(next: number) { page.value = Math.max(0, Math.min(pageCount.value - 1, next)); resetScroll() }
function scroll() { scrollTop.value = viewport.value?.scrollTop ?? 0 }
</script>

<template>
  <p class="muted">{{ items.length }} rows in the served window. Showing {{ items.length ? pageOffset + 1 : 0 }}–{{ Math.min(items.length, pageOffset + pageSize) }}.</p>
  <nav v-if="pageCount > 1" :aria-label="`${label} pages`">
    <button type="button" :disabled="page === 0" @click="changePage(page - 1)">Previous page</button>
    <span>Page {{ page + 1 }} of {{ pageCount }}</span>
    <button type="button" :disabled="page + 1 >= pageCount" @click="changePage(page + 1)">Next page</button>
  </nav>
  <div ref="viewport" class="windowed-table" role="region" :aria-label="label" tabindex="0" @scroll="scroll">
    <table :aria-label="label" :aria-rowcount="items.length + 1">
      <thead><slot name="head" /></thead>
      <tbody>
        <tr v-if="start" aria-hidden="true" class="spacer"><td :colspan="columns" :style="{ height: `${start * rowHeight}px` }" /></tr>
        <tr v-for="(item, offset) in visible" :key="rowKey(item)" class="data-row" :aria-rowindex="pageOffset + start + offset + 2"><slot name="row" :item="item" /></tr>
        <tr v-if="end < pageItems.length" aria-hidden="true" class="spacer"><td :colspan="columns" :style="{ height: `${(pageItems.length - end) * rowHeight}px` }" /></tr>
        <tr v-if="items.length === 0"><td :colspan="columns">{{ empty }}</td></tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.windowed-table { max-height: 384px; overflow: auto; position: relative; }
table { min-width: 100%; border-collapse: separate; border-spacing: 0; margin-top: 0; overflow: visible; }
thead { position: sticky; top: 0; z-index: 1; background: var(--surface, #fff); }
.data-row { height: 64px; }
.data-row :deep(td) { height: 64px; box-sizing: border-box; white-space: nowrap; line-height: 20px; padding: 8px; }
.spacer td { padding: 0; border: 0; }
</style>
