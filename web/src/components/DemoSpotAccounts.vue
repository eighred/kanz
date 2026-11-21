<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ApiError, type Identity } from '../api/client'
import { demoSpotVenues, readDemoAccountTerms, type DemoAccountTerms, type DemoSpotVenue } from '../api/spot_depth'

const props = defineProps<{ portfolio: string; identity: Identity; instrument: string }>()
type AccountRead = { observation: DemoAccountTerms; error?: undefined } | { error: string; observation?: undefined }
const reads = ref<Partial<Record<DemoSpotVenue, AccountRead>>>()
const rows = computed(() => demoSpotVenues.map(venue => ({ venue, observation: reads.value?.[venue]?.observation, error: reads.value?.[venue]?.error })))
let active: AbortController | undefined

function reset() {
  active?.abort()
  active = undefined
  reads.value = undefined
}
watch(() => [props.portfolio, props.identity, props.identity.tenant, props.identity.subject, props.instrument], reset, { flush: 'sync' })
onBeforeUnmount(reset)

async function refresh() {
  reset()
  const request = new AbortController()
  active = request
  reads.value = {}
  await Promise.all(demoSpotVenues.map(async venue => {
    let result: AccountRead
    try {
      const observation = await readDemoAccountTerms(props.portfolio, props.identity.tenant, props.instrument.trim(), venue, request.signal)
      result = { observation }
    } catch (error) {
      // Only expose the HTTP outcome. Private upstream response text is outside this view.
      result = { error: error instanceof ApiError ? `Account read unavailable (HTTP ${error.status}).` : 'Account observation unavailable.' }
    }
    if (active === request && reads.value) reads.value[venue] = result
  }))
}
</script>

<template>
  <section aria-label="Demo spot accounts">
    <button type="button" :disabled="!instrument.trim()" @click="refresh">Read demo accounts</button>
    <div v-if="reads" class="account-grid" aria-live="polite">
      <article v-for="{ venue, observation, error } in rows" :key="venue" :aria-label="venue + ' demo account'">
        <h3>{{ venue }}</h3>
        <p v-if="!observation && !error">Reading account…</p>
        <template v-else-if="observation">
          <p>{{ observation.account }} · {{ observation.funds.source }}</p>
          <dl>
            <dt>Available {{ observation.base }}</dt><dd>{{ observation.funds.available_base }}</dd>
            <dt>Available {{ observation.quote }}</dt><dd>{{ observation.funds.available_quote }}</dd>
          </dl>
          <p>Funds read {{ observation.funds.request_started_at }} to {{ observation.funds.received_at }}.</p>
          <template v-if="observation.fees">
            <p>{{ observation.fees.source }} · Buy {{ observation.fees.buyBps }} bps in {{ observation.fees.buyCurrency }} · Sell {{ observation.fees.sellBps }} bps in {{ observation.fees.sellCurrency }}.</p>
            <p>Fees read {{ observation.fees.request_started_at }} to {{ observation.fees.received_at }}.</p>
            <p v-if="venue === 'OKX'">The group schedule requires a separate check for zero-fee promotions.</p>
          </template>
          <p v-else>Taker fees unknown.</p>
        </template>
        <p v-else>{{ error }}</p>
      </article>
    </div>
  </section>
</template>

<style scoped>
.account-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr)); gap: 1rem; }
dd { margin-inline-start: 0; font-variant-numeric: tabular-nums; }
</style>
