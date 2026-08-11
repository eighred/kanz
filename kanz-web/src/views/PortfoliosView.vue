<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { risk, describeRisk, type PortfolioSummary } from '../api/risk'

// THE SCREEN THAT HAD NOWHERE TO GET AN ID (#399).
//
// Exposure has been reachable at /v1/portfolios/{id}/exposure since long before
// the web app existed, and every route on that surface needs an id the caller
// must already hold. There was no way to obtain one, so a "read screen" could
// only ever have been a box to type an id into. This is the list that route
// always assumed.
//
// WHAT THIS IS NOT: the fund's book of record. The risk engine returns the
// portfolios it has folded state for, so one that exists and has had no event
// reach the engine is absent. Saying "your portfolios" here would be a claim
// this data cannot support, which is why the subtitle says what it actually is.

const rows = ref<PortfolioSummary[]>([])
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    rows.value = await risk.portfolios()
  } catch (e) {
    // A 404 here does NOT mean "you have no portfolios" — the gateway answers
    // it for "not yours" too, so that a status code cannot enumerate another
    // tenant's portfolios. Drawn as an empty table it would say the fund has
    // none, which is the one reading this screen must never offer.
    error.value = describeRisk(e)
  } finally {
    loading.value = false
  }
})

/** age renders as_of as a coarse duration. Staleness is why the column exists. */
function age(iso: string): string {
  if (!iso) return '—'
  const ms = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const days = Math.floor(ms / 86_400_000)
  if (days >= 1) return `${days}d`
  const hours = Math.floor(ms / 3_600_000)
  if (hours >= 1) return `${hours}h`
  return `${Math.max(1, Math.floor(ms / 60_000))}m`
}

// A PORTFOLIO NOT FOLDED IN A DAY IS WORTH SEEING AS SUCH. The engine reports
// as_of; this is the only screen where two otherwise identical rows differ by
// it, so it is the only place the difference can be noticed before someone acts
// on a stale figure.
const STALE_MS = 86_400_000

function stale(iso: string): boolean {
  if (!iso) return true
  const t = new Date(iso).getTime()
  return !Number.isFinite(t) || Date.now() - t > STALE_MS
}
</script>

<template>
  <h1>Portfolios</h1>
  <p class="muted">
    Portfolios the risk engine holds state for. One the fund has opened but that no event has
    reached yet will not appear here.
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <table v-else>
    <thead>
      <tr><th>Portfolio</th><th>Currency</th><th>Positions</th><th>Last folded</th></tr>
    </thead>
    <tbody>
      <tr v-for="p in rows" :key="p.portfolio_id">
        <td>
          <!-- THE ID IS SHOWN, NOT HIDDEN BEHIND THE NAME, because it is what
               every risk route takes and what an operator needs when reading a
               URL or an error. It is not yet a link: the exposure screen it
               would open renders common.v1.Decimal money, and a Decimal is
               {coefficient, exponent} with an int64 coefficient that protojson
               sends as a STRING. Reading that through a JS number loses
               precision above 2^53 — silently, and on a valuation. That screen
               needs a decimal renderer decided on its own merits rather than
               written in passing here. -->
          <code>{{ p.portfolio_id }}</code>
          <span v-if="p.display_name" class="muted"> · {{ p.display_name }}</span>
        </td>
        <td>{{ p.base_currency || '—' }}</td>
        <td>{{ p.position_count }}</td>
        <td :class="stale(p.as_of) ? 'state-warn' : 'state-ok'">{{ age(p.as_of) }}</td>
      </tr>
      <tr v-if="rows.length === 0">
        <td colspan="4">
          The risk engine holds no portfolio state. This is an engine that has folded nothing
          yet, not necessarily a fund with no portfolios.
        </td>
      </tr>
    </tbody>
  </table>
</template>
