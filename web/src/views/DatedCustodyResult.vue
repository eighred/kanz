<script setup lang="ts">
import { onUnmounted, reactive, ref, watch } from 'vue'
import { useSession } from '../stores/session'
import { custody, downloadCustodyComparison, type DatedCustodyResult } from '../api/custody'
import { ApiError } from '../api/client'
import { formatDecimal } from '../api/decimal'

const session = useSession()
const subject = reactive({ portfolio_id: '', custodian_id: '', business_date: '' })
const savedRun = ref('')
const result = ref<DatedCustodyResult>()
const error = ref('')
const pending = ref(false)
let active: AbortController | undefined
function clear() {
  active?.abort()
  active = undefined
  result.value = undefined
  error.value = ''
  pending.value = false
}
watch(subject, clear, { flush: 'sync' })
watch(savedRun, clear, { flush: 'sync' })
watch(() => session.identity, () => { clear(); savedRun.value = ''; Object.assign(subject, { portfolio_id: '', custodian_id: '', business_date: '' }) }, { deep: true, flush: 'sync' })
onUnmounted(clear)
async function read(correctionStatement?: string, reportedFigures = false) {
  if (pending.value || !session.identity) return
  const request = new AbortController()
  active = request
  const previous = result.value
  result.value = undefined
  error.value = ''
  pending.value = true
  try {
    const dated = await custody.dated({ ...subject }, session.identity.tenant, request.signal, savedRun.value, reportedFigures)
    if (correctionStatement && (dated.state !== 'recorded' || dated.outcome !== 'historical_quantity_correction' ||
        dated.statement_id !== correctionStatement)) throw new Error('Correction source changed.')
    if (reportedFigures && (previous?.state !== 'recorded' || dated.state !== 'recorded' ||
        previous.historical?.signed_statement_sha256 !== dated.historical?.signed_statement_sha256 || !dated.historical?.reported_execution_figures)) throw new Error('Unverified source.')
    if (!request.signal.aborted) result.value = dated
  } catch (e) {
    if (!request.signal.aborted && reportedFigures && e instanceof ApiError && e.status === 422) {
      result.value = previous
      error.value = 'Signed execution figures are unavailable within the receipt limit.'
      return
    }
    if (!request.signal.aborted) error.value = e instanceof ApiError && [403, 404].includes(e.status)
      ? 'Access to this dated result is unavailable.' : 'Unable to verify the dated result.'
  } finally { if (!request.signal.aborted) pending.value = false }
}
function openCorrection() {
  const current = result.value
  if (current?.state !== 'pending' || !current.historical_quantity_correction_run_id || !current.statement_id) return
  savedRun.value = current.historical_quantity_correction_run_id
  void read(current.statement_id)
}
const outcomes = { historical_quantity_correction: 'Historical execution quantities corrected', clean: 'Balances and transaction references agree', breaks: 'Differences found', no_statement: 'No statement received', failed: 'Comparison failed' }
function recordedLabel(result: Extract<DatedCustodyResult, { state: 'recorded' }>): string {
  if (result.outcome === 'clean' && (result.transactions?.transaction_pass !== 'ran' || result.transactions.unreferenced_book_trades !== '0')) return 'Balances agree'
  return outcomes[result.outcome]
}
const states = { not_compared: 'No comparison recorded', pending: 'Statement awaiting comparison', unknown: 'Result unknown' }
const pendingLabels = {
  journal_changed: 'Journal changes awaiting comparison',
  assignment_changed: 'Custody account assignment changed since comparison',
  assignment_unproven: 'Awaiting comparison with a verified custody account assignment',
}
</script>

