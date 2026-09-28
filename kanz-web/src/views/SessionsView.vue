<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiError } from '../api/client'
import { useSession } from '../stores/session'
interface BrowserSession { id: string; created_at: string; expires_at: string; current: boolean }
const rows = ref<BrowserSession[] | null>(null)
const busy = ref(false)
const error = ref('')
const message = ref('')
const session = useSession()
const router = useRouter()
let mounted = true
async function load() {
  const result = await api.get<{ sessions: BrowserSession[] }>('/auth/sessions')
  if (mounted) rows.value = result.sessions
}
async function perform(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true; error.value = ''; message.value = ''
  try { await action() }
  catch (e) { if (mounted) error.value = e instanceof ApiError ? e.message : 'Sessions are unavailable. Try again shortly.' }
  finally { if (mounted) busy.value = false }
}
async function revoke(row: BrowserSession) {
  await perform(async () => {
    await api.post(`/auth/sessions/${encodeURIComponent(row.id)}/revoke`, {})
    if (!mounted) return
    if (row.current) { session.forget(); await router.replace('/login'); return }
    message.value = 'Session signed out.'
    await load()
  })
}
onMounted(() => perform(load))
onUnmounted(() => { mounted = false })
</script>
<template>
  <section class="card">
    <h1>Browser sessions</h1>
    <p>Review sessions for this account and sign out sessions you no longer need.</p>
    <p class="muted">Sessions from another sign-in provider are managed separately. Signing out stops new browser requests; work already accepted by the platform may finish.</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="message" role="status">{{ message }}</p>
    <button :disabled="busy" @click="perform(load)">Refresh sessions</button>
    <p v-if="busy" role="status">Updating sessions…</p>
    <table v-if="rows?.length">
      <thead><tr><th>Session</th><th>Signed in</th><th>Expires</th><th>Action</th></tr></thead>
      <tbody><tr v-for="(row, index) in rows" :key="row.id">
        <td>{{ row.current ? 'This browser' : `Other session ${index + 1}` }}</td>
        <td>{{ new Date(row.created_at).toLocaleString() }}</td>
        <td>{{ new Date(row.expires_at).toLocaleString() }}</td>
        <td><button :disabled="busy" @click="revoke(row)">{{ row.current ? 'Sign out this browser' : 'Sign out session' }}</button></td>
      </tr></tbody>
    </table>
    <p v-else-if="rows">No active sessions.</p>
    <RouterLink to="/authentication">Authentication</RouterLink>
  </section>
</template>
