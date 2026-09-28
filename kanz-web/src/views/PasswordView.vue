<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { api, ApiError } from '../api/client'
import { useSession } from '../stores/session'

const current = ref('')
const replacement = ref('')
const confirmation = ref('')
const busy = ref(false)
const error = ref('')
const done = ref(false)
const session = useSession()
function clear() { current.value = ''; replacement.value = ''; confirmation.value = '' }
onBeforeUnmount(clear)
async function submit() {
  if (busy.value) return
  error.value = ''
  if (replacement.value !== confirmation.value) { error.value = 'New passwords do not match.'; return }
  busy.value = true
  try {
    await api.post<void>('/auth/credential', { current_credential: current.value, new_credential: replacement.value })
    session.forget()
    done.value = true
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 429 ? 'Too many attempts. Wait a moment and try again.'
      : e instanceof ApiError && e.status === 401 ? 'Credentials or session were not accepted. Sign in again to retry.'
      : e instanceof ApiError && e.status === 400 ? 'Use a different password of 12 to 1024 characters.'
      : 'The change could not be confirmed. Sign in with your new password; if refused, try your previous password.'
  } finally { clear(); busy.value = false }
}
</script>

<template>
  <section class="card">
    <h1>Change password</h1>
    <p v-if="done" role="status">Password changed. Sign in again with your new password.</p>
    <form v-else @submit.prevent="submit">
      <p class="muted">Choose a different password of 12 to 1024 characters. This revokes your existing sessions.</p>
      <label>Current password<input v-model="current" type="password" autocomplete="current-password" required :disabled="busy" /></label>
      <label>New password<input v-model="replacement" type="password" autocomplete="new-password" required :disabled="busy" /></label>
      <label>Confirm new password<input v-model="confirmation" type="password" autocomplete="new-password" required :disabled="busy" /></label>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <button type="submit" :disabled="busy || !current || !replacement || !confirmation">{{ busy ? 'Changing password…' : 'Change password' }}</button>
    </form>
    <RouterLink v-if="done || error" to="/login">Sign in</RouterLink>
  </section>
</template>
