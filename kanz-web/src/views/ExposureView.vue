<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { describeFlag, describeRisk, risk, type ExposureResponse, type ExposureState } from '../api/risk'
import { formatMoney, isNegative } from '../api/decimal'

// ONE PORTFOLIO'S EXPOSURE (#399/#371 slice 5).
//
// THE NUMBERS ARE RENDERED WITHOUT A FLOAT ANYWHERE. Every figure here is a
// common.v1.Decimal whose sint64 coefficient arrives as a STRING, because 64
// bits do not survive a JSON number. api/decimal.ts formats it by moving the
// decimal point over a digit string; nothing on this screen calls Number() on a
// value. A valuation that is quietly wrong is worse than one that fails to
// render, which is why an unrenderable figure shows as "—" and never as 0.
//
// THE QUALITY FLAGS ARE NOT DECORATION. CURRENCY_EXCLUDED means positions were
// OMITTED from these totals, so the real exposure is LARGER than what is shown.
// The proto says a gate that must not under-report has to refuse to act on such
// a response. A screen cannot refuse on the reader's behalf, so it says plainly
// that the totals are incomplete rather than printing them as totals.

const route = useRoute()
const id = computed(() => String(route.params.id ?? ''))

const data = ref<ExposureResponse | null>(null)
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    data.value = await risk.exposure(id.value)
  } catch (e) {
    // A 404 here is BOTH "no such portfolio" and "not yours" — the gateway
    // collapses them so the route cannot be used to enumerate another tenant's
    // portfolios by status code. Neither reading is "this portfolio is empty".
    error.value = describeRisk(e)
  } finally {
    loading.value = false
  }
})

const flags = computed(() =>
  (data.value?.quality_flags ?? []).filter((f) => f !== 'QUALITY_FLAG_UNSPECIFIED'),
)

/** underReporting is the flag that changes what the totals MEAN. */
const underReporting = computed(() => flags.value.includes('QUALITY_FLAG_CURRENCY_EXCLUDED'))

/** byDimension groups the flat exposure list into one table per axis. */
const byDimension = computed(() => {
  const groups = new Map<string, ExposureState[]>()
  for (const e of data.value?.set?.exposures ?? []) {
    const key = e.dimension ?? 'EXPOSURE_DIMENSION_UNSPECIFIED'
    const list = groups.get(key)
    if (list) list.push(e)
    else groups.set(key, [e])
  }
  return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
})

/** label turns the enum name into something readable without a lookup table. */
function label(dimension: string): string {
  return dimension.replace(/^EXPOSURE_DIMENSION_/, '').replaceAll('_', ' ').toLowerCase()
}

// "—" NOT "0". An amount that cannot be rendered — an out-of-domain exponent, a
// malformed coefficient — must look unrenderable. internal/dec makes the same
// rule at the same boundary: a zero is not a safe fallback, because it reads as
// "flat" and is dropped from whatever is checked downstream.
function money(m: Parameters<typeof formatMoney>[0]): string {
  return formatMoney(m) ?? '—'
}
</script>

<template>
  <h1>Exposure</h1>
  <p class="muted">
    <RouterLink to="/portfolios">Portfolios</RouterLink> ·
    <code>{{ id }}</code>
    <span v-if="data?.as_of"> · as of {{ data.as_of }}</span>
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <!-- THE UNDER-REPORTING FLAG GETS THE ERROR TREATMENT, not a hint. It does
         not mean the data is old; it means the totals below are missing
         positions and are therefore smaller than the truth. -->
    <p v-for="f in flags" :key="f"
       :class="f === 'QUALITY_FLAG_CURRENCY_EXCLUDED' ? 'error' : 'hint'"
       :role="f === 'QUALITY_FLAG_CURRENCY_EXCLUDED' ? 'alert' : undefined">
      {{ describeFlag(f) }}
    </p>

    <p v-if="underReporting" class="muted">
      The figures below are shown because hiding them would be worse — but they are a floor, not a
      total.
    </p>

    <section v-for="[dimension, rows] in byDimension" :key="dimension">
      <h2>{{ label(dimension) }}</h2>
      <table>
        <thead><tr><th>Bucket</th><th>Gross</th><th>Net</th></tr></thead>
        <tbody>
          <tr v-for="e in rows" :key="e.bucket">
            <td>{{ e.bucket }}</td>
            <td>{{ money(e.gross) }}</td>
            <!-- The sign is read off the coefficient STRING, never a parsed
                 number: a short position beyond 2^53 must still colour red. -->
            <td :class="isNegative(e.net?.amount) ? 'state-bad' : ''">{{ money(e.net) }}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <p v-if="byDimension.length === 0" class="muted">
      The engine holds this portfolio but has computed no exposure for it.
    </p>
  </template>
</template>
