<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { usePortfolioRead } from '../usePortfolioRead'
import { useRoute } from 'vue-router'
import { risk, describeOrders, orderHistoryLimit, type OrderSummary } from '../api/risk'
import { useSession } from '../stores/session'
import { formatDecimal } from '../api/decimal'

// ONE PORTFOLIO'S TRADING HISTORY (#399).
//
// THE SCREEN'S HARDEST REQUIREMENT IS NOT RENDERING ORDERS. It is being honest
// about the ones it cannot show. Orders admitted before the OMS's portfolio
// index carry no portfolio_id — their real one is inside a marshaled blob no
// index can see — so they can never appear in any page, however this query is
// written. The API returns how many there are, and this says so. A history that
// silently omits them is a partial book presented as a complete one, which is
// the reading a person acts on.
//
// Quantities and prices are common.v1.Decimal and go through api/decimal.ts: no
// figure on this page passes through a JS number, and one that cannot be
// rendered shows as "—" rather than 0.

const route = useRoute()
const id = computed(() => String(route.params.id ?? ''))

const session = useSession()
const refresh = ref(0)
const selection = computed(() => ({ portfolio: id.value, tenant: session.identity?.tenant, subject: session.identity?.subject, refresh: refresh.value }))
const { data, error, loading } = usePortfolioRead(selection, async ({ portfolio, tenant, subject }, signal) => {
  if (!tenant || !subject) throw new Error('Sign in to read order history.')
  // Start the observation window before the read so a slow response cannot
  // reset the age of the snapshot when it arrives.
  const started = performance.now()
  const requestedAt = new Date().toISOString()
  const page = await risk.orders(portfolio, tenant, signal)
  return { ...page, started, requestedAt }
}, describeOrders)

// The age of the page is independent of any order's last effective time.
const stale = ref(false)
watch(data, (page, _, cleanup) => {
  stale.value = false
  if (!page) return
  const remaining = 60_000 - (performance.now() - page.started)
  if (remaining <= 0) {
    stale.value = true
    return
  }
  const timer = window.setTimeout(() => { stale.value = true }, remaining)
  cleanup(() => window.clearTimeout(timer))
}, { flush: 'sync' })

const orders = computed<OrderSummary[]>(() => data.value?.orders ?? [])

// unindexed arrives as a STRING (int64). It is compared as a string rather than
// parsed: the only question asked of it is "is it zero", and parsing an int64
// to answer that is how a large one becomes 9007199254740992.
const unindexed = computed(() => data.value?.unindexed)
const hasUnindexed = computed(() => unindexed.value !== undefined && unindexed.value !== '0')

function qty(d: Parameters<typeof formatDecimal>[0]): string {
  return formatDecimal(d) ?? '—'
}
function enumLabel(value: string | number | undefined, prefix: string): string {
  if (typeof value === 'number') return `unknown (${value})`
  if (!value || value === prefix + 'UNSPECIFIED') return 'unknown'
  return value.replace(prefix, '').replaceAll('_', ' ').toLowerCase()
}

/** tone colours an order by outcome, with unknown deliberately not green. */
function tone(o: OrderSummary): string {
  switch (o.status) {
    case 'ORDER_STATUS_FILLED':
      return 'state-ok'
    case 'ORDER_STATUS_REJECTED':
    case 'ORDER_STATUS_EXPIRED':
      return 'state-bad'
    case 'ORDER_STATUS_CANCELLED':
      return 'state-warn'
    case 'ORDER_STATUS_WORKING_SCHEDULED':
    case 'ORDER_STATUS_PARTIALLY_FILLED':
    case 'ORDER_STATUS_ROUTED':
    case 'ORDER_STATUS_PENDING_NEW':
      return 'state-warn'
    default:
      // A status the platform did not state is not an order that succeeded.
      return 'state-unset'
  }
}
</script>

<template>
  <h1>Orders</h1>
  <p class="muted">
    <RouterLink to="/portfolios">Portfolios</RouterLink> ·
    <RouterLink :to="{ name: 'exposure', params: { id } }">Exposure</RouterLink> ·
    <code>{{ id }}</code>
  </p>

  <button type="button" :disabled="loading" @click="refresh++">Refresh order history</button>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="data" :class="stale ? 'state-warn' : 'muted'" :role="stale ? 'status' : undefined">
      OMS history requested at <time :datetime="data.requestedAt">{{ data.requestedAt }}</time>.
      <span v-if="stale">This snapshot is stale. Refresh to see subsequent order changes.</span>
    </p>
    <!-- NOT A HINT. These orders are missing from the table below and cannot be
         retrieved by any page of it, so the count is part of the answer rather
         than a note about it. -->
    <p v-if="hasUnindexed" class="error" role="alert">
      {{ unindexed }} order(s) in this tenant are not indexed by portfolio and cannot appear
      here. They were placed before the OMS recorded the portfolio alongside each order, so
      this is a partial history — not necessarily this portfolio's whole trading record.
    </p>

    <p v-if="orders.length >= orderHistoryLimit" class="state-warn" role="status">
      Showing the {{ orderHistoryLimit }} most recent indexed orders. Earlier indexed orders may exist.
    </p>
    <table>
      <thead>
        <tr>
          <th>Order</th><th>Instrument</th><th>Side</th><th>Status</th>
          <th>Quantity</th><th>Filled</th><th>Limit price</th><th>Avg fill</th>
          <th>Unfilled quantity</th><th>Market mark at admission</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="o in orders" :key="o.order_id">
          <td>
            <code>{{ o.order_id }}</code>
            <span v-if="o.venue" class="muted"><br />{{ o.venue }}</span>
          </td>
          <td>{{ o.instrument_id || '—' }}</td>
          <td>{{ enumLabel(o.side, 'SIDE_') }}</td>
          <td :class="o.quarantine ? 'state-warn' : tone(o)">
            <template v-if="o.quarantine">
              Remote outcome unknown · reconciliation required<br />
              Last recorded status: {{ enumLabel(o.status, 'ORDER_STATUS_') }}<br />
              Quarantined at <time :datetime="o.quarantine.at">{{ o.quarantine.at }}</time>
              <template v-if="o.quarantine.last_query_at">
                <br />Last venue query: <time :datetime="o.quarantine.last_query_at">{{ o.quarantine.last_query_at }}</time>
              </template>
            </template>
            <template v-else>{{ enumLabel(o.status, 'ORDER_STATUS_') }}</template>
            <template v-if="o.as_of">
              <br />Effective at <time :datetime="o.as_of">{{ o.as_of }}</time>
            </template>
          </td>
          <td>{{ qty(o.ordered_quantity) }}</td>
          <td>{{ qty(o.filled_quantity) }}</td>
          <td>{{ qty(o.limit_price) }}</td>
          <td>{{ qty(o.average_fill_price) }}</td>
          <td>{{ qty(o.leaves_quantity) }}</td>
          <td>
            <template v-if="o.arrival_at && formatDecimal(o.arrival_price) !== null">
              {{ qty(o.arrival_price) }}<br /><time :datetime="o.arrival_at">{{ o.arrival_at }}</time>
            </template>
            <template v-else>Unavailable</template>
          </td>
        </tr>
        <tr v-if="orders.length === 0">
          <td colspan="10">
            No orders are indexed for this portfolio.
            <template v-if="hasUnindexed">
              That is not the same as none having been placed — see the count above.
            </template>
          </td>
        </tr>
      </tbody>
    </table>
  </template>
</template>
