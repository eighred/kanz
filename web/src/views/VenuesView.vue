<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { control, describe, type VenueKeyStatus } from '../api/control'

// The page that started all this: /venues answered "Login" because the TUI's
// device flow could not complete against an SSO that does not exist.
//
// PRESENCE ONLY, NEVER KEY MATERIAL. The gateway returns a `configured` flag and
// nothing else, and this screen must never grow a way to read a key back — a
// surface that can display one is a surface that can leak one. The form below
// WRITES; there is deliberately no path that reads.
const rows = ref<VenueKeyStatus[]>([])
const error = ref('')
const loading = ref(true)

// THE WRITE FORM. Keys are held in refs only while the form is open and cleared
// the moment it closes or succeeds — a credential that outlives the interaction
// is a credential sitting in a tab someone walks away from.
const editing = ref<VenueKeyStatus | null>(null)
const apiKey = ref('')
const apiSecret = ref('')
const passphrase = ref('')
const typed = ref('')
const busy = ref(false)
const formError = ref('')
const account = ref('')

async function load() {
  try {
    rows.value = await control.venues()
    error.value = ''
  } catch (e) {
    // A 403 here is the gateway refusing a caller without operator authority,
    // and it is CORRECT: this page renders for anyone signed in, because hiding
    // it would be a courtesy and not a control. A 404 means the gateway fronts
    // no control plane — which must not be drawn as "no venues configured".
    error.value = describe(e)
  } finally {
    loading.value = false
  }
}

onMounted(load)

function edit(v: VenueKeyStatus) {
  editing.value = v
  clearSecrets()
  formError.value = ''
  account.value = ''
}

/** clearSecrets is called on every exit from the form, success or not. */
function clearSecrets() {
  apiKey.value = ''
  apiSecret.value = ''
  passphrase.value = ''
  typed.value = ''
}

function cancel() {
  editing.value = null
  clearSecrets()
}

/**
 * replacing is true when the venue ALREADY has credentials.
 *
 * IT CHANGES THE CONFIRMATION, and that asymmetry is the point. Writing keys to
 * a venue with none is setup. Overwriting the keys of a venue that is trading
 * replaces the credential live orders are authenticating with, and a wrong key
 * there is not a form error — it is an outage on the capital path. So a replace
 * asks for the venue's name to be typed; a first-time write does not.
 *
 * `configured` is omitted by protojson when false, so only an explicit true
 * counts — an unknown state is treated as "not configured", which asks for LESS
 * friction. That is the safe direction here: the dangerous act is overwriting,
 * and a venue reported as unconfigured has nothing to overwrite.
 */
function replacing(): boolean {
  return editing.value?.configured === true
}

function submittable(): boolean {
  if (!editing.value || busy.value) return false
  if (apiKey.value.trim() === '' || apiSecret.value.trim() === '') return false
  if (replacing() && typed.value !== editing.value.venue) return false
  return true
}

async function submit() {
  const v = editing.value
  if (!v || !submittable()) return
  busy.value = true
  formError.value = ''
  try {
    const res = await control.setVenueKeys(v.venue, apiKey.value, apiSecret.value, passphrase.value)
    // THE EXCHANGE'S OWN ACCOUNT ID IS THE PROOF, and it is a public fact rather
    // than key material. Empty means the deployment has no pre-write proof
    // configured — the keys were written UNPROVEN, and saying so is the whole
    // difference between "accepted" and "works".
    account.value = res.exchange_account_id ?? ''
    editing.value = null
    clearSecrets()
    await load()
  } catch (e) {
    // The message never carries what was typed: describe() maps a status, and a
    // 412 is the VENUE refusing the credential rather than this app rejecting it.
    formError.value = describe(e)
    // The secret stays in the form on failure — retyping a 64-character key
    // because the venue was briefly unreachable is how people start pasting
    // credentials into a text editor first.
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <h1>Venues</h1>
  <p class="muted">Whether trading credentials are present. Keys themselves are never returned.</p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="account" class="notice" role="status">
      Credentials accepted. The exchange reports account <code>{{ account }}</code> for them.
    </p>

    <table>
      <thead><tr><th>Venue</th><th>Keys</th><th>Actions</th></tr></thead>
      <tbody>
        <tr v-for="r in rows" :key="r.venue">
          <td>{{ r.venue }}</td>
          <!-- `configured` is omitted by protojson when false, so only an
               explicit true counts as configured. -->
          <td :class="r.configured === true ? 'state-ok' : 'state-unset'">
            {{ r.configured === true ? 'configured' : 'not set' }}
          </td>
          <td class="actions">
            <button :class="r.configured === true ? 'danger' : 'quiet'" @click="edit(r)">
              {{ r.configured === true ? 'Replace keys' : 'Set keys' }}
            </button>
          </td>
        </tr>
        <tr v-if="rows.length === 0"><td colspan="3">No venues returned.</td></tr>
      </tbody>
    </table>
  </template>

  <div v-if="editing" class="confirm card" role="dialog" aria-modal="true" aria-labelledby="keys-title">
    <h2 id="keys-title">
      {{ replacing() ? 'Replace' : 'Set' }} credentials for {{ editing.venue }}
    </h2>

    <p v-if="replacing()" class="hint">
      This venue already holds credentials. Replacing them changes what live orders authenticate
      with — a wrong key here is an outage on the capital path, not a form error.
    </p>

    <!-- autocomplete off and type=password on both secrets: a browser offering
         to save an exchange API secret is a credential leaving the operator's
         control by a route nobody chose. -->
    <label>
      API key
      <input v-model="apiKey" name="api-key" type="password" autocomplete="off" spellcheck="false" />
    </label>
    <label>
      API secret
      <input v-model="apiSecret" name="api-secret" type="password" autocomplete="off" spellcheck="false" />
    </label>
    <label>
      Passphrase
      <input v-model="passphrase" name="passphrase" type="password" autocomplete="off" spellcheck="false" />
    </label>
    <p class="muted">
      Required by OKX; Binance must leave it empty. The venue decides — this form does not guess
      from the name.
    </p>

    <label v-if="replacing()">
      Type <strong>{{ editing.venue }}</strong> to confirm the replacement
      <input v-model="typed" name="confirm-venue" autocomplete="off" />
    </label>

    <p v-if="formError" class="error" role="alert">{{ formError }}</p>

    <div class="actions">
      <button :disabled="!submittable()" :class="replacing() ? 'danger' : ''" @click="submit">
        {{ busy ? 'Writing…' : replacing() ? 'Replace credentials' : 'Set credentials' }}
      </button>
      <button class="quiet" :disabled="busy" @click="cancel">Cancel</button>
    </div>
  </div>
</template>
