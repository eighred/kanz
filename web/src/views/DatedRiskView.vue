<script setup lang="ts">
import { computed, onUnmounted, ref, shallowRef, watch } from 'vue'
import { useRoute } from 'vue-router'
import { lossMeasures, readDatedRisk, reportedLoss, type DatedRiskResponse } from '../api/dated-risk'
import { describeFlag, describeRisk } from '../api/risk'

const route = useRoute()
const portfolio = computed(() => String(route.params.id ?? ''))
const cutoff = ref('')
const report = shallowRef<DatedRiskResponse>()
const error = ref('')
const loading = ref(false)
let pending: AbortController | undefined

function clearReport() {
  pending?.abort()
  report.value = undefined
  loading.value = false
  error.value = ''
}
watch([portfolio, cutoff], clearReport, { flush: 'sync' })
onUnmounted(clearReport)

async function load() {
  clearReport()
  const request = new AbortController()
  pending = request
  loading.value = true
  try {
    const result = await readDatedRisk(portfolio.value, cutoff.value, request.signal)
    if (!request.signal.aborted) report.value = result
  } catch (e) {
    if (!request.signal.aborted) error.value = describeRisk(e)
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}

const rows = computed(() => lossMeasures.map(({ key, label }) => ({
  label, measure: report.value?.set.measures?.find(m => m.name === key),
})))

function exportReport() {
  if (!report.value) return
  const body = JSON.stringify({ requested_cutoff: cutoff.value, response: report.value }, null, 2)
  const url = URL.createObjectURL(new Blob([body], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = 'portfolio-risk-' + cutoff.value.replace(/:/g, '-') + '.json'
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
</script>

<template>
  <h1>Historical portfolio risk</h1>
  <p>Portfolio <code>{{ portfolio }}</code></p>
  <form @submit.prevent="load">
    <label for="risk-cutoff">Knowledge cutoff (UTC)</label>
    <input id="risk-cutoff" v-model.trim="cutoff" required placeholder="2026-01-31T23:59:59Z" />
    <button type="submit" :disabled="loading">Read evaluation</button>
  </form>
  <p v-if="loading" role="status">Reading retained evaluation…</p>
  <p v-if="error" role="alert" class="error">{{ error }}</p>
  <section v-if="report">
    <h2>Evaluation at {{ report.as_of }}</h2>
    <p v-for="flag in report.quality_flags" :key="flag" class="state-warn">{{ describeFlag(flag) }}</p>
    <article v-for="{ label, measure } in rows" :key="label">
      <h3>{{ label }}</h3>
      <p><strong>{{ reportedLoss(measure, report.retained_evaluation) }}</strong></p>
      <p v-if="measure?.coverage">
        {{ measure.coverage.contributed }} positions contributed · {{ measure.coverage.excluded_count }} excluded
      </p>
      <ul v-if="measure?.coverage?.exclusions?.length">
        <li v-for="(exclusion, index) in measure.coverage.exclusions" :key="index">
          {{ exclusion.instrument_id || 'Evaluation' }} · {{ exclusion.reason }}
        </li>
      </ul>
      <template v-if="measure?.provenance">
        <p>Method · {{ measure.provenance.method || 'Unknown' }}</p>
        <p v-if="measure.provenance.model_id">Model · {{ measure.provenance.model_id }}</p>
        <template v-if="measure.provenance.method === 'historical_simulation'">
          <p>Confidence · {{ measure.provenance.params?.confidence || 'Unknown' }}</p>
          <p>Historical scenarios · {{ measure.provenance.params?.panel_scenarios || 'Unknown' }}</p>
          <p>Return horizon · {{ measure.provenance.params?.panel_calendar === 'continuous_utc_24h' ? '24 hours (UTC)' : 'Unknown' }}</p>
        </template>
        <details v-if="measure.provenance.params">
          <summary>Calculation parameters</summary>
          <dl>
            <template v-for="(value, parameter) in measure.provenance.params" :key="parameter">
              <dt>{{ parameter }}</dt><dd>{{ value }}</dd>
            </template>
          </dl>
        </details>
      </template>
    </article>
    <dl v-if="report.retained_evaluation">
      <dt>Original code revision</dt><dd><code>{{ report.retained_evaluation.code_revision }}</code></dd>
      <dt>Input manifest SHA-256</dt><dd><code>{{ report.retained_evaluation.input_manifest_digest }}</code></dd>
    </dl>
    <p v-if="report.source_position">
      Source {{ report.source_position.topic }} · partition {{ report.source_position.partition }} · offset {{ report.source_position.offset }}
    </p>
    <button type="button" @click="exportReport">Download JSON</button>
  </section>
</template>

<style scoped>
section code, dd { overflow-wrap: anywhere; }
article { margin-block: 1.5rem; }
form { display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap; }
input { min-width: min(28ch, 100%); }
</style>
