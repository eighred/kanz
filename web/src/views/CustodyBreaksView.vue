<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { custody, describeCustody, describeCustodyAction, type CustodyBreak, type CustodyDecision, type CustodyEvidence } from '../api/custody'
import { ApiError } from '../api/client'

const breaks = ref<CustodyBreak[]>([])
const loading = ref(true)
const error = ref('')
const selected = ref<CustodyBreak | null>(null)
const kind = ref<'claim' | 'explain'>('claim')
const explanation = ref('')
const decision = ref<CustodyDecision | null>(null)
const saving = ref(false)
const actionError = ref('')
const evidence = ref<CustodyEvidence | null>(null)
const explanationField = ref<HTMLTextAreaElement | null>(null)
const reviewButton = ref<HTMLButtonElement | null>(null)
const confirmButton = ref<HTMLButtonElement | null>(null)
const reloadButton = ref<HTMLButtonElement | null>(null)
const receiptMessage = ref<HTMLElement | null>(null)
const errorMessage = ref<HTMLElement | null>(null)
let actionTrigger: HTMLButtonElement | null = null
const figures = ['ibor', 'custodian', 'difference'] as const

function unavailable(item: CustodyBreak): string {
  if (item.values_state === 'unavailable') return 'Exact decimal review unavailable.'
  if (item.values_state !== 'exact') return 'Exact values unavailable; source replay and reconciliation required.'
  return 'Actions unavailable in this deployment.'
}

async function reload() {
  loading.value = true
  error.value = ''
  breaks.value = []
  try { breaks.value = await custody.breaks() }
  catch (e) { error.value = describeCustody(e) }
  finally { loading.value = false }
}
onMounted(reload)

async function choose(item: CustodyBreak, action: 'claim' | 'explain', event: MouseEvent) {
  actionTrigger = event.currentTarget as HTMLButtonElement
  selected.value = { ...item }
  kind.value = action
  explanation.value = ''
  decision.value = null
  actionError.value = ''
  await nextTick()
  const target = action === 'explain' ? explanationField.value : reviewButton.value
  target?.focus()
}
async function review() {
  if (!selected.value?.revision || (kind.value === 'explain' && !explanation.value.trim())) return
  decision.value = { request_id: crypto.randomUUID(), expected_revision: selected.value.revision, action: kind.value, review_contract: 'exact-v1', ...(kind.value === 'explain' ? { explanation: explanation.value } : {}) }
  await nextTick()
  confirmButton.value?.focus()
}
function closeReview() {
  selected.value = null
  decision.value = null
}
async function cancel() {
  closeReview()
  await nextTick()
  const target = actionTrigger?.isConnected ? actionTrigger : reloadButton.value
  target?.focus()
  actionTrigger = null
}
async function confirm() {
  if (saving.value || !selected.value || !decision.value) return
  saving.value = true
  actionError.value = ''
  try {
    evidence.value = await custody.act(selected.value, decision.value)
    closeReview()
    await reload()
  } catch (e) {
    actionError.value = describeCustodyAction(e)
    if (e instanceof ApiError && [400, 403, 404, 409].includes(e.status)) { closeReview(); await reload() }
  } finally {
    saving.value = false
    await nextTick()
    const target = selected.value ? confirmButton.value : actionError.value ? errorMessage.value : receiptMessage.value
    target?.focus()
    if (!selected.value) actionTrigger = null
  }
}

function age(seconds: number): string {
  if (seconds >= 86_400) return `${Math.floor(seconds / 86_400)}d`
  if (seconds >= 3_600) return `${Math.floor(seconds / 3_600)}h`
  return `${Math.floor(seconds / 60)}m`
}
</script>

