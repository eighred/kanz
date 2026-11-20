<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useSession } from '../stores/session'
import { custody, describeCustody, describeCustodyAction, type CustodyBreak, type CustodyDecision, type CustodyEvidence } from '../api/custody'
import { ApiError } from '../api/client'
import CustodyRerunForm from './CustodyRerunForm.vue'
import DatedCustodyResult from './DatedCustodyResult.vue'
import type { CustodyRerunReceipt } from '../api/custody'

const session = useSession()
let queueRead: AbortController | undefined
let command: AbortController | undefined
const breaks = ref<CustodyBreak[]>([])
interface QueuePage { number: number; after?: string }
const pages = ref<QueuePage[]>([{ number: 1 }])
const page = computed(() => pages.value.at(-1)!)
const nextCursor = ref<string>()
const rerunActor = ref('')
const reviewingRerun = ref(false)
const rerunReceipt = ref<CustodyRerunReceipt | null>(null)
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

function reload() { return loadPage([{ number: 1 }]) }

function nextPage() {
  if (!nextCursor.value) return
  // Retain a bounded back history; Reload always starts from the first page.
  return loadPage([...pages.value.slice(-31), { number: page.value.number + 1, after: nextCursor.value }])
}

async function loadPage(history: QueuePage[]) {
  queueRead?.abort()
  if (!session.signedIn) return
  const request = new AbortController()
  queueRead = request
  pages.value = history
  nextCursor.value = undefined
  loading.value = true
  error.value = ''
  breaks.value = []
  rerunActor.value = ''
  rerunReceipt.value = null
  try {
    const result = await custody.queue(request.signal, page.value.after)
    if (request.signal.aborted) return
    if (result.nextCursor && history.some(entry => entry.after === result.nextCursor)) throw new Error('Repeated custody queue cursor.')
    nextCursor.value = result.nextCursor
    breaks.value = result.breaks
    rerunActor.value = result.rerunActor ?? ''
  }
  catch (e) { if (!request.signal.aborted) error.value = describeCustody(e) }
  finally { if (!request.signal.aborted) loading.value = false }
}
function clearSession() {
  queueRead?.abort()
  command?.abort()
  pages.value = [{ number: 1 }]
  nextCursor.value = undefined
  breaks.value = []
  rerunActor.value = ''
  rerunReceipt.value = null
  evidence.value = null
  selected.value = null
  decision.value = null
  explanation.value = ''
  actionError.value = ''
  error.value = ''
  loading.value = false
  saving.value = false
  reviewingRerun.value = false
  actionTrigger = null
}
watch(() => session.identity, () => { clearSession(); void reload() }, { deep: true, flush: 'sync' })
onMounted(reload)
onUnmounted(clearSession)

