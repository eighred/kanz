<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { describeReference, reference, type DataException, type SecurityRecord } from '../api/reference'

const instrumentID = ref('')
const security = ref<SecurityRecord | null>(null)
const securityError = ref('')
const securityLoading = ref(false)
const exceptions = ref<DataException[]>([])
const queueError = ref('')
const queueLoading = ref(true)
const provenance = computed(() => Object.entries(security.value?.provenance ?? {}))

onMounted(async () => {
  try { exceptions.value = await reference.exceptions() }
  catch (e) { queueError.value = describeReference(e) }
  finally { queueLoading.value = false }
})

async function lookup() {
  const id = instrumentID.value.trim()
  if (!id || securityLoading.value) return
  securityLoading.value = true
  security.value = null
  securityError.value = ''
  try { security.value = await reference.security(id) }
  catch (e) { securityError.value = describeReference(e) }
  finally { securityLoading.value = false }
}
</script>

<template>
  <h1>Reference data</h1>
  <p class="muted">Look up the resolved security master and review open data-quality exceptions. Price observation remains unavailable until its read contract is side-effect free.</p>
  <form @submit.prevent="lookup">
    <label>Instrument ID<input v-model="instrumentID" name="instrument-id" required :disabled="securityLoading" /></label>
    <button type="submit" :disabled="securityLoading">{{ securityLoading ? 'Looking up…' : 'Look up security' }}</button>
  </form>
  <p v-if="securityError" class="error" role="alert">{{ securityError }}</p>
  <dl v-else-if="security" class="event-detail">
    <div><dt>Instrument</dt><dd><code>{{ security.instrument_id }}</code></dd></div>
    <div><dt>Description</dt><dd>{{ security.description || 'Unstated' }}</dd></div>
    <div><dt>Asset class</dt><dd>{{ security.asset_class || 'Unstated' }}</dd></div>
    <div><dt>Currency</dt><dd>{{ security.currency_code || 'Unstated' }}</dd></div>
    <div><dt>Issuer</dt><dd>{{ security.issuer_id || 'Unresolved' }}</dd></div>
    <div><dt>Sector</dt><dd>{{ security.sector.name || 'Unresolved' }}<span v-if="security.sector.code" class="muted"> · {{ security.sector.taxonomy }} / {{ security.sector.code }}</span></dd></div>
    <div><dt>As of</dt><dd :class="security.as_of ? '' : 'state-warn'">{{ security.as_of || 'No source timestamp' }}</dd></div>
    <div><dt>Identifiers</dt><dd><span v-for="(value, key) in security.identifiers" :key="key"><template v-if="value"><strong>{{ key.toUpperCase() }}</strong> {{ value }}<br /></template></span></dd></div>
    <div><dt>Field provenance</dt><dd><span v-for="([field, vendor]) in provenance" :key="field"><strong>{{ field }}</strong> {{ vendor }}<br /></span><template v-if="provenance.length === 0">Unstated</template></dd></div>
  </dl>

  <h2>Open data exceptions</h2>
  <p v-if="queueLoading">Loading…</p>
  <p v-else-if="queueError" class="error" role="alert">{{ queueError }}</p>
  <div v-else class="table-scroll"><table><thead><tr><th>Detected</th><th>Exception</th><th>Instrument</th><th>Kind</th><th>Status</th><th>Detail</th><th>Recorded overrides</th></tr></thead>
    <tbody><tr v-for="item in exceptions" :key="item.ID"><td>{{ item.DetectedAt }}</td><td><code>{{ item.ID }}</code></td><td>{{ item.InstrumentID }}</td><td>{{ item.Kind }}</td><td>{{ item.Status }}</td><td>{{ item.Detail }}</td><td>{{ item.overrideCount }}</td></tr>
    <tr v-if="exceptions.length === 0"><td colspan="7">No open reference-data exceptions were returned.</td></tr></tbody></table></div>
</template>
