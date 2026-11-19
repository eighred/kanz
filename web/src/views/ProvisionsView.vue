<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { control, describe, settled, type Provision } from '../api/control'
import { ApiError } from '../api/client'

// NODE PROVISIONING RUNS (#371 slice 4).
//
// GET /v1/control/provisions has existed on the gateway since the control plane
// did, and nothing in the browser had ever called it. This is the tail of the
// add-node workflow: a run is a Kubernetes Job that SSHes to a host, installs
// k3s and joins it, and the only place its outcome is visible.
//
// A FAILED RUN CARRIES ITS REASON, AND THAT IS THE WHOLE VALUE OF THE SCREEN.
// The proto puts the failure text on the message field for FAILED and leaves it
// empty otherwise, so this renders it in full rather than truncating: a
// provisioning failure is an SSH or install error an operator has to read to
// act on, and a table that elides it sends them to kubectl.

const rows = ref<Provision[]>([])
const error = ref('')
const loading = ref(true)
const adding = ref(false)
const submitting = ref(false)
const submissionError = ref('')
const receipt = ref('')
let timer: ReturnType<typeof setInterval> | undefined
let activeRead: AbortController | undefined
let activeSubmission: AbortController | undefined

// A run is a Job that takes minutes, so a static page would be wrong within
// seconds of opening it. Same cadence as Estate.
const REFRESH_MS = 30_000

// IN-FLIGHT IS DERIVED FROM "NOT SETTLED", NOT FROM A LIST OF RUNNING STATES.
// A status this build does not know about — a value added to the enum later —
// then counts as in-flight rather than silently disappearing from both columns.
const running = computed(() => rows.value.filter((p) => !settled(p)).length)
const failed = computed(() => rows.value.filter((p) => p.status === 'PROVISION_STATUS_FAILED').length)

async function submit(event: SubmitEvent) {
  if (submitting.value) return
  const form = event.currentTarget as HTMLFormElement
  const fields = new FormData(form)
  const key = fields.get('bootstrap')
  if (!(key instanceof File) || !key.size) return
  const submission = new AbortController()
  activeSubmission = submission
  submitting.value = true
  submissionError.value = ''
  receipt.value = ''
  try {
    const value = (name: string) => String(fields.get(name)).trim()
    receipt.value = await control.addNode({
      hostname: value('hostname'), ip: value('ip'), sshUser: value('user'), sshHostKey: value('hostKey'),
    }, key, submission.signal)
    form.reset()
    adding.value = false
    await load()
  } catch (e) {
    if (submission.signal.aborted) return
    // A dropped response may follow Job creation. Never replay the private key.
    submissionError.value = e instanceof ApiError && e.status === 403
      ? describe(e) : 'No provisioning receipt was confirmed. Check the runs below before submitting again.'
  } finally {
    activeSubmission = undefined
    const input = form.elements.namedItem('bootstrap') as HTMLInputElement | null
    if (input) input.value = ''
    submitting.value = false
  }
}

async function load(initial = false) {
  activeRead?.abort()
  const read = new AbortController()
  activeRead = read
  if (initial) loading.value = true
  try {
    const latest = await control.provisions(read.signal)
    if (read.signal.aborted) return
    rows.value = latest
    error.value = ''
  } catch (e) {
    // A 404 is "this gateway fronts no control plane", not "no provisioning has
    // ever run". Drawn as an empty table the two are indistinguishable.
    if (!read.signal.aborted) error.value = describe(e)
  } finally {
    if (!read.signal.aborted) loading.value = false
  }
}

onMounted(() => {
  void load(true)
  timer = setInterval(() => void load(), REFRESH_MS)
})
onUnmounted(() => { clearInterval(timer); activeRead?.abort(); activeSubmission?.abort() })

/** label turns the enum name into something readable. */
function label(status?: string): string {
  if (!status || status === 'PROVISION_STATUS_UNSPECIFIED') return 'unknown'
  return status.replace(/^PROVISION_STATUS_/, '').toLowerCase()
}

/** tone colours a run by outcome, with unknown deliberately not green. */
function tone(p: Provision): string {
  switch (p.status) {
    case 'PROVISION_STATUS_JOINED':
      return 'state-ok'
    case 'PROVISION_STATUS_FAILED':
      return 'state-bad'
    case 'PROVISION_STATUS_PENDING':
    case 'PROVISION_STATUS_INSTALLING':
      return 'state-warn'
    default:
      // Not green. A run whose status the platform did not state is not a run
      // that succeeded.
      return 'state-unset'
  }
}
</script>

<template>
  <h1>Provisions</h1>
  <p class="muted">
    Node provisioning runs: SSH, k3s install, join. Refreshes every 30s. Nodes themselves are on
    <RouterLink to="/nodes">Nodes</RouterLink>.
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <button v-if="!adding" @click="adding = true; submissionError = ''; receipt = ''">Add node</button>
    <form v-else class="card" @submit.prevent="submit">
      <h2>Add node</h2>
      <label>Hostname <input name="hostname" required autocomplete="off" /></label>
      <label>IP address <input name="ip" required autocomplete="off" /></label>
      <label>SSH user <input name="user" required autocomplete="off" /></label>
      <label>Verified SSH host public key <input name="hostKey" required autocomplete="off" spellcheck="false" /></label>
      <label>One-use SSH private key (PEM) <input name="bootstrap" type="file" required /></label>
      <p>Verify the host public key independently. Submitting installs the k3s agent over SSH and joins the target to this estate.</p>
      <button :disabled="submitting" type="submit">{{ submitting ? 'Submitting…' : 'Provision node' }}</button>
      <button class="quiet" type="button" :disabled="submitting" @click="adding = false; submissionError = ''">Cancel</button>
    </form>
    <p v-if="submissionError" class="error" role="alert">{{ submissionError }}</p>
    <p v-if="receipt" role="status">Provisioning requested. Run <code>{{ receipt }}</code>.</p>
    <p class="muted">
      {{ rows.length }} run{{ rows.length === 1 ? '' : 's' }} ·
      <span :class="running ? 'state-warn' : 'state-ok'">{{ running }} in flight</span> ·
      <span :class="failed ? 'state-bad' : 'state-ok'">{{ failed }} failed</span>
    </p>

    <table>
      <thead><tr><th>Host</th><th>Status</th><th>Detail</th></tr></thead>
      <tbody>
        <tr v-for="p in rows" :key="p.id">
          <td>{{ p.hostname || '—' }}<br /><span class="muted"><code>{{ p.id }}</code></span></td>
          <td :class="tone(p)">{{ label(p.status) }}</td>
          <!-- IN FULL. The message is the SSH or install error, and it is the
               reason to open this page at all. Truncating it sends the operator
               to kubectl for the half they actually needed. -->
          <td>{{ p.message || '—' }}</td>
        </tr>
        <tr v-if="rows.length === 0">
          <td colspan="3">No provisioning has been run through this control plane.</td>
        </tr>
      </tbody>
    </table>
  </template>
</template>
