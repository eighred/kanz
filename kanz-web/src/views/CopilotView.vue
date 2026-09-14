<script setup lang="ts">
import { computed, ref } from 'vue'
import { copilot, describeInvestigation, type Investigation } from '../api/copilot'

const question = ref('')
const submittedQuestion = ref('')
const loading = ref(false)
const error = ref('')
const result = ref<Investigation | null>(null)
const bytes = computed(() => new TextEncoder().encode(question.value).length)
async function ask() {
  if (loading.value || !question.value.trim() || bytes.value > 16384) return
  loading.value = true
  result.value = null
  error.value = ''
  submittedQuestion.value = question.value
  try { result.value = await copilot.ask(submittedQuestion.value) }
  catch (e) { error.value = describeInvestigation(e) }
  finally { loading.value = false }
}
</script>

<template>
  <h1>Copilot investigation</h1>
  <p class="muted">Ask a question using the read-only sources your session may access. Model output does not authorize a financial action.</p>
  <form @submit.prevent="ask">
    <label>Question<textarea v-model="question" name="question" rows="5" required :disabled="loading" /></label>
    <p class="muted">{{ bytes }} / 16384 bytes</p>
    <button type="submit" :disabled="loading || !question.trim() || bytes > 16384">{{ loading ? 'Investigating…' : 'Investigate' }}</button>
  </form>
  <p v-if="loading" role="status">Waiting for the governed investigation. A new submission starts a separate investigation.</p>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <section v-if="result" aria-label="Investigation result">
    <h2>{{ result.refused ? 'Model refused' : result.budget_exhausted ? 'Investigation incomplete' : 'Model response' }}</h2>
    <p><strong>Submitted question:</strong> {{ submittedQuestion }}</p>
    <p v-if="result.budget_exhausted" class="state-warn">The tool-use budget was exhausted. This is not a completed answer.</p>
    <p v-if="result.injection_flagged" class="state-warn">Potential prompt injection was flagged in retrieved content.</p>
    <p v-if="!result.grounded" class="state-warn">The grounding review could not match all numeric claims to cited values.</p>
    <p v-else class="muted">The service reported its grounding check passed. This does not establish completeness, freshness, or financial approval.</p>
    <p class="answer">{{ result.answer || 'No answer text was returned.' }}</p>
    <template v-if="result.ungrounded.length">
      <h3>Unmatched numeric claims</h3>
      <ul><li v-for="(claim, i) in result.ungrounded" :key="i"><code>{{ claim }}</code></li></ul>
    </template>
    <h3>Source citations</h3>
    <p v-if="!result.citations.length">No source citations were returned.</p>
    <ul v-else><li v-for="(citation, i) in result.citations" :key="i"><code>{{ citation }}</code></li></ul>
  </section>
</template>

<style scoped>
.answer { white-space: pre-wrap; overflow-wrap: anywhere; }
textarea { display: block; width: 100%; box-sizing: border-box; }
code { overflow-wrap: anywhere; }
</style>
