<script setup lang="ts">
import { computed, ref } from 'vue'
import { households, describeHousehold, driftDescriptions, type Household } from '../api/households'
const id = ref('')
const snapshot = ref<Household | null>(null)
const loading = ref(false)
const error = ref('')
const positions = computed(() => Object.entries(snapshot.value?.holdings ?? {}).sort(([a], [b]) => a.localeCompare(b)))
const classes = computed(() => Object.entries(snapshot.value?.asset_class ?? {}).sort(([a], [b]) => a.localeCompare(b)))
const deviations = computed(() => Object.entries(snapshot.value?.drift.by_instrument ?? {}).sort(([a], [b]) => a.localeCompare(b)))
async function lookup() {
  const householdID = id.value.trim()
  if (!householdID || loading.value) return
  loading.value = true; error.value = ''; snapshot.value = null
  try { snapshot.value = await households.get(householdID) }
  catch (e) { error.value = describeHousehold(e) }
  finally { loading.value = false }
}
</script>
<template>
  <h1>Households</h1>
  <p class="muted">Review a household valuation and its drift against the published model allocation.</p>
  <form @submit.prevent="lookup">
    <label>Household ID<input v-model="id" name="household-id" maxlength="256" required :disabled="loading" /></label>
    <button type="submit" :disabled="loading">{{ loading ? 'Loading valuation…' : 'Look up household' }}</button>
  </form>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <section v-else-if="snapshot" aria-label="Household valuation">
    <h2>{{ snapshot.household_id }}</h2>
    <p class="state-warn">Valuation snapshot. Freshness has not been established; use the source valuation time below.</p>
    <dl class="event-detail">
      <div><dt>Total value · {{ snapshot.currency_code }}</dt><dd>{{ snapshot.total_value }}</dd></div>
      <div><dt>Cash · {{ snapshot.currency_code }}</dt><dd>{{ snapshot.cash }}</dd></div>
      <div><dt>Valuation time</dt><dd>{{ snapshot.as_of }}</dd></div>
      <div><dt>Risk profile</dt><dd>{{ snapshot.risk_profile }}</dd></div>
      <div><dt>Source recorded by</dt><dd>{{ snapshot.recorded_by }}</dd></div>
      <div><dt>Source reference</dt><dd>{{ snapshot.source_reason }}</dd></div>
    </dl>
    <p class="muted">Amounts and allocation ratios are exact. A fraction such as 1/3 is shown without rounding; ratios are shares of total value.</p>
    <h2>Holdings</h2>
    <div class="table-scroll"><table><thead><tr><th>Instrument</th><th>Value · {{ snapshot.currency_code }}</th><th>Share of total</th></tr></thead>
      <tbody><tr v-for="([instrument, value]) in positions" :key="instrument"><td><code>{{ instrument }}</code></td><td>{{ value }}</td><td>{{ snapshot.weights?.[instrument] ?? 'Unavailable' }}</td></tr>
      <tr v-if="positions.length === 0"><td colspan="3">No holdings are recorded in this snapshot.</td></tr></tbody>
    </table></div>
    <h2>Asset allocation</h2>
    <p v-if="snapshot.weights_state === 'unavailable'" class="state-warn">Allocation weights are unavailable. A zero or negative total cannot supply meaningful portfolio shares.</p>
    <div v-else class="table-scroll"><table><thead><tr><th>Asset class</th><th>Share of total</th></tr></thead>
      <tbody><tr v-for="([assetClass, weight]) in classes" :key="assetClass"><td>{{ assetClass || 'Unclassified' }}</td><td>{{ weight }}</td></tr>
      <tr v-if="classes.length === 0"><td colspan="2">No asset allocation was returned.</td></tr></tbody>
    </table></div>
    <h2>Model drift</h2>
    <p :class="snapshot.drift.outcome === 'in_band' ? '' : 'state-warn'">{{ driftDescriptions[snapshot.drift.outcome] }}</p>
    <template v-if="snapshot.drift.evaluated">
      <dl class="event-detail">
        <div><dt>Model</dt><dd>{{ snapshot.drift.model_id }}</dd></div>
        <div><dt>Permitted band</dt><dd>{{ snapshot.drift.tolerance }}</dd></div>
        <div><dt>Maximum absolute drift</dt><dd>{{ snapshot.drift.max }}</dd></div>
        <div><dt>Total absolute drift</dt><dd>{{ snapshot.drift.total }}</dd></div>
      </dl>
      <div class="table-scroll"><table><thead><tr><th>Instrument</th><th>Actual − target share</th></tr></thead><tbody><tr v-for="([instrument, deviation]) in deviations" :key="instrument"><td>{{ instrument }}</td><td>{{ deviation }}</td></tr></tbody></table></div>
    </template>
  </section>
  <p v-else-if="!loading" class="muted">Enter a household ID to retrieve its valuation snapshot.</p>
</template>
