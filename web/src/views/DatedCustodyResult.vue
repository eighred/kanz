<script setup lang="ts">
import { onUnmounted, reactive, ref, watch } from 'vue'
import { useSession } from '../stores/session'
import { custody, type DatedCustodyResult } from '../api/custody'
import { ApiError } from '../api/client'

const session = useSession()
const subject = reactive({ portfolio_id: '', custodian_id: '', business_date: '' })
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
watch(() => session.identity, () => { clear(); Object.assign(subject, { portfolio_id: '', custodian_id: '', business_date: '' }) }, { deep: true, flush: 'sync' })
onUnmounted(clear)
async function read() {
  if (pending.value || !session.signedIn) return
  const request = new AbortController()
  active = request
  result.value = undefined
  error.value = ''
  pending.value = true
  try {
    const dated = await custody.dated({ ...subject }, request.signal)
    if (!request.signal.aborted) result.value = dated
  } catch (e) {
    if (!request.signal.aborted) error.value = e instanceof ApiError && [403, 404].includes(e.status)
      ? 'Access to this dated result is unavailable.' : 'Unable to verify the dated result.'
  } finally { if (!request.signal.aborted) pending.value = false }
}
const outcomes = { clean: 'Clean comparison', breaks: 'Differences found', no_statement: 'No statement received', failed: 'Comparison failed' }
const states = { not_compared: 'No comparison recorded', pending: 'Statement awaiting comparison', unknown: 'Result unknown' }
</script>

<template>
  <section class="panel" aria-label="Dated custody result">
    <h2>Dated custody result</h2>
    <form @submit.prevent="read">
      <label>Portfolio ID <input v-model.trim="subject.portfolio_id" required maxlength="256" /></label>
      <label>Custodian ID <input v-model.trim="subject.custodian_id" required maxlength="256" /></label>
      <label>Business date <input v-model="subject.business_date" type="date" required /></label>
      <button :disabled="pending">View dated result</button>
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <output v-if="result" :data-state="result.state">
      <strong>{{ result.state === 'recorded' ? outcomes[result.outcome] : result.reason === 'journal_changed' ? 'Journal changes awaiting comparison' : states[result.state] }}</strong>
      · {{ result.portfolio_id }} · {{ result.custodian_id }} · {{ result.business_date }}
      <span v-if="result.statement_id"> · Statement <code>{{ result.statement_id }}</code></span>
      <template v-if="result.state === 'recorded'">
        · Run <code>{{ result.run_id }}</code> · Completed {{ result.completed_at }}
        <span v-if="result.journal_position !== undefined"> · Journal position <code>{{ result.journal_position }}</code> · Account scope {{ result.scope_mode }}</span>
      </template>
    </output>
  </section>
</template>
