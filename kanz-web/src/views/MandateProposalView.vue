<script setup lang="ts">
import { computed, ref } from 'vue'
import {
  mandates,
  readMandateDocument,
  type MandateDocument,
  type ProposedMandateChange,
} from '../api/mandates'

const source = ref('')
const reason = ref('')
const reviewed = ref(false)
const busy = ref(false)
const error = ref('')
const proposed = ref<ProposedMandateChange | null>(null)

const parsed = computed<{ mandate: MandateDocument | null; error: string }>(() => {
  if (!source.value.trim()) return { mandate: null, error: '' }
  try {
    const value = JSON.parse(source.value) as unknown
    const mandate = readMandateDocument(value)
    if (!mandate) {
      return {
        mandate: null,
        error:
          'The document must state mandate_id, tenant_id, portfolio_id, an exact string version, rules, and effective_at.',
      }
    }
    return { mandate, error: '' }
  } catch {
    return { mandate: null, error: 'The mandate is not valid JSON.' }
  }
})

const ready = computed(
  () => parsed.value.mandate !== null && reason.value.trim().length > 0 && reviewed.value,
)
const preview = computed(() =>
  parsed.value.mandate ? JSON.stringify(parsed.value.mandate, null, 2) : '',
)

async function loadFile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  source.value = await file.text()
  reviewed.value = false
  proposed.value = null
}

async function submit() {
  const mandate = parsed.value.mandate
  if (!mandate || !ready.value || busy.value) return
  busy.value = true
  error.value = ''
  proposed.value = null
  try {
    proposed.value = await mandates.propose(mandate, reason.value.trim())
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'the proposal could not be recorded'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <h1>Propose mandate change</h1>
  <p class="muted">
    Record a complete mandate for review by a different authenticated signatory. This action creates
    pending work; it does not publish the mandate or resume trading.
  </p>

  <form class="panel" @submit.prevent="submit">
    <label>
      Load approved mandate JSON
      <input type="file" accept="application/json,.json" @change="loadFile" />
    </label>
    <label>
      Complete compliance.v1.Mandate JSON
      <textarea
        v-model="source"
        rows="16"
        required
        spellcheck="false"
        placeholder="Paste the complete approved mandate document"
        @input="reviewed = false; proposed = null"
      />
    </label>
    <p v-if="parsed.error" class="error" role="alert">{{ parsed.error }}</p>

    <section v-if="parsed.mandate" class="mandate-preview" aria-labelledby="proposal-preview-title">
      <h2 id="proposal-preview-title">Exact proposal preview</h2>
      <dl>
        <div><dt>Tenant</dt><dd><code>{{ parsed.mandate.tenant_id }}</code></dd></div>
        <div><dt>Portfolio</dt><dd><code>{{ parsed.mandate.portfolio_id }}</code></dd></div>
        <div><dt>Mandate</dt><dd><code>{{ parsed.mandate.mandate_id }}</code></dd></div>
        <div><dt>Version</dt><dd><code>{{ parsed.mandate.version }}</code></dd></div>
        <div><dt>Effective</dt><dd>{{ parsed.mandate.effective_at }}</dd></div>
        <div><dt>Rules</dt><dd>{{ parsed.mandate.rules.length }}</dd></div>
      </dl>
      <pre>{{ preview }}</pre>
    </section>

    <label>
      Capital-owner-approved rationale
      <textarea v-model="reason" rows="4" required @input="reviewed = false; proposed = null" />
    </label>
    <label class="checkline">
      <input v-model="reviewed" type="checkbox" />
      I checked that the exact mandate and rationale above match the recorded capital-owner decision.
    </label>

    <button type="submit" :disabled="busy || !ready">
      {{ busy ? 'Recording…' : 'Record pending proposal' }}
    </button>
  </form>

  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <section v-if="proposed" class="notice" role="status">
    <strong>PENDING — the mandate is not published.</strong><br />
    Proposal <code>{{ proposed.proposal_id }}</code> for portfolio
    <code>{{ proposed.portfolio_id }}</code> now requires a different authenticated signatory.
    <RouterLink to="/mandate-changes">Open the review queue</RouterLink>.
  </section>
</template>
