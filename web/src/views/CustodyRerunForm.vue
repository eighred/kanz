<script setup lang="ts">
import { ref } from 'vue'
import { custody, type CustodyRerunReceipt, type CustodyRerunRequest } from '../api/custody'
import { ApiError } from '../api/client'

const props = defineProps<{ actor: string }>()
const emit = defineEmits<{ completed: [receipt: CustodyRerunReceipt]; reviewing: [active: boolean] }>()
const portfolio = ref('')
const custodian = ref('')
const day = ref('')
const reviewed = ref<CustodyRerunRequest | null>(null)
const sending = ref(false)
const uncertain = ref(false)
const error = ref('')
function review() {
  if (!portfolio.value.trim() || !custodian.value.trim() || !day.value) return
  error.value = ''
  emit('reviewing', true)
  reviewed.value = { portfolio_id: portfolio.value.trim(), custodian_id: custodian.value.trim(), business_date: day.value, request_id: crypto.randomUUID() }
}
function reset() {
  reviewed.value = null
  emit('reviewing', false)
}
async function submit() {
  if (!reviewed.value || sending.value) return
  sending.value = true
  error.value = ''
  try {
    const receipt = await custody.rerun(reviewed.value, props.actor)
    uncertain.value = false
    emit('reviewing', false)
    emit('completed', receipt)
  } catch (e) {
    if (e instanceof ApiError && [400, 403, 404, 409].includes(e.status)) {
      error.value = e.status === 409 ? 'The request conflicts with retained comparison history. Review the scope before starting a new request.' : 'The comparison was refused. Check your permission, scope and business date.'
      reset()
      uncertain.value = false
    } else {
      uncertain.value = true
      error.value = 'Completion could not be verified. Retry this same request to recover its receipt.'
    }
  } finally { sending.value = false }
}
</script>

<template>
  <section class="panel" aria-label="Rerun custody comparison">
    <h2>Rerun custody comparison</h2>
    <form v-if="!reviewed" @submit.prevent="review">
      <label>Portfolio ID <input v-model="portfolio" required maxlength="256" /></label>
      <label>Custodian ID <input v-model="custodian" required maxlength="256" /></label>
      <label>UTC business date <input v-model="day" type="date" required /></label>
      <button>Review comparison</button>
    </form>
    <template v-else>
      <p>Compare the stored statement with the book for {{ reviewed.portfolio_id }}, {{ reviewed.custodian_id }}, on {{ reviewed.business_date }}. The result can update the break queue.</p>
      <p>Request <code>{{ reviewed.request_id }}</code></p>
      <button :disabled="sending" @click="submit">{{ uncertain ? 'Retry same request' : 'Run comparison' }}</button>
      <button v-if="!uncertain" :disabled="sending" @click="reset">Back</button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
  </section>
</template>