async function showRerun(receipt: CustodyRerunReceipt) {
  const identity = session.identity
  const subject = identity?.subject
  const tenant = identity?.tenant
  await reload()
  if (identity === session.identity && tenant === session.identity?.tenant && subject === session.identity?.subject && receipt.actor === rerunActor.value) rerunReceipt.value = receipt
}

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
  const request = new AbortController()
  command = request
  saving.value = true
  actionError.value = ''
  try {
    const recorded = await custody.act(selected.value, decision.value, request.signal)
    if (request.signal.aborted) return
    evidence.value = recorded
    closeReview()
    await reload()
  } catch (e) {
    if (request.signal.aborted) return
    actionError.value = describeCustodyAction(e)
    if (e instanceof ApiError && [400, 403, 404, 409].includes(e.status)) { closeReview(); await reload() }
  } finally {
    if (!request.signal.aborted) {
      saving.value = false
      await nextTick()
      const target = selected.value ? confirmButton.value : actionError.value ? errorMessage.value : receiptMessage.value
      target?.focus()
      if (!selected.value) actionTrigger = null
    }
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
  <CustodyRerunForm v-if="rerunActor && !selected" :actor="rerunActor" @reviewing="reviewingRerun = $event" @completed="showRerun" />
  <section v-if="rerunReceipt" aria-label="Recorded custody comparison" role="status">
    <h2>Recorded comparison</h2>
    <p>{{ rerunReceipt.portfolio_id }} · {{ rerunReceipt.custodian_id }} · {{ rerunReceipt.business_date }}</p>
    <p>Outcome <strong>{{ rerunReceipt.outcome }}</strong> · {{ rerunReceipt.completed_at }}</p>
    <p v-if="rerunReceipt.failure_code === 'historical_lifecycle'">A newer comparison controls the break queue.</p>
    <p v-else-if="rerunReceipt.outcome === 'failed'">Accounting recorded a failed comparison. Use its run ID to investigate.</p>
    <p>Run <code>{{ rerunReceipt.run_id }}</code> · Request <code>{{ rerunReceipt.request_id }}</code></p>
    <p v-if="rerunReceipt.statement_id">Statement <code>{{ rerunReceipt.statement_id }}</code></p>
    <p v-if="rerunReceipt.journal_position !== undefined">Journal position <code>{{ rerunReceipt.journal_position }}</code></p>
  </section>
  <button ref="reloadButton" :disabled="loading || saving || !!selected || reviewingRerun" @click="reload">Reload queue</button>
  <nav v-if="page.number > 1 || nextCursor" aria-label="Custody queue pages">
    <p v-if="!loading && !error" role="status">Page {{ page.number }} · {{ breaks.length }} shown. {{ nextCursor ? 'More results are available.' : 'End of queue.' }}</p>
    <button v-if="pages.length > 1" :disabled="loading || saving || !!selected || reviewingRerun" @click="loadPage(pages.slice(0, -1))">Previous page</button>
    <button v-if="nextCursor" :disabled="loading || saving || !!selected || reviewingRerun" @click="nextPage">Next page</button>
  </nav>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <div v-else class="table-scroll">
    <table>
      <thead><tr><th>Break ID</th><th>Age</th><th>Kind / key</th><th>IBOR</th><th>Custodian</th><th>Difference</th><th>Status</th><th>Assignee</th><th>Last detected</th><th>Detection evidence</th><th>Explanation</th><th>Investigation</th></tr></thead>
      <tbody>
        <tr v-for="item in breaks" :key="item.break_id">
          <th scope="row"><code>{{ item.break_id }}</code></th>
          <td :class="item.age_seconds >= 86_400 ? 'state-warn' : ''">{{ age(item.age_seconds) }}</td>
          <td>{{ item.kind }}<br /><code>{{ item.key }}</code><template v-if="item.kind === 'execution_quantity'"><br />Instrument <code>{{ item.instrument_id }}</code></template></td>
          <td v-for="field in figures" :key="field">{{ item.values_state === 'exact' ? item[field] : item.values_state === 'unavailable' ? 'Unavailable' : 'Unverified' }}</td>
          <td>{{ item.status }}<p v-if="item.late_correction_statement_id">A later business date has closed. Signed revision <code>{{ item.late_correction_statement_id }}</code> awaits historical quantity review.</p></td><td>{{ item.assignee || 'Unassigned' }}</td>
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
              <button :disabled="!!selected || saving || reviewingRerun" @click="choose(item, 'claim', $event)">Claim</button>
              <button :disabled="!!selected || saving || reviewingRerun" @click="choose(item, 'explain', $event)">Explain</button>
            </div>
            <span v-else>{{ unavailable(item) }}</span>
          </td>
        </tr>
        <tr v-if="breaks.length === 0"><td colspan="12">{{ page.number > 1 ? 'No outstanding breaks remain on this page. Reload the queue to check the first page.' : 'No outstanding custody breaks were returned by the reconciliation service.' }}</td></tr>
      </tbody>
    </table>
  </div>
  <DatedCustodyResult />
</template>

<style scoped>
.custody-review { margin-bottom: 24px; }
.custody-review p { margin: 0; }
.actions { flex-wrap: wrap; }
.custody-origin { display: grid; gap: .2rem .6rem; grid-template-columns: max-content minmax(12rem, 1fr); }
.custody-origin dd { margin: 0; overflow-wrap: anywhere; white-space: normal; }
</style>
