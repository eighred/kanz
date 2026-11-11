<script setup lang="ts">
import { ref } from 'vue'
import { useSession } from '../stores/session'
import { evidence, reportTemplates, describeEvidence, downloadEvidence, type ReportTemplate, type EvidencePage, type ControlEvidenceReport } from '../api/evidence'
const session = useSession()
const template = ref<ReportTemplate>('authz-decisions')
const page = ref<EvidencePage | null>(null)
const busy = ref(false), error = ref('')
const from = ref(''), to = ref(''), controlsBusy = ref(false), controlsError = ref('')
const controls = ref<ControlEvidenceReport | null>(null)
async function load(next = false) {
  if (busy.value || !session.identity) return
  const selected = next && page.value ? page.value.template : template.value
  const cursor = next ? page.value?.next_cursor ?? '' : ''
  busy.value = true; error.value = ''; page.value = null
  try { page.value = await evidence.page(session.identity.tenant, selected, cursor) }
  catch (e) { error.value = describeEvidence(e) }
  finally { busy.value = false }
}
async function collect() {
  if (controlsBusy.value || !session.identity) return
  controlsBusy.value = true; controlsError.value = ''; controls.value = null
  try { controls.value = await evidence.controls(session.identity.tenant, from.value.trim(), to.value.trim()) }
  catch (e) { controlsError.value = describeEvidence(e) }
  finally { controlsBusy.value = false }
}
</script>
<template>
  <h1>Audit evidence</h1>
  <p class="muted">Bounded evidence for your tenant. This view does not run estate-wide hash verification or provide a compliance opinion.</p>
  <h2>Report metadata</h2>
  <form @submit.prevent="load(false)">
    <label>Report template<select v-model="template" :disabled="busy"><option v-for="name in reportTemplates" :key="name" :value="name">{{ name }}</option></select></label>
    <button type="submit" :disabled="busy">{{ busy ? 'Loading page…' : 'Read first page' }}</button>
  </form>
  <p v-if="error" role="alert" class="error">{{ error }}</p>
  <section v-else-if="page" aria-label="Report result">
    <h3>{{ page.title }}</h3>
    <p>{{ page.count }} records on this page · {{ page.generated_at }} · Tenant {{ page.tenant_id }}</p>
    <p class="state-warn">Integrity not checked. This metadata extract is not a signed attestation.</p>
    <p>{{ page.complete ? 'End of selection reached for this page request.' : 'Partial export: more matching records remain.' }}</p>
    <button v-if="!page.complete" type="button" @click="load(true)">Read next page</button>
    <button type="button" @click="downloadEvidence(page, `audit-${page.template}-page.json`)">Download this page as JSON</button>
    <div class="table-scroll"><table><thead><tr><th>Sequence</th><th>Occurred</th><th>Event</th><th>Type</th><th>Kind</th><th>Source</th></tr></thead>
      <tbody><tr v-for="row in page.records" :key="row.event_id"><td>{{ row.seq }}</td><td>{{ row.occurred_at }}</td><td><RouterLink :to="{ name: 'audit-event', params: { id: row.event_id } }">{{ row.event_id }}</RouterLink></td><td>{{ row.event_type }}</td><td>{{ row.kind }}</td><td>{{ row.source }}</td></tr>
      <tr v-if="page.records.length === 0"><td colspan="6">No matching audit records were returned.</td></tr></tbody>
    </table></div>
  </section>
  <h2>Control evidence window</h2>
  <p class="muted">Provide RFC3339 times with a timezone, for example 2026-01-01T00:00:00Z. Leave the end blank to use the server's current time.</p>
  <form @submit.prevent="collect">
    <label>Window start<input v-model="from" name="window-start" required :disabled="controlsBusy" /></label>
    <label>Window end (optional)<input v-model="to" name="window-end" :disabled="controlsBusy" /></label>
    <button type="submit" :disabled="controlsBusy">{{ controlsBusy ? 'Collecting evidence…' : 'Read control evidence' }}</button>
  </form>
  <p v-if="controlsError" role="alert" class="error">{{ controlsError }}</p>
  <section v-else-if="controls" aria-label="Control evidence result">
    <p>{{ controls.from }} → {{ controls.to }} · {{ controls.total_count }} supporting records · Tenant {{ controls.tenant_id }}</p>
    <p class="state-warn">Control effectiveness is not assessed. Record counts establish evidence presence, not correct operation throughout the period.</p>
    <p>{{ controls.minimums_met ? 'Minimum record counts met for the mapped controls.' : 'Some mapped controls have insufficient supporting records.' }}</p>
    <button type="button" @click="downloadEvidence(controls, 'control-evidence-window.json')">Download this window as JSON</button>
    <div class="table-scroll"><table><thead><tr><th>Control</th><th>Category</th><th>Records</th><th>Minimum</th><th>Evidence presence</th><th>Sample events</th></tr></thead>
      <tbody><tr v-for="control in controls.controls" :key="control.id"><td>{{ control.id }}</td><td>{{ control.category }}</td><td>{{ control.count }}</td><td>{{ control.minimum }}</td><td>{{ control.minimum_met ? 'Minimum met' : 'Insufficient records' }}</td><td><span v-for="id in control.samples" :key="id"><RouterLink :to="{ name: 'audit-event', params: { id } }">{{ id }}</RouterLink><br /></span><span v-if="control.samples.length === 0">No sample events</span></td></tr></tbody>
    </table></div>
  </section>
</template>
