<script setup lang="ts">
import { onBeforeUnmount, ref, toRaw, watch } from 'vue'
import { amount, priceDepth, separateSellFees, type DepthInput } from '../api/spot_depth'

type Read = Awaited<ReturnType<typeof priceDepth>>
const props = defineProps<{ portfolio: string; tenant: string; inputs: DepthInput; baseline: Read['result'] }>()
const emit = defineEmits<{ save: [value: { inputs: DepthInput; result: Read['result']; service_timing: Read['serviceTiming'] }] }>()
const draft = ref<DepthInput>()
const priced = ref<Read>()
const error = ref('')
const busy = ref(false)
let active: AbortController | undefined

function invalidate() {
  active?.abort()
  active = undefined
  busy.value = false
  priced.value = undefined
  error.value = ''
}
function close() {
  draft.value = undefined
  invalidate()
}
watch(() => [props.portfolio, props.tenant, props.inputs, props.baseline], close, { flush: 'sync' })
watch(draft, invalidate, { deep: true, flush: 'sync' })
onBeforeUnmount(close)

function open() {
  const input = structuredClone(toRaw(props.inputs))
  separateSellFees(input.venues)
  for (const venue of input.venues) {
    delete venue.fundsObservation
    delete venue.feeObservation
  }
  draft.value = input
}
function zeroFees() {
  for (const v of draft.value!.venues) {
    v.feeBps = v.sellFeeBps = '0'
    v.feeCurrency ||= props.baseline.quote_currency
    v.sellFeeCurrency ||= props.baseline.quote_currency
  }
}
async function compare() {
  if (!draft.value) return
  invalidate()
  const request = new AbortController()
  active = request
  busy.value = true
  try {
    const read = await priceDepth(props.portfolio, props.tenant, draft.value, request.signal)
    if (active === request) priced.value = read
  } catch (e) {
    if (active === request) error.value = e instanceof Error ? e.message : 'Comparison unavailable.'
  } finally {
    if (active === request) busy.value = false
  }
}
</script>

<template>
  <button v-if="!draft" type="button" @click="open">Compare alternative fees and balances</button>
  <section v-else aria-label="Fee and balance comparison">
    <h3>Alternative fees and balances</h3>
    <p>Reprice the requested {{ draft.quantity }} {{ baseline.base_currency }} on the same books at {{ draft.cutoff }}. Each balance and fee below is a scenario assumption.</p>
    <form @submit.prevent="compare">
      <fieldset v-for="v in draft.venues" :key="v.venue">
        <legend>{{ v.venue }}</legend>
        <div class="terms">
          <label>Available {{ baseline.base_currency }} <input v-model="v.baseBalance" /></label>
          <label>Available {{ baseline.quote_currency }} <input v-model="v.quoteBalance" /></label>
          <label>Buy fee (bps) <input v-model="v.feeBps" /></label>
          <label>Buy fee asset <input v-model="v.feeCurrency" /></label>
          <label>Sell fee (bps) <input v-model="v.sellFeeBps" /></label>
          <label>Sell fee asset <input v-model="v.sellFeeCurrency" /></label>
        </div>
      </fieldset>
      <button type="button" @click="zeroFees">Set alternative fees to zero</button>
      <button type="submit" :disabled="busy">Reprice alternative</button>
      <button type="button" @click="close">Close comparison</button>
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="priced">
      <div class="comparison-table">
        <table>
          <caption>Original and alternative at the same cutoff</caption>
          <thead><tr><th>Buy → sell</th><th>Terms</th><th>Buy size</th><th>Sell size</th><th>Net after rebalancing</th><th>Worst one-leg loss</th><th>Loss limit</th></tr></thead>
          <tbody v-for="(original, i) in baseline.directions" :key="original.buy_venue">
            <tr v-for="(d, j) in [original, priced.result.directions[i]!]" :key="j">
              <th scope="row">{{ d.buy_venue }} → {{ d.sell_venue }}</th>
              <td>{{ j === 0 ? 'Original' : 'Alternative' }}</td>
              <template v-if="d.status === 'SUPPORTED'">
                <td>{{ amount(d.buy_quantity, baseline.base_currency) }}</td>
                <td>{{ amount(d.sell_quantity, baseline.base_currency) }}</td>
                <td>{{ amount(d.marked_net_after_rebalance, baseline.quote_currency) }}</td>
                <td>{{ amount(d.worst_one_leg_loss, baseline.quote_currency) }}</td>
                <td>{{ d.one_leg_limit_status ?? 'UNKNOWN' }}</td>
              </template>
              <td v-else colspan="5">{{ d.status }} · {{ d.reason }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <button type="button" @click="emit('save', { inputs: draft, result: priced.result, service_timing: priced.serviceTiming })">Save both scenarios</button>
    </template>
  </section>
</template>

<style scoped>
.terms { display: flex; flex-wrap: wrap; gap: .75rem; }
.terms label { display: grid; gap: .25rem; }
.terms input { max-width: 12rem; }
.comparison-table { overflow-x: auto; }
</style>
