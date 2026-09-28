<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, ApiError } from '../api/client'
import { useSession } from '../stores/session'

const route = useRoute()
const router = useRouter()
const verify = ref(false)
const token = ref('')
const hasLink = ref(false)
const subject = ref('')
const credential = ref('')
const confirmation = ref('')
const busy = ref(false)
const error = ref('')
const done = ref('')
const session = useSession()
let flowVersion = 0
// Email navigation can change only the fragment while Vue reuses this view.
// Accept a new proof, but do not clear it when stripping its URL fragment.
watch([() => route.path, () => route.hash], ([path, hash], previous) => {
  if (previous && path === previous[0] && !hash) return
  flowVersion++; busy.value = false
  verify.value = path === '/verify-mailbox'
  token.value = new URLSearchParams(hash.slice(1)).get('token') || ''
  hasLink.value = token.value !== ''
  credential.value = ''; confirmation.value = ''; error.value = ''; done.value = ''
  if (hash) void router.replace({ path, hash: '' })
}, { immediate: true })
onBeforeUnmount(() => { flowVersion++; token.value = ''; credential.value = ''; confirmation.value = '' })
async function submit() {
  if (busy.value || done.value) return
  error.value = ''
  if (hasLink.value && !verify.value && credential.value !== confirmation.value) { error.value = 'Passwords do not match.'; return }
  busy.value = true
  const version = flowVersion
  const verifying = verify.value
  try {
    if (!hasLink.value && !verify.value) {
      await api.post('/auth/recovery', { subject: subject.value })
      if (version !== flowVersion) return
      done.value = 'If this account has a verified recovery mailbox and is eligible, a link will be sent there. This does not confirm delivery. Allow a few minutes before requesting another link.'
    } else {
      await api.post(verify.value ? '/auth/mailbox/verify' : '/auth/recovery/consume', verify.value ? { token: token.value } : { token: token.value, credential: credential.value })
      if (!verifying) session.forget()
      if (version !== flowVersion) return
      token.value = ''
      done.value = verify.value ? 'Recovery mailbox verified.' : 'Password reset. Existing sessions have been revoked. Sign in with your new password.'
    }
  } catch (e) {
    if (version !== flowVersion) return
    error.value = e instanceof ApiError && e.status === 404 ? 'Recovery is not configured. Contact your administrator.'
      : e instanceof ApiError && e.status === 429 ? 'Too many attempts. Wait before trying again.'
      : e instanceof ApiError && e.status === 401 ? 'This link is invalid, expired, or already used.'
      : e instanceof ApiError && e.status === 400 ? 'Use a password of 12 to 1024 characters.'
      : 'The result could not be confirmed. For a password reset, try signing in with the new password before requesting another link.'
  } finally { if (version === flowVersion) { credential.value = ''; confirmation.value = ''; busy.value = false } }
}
</script>

<template>
  <section class="card">
    <h1>{{ verify ? 'Verify recovery mailbox' : 'Recover account' }}</h1>
    <p v-if="done" role="status">{{ done }}</p>
    <p v-else-if="verify && !hasLink" role="alert">Open the verification link sent to your mailbox.</p>
    <form v-else @submit.prevent="submit">
      <template v-if="!hasLink">
        <p>Recovery is available only after you have verified a mailbox while signed in.</p>
        <label>Account<input v-model.trim="subject" autocomplete="username" maxlength="256" required :disabled="busy" /></label>
      </template>
      <template v-else-if="!verify">
        <p>Choose a password of 12 to 1024 characters. Resetting revokes your existing sessions.</p>
        <label>New password<input v-model="credential" type="password" autocomplete="new-password" required :disabled="busy" /></label>
        <label>Confirm new password<input v-model="confirmation" type="password" autocomplete="new-password" required :disabled="busy" /></label>
      </template>
      <p v-else>Confirm that this mailbox may be used to recover your Kanz account.</p>
      <p v-if="error" role="alert" class="error">{{ error }}</p>
      <button :disabled="busy" type="submit">{{ busy ? 'Submitting…' : verify ? 'Verify mailbox' : hasLink ? 'Reset password' : 'Request recovery link' }}</button>
    </form>
    <RouterLink to="/login">Sign in</RouterLink>
  </section>
</template>
