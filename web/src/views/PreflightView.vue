<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { preflight, type PreflightCheck, type PreflightState, type PreflightStatus } from '../api/preflight'

const result = ref<PreflightStatus | null>(null)
const loading = ref(true)
const transportError = ref('')
let timer: ReturnType<typeof setInterval> | undefined
const REFRESH_MS = 30_000

function knownState(value: unknown): value is PreflightState {
  return value === 'PASS' || value === 'FAIL' || value === 'UNKNOWN'
}

function normalize(value: PreflightStatus): PreflightStatus {
  if (!knownState(value.status) || !Array.isArray(value.checks)) {
    return { status: 'UNKNOWN', reason: 'preflight response is incomplete', max_age_seconds: 0, checks: [] }
  }
  const checks = value.checks.map((check): PreflightCheck => ({
    name: typeof check?.name === 'string' ? check.name : 'unnamed control',
    status: knownState(check?.status) ? check.status : 'UNKNOWN',
    observed: check?.observed,
  }))
  if (value.status === 'PASS' && checks.some((check) => check.status !== 'PASS')) {
    return { ...value, status: 'UNKNOWN', reason: 'preflight verdict disagrees with its controls', checks }
  }
  return { ...value, checks }
}

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    result.value = normalize(await preflight.status())
    transportError.value = ''
  } catch {
    result.value = null
    transportError.value = 'Preflight evidence is unreachable. Status is UNKNOWN.'
  } finally {
    loading.value = false
  }
}

function stateClass(state: PreflightState): string {
  if (state === 'PASS') return 'state-ok'
  if (state === 'FAIL') return 'state-bad'
  return 'state-warn'
}

function displayName(value: string): string { return value.replaceAll('_', ' ') }

function observed(value: unknown): string {
  if (typeof value === 'string') return value
  if (value === null || value === undefined) return 'not stated'
  return JSON.stringify(value)
}

function when(value?: string): string {
  if (!value) return 'not stated'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'invalid timestamp' : date.toLocaleString()
}

onMounted(() => {
  void load(true)
  timer = setInterval(() => void load(), REFRESH_MS)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <h1>Trading preflight</h1>
  <p class="muted">
    Read-only evidence from the exact-main capital-admission verifier. A PASS supports human
    reassessment only; this page cannot resume the OMS, publish a resume FACT, or submit an order.
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="transportError" class="preflight-verdict state-warn" role="alert">UNKNOWN</p>
  <p v-if="transportError" class="error" role="alert">{{ transportError }}</p>

  <template v-else-if="result">
    <section class="preflight-summary" :class="stateClass(result.status)" aria-live="polite">
      <strong class="preflight-verdict">{{ result.status }}</strong>
      <span>{{ result.reason || `${result.checks.filter((check) => check.status === 'PASS').length} of ${result.checks.length} controls passed` }}</span>
    </section>

    <dl class="evidence-meta">
      <div><dt>Observed</dt><dd>{{ when(result.observed_at) }}</dd></div>
      <div><dt>Deployed commit</dt><dd class="digest">{{ result.deployed_commit || 'not stated' }}</dd></div>
      <div><dt>Verifier commit</dt><dd class="digest">{{ result.verifier_commit || 'not stated' }}</dd></div>
      <div><dt>Evidence command</dt><dd class="digest">{{ result.command_id || 'not stated' }}</dd></div>
    </dl>

    <table>
      <thead><tr><th>Control</th><th>Result</th><th>Observed evidence</th></tr></thead>
      <tbody>
        <tr v-for="check in result.checks" :key="check.name">
          <td>{{ displayName(check.name) }}</td>
          <td :class="stateClass(check.status)"><strong>{{ check.status }}</strong></td>
          <td class="evidence-value">{{ observed(check.observed) }}</td>
        </tr>
        <tr v-if="result.checks.length === 0"><td colspan="3">No verified control evidence is available.</td></tr>
      </tbody>
    </table>

    <table v-if="result.workload_images && Object.keys(result.workload_images).length">
      <thead><tr><th>Workload</th><th>Deployed image</th></tr></thead>
      <tbody>
        <tr v-for="(image, name) in result.workload_images" :key="name">
          <td>{{ displayName(name) }}</td><td class="digest">{{ image }}</td>
        </tr>
      </tbody>
    </table>
  </template>
</template>
