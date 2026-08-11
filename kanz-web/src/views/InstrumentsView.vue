<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  byInstrument,
  describeInstruments,
  instruments as instrumentsApi,
  type TradeableInstrument,
} from '../api/instruments'

// WHAT THIS DEPLOYMENT CAN ACTUALLY TRADE (#406).
//
// It answers two questions with one list. For an operator: "what can this
// deployment trade?", which previously had no answer short of reading a
// manifest. For a pair picker: the only honest source of pairs — the frontend
// must never hardcode a list, because the set is a property of the deployment's
// venue adapters and their symbol maps.
//
// EVERY PAIR HERE IS ROUTABLE. The API reports configuration, not an exchange's
// catalogue, so a pair shown here has a symbol mapping and will not be refused
// at admission with VENUE_NOT_CONFIGURED. That refusal reads to an operator as a
// platform fault, so offering an unroutable pair would be worse than omitting it.

const rows = ref<TradeableInstrument[]>([])
const error = ref('')
const loading = ref(true)
const served = ref(true)

onMounted(async () => {
  try {
    const resp = await instrumentsApi.list()
    rows.value = resp.instruments ?? []
  } catch (e) {
    served.value = false
    error.value = describeInstruments(e)
  } finally {
    loading.value = false
  }
})

/** One row per canonical id, carrying every venue that lists it. */
const grouped = computed(() => byInstrument(rows.value))

/**
 * quoteDiffers marks a pair whose exchange symbol does not match what the
 * canonical id claims — "BTC-USD" trading as BTCUSDT.
 *
 * IT IS SHOWN, not smoothed over. A USDT-quoted position carries USDT credit
 * exposure, and marking it USD assumes a peg this platform never states and
 * cannot monitor. The user picking this pair is the person who should know.
 */
function quoteDiffers(in_: TradeableInstrument): boolean {
  const quote = in_.instrument_id.split('-')[1] ?? ''
  if (!quote) return false
  const symbol = in_.venue_symbol.toUpperCase().replaceAll('-', '')
  return !symbol.endsWith(quote.toUpperCase())
}
</script>

<template>
  <h1>Instruments</h1>
  <p class="muted">
    <RouterLink to="/portfolios">Portfolios</RouterLink> · what this deployment can route
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <!-- AN EMPTY CATALOGUE IS AN ANSWER, and a loud one: this deployment's
         adapters hold no symbol map, so it can route nothing. Rendering it as a
         blank table would let a misconfigured estate look merely quiet. -->
    <p v-if="grouped.length === 0" class="error" role="alert">
      This deployment can trade nothing. No venue adapter reports a symbol map, so every order would
      be refused at admission. This is a configuration state, not an empty market.
    </p>

    <table v-else>
      <thead>
        <tr>
          <th>Instrument</th>
          <th>Venue</th>
          <th>Exchange symbol</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="g in grouped" :key="g.instrument_id">
          <tr v-for="(v, i) in g.venues" :key="v.mic">
            <td>
              <code v-if="i === 0">{{ g.instrument_id }}</code>
              <span v-else class="muted">↳</span>
            </td>
            <td>{{ v.mic }}</td>
            <td>
              <code>{{ v.venue_symbol }}</code>
              <!-- NOT A COSMETIC NOTE. The id says USD and the venue trades USDT:
                   a different asset, a different credit exposure, and a peg
                   nothing here monitors. #407 carries the fix; until then this
                   is where a person can see it. -->
              <span v-if="quoteDiffers(v)" class="state-warn">
                — quoted in
                {{ v.venue_symbol.toUpperCase().replaceAll('-', '').slice(-4) }}, not
                {{ (g.instrument_id.split('-')[1] ?? '').toUpperCase() }}
              </span>
            </td>
          </tr>
        </template>
      </tbody>
    </table>
  </template>
</template>
