<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { describeRisk, risk, type OrderSummary, type OrdersResponse } from '../api/risk'
import { formatDecimal, formatMoney } from '../api/decimal'

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

const data = ref<OrdersResponse | null>(null)
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    data.value = await risk.orders(id.value)
  } catch (e) {
    error.value = describeRisk(e)
  } finally {
    loading.value = false
  }
})

const orders = computed<OrderSummary[]>(() => data.value?.orders ?? [])

// unindexed arrives as a STRING (int64). It is compared as a string rather than
// parsed: the only question asked of it is "is it zero", and parsing an int64
// to answer that is how a large one becomes 9007199254740992.
const unindexed = computed(() => data.value?.unindexed ?? '0')
const hasUnindexed = computed(() => unindexed.value !== '0' && unindexed.value !== '')

function qty(d: Parameters<typeof formatDecimal>[0]): string {
  return formatDecimal(d) ?? '—'
}
function money(m: Parameters<typeof formatMoney>[0]): string {
  return formatMoney(m) ?? '—'
}

/** label turns the enum name into something readable. */
function label(status?: string): string {
  if (!status || status === 'ORDER_STATUS_UNSPECIFIED') return 'unknown'
  return status.replace(/^ORDER_STATUS_/, '').replaceAll('_', ' ').toLowerCase()
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

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <!-- NOT A HINT. These orders are missing from the table below and cannot be
         retrieved by any page of it, so the count is part of the answer rather
         than a note about it. -->
    <p v-if="hasUnindexed" class="error" role="alert">
      {{ unindexed }} order(s) in this tenant are not indexed by portfolio and cannot appear
      here. They were placed before the OMS recorded the portfolio alongside each order, so
      this is a partial history — not necessarily this portfolio's whole trading record.
    </p>

    <table>
      <thead>
        <tr>
          <th>Order</th><th>Instrument</th><th>Side</th><th>Status</th>
          <th>Quantity</th><th>Filled</th><th>Price</th><th>Avg fill</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="o in orders" :key="o.order_id">
          <td>
            <code>{{ o.order_id }}</code>
            <span v-if="o.venue" class="muted"><br />{{ o.venue }}</span>
          </td>
          <td>{{ o.instrument_id || '—' }}</td>
          <td>{{ (o.side || '').replace(/^SIDE_/, '').toLowerCase() || '—' }}</td>
          <td :class="tone(o)">{{ label(o.status) }}</td>
          <td>{{ qty(o.quantity) }}</td>
          <td>{{ qty(o.filled_quantity) }}</td>
          <td>{{ money(o.limit_price) }}</td>
          <td>{{ money(o.average_fill_price) }}</td>
        </tr>
        <tr v-if="orders.length === 0">
          <td colspan="8">
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
