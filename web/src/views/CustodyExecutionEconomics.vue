<script setup lang="ts">
import { economicsMeasures, type EconomicsMeasure, type ExecutionEconomics } from '../api/custody-quantities'
import { formatDecimal } from '../api/decimal'

defineProps<{ comparison: ExecutionEconomics }>()
const labels: Record<EconomicsMeasure, string> = {
  quantity: 'Signed quantity', price: 'Unit price', gross_cash: 'Signed gross cash',
  commission: 'Commission', net_cash: 'Signed net cash', settled_cash: 'Settled cash',
}
const reasons: Record<string, string> = {
  convention_missing: 'The signed statement gives no price and cash convention.',
  convention_unsupported: 'The signed convention is unsupported.',
  source_terms_unverified: 'The signed execution terms could not be verified.',
  source_cash_inconsistent: 'The signed cash is inconsistent with the stated execution terms.',
  book_terms_unverified: 'The book execution or settlement could not be verified.',
  precision_unverified: 'The exact values exceed the precision supported by this receipt.',
  currency_mismatch: 'The book and signed currencies differ.',
}
</script>

<template>
  <section aria-label="Execution economics">
    <h3>Execution economics</h3>
    <p v-if="comparison.unassessed_reason">Execution economics unknown. The signed reference count exceeds the receipt limit.</p>
    <p v-else-if="!comparison.rows.length">The signed statement contains no execution lines to compare.</p>
    <section v-for="row in comparison.rows" :key="row.external_ref" :aria-label="'Execution ' + row.external_ref" :data-status="row.status">
      <h4>{{ row.external_ref }} · {{ { equal: 'Equal', mismatch: 'Mismatch', unknown: 'Unknown' }[row.status] }}</h4>
      <p v-if="row.instrument_id">Instrument {{ row.instrument_id }}</p>
      <p v-if="row.status === 'unknown'">{{ reasons[row.reason] ?? row.reason }}</p>
      <template v-else>
        <p>Book entry <code>{{ row.entry_id }}</code> · Book settlement {{ row.book_settlement_date }} · Custodian settlement {{ row.custodian_settlement_date }}</p>
        <table :aria-label="'Execution economics for ' + row.external_ref">
          <thead><tr><th scope="col">Measure</th><th scope="col">Unit</th><th scope="col">Book</th>
            <th scope="col">Signed custodian</th><th scope="col">Book minus custodian</th></tr></thead>
          <tbody><tr v-for="key in economicsMeasures" :key="key">
            <th scope="row">{{ labels[key] }}</th>
            <td>{{ key === 'quantity' ? row.instrument_id : key === 'price' ? row.currency_code + ' per ' + row.instrument_id : row.currency_code }}</td>
            <td>{{ formatDecimal(row.book[key]) }}</td><td>{{ formatDecimal(row.custodian[key]) }}</td><td>{{ formatDecimal(row.difference[key]) }}</td>
          </tr></tbody>
        </table>
      </template>
    </section>
  </section>
</template>
