<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { audit, describeAudit, type AuditLineage, type AuditRecord } from '../api/audit'

const route = useRoute()
const id = computed(() => String(route.params.id ?? ''))
const record = ref<AuditRecord | null>(null)
const lineage = ref<AuditLineage | null>(null)
const error = ref('')
const lineageError = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    record.value = await audit.event(id.value)
    try { lineage.value = await audit.lineage(id.value) }
    catch (e) { lineageError.value = describeAudit(e) }
  } catch (e) { error.value = describeAudit(e) }
  finally { loading.value = false }
})
</script>

<template>
  <h1>Audit event</h1>
  <p class="muted"><RouterLink to="/audit">Audit search</RouterLink> · <code>{{ id }}</code></p>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <template v-else-if="record">
    <dl class="event-detail">
      <div><dt>Event type</dt><dd>{{ record.event_type }}</dd></div>
      <div><dt>Class</dt><dd>{{ record.event_class || '—' }}</dd></div>
      <div><dt>Domain / kind</dt><dd>{{ record.domain || '—' }} / {{ record.kind || '—' }}</dd></div>
      <div><dt>Source</dt><dd>{{ record.source }}</dd></div>
      <div><dt>Occurred</dt><dd>{{ record.occurred_at }}</dd></div>
      <div><dt>Recorded</dt><dd>{{ record.recorded_at }}</dd></div>
      <div><dt>Correlation</dt><dd><code>{{ record.correlation_id || '—' }}</code></dd></div>
      <div><dt>Direct cause</dt><dd><code>{{ record.causation_id || '—' }}</code></dd></div>
      <div><dt>Schema</dt><dd>{{ record.schema_ref || '—' }}</dd></div>
      <div><dt>Previous hash</dt><dd><code>{{ record.prev_hash || '—' }}</code></dd></div>
      <div><dt>Record hash</dt><dd><code>{{ record.hash }}</code></dd></div>
    </dl>
    <p class="muted">Payload attributes and free-form summaries are intentionally omitted from the browser view.</p>

    <h2>Direct causal ancestry</h2>
    <p v-if="lineageError" class="error" role="alert">{{ lineageError }}</p>
    <ol v-else-if="lineage" class="lineage-list">
      <li v-for="item in lineage.ancestry" :key="item.event_id" :aria-current="item.event_id === id ? 'true' : undefined">
        <RouterLink :to="{ name: 'audit-event', params: { id: item.event_id } }"><code>{{ item.event_id }}</code></RouterLink>
        <span>{{ item.event_type }} · {{ item.occurred_at }}</span>
      </li>
    </ol>
  </template>
</template>
