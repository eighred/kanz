<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { broker, describeBroker, type BrokerAccountDetail } from '../api/broker'

import { useSession } from '../stores/session'
import WindowedTable from '../components/WindowedTable.vue'

const session = useSession()
const route = useRoute()
const id = computed(() => String(route.params.id ?? ''))
const data = ref<BrokerAccountDetail | null>(null)
const loading = ref(true)
const error = ref('')
// The broker summary folds gross fills in USD. A retained window cannot
// establish the fee units of executions that have already left the view.
const unqualifiedTotals = computed(() => data.value?.executionsRetainedFrom ||
  data.value?.executions.some(e => e.baseCurrency || e.fee === undefined || e.feeCurrency !== 'USD'))

// A route/session transition invalidates the whole four-read snapshot. Abort is
// best effort; generation checks also reject adapters completing after abort.
let generation = 0
watch(() => [id.value, session.identity, session.identity?.tenant, session.identity?.subject] as const, async ([account, identity], _, onCleanup) => {
  const current = ++generation
  const controller = new AbortController()
  let active = true
  onCleanup(() => { active = false; controller.abort() })
  data.value = null
  error.value = ''
  loading.value = true
  if (!identity) { loading.value = false; error.value = 'Sign in to read broker account data.'; return }
  try {
    const result = await broker.account(account, controller.signal)
    if (!active || current !== generation) return
    if (result.state.id !== account) throw new Error('Broker account identity differs.')
    data.value = result
  } catch (e) {
    if (active && current === generation) error.value = describeBroker(e)
  } finally {
    if (active && current === generation) loading.value = false
  }
}, { immediate: true, flush: 'sync' })

</script>

<template>
  <h1>Broker account</h1>
  <p class="muted"><RouterLink to="/broker-accounts">Broker accounts</RouterLink> · <code>{{ id }}</code></p>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <template v-else-if="data">
    <p class="state-warn">State and positions are current in-memory folds, but this API provides no observation timestamp. Their freshness cannot be established from this response.</p>
    <p v-if="unqualifiedTotals" class="state-warn">Position and account totals are unavailable for reconciliation. The broker model assumes USD, and the execution history includes spot movements, incomplete fee evidence or an incomplete window. Check the individual receipts below.</p>
    <p v-else class="muted">The broker model estimates account values in USD from a zero opening cash balance.</p>
    <dl class="metric-grid">
      <div><dt>Model currency</dt><dd>{{ data.state.currency }}</dd></div>
      <template v-if="!unqualifiedTotals">
      <div><dt>Balance</dt><dd>{{ data.state.balance }}</dd></div>
      <div><dt>Equity</dt><dd>{{ data.state.equity }}</dd></div>
      <div><dt>Open positions</dt><dd>{{ data.state.openPositions }}</dd></div>
      <div><dt>Realized P&amp;L</dt><dd>{{ data.state.realizedPl }}</dd></div>
      <div><dt>Unrealized P&amp;L</dt><dd>{{ data.state.unrealizedPl }}</dd></div>
      </template>
    </dl>

    <h2 v-if="!unqualifiedTotals">Positions</h2>
    <div v-if="!unqualifiedTotals" class="table-scroll"><table><thead><tr><th>Instrument</th><th>Side</th><th>Quantity</th><th>Average price</th><th>Realized P&amp;L</th><th>Unrealized P&amp;L</th></tr></thead>
      <tbody><tr v-for="position in data.positions" :key="position.instrument"><td>{{ position.instrument }}</td><td>{{ position.side }}</td><td>{{ position.qty }}</td><td>{{ position.avgPrice }}</td><td>{{ position.realizedPl }}</td><td>{{ position.unrealizedPl ?? '—' }}</td></tr>
      <tr v-if="data.positions.length === 0"><td colspan="6">No open positions in the current fold.</td></tr></tbody></table></div>

    <h2>Orders</h2>
    <p v-if="data.ordersRetainedFrom" class="state-warn">This is a retained window beginning {{ data.ordersRetainedFrom }}. Earlier orders are not shown.</p>
    <WindowedTable :items="data.orders" :row-key="order => order.id" label="Orders in served window" :columns="8" empty="No orders are present in the served window.">
      <template #head><tr><th>Updated</th><th>Order</th><th>Instrument</th><th>Side</th><th>Status</th><th>Quantity</th><th>Filled</th><th>Leaves</th></tr></template>
      <template #row="{ item: order }"><td>{{ order.updatedAt }}</td><td><code>{{ order.id }}</code><span v-if="order.parentOrderId" class="muted"><br />Parent {{ order.parentOrderId }}</span></td><td>{{ order.instrument }}<span v-if="order.venue" class="muted"><br />{{ order.venue }}</span></td><td>{{ order.side }}</td><td>{{ order.status }}</td><td>{{ order.qty }}</td><td>{{ order.filledQty }}</td><td>{{ order.leavesQty }}</td></template>
    </WindowedTable>

    <h2>Executions</h2>
    <p v-if="data.executionsRetainedFrom" class="state-warn">This is a retained window beginning {{ data.executionsRetainedFrom }}. Earlier executions are not shown.</p>
    <WindowedTable :items="data.executions" :row-key="execution => execution.id" label="Executions in served window" :columns="11" empty="No executions are present in the served window.">
      <template #head><tr><th>Executed</th><th>Fill</th><th>Order</th><th>Instrument</th><th>Venue</th><th>Side</th><th>Quantity</th><th>Price</th><th>Reported fee</th><th>Spot inventory change</th><th>Spot cash change</th></tr></template>
      <template #row="{ item: execution }">
        <td>{{ execution.time }}</td><td><code>{{ execution.id }}</code></td><td><code>{{ execution.orderId }}</code></td>
        <td>{{ execution.instrument }}</td><td>{{ execution.venue }}</td><td>{{ execution.side }}</td>
        <td>{{ execution.qty }} {{ execution.baseCurrency }}</td><td>{{ execution.price }} {{ execution.quoteCurrency }}</td>
        <td>
          <span v-if="execution.fee !== undefined">{{ execution.fee }} {{ execution.feeCurrency || '(currency unknown)' }}</span>
          <span v-else>Fee unknown</span>
        </td>
        <td><template v-if="execution.inventoryChange !== undefined">{{ execution.inventoryChange }} {{ execution.baseCurrency }}</template><template v-else>—</template></td>
        <td><template v-if="execution.cashChange !== undefined">{{ execution.cashChange }} {{ execution.quoteCurrency }}</template><template v-else>—</template></td>
      </template>
    </WindowedTable>
  </template>
</template>