<template>
  <h1>Custody breaks</h1>
  <p class="muted">Outstanding differences between the investment book of record and custodian statements. Claim an investigation or record an explanation after review. Only reconciliation can resolve a break.</p>
  <p v-if="evidence" ref="receiptMessage" role="status" tabindex="-1">Recorded {{ evidence.action }} for break <code>{{ evidence.break_id }}</code> by {{ evidence.actor }} at {{ evidence.recorded_at }} (revision {{ evidence.before.revision }} → {{ evidence.after.revision }}). Request <code>{{ evidence.request_id }}</code>.</p>
  <p v-if="actionError" ref="errorMessage" class="error" role="alert" tabindex="-1">{{ actionError }}</p>
  <section v-if="selected" class="panel custody-review" aria-label="Custody action review">
    <h2>{{ kind === 'claim' ? 'Claim for my authenticated account' : 'Record explanation' }}</h2>
    <p>Break ID <code>{{ selected.break_id }}</code></p>
    <p><code>{{ selected.key }}</code> · {{ selected.kind }} · Revision {{ selected.revision }} · {{ selected.status }}</p>
    <p>IBOR: {{ selected.ibor }} · Custodian: {{ selected.custodian }} · Difference: {{ selected.difference }}</p>
    <p>Current assignee: {{ selected.assignee || 'Unassigned' }}. Current explanation: {{ selected.explanation || 'None' }}.</p>
    <template v-if="!decision">
      <label v-if="kind === 'explain'">Explanation <textarea ref="explanationField" v-model="explanation" maxlength="4096" /></label>
    </template>
    <template v-else>
      <p v-if="decision.action === 'explain'">Explanation to record: {{ decision.explanation }}</p>
      <p v-else>Ownership will move to the authenticated account making this request.</p>
      <p>Proposed status: {{ decision.action === 'claim' ? 'assigned' : 'explained' }}.</p>
      <p>This records an investigation decision and does not resolve the discrepancy.</p>
      <p class="muted">Request <code>{{ decision.request_id }}</code></p>
    </template>
    <div class="actions">
      <button v-if="!decision" ref="reviewButton" :disabled="kind === 'explain' && !explanation.trim()" @click="review">Review action</button>
      <button v-else ref="confirmButton" :disabled="saving" @click="confirm">{{ saving ? 'Recording…' : 'Confirm reviewed action' }}</button>
      <button :disabled="saving" @click="cancel">Cancel</button>
    </div>
  </section>
  <button ref="reloadButton" :disabled="loading || saving || !!selected" @click="reload">Reload queue</button>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <div v-else class="table-scroll">
    <table>
      <thead><tr><th>Break ID</th><th>Age</th><th>Kind / key</th><th>IBOR</th><th>Custodian</th><th>Difference</th><th>Status</th><th>Assignee</th><th>Last detected</th><th>Detection evidence</th><th>Explanation</th><th>Investigation</th></tr></thead>
      <tbody>
        <tr v-for="item in breaks" :key="item.break_id">
          <th scope="row"><code>{{ item.break_id }}</code></th>
          <td :class="item.age_seconds >= 86_400 ? 'state-warn' : ''">{{ age(item.age_seconds) }}</td>
          <td>{{ item.kind }}<br /><code>{{ item.key }}</code></td>
          <td v-for="field in figures" :key="field">{{ item.values_state === 'exact' ? item[field] : item.values_state === 'unavailable' ? 'Unavailable' : 'Unverified' }}</td>
          <td>{{ item.status }}</td><td>{{ item.assignee || 'Unassigned' }}</td>
          <td>{{ item.last_seen_at }}</td>
          <td>
            <details v-if="item.detection || item.portfolio_id">
              <summary>View detection evidence</summary>
              <dl class="custody-origin">
                <dt>Portfolio</dt><dd>{{ item.portfolio_id }}</dd>
                <dt>Custodian ID</dt><dd>{{ item.custodian_id }}</dd>
                <template v-if="item.detection">
                  <dt>Business date</dt><dd>{{ item.detection.business_date }}</dd>
                  <dt>Reconciliation run</dt><dd><code>{{ item.detection.run_id }}</code></dd>
                  <dt>Statement</dt><dd><code>{{ item.detection.statement_id }}</code></dd>
                  <dt>Journal position</dt><dd>{{ item.detection.journal_position ?? 'Unavailable' }}</dd>
                  <dt>Account scope</dt><dd>{{ item.detection.scope_mode ?? 'Unavailable' }}</dd>
                </template>
              </dl>
              <span v-if="!item.detection">Detection source unavailable.</span>
            </details>
            <span v-else>Detection source unavailable.</span>
          </td>
          <td>{{ item.explanation || '—' }}</td>
          <td>
            <div v-if="item.actions_enabled" class="actions">
              <button :disabled="!!selected || saving" @click="choose(item, 'claim', $event)">Claim</button>
              <button :disabled="!!selected || saving" @click="choose(item, 'explain', $event)">Explain</button>
            </div>
            <span v-else>{{ unavailable(item) }}</span>
          </td>
        </tr>
        <tr v-if="breaks.length === 0"><td colspan="12">No outstanding custody breaks were returned by the reconciliation service.</td></tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.custody-review { margin-bottom: 24px; }
.custody-review p { margin: 0; }
.actions { flex-wrap: wrap; }
.custody-origin { display: grid; gap: .2rem .6rem; grid-template-columns: max-content minmax(12rem, 1fr); }
.custody-origin dd { margin: 0; overflow-wrap: anywhere; white-space: normal; }
</style>
