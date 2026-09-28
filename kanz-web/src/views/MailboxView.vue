<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError } from '../api/client'
const address = ref('')
const credential = ref('')
const busy = ref(false)
const error = ref('')
const done = ref(false)
const status = ref<{ verified_address: string; pending_address: string; verification_delivery: string } | null>(null)
async function reload() {
  try { status.value = await api.get('/auth/mailbox') }
  catch { status.value = null; error.value = 'Mailbox status could not be loaded. Recovery may not be configured.' }
}
onMounted(reload)
onBeforeUnmount(() => { credential.value = '' })
async function submit() {
  if (busy.value) return
  busy.value = true; error.value = ''; done.value = false
  try {
    await api.post('/auth/mailbox', { address: address.value, credential: credential.value })
    done.value = true
    await reload()
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 404 ? 'Recovery is not configured. Contact your administrator.'
      : e instanceof ApiError && e.status === 401 ? 'The current password or session was not accepted.'
      : e instanceof ApiError && e.status === 429 ? 'Too many attempts. Wait before trying again.'
      : 'Verification could not be requested. Check the address and try again later.'
  } finally { credential.value = ''; busy.value = false }
}
</script>
<template>
  <section class="card">
    <h1>Recovery mailbox</h1>
    <p v-if="status">Verified address: {{ status.verified_address || 'None' }}. Verification delivery: {{ status.verification_delivery }}{{ status.pending_address ? ` (${status.pending_address})` : '' }}.</p>
    <button type="button" :disabled="busy" @click="reload">Reload status</button>
    <p>A mailbox becomes a recovery destination only after you open its verification link. Your existing verified address remains active until then.</p>
    <p v-if="done" role="status">Verification requested. Check your mailbox and follow the link. Allow a few minutes before requesting another message; this confirmation does not attest delivery.</p>
    <form @submit.prevent="submit">
      <label>Email address<input v-model.trim="address" type="email" autocomplete="email" maxlength="254" required :disabled="busy" /></label>
      <label>Current password<input v-model="credential" type="password" autocomplete="current-password" required :disabled="busy" /></label>
      <p v-if="error" role="alert" class="error">{{ error }}</p>
      <button type="submit" :disabled="busy || !address || !credential">{{ busy ? 'Requesting…' : 'Request verification' }}</button>
    </form>
  </section>
</template>
