<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { custody, describeCustody, type CustodyBreak } from '../api/custody'

const breaks = ref<CustodyBreak[]>([])
const loading = ref(true)
const error = ref('')

onMounted(async () => {
  try { breaks.value = await custody.breaks() }
  catch (e) { error.value = describeCustody(e) }
  finally { loading.value = false }
})

function age(seconds: number): string {
  if (seconds >= 86_400) return `${Math.floor(seconds / 86_400)}d`
  if (seconds >= 3_600) return `${Math.floor(seconds / 3_600)}h`
  return `${Math.floor(seconds / 60)}m`
}
</script>

<template>
  <h1>Custody breaks</h1>
  <p class="muted">Outstanding differences between the investment book of record and custodian statements. This page is read-only.</p>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <div v-else class="table-scroll"><table><thead><tr><th>Age</th><th>Kind / key</th><th>IBOR</th><th>Custodian</th><th>Difference</th><th>Status</th><th>Assignee</th><th>Last detected</th><th>Explanation</th></tr></thead>
    <tbody><tr v-for="item in breaks" :key="item.break_id"><td :class="item.age_seconds >= 86_400 ? 'state-warn' : ''">{{ age(item.age_seconds) }}</td><td>{{ item.kind }}<br /><code>{{ item.key }}</code></td><td>{{ item.ibor }}</td><td>{{ item.custodian }}</td><td>{{ item.difference }}</td><td>{{ item.status }}</td><td>{{ item.assignee || 'Unassigned' }}</td><td>{{ item.last_seen_at }}</td><td>{{ item.explanation || '—' }}</td></tr>
    <tr v-if="breaks.length === 0"><td colspan="9">No outstanding custody breaks were returned by the reconciliation service.</td></tr></tbody></table></div>
</template>
