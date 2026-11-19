<script setup lang="ts">
import { nextTick, onUnmounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import { reviewModelProposal, type ModelProposal, type Scenario, type ScenarioAsset } from '../api/model-proposal'
import { useSession } from '../stores/session'

const session = useSession()
const emptyAsset = (): ScenarioAsset => ({ instrument: '', current: '', price: '', expectedReturn: '' })
const emptyScenario = (): Scenario => ({ portfolio: '', currency: '', nav: '', threshold: '', assets: [emptyAsset()] })
const form = ref(emptyScenario())
const proposal = ref<ModelProposal | null>(null)
const error = ref('')
const loading = ref(false)
const outcome = ref<HTMLElement>()
let pending: AbortController | undefined

function clearResult() {
  pending?.abort()
  pending = undefined
  proposal.value = null
  error.value = ''
  loading.value = false
}
watch(form, clearResult, { deep: true, flush: 'sync' })
watch(() => session.identity, () => { clearResult(); form.value = emptyScenario() }, { deep: true, flush: 'sync' })
onUnmounted(clearResult)

async function review() {
  if (loading.value || !session.signedIn) return
  clearResult()
  const request = new AbortController()
  pending = request
  loading.value = true
  try {
    const result = await reviewModelProposal(form.value, request.signal)
    if (!request.signal.aborted) proposal.value = result
  } catch (e) {
    if (request.signal.aborted) return
    if (e instanceof ApiError) {
      switch (e.status) {
        case 403: error.value = 'You do not have permission to review this portfolio.'; break
        case 404: case 410: error.value = 'Exact model proposals are unavailable on this deployment.'; break
        case 400: case 422: error.value = 'Optimization refused these inputs or could not evaluate the portfolio mandate.'; break
        default: error.value = 'Optimization could not confirm a proposal. Review the inputs before trying again.'
      }
    } else error.value = e instanceof Error ? e.message : 'The proposal could not be verified.'
  } finally {
    if (pending === request) {
      pending = undefined
      loading.value = false
      await nextTick()
      if (!request.signal.aborted) outcome.value?.focus()
    }
  }
}
</script>

<template>
  <h1>Model proposal review</h1>
  <p>Evaluate a hypothetical portfolio for maximum expected return. You supply the book, prices and per-period return estimates. Each calculation is read-only.</p>
  <form @submit.prevent="review">
    <fieldset>
      <legend>Scenario</legend>
      <label>Portfolio ID <input v-model="form.portfolio" required maxlength="256" /></label>
      <label>Valuation currency <input v-model="form.currency" required maxlength="12" placeholder="USD" /></label>
      <label>NAV in valuation currency <input v-model="form.nav" required maxlength="400" /></label>
      <label>Weight-change threshold <input v-model="form.threshold" required maxlength="400" /></label>
    </fieldset>
    <p>Enter weights as fractions of NAV. Use zero for an unheld instrument. Weights must total one for an invested book or zero for an empty book. Enter each price in the valuation currency.</p>
    <fieldset v-for="(asset, index) in form.assets" :key="index">
      <legend>Instrument {{ index + 1 }}</legend>
      <label>Instrument ID <input v-model="asset.instrument" required maxlength="256" /></label>
      <label>Current weight <input v-model="asset.current" required maxlength="400" /></label>
      <label>Price <input v-model="asset.price" required maxlength="400" /></label>
      <label>Expected return per period <input v-model="asset.expectedReturn" required maxlength="100" placeholder="0.05" /></label>
      <button v-if="form.assets.length > 1" type="button" @click="form.assets.splice(index, 1)">Remove instrument {{ index + 1 }}</button>
    </fieldset>
    <div class="actions">
      <button type="button" :disabled="form.assets.length >= 128" @click="form.assets.push(emptyAsset())">Add instrument</button>
      <button type="submit" :disabled="loading">{{ loading ? 'Evaluating…' : 'Evaluate candidate' }}</button>
    </div>
  </form>
  <p v-if="error" ref="outcome" tabindex="-1" role="alert" class="error">{{ error }}</p>
  <section v-if="proposal" aria-labelledby="proposal-review-heading">
    <h2 id="proposal-review-heading" ref="outcome" tabindex="-1">Candidate for {{ proposal.portfolio }}</h2>
    <p>Calculated by Optimization at <time :datetime="proposal.asOf">{{ proposal.asOf }}</time> using the scenario above. No approval or order was created.</p>
    <h3>Mandate evaluation</h3>
    <p v-if="proposal.verdict === 'UNCHECKED'">Unchecked. A mandate verdict is unavailable.</p>
    <p v-else-if="proposal.verdict === 'FEASIBLE'">Feasible for this hypothetical candidate under the evaluated mandate.</p>
    <p v-else>Infeasible under the evaluated mandate.</p>
    <ul v-if="proposal.violations.length"><li v-for="(reason, index) in proposal.violations" :key="index">{{ reason }}</li></ul>
    <h3>Exact candidate book</h3>
    <p>The optimizer rounds target weights to 12 decimal places using nearest-even rounding and assigns the residual to the largest absolute weight. The mandate evaluation uses the candidate after the threshold is applied.</p>
    <div class="table-scroll"><table>
      <thead><tr><th>Instrument</th><th>Current weight</th><th>Solver target</th><th>Evaluated weight</th></tr></thead>
      <tbody><tr v-for="row in proposal.book" :key="row.id"><td>{{ row.id }}</td><td>{{ row.current }}</td><td>{{ row.target }}</td><td>{{ row.candidate }}</td></tr></tbody>
    </table></div>
    <p>Turnover as a fraction of NAV: {{ proposal.turnover }}</p>
    <h3>Hypothetical trades</h3>
    <p v-if="!proposal.trades.length">No trades exceed the scenario threshold.</p>
    <div v-else class="table-scroll"><table>
      <thead><tr><th>Instrument</th><th>Side</th><th>Quantity</th><th>Notional ({{ proposal.currency }})</th></tr></thead>
      <tbody><tr v-for="trade in proposal.trades" :key="trade.id"><td>{{ trade.id }}</td><td>{{ trade.side }}</td><td>{{ trade.quantity }}</td><td>{{ trade.notional }}</td></tr></tbody>
    </table></div>
    <h3>Statistical estimates</h3>
    <p>Estimated return per period: {{ proposal.expectedReturn }}</p>
    <p>Risk estimate unavailable. This maximum-return scenario uses no covariance estimate.</p>
  </section>
</template>

<style scoped>
form { display: grid; gap: 1rem; }
fieldset { display: flex; flex-wrap: wrap; align-items: end; gap: .75rem; min-width: 0; }
label { display: grid; gap: .25rem; }
input { width: 12rem; max-width: 100%; }
section { margin-top: 1.5rem; }
</style>
