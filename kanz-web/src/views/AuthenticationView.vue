<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { api, ApiError } from '../api/client'
import { webAuthn, WebAuthnError, type MFACeremony } from '../api/webauthn'
import { useSession } from '../stores/session'

interface Factor { id: string; name: string; created_at: string; last_used_at: string | null }
interface Status { enabled: boolean; recent: boolean; verified_until?: string; factors: Factor[] }
const status = ref<Status | null>(null)
const name = ref('')
const password = ref('')
const busy = ref(false)
const error = ref('')
const message = ref('')
const session = useSession()
const controller = new AbortController()
let expiryTimer: ReturnType<typeof setTimeout> | undefined
async function load() {
  const loaded = await api.get<Status>('/auth/mfa')
  if (controller.signal.aborted) return
  status.value = loaded
  clearTimeout(expiryTimer)
  if (loaded.recent && loaded.verified_until) {
    const remaining = Date.parse(loaded.verified_until) - Date.now()
    if (!Number.isFinite(remaining) || remaining <= 0) loaded.recent = false
    else expiryTimer = setTimeout(() => { if (status.value) status.value.recent = false }, remaining)
  }
}
async function perform(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true; error.value = ''; message.value = ''
  try { await action(); if (!controller.signal.aborted) await load() }
  catch (e) { if (!controller.signal.aborted) error.value = e instanceof WebAuthnError || e instanceof ApiError ? e.message : 'Authentication settings are unavailable.' }
  finally { password.value = ''; busy.value = false }
}
async function enroll() {
  await perform(async () => {
    const c = await api.post<MFACeremony>('/auth/mfa/register/begin', { name: name.value, password: password.value })
    password.value = ''
    const credential = await webAuthn(c, true, controller.signal)
    await api.post('/auth/mfa/register/finish', { id: c.id, credential })
    await session.resolve(); name.value = ''; message.value = 'Factor enrolled. Other sessions were revoked.'
  })
}
async function stepup() {
  await perform(async () => {
    const c = await api.post<MFACeremony>('/auth/mfa/stepup/begin', {})
    const credential = await webAuthn(c, false, controller.signal)
    await api.post('/auth/mfa/stepup/finish', { id: c.id, credential })
    await session.resolve(); message.value = 'Verified. Privileged access is available for five minutes. Return to your action and submit it explicitly.'
  })
}
async function remove(f: Factor) {
  await perform(async () => {
    await api.post('/auth/mfa/remove', { id: f.id })
    await session.resolve(); message.value = 'Factor removed. Other sessions were revoked.'
  })
}
onMounted(() => perform(async () => {}))
onUnmounted(() => { controller.abort(); clearTimeout(expiryTimer); password.value = '' })
</script>

<template>
  <section class="card">
    <h1>Authentication</h1>
    <p>Use a security key or passkey with its PIN, biometrics, or device verification. Password recovery keeps these factors required.</p>
    <p class="muted">Enroll a spare factor before losing access to your first. Email recovery cannot remove MFA, and the last factor cannot be deleted here.</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="message" role="status">{{ message }}</p>
    <template v-if="status">
      <p>{{ status.enabled ? 'MFA is enabled.' : 'MFA is not enrolled.' }} {{ status.recent ? 'Recent verification is active.' : 'Privileged actions may require fresh verification.' }}</p>
      <button v-if="status.enabled" :disabled="busy" @click="stepup">Verify security key</button>
      <h2>Enrolled factors</h2>
      <ul v-if="status.factors.length">
        <li v-for="f in status.factors" :key="f.id">
          {{ f.name }} — added {{ new Date(f.created_at).toLocaleDateString() }}
          <button :disabled="busy || !status.recent || status.factors.length < 2" :aria-label="`Remove ${f.name}`" @click="remove(f)">Remove</button>
        </li>
      </ul>
      <p v-else>No factors enrolled.</p>
      <form @submit.prevent="enroll">
        <h2>Add a factor</h2>
        <p v-if="status.enabled && !status.recent">Verify an existing key before adding another.</p>
        <label>Factor name<input v-model.trim="name" maxlength="80" required :disabled="busy" autocomplete="off" /></label>
        <label>Current password<input v-model="password" type="password" autocomplete="current-password" required :disabled="busy" /></label>
        <button type="submit" :disabled="busy || !name || !password || status.factors.length >= 5 || (status.enabled && !status.recent)">Add security key or passkey</button>
      </form>
    </template>
    <RouterLink to="/password">Password</RouterLink> · <RouterLink to="/mailbox">Recovery mailbox</RouterLink>
  </section>
</template>
