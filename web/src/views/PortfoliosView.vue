<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
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

const now = ref(Date.now())
let clock: ReturnType<typeof setInterval>
onMounted(async () => {
  clock = setInterval(() => { now.value = Date.now() }, 60_000)
  try {
    rows.value = await risk.portfolios()
    now.value = Date.now()
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

onUnmounted(() => clearInterval(clock))

// The gateway emits UTC protobuf timestamps. Refuse malformed or future values
// before assigning the same healthy style as a recent observation.
function freshness(iso: string, current: number) {
  if (typeof iso !== 'string') return { label: 'Unknown', warning: true }
  const folded = Date.parse(iso)
  const age = current - folded
  if (!/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(iso) || !Number.isFinite(folded) ||
      new Date(folded).toISOString().slice(0, 19) !== iso.slice(0, 19) || age < 0) {
    return { label: 'Unknown', warning: true }
  }
  const minutes = Math.floor(age / 60_000)
  if (minutes < 1) return { label: 'Less than 1m ago', warning: false }
  if (minutes < 60) return { label: `${minutes}m ago`, warning: false }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return { label: `${hours}h ago`, warning: false }
  return { label: `${Math.floor(hours / 24)}d ago (stale)`, warning: true }
}

const portfolios = computed(() => rows.value.map(row => ({ ...row, freshness: freshness(row.as_of, now.value) })))
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
      <tr v-for="p in portfolios" :key="p.portfolio_id">
        <td>
          <!-- THE ID IS THE LINK, not the display name: it is what every risk
               route takes and what an operator needs when reading a URL or an
               error. The name sits beside it rather than replacing it. -->
          <RouterLink :to="{ name: 'exposure', params: { id: p.portfolio_id } }">
            <code>{{ p.portfolio_id }}</code>
          </RouterLink>
          <span v-if="p.display_name" class="muted"> · {{ p.display_name }}</span>
        </td>
        <td>{{ p.base_currency || '—' }}</td>
        <td>{{ p.position_count }}</td>
        <td :class="p.freshness.warning ? 'state-warn' : 'state-ok'">{{ p.freshness.label }}</td>
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
