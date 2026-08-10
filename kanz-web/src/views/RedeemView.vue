<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useSession } from '../stores/session'
import { ApiError } from '../api/client'

// ACCEPTING AN INVITATION. Redeeming sets the credential AND signs the person
// in, in one step, so nobody is asked for a password they chose one second ago.
//
// THE INVITE IS SINGLE-USE AND THE ATTEMPT IS NOT REPLAYABLE. That is what makes
// the confirmation field below a correctness control rather than a nicety: a
// mistyped password creates the account with a value the invitee does not know
// AND burns the invitation, so recovering needs an operator to issue a new one.
const token = ref('')
const credential = ref('')
const confirm = ref('')
const error = ref('')
const busy = ref(false)

const session = useSession()
const router = useRouter()
const route = useRoute()

// MINIMUM LENGTH IS ENFORCED BY THE SERVER, and is repeated here only so the
// person finds out before spending their single-use invitation on a refusal.
// This check is a courtesy; it is not the control.
const MIN_CREDENTIAL = 12

const tooShort = computed(() => credential.value.length > 0 && credential.value.length < MIN_CREDENTIAL)
const mismatch = computed(() => confirm.value.length > 0 && credential.value !== confirm.value)
const ready = computed(
  () => token.value.length > 0 && credential.value.length >= MIN_CREDENTIAL && credential.value === confirm.value,
)

onMounted(() => {
  const fromLink = typeof route.query.token === 'string' ? route.query.token : ''
  if (!fromLink) return
  token.value = fromLink
  // THE TOKEN IS A BEARER CREDENTIAL AND IT ARRIVED IN A URL. Left there it
  // persists in browser history, in any bookmark, and in the Referer of every
  // subsequent request this page makes. Stripping it costs nothing and removes
  // the copy this application is responsible for — the one in the invitee's
  // mail client is not ours to delete, which is why invitations expire.
  router.replace({ path: route.path, query: {} })
})

async function submit() {
  // GUARDED HERE, NOT ONLY BY THE DISABLED BUTTON. A disabled control is a
  // rendering decision; this is the check that decides whether a SINGLE-USE
  // invitation gets spent. Anything that reaches this handler another way — a
  // submit event, an autofill, a future keyboard shortcut — must hit the same
  // rule, because the cost of getting it wrong is an account created with a
  // password its owner does not know and an invitation that cannot be reused.
  if (!ready.value || busy.value) return

  error.value = ''
  busy.value = true
  try {
    await session.redeem(token.value, credential.value)
    await router.push('/venues')
  } catch (e) {
    if (e instanceof ApiError && e.status === 429) {
      error.value = 'Too many attempts from your network. Wait a moment and try again.'
    } else if (e instanceof ApiError && e.status === 401) {
      // ONE ANSWER FOR THREE STATES — unknown, expired, already redeemed. The
      // server collapses them because distinguishing them would confirm to
      // someone holding only a link that an account was offered to somebody.
      // Re-separating them here would undo that at the last step.
      error.value =
        'That invitation is not valid. It may have expired or already been used — ask your operator for a new one.'
    } else {
      error.value = e instanceof Error ? e.message : 'could not accept the invitation'
    }
  } finally {
    busy.value = false
    credential.value = ''
    confirm.value = ''
  }
}
</script>

<template>
  <form class="card" @submit.prevent="submit">
    <h1>Accept your invitation</h1>
    <p class="muted">
      This creates your account and signs you in. The invitation can be used once.
    </p>

    <label>
      Invitation token
      <input
        v-model.trim="token"
        required
        autocomplete="off"
        spellcheck="false"
        placeholder="paste the token from your invitation"
      />
    </label>

    <label>
      Choose a password
      <input v-model="credential" type="password" autocomplete="new-password" required />
    </label>
    <p v-if="tooShort" class="hint">At least {{ MIN_CREDENTIAL }} characters.</p>

    <label>
      Confirm password
      <input v-model="confirm" type="password" autocomplete="new-password" required />
    </label>
    <p v-if="mismatch" class="hint">
      These do not match. The invitation is single-use, so a typo here would create your
      account with a password you do not know.
    </p>

    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <button type="submit" :disabled="busy || !ready">
      {{ busy ? 'Creating your account…' : 'Accept and sign in' }}
    </button>

    <p class="muted">Already have an account? <RouterLink to="/login">Sign in</RouterLink></p>
  </form>
</template>
