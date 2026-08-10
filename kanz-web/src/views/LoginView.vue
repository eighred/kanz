<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useSession } from '../stores/session'
import { ApiError } from '../api/client'

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
    // `next` comes from the router guard, never from the server. It is checked
    // for a leading slash so a crafted link cannot turn a successful sign-in
    // into a redirect to another origin.
    const next = typeof route.query.next === 'string' ? route.query.next : '/venues'
    await router.push(next.startsWith('/') && !next.startsWith('//') ? next : '/venues')
  } catch (e) {
    if (e instanceof ApiError && e.status === 429) {
      // 429 IS NOT 401, AND SAYING SO MATTERS. This caller may well be holding
      // the correct credential — telling them it was wrong sends them to reset
      // a password that works, while the real answer is to wait.
      error.value = 'Too many attempts from your network. Wait a moment and try again.'
    } else {
      // EVERY OTHER REFUSAL GETS ONE ANSWER. Unknown account, wrong credential
      // and disabled account are deliberately indistinguishable at the server,
      // because the difference is what turns a login form into a list of the
      // fund's staff. Being more specific here would undo that at the last step.
      error.value = 'Those credentials were not accepted.'
    }
  } finally {
    busy.value = false
    credential.value = ''
  }
}
</script>

<template>
  <form class="card" @submit.prevent="submit">
    <h1>Sign in</h1>
    <p class="muted">Eighred operator and trading control plane.</p>

    <label>
      Account
      <input v-model.trim="subject" autocomplete="username" required placeholder="user:you" />
    </label>
    <label>
      Password
      <input v-model="credential" type="password" autocomplete="current-password" required />
    </label>

    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <button type="submit" :disabled="busy || !subject || !credential">
      {{ busy ? 'Signing in…' : 'Sign in' }}
    </button>

    <!-- THERE IS NO "CREATE AN ACCOUNT" LINK, AND THAT IS THE DESIGN. Accounts
         are provisioned by an operator and accepted through a single-use
         invitation; self-registration on a trading control plane would mean
         anyone who can reach the page can create a principal. -->
    <p class="muted">
      Have an invitation? <RouterLink to="/redeem">Accept it here</RouterLink>
    </p>
  </form>
</template>
