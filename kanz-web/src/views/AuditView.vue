<script setup lang="ts">
import { ref } from 'vue'
import { audit, describeAudit, type AuditEvent } from '../api/audit'

const correlation = ref('')
const eventType = ref('')
const rows = ref<AuditEvent[]>([])
const loading = ref(false)
const searched = ref(false)
const error = ref('')

async function search() {
  if (loading.value) return
  loading.value = true
  rows.value = []
  error.value = ''
  searched.value = false
  try {
    rows.value = await audit.events(correlation.value, eventType.value)
    searched.value = true
  } catch (e) {
    error.value = describeAudit(e)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <h1>Audit events</h1>
  <p class="muted">Recorded event metadata for your authorized tenant. Search returns at most 100 events; it does not verify the audit hash chain.</p>
  <form @submit.prevent="search">
    <label>Correlation ID<input v-model="correlation" :disabled="loading" name="correlation" /></label>
    <label>Event type<input v-model="eventType" :disabled="loading" name="event-type" /></label>
    <button type="submit" :disabled="loading">{{ loading ? 'Searching…' : 'Search events' }}</button>
  </form>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <template v-else-if="searched">
    <p role="status">{{ rows.length }} events returned.</p>
    <p v-if="rows.length === 100" class="state-warn">Result limit reached. Narrow the search; more matching events may exist.</p>
    <div class="table-scroll">
      <table>
        <thead><tr><th>Occurred at</th><th>Event</th><th>Type</th><th>Kind</th><th>Source</th><th>Correlation</th></tr></thead>
        <tbody>
          <tr v-for="event in rows" :key="event.event_id">
            <td>{{ event.occurred_at }}</td><td><RouterLink :to="{ name: 'audit-event', params: { id: event.event_id } }"><code>{{ event.event_id }}</code></RouterLink></td><td>{{ event.event_type }}</td>
            <td>{{ event.kind }}</td><td>{{ event.source }}</td><td>{{ event.correlation_id || '—' }}</td>
          </tr>
          <tr v-if="rows.length === 0"><td colspan="6">No recorded events matched this search.</td></tr>
        </tbody>
      </table>
    </div>
  </template>
</template>
