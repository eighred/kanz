<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useSession } from '../stores/session'

const subject = ref('')
const credential = ref('')
const error = ref('')
const busy = ref(false)

const session = useSession()
const router = useRouter()
const route = useRoute()

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await session.signIn(subject.value, credential.value)
    // `next` comes from the router guard, never from the server, so it cannot
    // be used to bounce a signed-in user to another origin.
    const next = typeof route.query.next === 'string' ? route.query.next : '/venues'
    await router.push(next.startsWith('/') ? next : '/venues')
  } catch (e) {
    // The BFF answers every refusal identically on purpose — unknown account,
    // wrong credential, disabled — so that a caller cannot enumerate users.
    // Showing anything more specific here would undo that at the last step.
    error.value = e instanceof Error ? e.message : 'sign-in failed'
  } finally {
    busy.value = false
    credential.value = ''
  }
}
</script>

<template>
  <form @submit.prevent="submit">
    <h1>Sign in</h1>
    <label>Account <input v-model="subject" autocomplete="username" required /></label>
    <label>
      Credential
      <input v-model="credential" type="password" autocomplete="current-password" required />
    </label>
    <p v-if="error" role="alert">{{ error }}</p>
    <button type="submit" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
  </form>
</template>
