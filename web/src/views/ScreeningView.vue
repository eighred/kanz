<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { screening, describeScreenError, validScreenInput, type ScreenInput, type ScreenResult } from '../api/screening'

const input = ref<ScreenInput>({ portfolio_id: '', base_currency: '', as_of: '', positions: [], excluded_sectors: [], excluded_issuers: [] })
const sectors = ref('')
const issuers = ref('')
const loading = ref(false)
const error = ref('')
const result = ref<ScreenResult | null>(null)
const submitted = ref<ScreenInput | null>(null)
const request = computed<ScreenInput>(() => ({ ...input.value, excluded_sectors: sectors.value.split('\n').map(s => s.trim()).filter(Boolean), excluded_issuers: issuers.value.split('\n').map(s => s.trim()).filter(Boolean) }))
watch([input, sectors, issuers], () => { if (!loading.value) { result.value = null; error.value = '' } }, { deep: true })
async function screen() {
  if (loading.value || !validScreenInput(request.value)) return
  loading.value = true
  error.value = ''
  result.value = null
  submitted.value = { ...request.value, positions: request.value.positions.map(p => ({ ...p })) }
  try { result.value = await screening.esg(submitted.value) }
  catch (e) { error.value = describeScreenError(e) }
  finally { loading.value = false }
}
</script>

<template>
  <h1>ESG exclusion screening</h1>
  <p class="muted">Evaluate a caller-supplied snapshot against explicit sector and issuer exclusions. The server resolves classifications. This screen does not certify the recorded book or authorize trading.</p>
  <form @submit.prevent="screen">
    <fieldset :disabled="loading">
      <legend>Snapshot and policy</legend>
      <label>Portfolio ID<input v-model="input.portfolio_id" name="portfolio" required maxlength="256" /></label>
      <label>Base currency<input v-model="input.base_currency" name="currency" required maxlength="16" /></label>
      <label>Snapshot time (RFC3339 with timezone)<input v-model="input.as_of" name="as-of" required /></label>
      <label>Excluded sector codes (one per line)<textarea v-model="sectors" name="sectors" rows="3" /></label>
      <label>Excluded issuer IDs (one per line)<textarea v-model="issuers" name="issuers" rows="3" /></label>
      <p>At least one exclusion is required. Use actual source values; no policy is inferred.</p>
      <h2>Positions</h2>
      <p v-if="!input.positions.length">No positions entered. Submitting this snapshot declares it empty.</p>
      <div v-for="(position, i) in input.positions" :key="i" class="position-row">
        <label>Instrument ID<input v-model="position.instrument_id" required maxlength="256" /></label>
        <label>Quantity<input v-model="position.quantity" maxlength="400" /></label>
        <label>Market value<input v-model="position.market_value" maxlength="400" /></label>
        <label>Currency<input v-model="position.currency" maxlength="16" /></label>
        <button type="button" @click="input.positions.splice(i, 1)">Remove position</button>
      </div>
      <p class="muted">Blank amounts remain unavailable. An explicit zero remains zero. Blank position currency uses the stated base currency.</p>
      <button type="button" :disabled="input.positions.length >= 4096" @click="input.positions.push({ instrument_id: '', quantity: '', market_value: '', currency: '' })">Add position</button>
      <button type="submit" :disabled="!validScreenInput(request)">{{ loading ? 'Screening…' : 'Screen snapshot' }}</button>
    </fieldset>
  </form>
  <p v-if="loading" role="status">Evaluating the submitted snapshot…</p>
  <p v-if="error" role="alert" class="error">{{ error }}</p>
  <section v-if="result && submitted" aria-label="Screening result">
    <h2>{{ result.status === 'COMPLIANCE_STATUS_PASS' ? 'Exclusion screen passed' : result.violations.some(v => v.unavailable) ? 'Screening refused or breached' : 'Exclusion breach' }}</h2>
    <p>{{ submitted.portfolio_id }} · {{ result.evaluated_at }} · {{ submitted.positions.length }} submitted positions</p>
    <p v-if="!submitted.positions.length" class="state-warn">The supplied snapshot was empty. No holding was assessed.</p>
    <p class="muted">Snapshot completeness and freshness are not established. This result covers only the exclusions and holdings you submitted.</p>
    <article v-for="violation in result.violations" :key="violation.rule_id">
      <h3>{{ violation.unavailable ? 'Required data unavailable' : 'Excluded holding' }} · {{ violation.rule_id }}</h3>
      <p>{{ violation.message }}</p>
      <dl><template v-for="(value, key) in violation.evidence" :key="key"><dt>{{ key }}</dt><dd>{{ value }}</dd></template></dl>
    </article>
  </section>
</template>

<style scoped>
fieldset { border: 1px solid var(--border, #ddd); }
textarea { display: block; width: 100%; box-sizing: border-box; }
.position-row { padding: 1rem 0; border-bottom: 1px solid var(--border, #ddd); }
dd { overflow-wrap: anywhere; }
</style>
