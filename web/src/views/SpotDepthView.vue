<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useSession } from '../stores/session'
import { amount, captureDepth, elapsedMillis, fundingRate, isDepthInput, priceDepth, separateSellFees, useScheduledFees, type DepthInput } from '../api/spot_depth'
import example from '../api/spot_depth_example.json'

const route = useRoute(),
  session = useSession()
const id = computed(() => String(route.params.id ?? ''))
const inputs = ref<DepthInput>()
const liveCapture = ref(false)
const requestedDemoFunds = ref(false)
const waitForBooks = ref(false)
const maxBookAgeMs = ref(250)
const instrument = ref('BTC-USDT')
const source = ref(''),
  error = ref(''),
  busy = ref(false)
const read = ref<Awaited<ReturnType<typeof priceDepth>>>()
const result = computed(() => read.value?.result)
let active: AbortController | undefined,
  generation = 0,
  fileRead = 0
function invalidate() {
  generation++
  active?.abort()
  busy.value = false
  read.value = undefined
  error.value = ''
}
function reset() {
  fileRead++
  invalidate()
  inputs.value = undefined
  liveCapture.value = false
  requestedDemoFunds.value = false
  source.value = ''
}
watch(() => [id.value, session.identity, session.identity?.tenant, session.identity?.subject], reset, {
  flush: 'sync',
})
watch(inputs, invalidate, { deep: true, flush: 'sync' })
watch(maxBookAgeMs, invalidate, { flush: 'sync' })
onBeforeUnmount(reset)
function loadExample() {
  reset()
  if (isDepthInput(example)) {
    inputs.value = structuredClone(example)
    separateSellFees(inputs.value.venues)
    source.value = 'Synthetic worked example'
  }
}
async function loadObserved(includeDemoFunds: boolean) {
  if (!session.identity) return
  const requested = instrument.value.trim()
  const previous = inputs.value
  fileRead++
  invalidate()
  const seq = generation
  active = new AbortController()
  busy.value = true
  try {
    const next = await captureDepth(id.value, session.identity.tenant, requested, active.signal, includeDemoFunds, waitForBooks.value)
    if (seq !== generation) return
    if (
      liveCapture.value && previous?.observation &&
      previous.instrument === requested &&
      next.venues.every(
        (v, i) =>
          v.venue === previous.venues[i]?.venue &&
          v.symbol === previous.venues[i]?.symbol &&
          v.baseAssetRef === previous.venues[i]?.baseAssetRef &&
          v.quoteAssetRef === previous.venues[i]?.quoteAssetRef,
      )
    ) {
      next.quantity = previous.quantity
      next.sellFillBps = previous.sellFillBps
      next.adverseMoveBps = previous.adverseMoveBps
      next.oneLegLossLimit = previous.oneLegLossLimit ?? ''
    }
    inputs.value = next
    liveCapture.value = true
    requestedDemoFunds.value = includeDemoFunds
    source.value = 'Observed by the risk service · ' + next.observation!.source
  } catch (e) {
    if (seq === generation) {
      const reason = e instanceof Error ? e.message : 'Observation unavailable.'
      error.value = previous ? reason + ' The form still shows its previous dated inputs.' : reason
    }
  } finally {
    if (seq === generation) busy.value = false
  }
}
async function loadFile(event: Event) {
  const field = event.target as HTMLInputElement,
    file = field.files?.[0]
  field.value = ''
  if (!file) return
  reset()
  const seq = fileRead
  try {
    if (file.size > 256 * 1024) throw new Error('Scenario files must be at most 256 KiB.')
    const parsed: unknown = JSON.parse(await file.text())
    if (seq !== fileRead) return
    if (!isDepthInput(parsed)) throw new Error('Invalid scenario file. Use the example inputs as a template.')
    // Imported amounts are editable assumptions in this session. A subsequent
    // export must not attribute an edited value to a private venue read.
    for (const v of parsed.venues) {
      delete v.fundsObservation
      delete v.feeObservation
    }
    separateSellFees(parsed.venues)
    inputs.value = parsed
    source.value = 'Imported scenario · ' + file.name
  } catch (e) {
    if (seq === fileRead) error.value = e instanceof Error ? e.message : 'Could not read scenario.'
  }
}
function download(name: string, value: unknown) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(value, null, 2)], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
async function calculate() {
  if (!inputs.value || !session.identity) return
  invalidate()
  const seq = generation
  active = new AbortController()
  busy.value = true
  try {
    const reply = await priceDepth(id.value, session.identity.tenant, inputs.value, active.signal, liveCapture.value ? maxBookAgeMs.value : undefined)
    if (seq === generation) read.value = reply
  } catch (e) {
    if (seq === generation) error.value = e instanceof Error ? e.message : 'Scenario unavailable.'
  } finally {
    if (seq === generation) busy.value = false
  }
}
</script>
<template>
  <h1>Spot depth comparison</h1>
  <p>
    <RouterLink :to="{ name: 'exposure', params: { id } }">{{ id }}</RouterLink>
  </p>
  <p>
    Price both trading directions against dated books. Capture available demo balances or enter a balance
    assumption. Compare taker fees for each side and enter the venue order limits.
  </p>
  <div class="actions">
    <label>Spot instrument <input v-model="instrument" maxlength="128" :disabled="busy" /></label>
    <label><input v-model="waitForBooks" type="checkbox" :disabled="busy" /> Wait up to 1 second for fresh books</label>
    <button type="button" :disabled="busy || !instrument.trim()" @click="loadObserved(false)">
      Capture current books
    </button>
    <button type="button" :disabled="busy || !instrument.trim()" @click="loadObserved(true)">Capture books, demo balances and fees</button>
    <button type="button" @click="loadExample">Load synthetic example</button>
    <label>Import scenario JSON <input type="file" accept=".json,application/json" @change="loadFile" /></label>
  </div>
  <p v-if="error" role="alert" class="error">{{ error }}</p>
  <form v-if="inputs" @submit.prevent="calculate">
    <h2>{{ inputs.instrument }}</h2>
    <p>{{ source }} · Cutoff {{ inputs.cutoff }}</p>
    <p v-if="inputs.observation">
      Book source {{ inputs.observation.source }} · release {{ inputs.observation.producer_version }} · data
      reference {{ inputs.observation.rights_reference }}. Receipt times refer to the risk service.
    </p>
    <div class="fields">
      <label>Requested buy quantity <input v-model="inputs.quantity" required /></label>
      <label>Rebalance cost (quote units) <input v-model="inputs.rebalanceCost" /></label>
      <label>Sell leg filled (bps) <input v-model="inputs.sellFillBps" /></label>
      <label>Adverse mark change (bps) <input v-model="inputs.adverseMoveBps" /></label>
      <label>One-leg loss limit (quote units) <input v-model="inputs.oneLegLossLimit" /></label>
      <label v-if="liveCapture">Maximum book age after pricing (ms) <input v-model.number="maxBookAgeMs" type="number" min="1" max="2000" step="1" required /></label>
    </div>
    <div class="venues">
      <fieldset v-for="v in inputs.venues" :key="v.venue">
        <legend>{{ v.venue }} · {{ v.symbol }}</legend>
        <p v-if="liveCapture">
          Book age at capture <strong>{{ elapsedMillis(v.observed, inputs.cutoff) }}</strong><br />
          Venue event to risk receipt {{ elapsedMillis(v.observed, v.received) }}
          · Risk receipt to capture {{ elapsedMillis(v.received, inputs.cutoff) }}
        </p>
        <p class="muted">Observed {{ v.observed }}<br />Received {{ v.received }} · Sequence {{ v.sequence }}</p>
        <div class="fields">
          <label v-if="liveCapture && v.portfolioAccount">Portfolio account <input :value="v.portfolioAccount" readonly /></label>
          <label v-else>Scenario account <input v-model="v.account" required /></label>
          <label>Base available <input v-model="v.baseBalance" :readonly="liveCapture && !!v.fundsObservation" /></label>
          <label>Quote available <input v-model="v.quoteBalance" :readonly="liveCapture && !!v.fundsObservation" /></label>
          <label>Buy taker fee (bps) <input v-model="v.feeBps" :readonly="v.feeObservation?.source === 'Binance account commission'" /></label>
          <label>Buy fee currency <input v-model="v.feeCurrency" :readonly="v.feeObservation?.source === 'Binance account commission'" /></label>
          <label>Sell taker fee (bps) <input v-model="v.sellFeeBps" :readonly="v.feeObservation?.source === 'Binance account commission'" /></label>
          <label>Sell fee currency <input v-model="v.sellFeeCurrency" :readonly="v.feeObservation?.source === 'Binance account commission'" /></label>
        </div>
        <div class="fields" @input="delete v.ruleEvidence">
          <label>Quantity step <input v-model="v.step" /></label>
          <label>Minimum quantity <input v-model="v.minQty" /></label>
          <label>Maximum quantity <input v-model="v.maxQty" /></label>
          <label>Additional minimum at book prices <input v-model="v.minNotional" /></label>
        </div>
        <details :open="!!(v.minMarketValue || v.maxMarketValue || v.limitReferencePrice || v.limitCurrency)" @input="delete v.ruleEvidence">
          <summary>Market order value limits</summary>
          <div class="fields">
            <label>Limit currency <input v-model="v.limitCurrency" /></label>
            <label>Reference price per base unit <input v-model="v.limitReferencePrice" /></label>
            <label>Minimum market order value <input v-model="v.minMarketValue" /></label>
            <label>Maximum market order value <input v-model="v.maxMarketValue" /></label>
          </div>
        </details>
        <details :open="!!(v.buyPriceMin || v.buyPriceMax || v.sellPriceMin || v.sellPriceMax)" @input="delete v.ruleEvidence">
          <summary>Taker execution price bounds</summary>
          <p class="muted">Use quote currency per base unit at the cutoff. A blank bound is unassessed. Matching stops at the first price outside a supplied bound.</p>
          <div class="fields">
            <label>Minimum buy execution price <input v-model="v.buyPriceMin" /></label>
            <label>Maximum buy execution price <input v-model="v.buyPriceMax" /></label>
            <label>Minimum sell execution price <input v-model="v.sellPriceMin" /></label>
            <label>Maximum sell execution price <input v-model="v.sellPriceMax" /></label>
          </div>
        </details>
        <p v-for="ref in v.ruleEvidence" :key="ref.source + ref.response_sha256" class="muted">
          Supplied rule reference · {{ ref.source }} · Received {{ ref.known_at }}
        </p>
        <p v-if="liveCapture && v.fundsObservation" class="muted">
          {{ v.fundsObservation.source }} · Read {{ v.fundsObservation.request_started_at }} to {{ v.fundsObservation.received_at }}
        </p>
        <p v-else-if="requestedDemoFunds" class="muted">Demo balances unavailable for this capture. Entered balances are assumptions.</p>
        <p v-if="liveCapture && v.feeObservation" class="muted">
          {{ v.feeObservation.source }} · Read {{ v.feeObservation.request_started_at }} to {{ v.feeObservation.received_at }}.
          Buy {{ v.feeObservation.buyBps }} bps in {{ v.feeObservation.buyCurrency }} · Sell {{ v.feeObservation.sellBps }} bps in {{ v.feeObservation.sellCurrency }}.
          <template v-if="v.venue === 'OKX'">
            Zero-fee promotions require separate confirmation.
            <button type="button" @click="useScheduledFees(v)">Use group fees as scenario assumptions</button>
          </template>
        </p>
        <p v-else class="muted">Fee inputs are scenario assumptions.</p>
        <p v-if="liveCapture && !v.portfolioAccount" class="muted">No portfolio account reference was available in this capture.</p>
      </fieldset>
    </div>
    <div class="actions">
      <button type="submit" :disabled="busy">{{ busy ? 'Calculating…' : 'Compare both directions' }}</button>
      <button type="button" @click="download('spot-depth-inputs.json', inputs)">Save inputs</button>
    </div>
  </form>
  <section v-if="result" aria-live="polite">
    <h2>Scenario results</h2>
    <p v-if="read?.bookCheck" role="status">
      <strong v-if="read.bookCheck.state === 'CURRENT'">Both priced books matched the risk service’s latest revisions within {{ read.bookCheck.max_age_ms }} ms.</strong>
      <strong v-else-if="read.bookCheck.state === 'SUPERSEDED'">A newer book reached the risk service. Recapture both books before using this price comparison.</strong>
      <strong v-else-if="read.bookCheck.state === 'STALE'">The books exceeded the {{ read.bookCheck.max_age_ms }} ms age limit. Recapture before using this price comparison.</strong>
      <strong v-else>The risk service could not confirm the priced books. Treat these results as a dated scenario.</strong>
      <span v-if="read.bookCheck.checked_at"> Checked at {{ read.bookCheck.checked_at }}.</span>
    </p>
    <template v-if="read?.serviceTiming">
      <p>Risk service pricing and portfolio lookup took {{ elapsedMillis(read.serviceTiming.started_at, read.serviceTiming.completed_at) }}.</p>
      <table v-if="liveCapture && inputs">
        <caption>Book age when the risk service finished pricing</caption>
        <thead><tr><th>Venue</th><th>Since venue event</th><th>Since risk receipt</th></tr></thead>
        <tbody><tr v-for="v in inputs.venues" :key="v.venue">
          <th scope="row">{{ v.venue }}</th>
          <td>{{ elapsedMillis(v.observed, read.serviceTiming.completed_at) }}</td>
          <td>{{ elapsedMillis(v.received, read.serviceTiming.completed_at) }}</td>
        </tr></tbody>
      </table>
    </template>
    <p class="muted">
      Net includes the assumed rebalance cost and values any remaining base asset at the buy venue’s best bid. Stress P&amp;L uses the
      shocked mark and excludes liquidation fees and rebalancing.
    </p>
    <article v-for="r in result.directions" :key="r.buy_venue">
      <h3>Buy {{ r.buy_venue }} → sell {{ r.sell_venue }}</h3>
      <p v-if="r.reason">{{ r.status }} · {{ r.reason }}</p>
      <p v-if="r.status === 'UNSUPPORTED'">
        Unfilled buy quantity {{ amount(r.shortfall, result.base_currency) }}
      </p>
      <template v-if="r.status === 'SUPPORTED'">
        <dl class="metric-grid">
          <div>
            <dt>Scenario net after rebalancing</dt>
            <dd>{{ amount(r.marked_net_after_rebalance, result.quote_currency) }}</dd>
          </div>
          <template v-if="r.funding">
            <div>
              <dt>Net before inventory restoration</dt>
              <dd>{{ amount(r.marked_net_before_rebalance, result.quote_currency) }}</dd>
              <small v-if="r.marked_net_before_rebalance?.coefficient?.startsWith('-')">The modeled trade loses money with zero restoration cost.</small>
            </div>
            <div>
              <dt>Required before execution</dt>
              <dd>{{ amount(r.funding.buy_quote_required, result.quote_currency) }} on {{ r.buy_venue }} ·
                {{ amount(r.funding.sell_base_required, result.base_currency) }} on {{ r.sell_venue }}</dd>
            </div>
            <div>
              <dt>Funded capital at the sell leg’s average price</dt>
              <dd>{{ amount(r.funding.funded_capital_quote, result.quote_currency) }}</dd>
            </div>
            <div>
              <dt>Return on funded capital</dt>
              <dd>{{ fundingRate(r.funding.return_on_capital_bps, r.marked_net_after_rebalance) }}</dd>
            </div>
            <div>
              <dt>Break-even cost change per leg</dt>
              <dd>{{ fundingRate(r.funding.break_even_cost_per_leg_bps, r.marked_net_after_rebalance) }}</dd>
              <small>A negative value is the cost reduction needed on each leg at these fill sizes.</small>
            </div>
            <div>
              <dt>Largest modeled one-leg loss</dt>
              <dd>{{ amount(r.worst_one_leg_loss, result.quote_currency) }}</dd>
              <small>{{ r.one_leg_limit_status === 'EXCEEDED' ? 'Exceeds the scenario limit' :
                r.one_leg_limit_status === 'WITHIN' ? 'Within the scenario limit' : 'Loss limit comparison unavailable' }}</small>
            </div>
          </template>
          <div>
            <dt>Buy / sell size</dt>
            <dd>
              {{ amount(r.buy_quantity, result.base_currency) }} /
              {{ amount(r.sell_quantity, result.base_currency) }}
            </dd>
          </div>
          <div>
            <dt>Buy size shortfall</dt>
            <dd>{{ amount(r.shortfall, result.base_currency) }}</dd>
          </div>
          <div>
            <dt>Purchase / sale notional</dt>
            <dd>
              {{ amount(r.buy_notional, result.quote_currency) }} /
              {{ amount(r.sell_notional, result.quote_currency) }}
            </dd>
          </div>
          <div>
            <dt>Buy / sell fee</dt>
            <dd>
              {{ amount(r.buy_fee?.amount, r.buy_fee?.currency_code ?? '') }} /
              {{ amount(r.sell_fee?.amount, r.sell_fee?.currency_code ?? '') }}
            </dd>
          </div>
          <div>
            <dt>Gross cash before fees / fees at execution prices</dt>
            <dd>
              {{ amount(r.gross_quote_cash_change, result.quote_currency) }} /
              {{ amount(r.total_fees_in_quote, result.quote_currency) }}
            </dd>
          </div>
          <div>
            <dt>Cash / residual inventory</dt>
            <dd>
              {{ amount(r.cash_change, result.quote_currency) }} /
              {{ amount(r.residual_base, result.base_currency) }}
            </dd>
          </div>
        </dl>
        <dl class="metric-grid">
          <div
            v-for="[venue, change] in [
              [r.buy_venue, r.buy_venue_change],
              [r.sell_venue, r.sell_venue_change],
            ] as const"
            :key="venue"
          >
            <dt>{{ venue }} inventory / cash change</dt>
            <dd>
              {{ amount(change?.base_change, result.base_currency) }} /
              {{ amount(change?.quote_change, result.quote_currency) }}
            </dd>
          </div>
        </dl>
        <section v-if="r.local_unwind">
          <h4>Restore inventory with trades on each venue</h4>
          <p class="muted">Assumes both opening fills have settled and the books are unchanged. Sell back on {{ r.buy_venue }} and buy back on {{ r.sell_venue }} at their own prices and fees.</p>
          <template v-if="r.local_unwind.status === 'SUPPORTED'">
            <dl class="metric-grid">
              <div><dt>Marked P&amp;L after four fills</dt><dd>{{ amount(r.local_unwind.marked_pnl, result.quote_currency) }}</dd></div>
              <div><dt>Net quote cash after four fills</dt><dd>{{ amount(r.local_unwind.net_quote_cash, result.quote_currency) }}</dd></div>
              <div><dt>Cost versus entry mark</dt><dd>{{ amount(r.local_unwind.cost_vs_entry_mark, result.quote_currency) }}</dd></div>
            </dl>
            <div class="stress-table">
              <table>
                <thead><tr><th>Closing leg</th><th>Size</th><th>Native fee</th><th>Base change from start</th><th>Cash change from start</th></tr></thead>
                <tbody><tr v-for="[label, leg] in [
                  ['Sell on ' + r.buy_venue, r.local_unwind.sell_back], ['Buy on ' + r.sell_venue, r.local_unwind.buy_back],
                ] as const" :key="label">
                  <th>{{ label }}</th><td>{{ amount(leg.quantity, result.base_currency) }}</td>
                  <td>{{ amount(leg.fee.amount, leg.fee.currency_code) }}</td>
                  <td>{{ amount(leg.change_from_start.base_change, result.base_currency) }}</td>
                  <td>{{ amount(leg.change_from_start.quote_change, result.quote_currency) }}</td>
                </tr></tbody>
              </table>
            </div>
            <small>Remaining base quantities are marked at each venue’s best bid. The four-fill calculation uses the closing trades in place of the entered rebalance cost.</small>
          </template>
          <p v-else>{{ r.local_unwind.status }} · {{ r.local_unwind.reason }}</p>
          <p v-if="r.local_unwind.extra_quote_required">Additional quote needed on {{ r.sell_venue }} after the opening sale · {{ amount(r.local_unwind.extra_quote_required, result.quote_currency) }}</p>
        </section>
        <div class="stress-table">
          <table>
            <thead>
              <tr>
                <th>Execution outcome</th>
                <th>Residual inventory</th>
                <th>Local stress mark</th>
                <th>Marked P&amp;L</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="[label, stress] in [
                  ['Only buy fills', r.buy_only],
                  ['Only sell fills', r.sell_only],
                  ['Partial sell', r.partial_sell],
                ] as const"
                :key="label"
              >
                <th>{{ label }}</th>
                <td>{{ amount(stress?.inventory_change, result.base_currency) }}</td>
                <td>{{ amount(stress?.mark_price, result.quote_currency) }}</td>
                <td>{{ amount(stress?.marked_pnl, result.quote_currency) }}</td>
              </tr>
            </tbody>
          </table>
          <small>Unhedged purchases use the shocked bid on {{ r.buy_venue }}. An unhedged sale uses the shocked ask on {{ r.sell_venue }}. These marks exclude closing fees and slippage.</small>
        </div>
      </template>
    </article>
    <button
      @click="
        download('spot-depth-result.json', {
          portfolio: id,
          tenant: session.identity?.tenant,
          source,
          inputs,
          result,
          service_timing: read?.serviceTiming,
          book_check: read?.bookCheck,
        })
      "
    >
      Save result and inputs
    </button>
    <p class="muted digest">Input SHA-256 {{ result.input_sha256 }}</p>
  </section>
</template>
<style scoped>
.fields,
.venues {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(165px, 1fr));
  gap: 10px;
}
.venues {
  margin: 18px 0;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 300px), 1fr));
}
label {
  display: grid;
  gap: 3px;
}
fieldset {
  min-width: 0;
}
input {
  min-width: 0;
}
.stress-table {
  overflow-x: auto;
}
.digest {
  overflow-wrap: anywhere;
}
.actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
}
article {
  margin-bottom: 26px;
}
</style>