<template>
  <section class="panel" aria-label="Dated custody result">
    <h2>Dated custody result</h2>
    <form @submit.prevent="read()">
      <label>Portfolio ID <input v-model.trim="subject.portfolio_id" required maxlength="256" /></label>
      <label>Custodian ID <input v-model.trim="subject.custodian_id" required maxlength="256" /></label>
      <label>Business date <input v-model="subject.business_date" type="date" required /></label>
      <label>Saved run ID (optional) <input v-model="savedRun" maxlength="512" /></label>
      <button :disabled="pending">View dated result</button>
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <output v-if="result" :data-state="result.state">
      <span v-if="savedRun">Historical comparison · </span>
      <strong>{{ result.state === 'recorded' ? recordedLabel(result) : result.reason ? pendingLabels[result.reason] : states[result.state] }}</strong>
      <strong v-if="result.state === 'recorded' && result.historical?.execution_quantities?.differences.length"> · Execution quantity differences found</strong>
      · {{ result.portfolio_id }} · {{ result.custodian_id }} · {{ result.business_date }}
      <span v-if="result.statement_id"> · Statement <code>{{ result.statement_id }}</code></span>
      <template v-if="result.state === 'recorded'">
        · Run <code>{{ result.run_id }}</code> · Completed {{ result.completed_at }}
        <span v-if="result.journal_position !== undefined"> · Journal position <code>{{ result.journal_position }}</code> · Account scope {{ result.scope_mode }}</span>
        <span v-if="result.outcome === 'clean' || result.outcome === 'breaks'">
          · {{ result.account_assignment_verified ? 'Custody account assignment verified for this close' : 'Custody account assignment unverified for this close' }}
        </span>
        <span v-if="result.transactions?.transaction_pass === 'ran'">
          · Independent reference comparison · Statement references {{ result.transactions.statement_reference_count }}
          · Book references {{ result.transactions.book_reference_count }}
          <template v-if="!result.historical?.execution_quantities"> · Individual trade quantities, prices and cash amounts remain unverified.</template>
          <strong v-if="result.transactions.unreferenced_book_trades !== '0'"> · Incomplete coverage · {{ result.transactions.unreferenced_book_trades }} book trades without independent references</strong>
        </span>
        <span v-else-if="result.transactions?.transaction_pass === 'balances_only'"> · Balances only · Transactions were not compared</span>
        <span v-else-if="result.transactions?.transaction_pass === 'unknown'"> · Transaction coverage unknown</span>
      </template>
    </output>
    <button v-if="result?.state === 'pending' && result.historical_quantity_correction_run_id" type="button" @click="openCorrection">View corrected execution quantities</button>
    <template v-if="result?.state === 'recorded' && result.historical">
      <p>Signed statement SHA-256 <code>{{ result.historical.signed_statement_sha256 }}</code></p>
      <button v-if="result.account_assignment_verified && result.transactions?.transaction_pass === 'ran' && !result.historical.reported_execution_figures"
        type="button" @click="read(undefined, true)">View custodian-reported price and cash</button>
      <section v-if="result.historical.reported_execution_figures" aria-label="Custodian-reported execution figures">
        <h3>Custodian-reported price and cash</h3>
        <p>These figures come from the signed statement above. The currency shown applies to cash. Execution prices, cash and fees have no independent book comparison in this receipt.</p>
        <table v-if="result.historical.reported_execution_figures.rows.length" aria-label="Reported execution figures">
          <thead><tr><th scope="col">Execution reference</th><th scope="col">Instrument</th><th scope="col">Reported unit price</th>
            <th scope="col">Reported signed cash</th><th scope="col">Cash currency</th></tr></thead>
          <tbody><tr v-for="row in result.historical.reported_execution_figures.rows" :key="row.external_ref">
            <th scope="row">{{ row.external_ref }}</th><td>{{ row.instrument_id }}</td><td>{{ row.reported_price ?? 'Unreported' }}</td>
            <td>{{ row.reported_cash ?? 'Unreported' }}</td><td>{{ row.cash_currency }}</td>
          </tr></tbody>
        </table>
        <p v-else>The signed statement reports zero execution lines.</p>
      </section>
      <section v-if="result.historical.execution_quantities" aria-label="Execution quantities">
        <h3>Execution quantities</h3>
        <p>Matched references compared: {{ result.historical.execution_quantities.compared }} · Unassessed: {{ result.historical.execution_quantities.unassessed }}.</p>
        <p v-if="result.historical.execution_quantities.unassessed">Some matched references lack comparable quantity evidence.</p>
        <p>Quantities use each instrument’s natural units. Trade prices and cash amounts remain unverified.</p>
        <table v-if="result.historical.execution_quantities.differences.length" aria-label="Execution quantity differences">
          <thead><tr><th scope="col">Execution reference</th><th scope="col">Instrument</th><th scope="col">Book</th>
            <th scope="col">Custodian</th><th scope="col">Book minus custodian</th></tr></thead>
          <tbody><tr v-for="row in result.historical.execution_quantities.differences" :key="row.external_ref">
            <th scope="row">{{ row.external_ref }}</th><td>{{ row.instrument_id }}</td><td>{{ formatDecimal(row.ibor) }}</td>
            <td>{{ formatDecimal(row.custodian) }}</td><td>{{ formatDecimal(row.difference) }}</td>
          </tr></tbody>
        </table>
        <p v-else-if="result.historical.execution_quantities.compared">Compared quantities agree.</p>
      </section>
      <section v-if="result.historical.historical_quantity_resolutions" aria-label="Corrected execution quantities">
        <h3>Corrected execution quantities</h3>
        <p>This receipt covers only the listed executions. The original close and the following close keep their own comparison results.</p>
        <p>Custody account assignment verified for this correction.</p>
        <table aria-label="Execution quantity resolutions">
          <thead><tr><th scope="col">Execution reference</th><th scope="col">Instrument</th><th scope="col">Agreed quantity</th>
            <th scope="col">Book entry</th><th scope="col">Original detection run</th><th scope="col">Discrepancy ID</th></tr></thead>
          <tbody><tr v-for="row in result.historical.historical_quantity_resolutions" :key="row.break_id">
            <th scope="row">{{ row.external_ref }}</th><td>{{ row.instrument_id }}</td><td>{{ formatDecimal(row.agreed_quantity) }}</td>
            <td>{{ row.entry_id }}</td><td>{{ row.detection_run_id }}</td><td>{{ row.break_id }}</td>
          </tr></tbody>
        </table>
      </section>
      <table v-if="result.historical.breaks?.length" aria-label="Differences at comparison">
        <thead><tr><th scope="col">Discrepancy</th><th scope="col">Kind</th><th scope="col">Key</th>
          <th scope="col">IBOR</th><th scope="col">Custodian</th><th scope="col">Difference</th></tr></thead>
        <tbody><tr v-for="row in result.historical.breaks" :key="row.break_id">
          <th scope="row">{{ row.break_id }}</th><td>{{ row.kind }}</td><td>{{ row.key }}</td>
          <td>{{ row.ibor || 'Unavailable' }}</td><td>{{ row.custodian || 'Unavailable' }}</td><td>{{ row.difference || 'Unavailable' }}</td>
        </tr></tbody>
      </table>
    </template>
    <button v-if="result?.state === 'recorded'" type="button" @click="downloadCustodyComparison(result)">Download comparison receipt</button>
  </section>
</template>
