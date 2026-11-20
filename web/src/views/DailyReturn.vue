<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { api, ApiError } from '../api/client'
import { formatDecimal, type Decimal } from '../api/decimal'
import { useSession } from '../stores/session'

const props = defineProps<{ portfolio: string }>()
const session = useSession()
const closeDate = ref('')
const currency = ref('')
const asOf = ref('')
const pending = ref(false)
const error = ref('')
const result = ref<{ value?: number; reason?: string; digest: string; flow?: { time: string; amount: string } }>()
let active: AbortController | undefined

const reasons: Record<string, string> = {
  nav_missing: 'One of the finalized NAVs is unavailable.',
  flow_coverage_unknown: 'External flow coverage is unavailable.',
  external_flows_present: 'The interval contains external flows.',
  opening_nav_nonpositive: 'The opening NAV is nonpositive.',
  nav_at_flow_missing: 'A valuation at the cash flow is unavailable.',
  post_flow_nav_nonpositive: 'The post-flow NAV is nonpositive.',
  return_out_of_range: 'The return exceeds the supported range.',
}
const percent = new Intl.NumberFormat('en', { style: 'percent', maximumSignificantDigits: 8 })

function horizonKey(value: string) {
  const parts = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,6}))?Z$/.exec(value)
  return parts && parts[1] + '.' + (parts[2] ?? '').padEnd(6, '0')
}

function clear() {
  active?.abort()
  active = undefined
  pending.value = false
  error.value = ''
  result.value = undefined
}

watch([() => props.portfolio, closeDate, currency, asOf, () => session.identity], clear, { flush: 'sync' })
onUnmounted(clear)

async function read() {
  clear()
  pending.value = true
  const request = new AbortController()
  active = request
  try {
    const query = new URLSearchParams({ close_date: closeDate.value, currency: currency.value, as_of: asOf.value })
    const report = await api.get<{ portfolio_id: string; currency: string; end: string; as_of: string
      source_digest: string; start: string; external_flow_count?: string; measured?: { portfolio_id: string; as_of: string; end: string; method: string; return_value: number }
      unassessed_reason?: string
      intraday_flow?: { effective_time: string; signed_amount: Decimal; pre_flow_nav_reference: string; post_flow_nav_reference: string }
    }>('/api/v1/portfolios/' + encodeURIComponent(props.portfolio) + '/daily-return?' + query, { signal: request.signal })
    const end = closeDate.value + 'T00:00:00Z'
    // Compare instants while allowing the Protobuf JSON serializer to use its
    // own fractional-second formatting. The service binds microseconds.
    if (report.portfolio_id !== props.portfolio || report.currency !== currency.value || report.end !== end ||
        !horizonKey(report.as_of) || horizonKey(report.as_of) !== horizonKey(asOf.value) || !/^[0-9a-f]{64}$/.test(report.source_digest)) throw new Error()
    if (report.measured) {
      const measured = report.measured
      const split = report.intraday_flow
      const amount = formatDecimal(split?.signed_amount)
      const flowTime = split && horizonKey(split.effective_time)
      const startTime = horizonKey(report.start)
      const endTime = horizonKey(report.end)
      const flow = split && flowTime && startTime && endTime && flowTime > startTime && flowTime < endTime && amount &&
        split.pre_flow_nav_reference && split.post_flow_nav_reference && split.pre_flow_nav_reference !== split.post_flow_nav_reference
        ? { time: split.effective_time, amount: amount + ' ' + report.currency } : undefined
      const covered = (!split && report.external_flow_count === '0') || (flow && report.external_flow_count === '1')
      if (report.unassessed_reason !== undefined || !covered || measured.portfolio_id !== props.portfolio ||
          measured.method !== 'RETURN_METHOD_TIME_WEIGHTED' || measured.as_of !== report.as_of ||
          measured.end !== report.end || !Number.isFinite(measured.return_value)) throw new Error()
      result.value = { value: measured.return_value, digest: report.source_digest, flow }
    } else if (report.unassessed_reason && Object.hasOwn(reasons, report.unassessed_reason)) {
      result.value = { reason: reasons[report.unassessed_reason], digest: report.source_digest }
    } else throw new Error()
  } catch (e) {
    if (!request.signal.aborted) error.value = e instanceof ApiError && [403, 404].includes(e.status)
      ? 'Daily return is unavailable for this portfolio and knowledge time.' : 'Unable to verify the daily return.'
  } finally {
    if (!request.signal.aborted) pending.value = false
  }
}
</script>

<template>
  <section aria-label="Daily return" class="panel">
    <h2>Daily return</h2>
    <form @submit.prevent="read">
      <label>UTC close date <input v-model="closeDate" type="date" required /></label>
      <label>Reporting currency <input v-model.trim="currency" pattern="[A-Z]{3}" maxlength="3" required /></label>
      <label>Knowledge time (UTC) <input v-model.trim="asOf" placeholder="2020-02-04T01:00:00Z" maxlength="32" required /></label>
      <button :disabled="pending">View daily return</button>
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <output v-if="result">
      <template v-if="result.value !== undefined">
        <div>Time-weighted return <strong>{{ percent.format(result.value) }}</strong></div>
        <div v-if="result.flow">Two linked subperiods · {{ result.flow.amount }} external flow at {{ result.flow.time }}</div>
      </template>
      <template v-else>Unassessed. {{ result.reason }}</template>
      <p>Signed source <code>{{ result.digest }}</code></p>
    </output>
  </section>
</template>
