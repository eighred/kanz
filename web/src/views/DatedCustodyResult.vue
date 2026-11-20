<script setup lang="ts">
import { onUnmounted, reactive, ref, watch } from 'vue'
import { useSession } from '../stores/session'
import { custody, downloadCustodyComparison, type DatedCustodyResult } from '../api/custody'
import { ApiError } from '../api/client'

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
async function read() {
  if (pending.value || !session.identity) return
  const request = new AbortController()
  active = request
  result.value = undefined
  error.value = ''
  pending.value = true
  try {
    const dated = await custody.dated({ ...subject }, session.identity.tenant, request.signal, savedRun.value)
    if (!request.signal.aborted) result.value = dated
  } catch (e) {
    if (!request.signal.aborted) error.value = e instanceof ApiError && [403, 404].includes(e.status)
      ? 'Access to this dated result is unavailable.' : 'Unable to verify the dated result.'
  } finally { if (!request.signal.aborted) pending.value = false }
}
const outcomes = { clean: 'Clean comparison', breaks: 'Differences found', no_statement: 'No statement received', failed: 'Comparison failed' }
function recordedLabel(result: Extract<DatedCustodyResult, { state: 'recorded' }>): string {
  if (result.outcome === 'clean' && (result.transactions?.transaction_pass !== 'ran' || result.transactions.unreferenced_book_trades !== '0')) return 'Balances agree'
  return outcomes[result.outcome]
}
const states = { not_compared: 'No comparison recorded', pending: 'Statement awaiting comparison', unknown: 'Result unknown' }
</script>

<template>
  <section class="panel" aria-label="Dated custody result">
    <h2>Dated custody result</h2>
    <form @submit.prevent="read">
      <label>Portfolio ID <input v-model.trim="subject.portfolio_id" required maxlength="256" /></label>
      <label>Custodian ID <input v-model.trim="subject.custodian_id" required maxlength="256" /></label>
      <label>Business date <input v-model="subject.business_date" type="date" required /></label>
      <label>Saved run ID (optional) <input v-model="savedRun" maxlength="512" /></label>
      <button :disabled="pending">View dated result</button>
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <output v-if="result" :data-state="result.state">
      <span v-if="savedRun">Historical comparison · </span>
      <strong>{{ result.state === 'recorded' ? recordedLabel(result) : result.reason === 'journal_changed' ? 'Journal changes awaiting comparison' : states[result.state] }}</strong>
      · {{ result.portfolio_id }} · {{ result.custodian_id }} · {{ result.business_date }}
      <span v-if="result.statement_id"> · Statement <code>{{ result.statement_id }}</code></span>
      <template v-if="result.state === 'recorded'">
        · Run <code>{{ result.run_id }}</code> · Completed {{ result.completed_at }}
        <span v-if="result.journal_position !== undefined"> · Journal position <code>{{ result.journal_position }}</code> · Account scope {{ result.scope_mode }}</span>
        <span v-if="result.transactions?.transaction_pass === 'ran'">
          · Independent reference comparison · Statement references {{ result.transactions.statement_reference_count }}
          · Book references {{ result.transactions.book_reference_count }}
          <strong v-if="result.transactions.unreferenced_book_trades !== '0'"> · Incomplete coverage · {{ result.transactions.unreferenced_book_trades }} book trades without independent references</strong>
        </span>
        <span v-else-if="result.transactions?.transaction_pass === 'balances_only'"> · Balances only · Transactions were not compared</span>
        <span v-else-if="result.transactions?.transaction_pass === 'unknown'"> · Transaction coverage unknown</span>
      </template>
    </output>
    <template v-if="result?.state === 'recorded' && result.historical">
      <p>Signed statement SHA-256 <code>{{ result.historical.signed_statement_sha256 }}</code></p>
      <table v-if="result.historical.breaks.length" aria-label="Differences at comparison">
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
