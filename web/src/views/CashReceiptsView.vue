<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import { isCashIdentifier, readCashReceipt, type CashReceipt } from '../api/cash-receipt'
import { useSession } from '../stores/session'

const session = useSession()
const portfolio = ref('')
const movement = ref('')
const receipt = ref<CashReceipt | null>(null)
const error = ref('')
const loading = ref(false)
let pending: AbortController | undefined

function clearLookup() {
  pending?.abort()
  pending = undefined
  receipt.value = null
  error.value = ''
  loading.value = false
}

watch([portfolio, movement], clearLookup, { flush: 'sync' })
watch(() => session.identity, () => {
  clearLookup()
  portfolio.value = ''
  movement.value = ''
}, { deep: true, flush: 'sync' })
onUnmounted(clearLookup)

async function lookup() {
  if (!session.signedIn || loading.value) return
  clearLookup()
  if (!isCashIdentifier(portfolio.value) || !isCashIdentifier(movement.value)) {
    error.value = 'Enter valid portfolio and movement IDs.'
    return
  }
  const request = new AbortController()
  pending = request
  loading.value = true
  try {
    const result = await readCashReceipt(portfolio.value, movement.value, request.signal)
    if (!request.signal.aborted) receipt.value = result
  } catch (e) {
    if (request.signal.aborted) return
    if (e instanceof ApiError && e.status === 403) error.value = 'You do not have permission to read this cash receipt.'
    else if (e instanceof ApiError && e.status === 404) error.value = 'No receipt is available for these IDs within your portfolio access.'
    else error.value = 'Accounting could not confirm this cash receipt.'
  } finally {
    if (pending === request) {
      loading.value = false
      pending = undefined
    }
  }
}
</script>

<template>
  <h1>Cash movement receipts</h1>
  <form @submit.prevent="lookup">
    <label>Portfolio ID <input v-model="portfolio" required /></label>
    <label>Movement ID <input v-model="movement" required /></label>
    <button :disabled="loading">Look up receipt</button>
  </form>
  <p v-if="loading" role="status">Looking up receipt…</p>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <section v-if="receipt" aria-labelledby="cash-receipt-title">
    <h2 id="cash-receipt-title">Receipt for {{ receipt.movement_id }}</h2>
    <dl>
      <dt>Portfolio</dt><dd>{{ portfolio }}</dd>
      <dt>Accounting status</dt><dd>{{ receipt.status === 'journal_recorded' ? 'Recorded in journal' : 'Command accepted' }}</dd>
      <dt>Custody settlement</dt><dd>Unknown</dd>
      <dt>Accepted at</dt><dd><time :datetime="receipt.accepted_at">{{ receipt.accepted_at }}</time></dd>
      <dt>Submitted by</dt><dd>{{ receipt.actor }}</dd>
      <dt>Command digest</dt><dd><code>{{ receipt.command_digest }}</code></dd>
    </dl>
  </section>
</template>

<style scoped>
form { display: grid; gap: 1rem; max-width: 28rem; }
button { justify-self: start; }
dd { margin: .25rem 0 1rem; overflow-wrap: anywhere; }
</style>
