<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { invitations, type CreatedInvitation, type InvitationSummary } from '../api/invitations'

const entries = ref<InvitationSummary[]>([])
const subject = ref('')
const rolesText = ref('')
const portfoliosText = ref('')
const loading = ref(true)
const busy = ref(false)
const error = ref('')
const created = ref<CreatedInvitation | null>(null)
const copied = ref(false)
const message = ref('')
const review = ref<{ invite: InvitationSummary; action: 'revoke' | 'reissue' } | null>(null)

function values(value: string): string[] {
  return [...new Set(value.split(/[\s,]+/).map((part) => part.trim()).filter(Boolean))]
}

const roles = computed(() => values(rolesText.value))
const portfolios = computed(() => values(portfoliosText.value))
const ready = computed(() => subject.value.trim().length > 0 && roles.value.length > 0)
const activationLink = computed(() => {
  if (!created.value?.invite_token) return ''
  const url = new URL('/redeem', window.location.origin)
  url.searchParams.set('token', created.value.invite_token)
  return url.toString()
})

function when(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'time not stated' : date.toLocaleString()
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    entries.value = await invitations.list()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'could not read invitations'
  } finally {
    loading.value = false
  }
}

async function create() {
  if (!ready.value || busy.value) return
  error.value = ''
  created.value = null
  copied.value = false
  busy.value = true
  try {
    created.value = await invitations.create({
      subject: subject.value.trim(),
      roles: roles.value,
      portfolios: portfolios.value,
    })
    subject.value = ''
    rolesText.value = ''
    portfoliosText.value = ''
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'could not create the invitation'
  } finally {
    busy.value = false
  }
}

async function copyLink() {
  if (!activationLink.value) return
  try {
    await navigator.clipboard.writeText(activationLink.value)
    copied.value = true
  } catch { error.value = 'Copy failed. Select the activation link and copy it manually.' }
}

function prepare(invite: InvitationSummary, action: 'revoke' | 'reissue') {
  if (busy.value) return
  review.value = { invite, action }
  message.value = ''
  error.value = ''
}

async function change() {
  if (!review.value || busy.value) return
  const { invite, action } = review.value
  busy.value = true
  error.value = ''
  dismissSecret()
  try {
    if (action === 'reissue') {
      created.value = await invitations.reissue(invite)
      message.value = 'Invitation reissued. The previous token is revoked.'
    } else {
      await invitations.revoke(invite)
      message.value = 'Invitation revoked. Its token can no longer activate an account.'
    }
    review.value = null
    await load()
  } catch (e) {
    error.value = (e instanceof Error ? e.message : 'The change could not be confirmed') + '. Reload invitations before another attempt.'
  } finally { busy.value = false }
}

onBeforeUnmount(dismissSecret)

function dismissSecret() {
  created.value = null
  copied.value = false
}

onMounted(load)
</script>

<template>
  <h1>User invitations</h1>
  <p class="muted">
    Create an account offer in your tenant. The invitee chooses their own password and receives
    exactly the roles and portfolio access recorded here.
  </p>

  <form class="panel" @submit.prevent="create">
    <h2>Create invitation</h2>
    <label>
      Canonical human account ID
      <input v-model.trim="subject" required autocomplete="off" placeholder="user:person" />
    </label>
    <label>
      Roles
      <input
        v-model="rolesText"
        required
        autocomplete="off"
        spellcheck="false"
        placeholder="comma-separated exact role names"
      />
    </label>
    <p class="hint">
      Roles grant authority. Enter only names approved for this person; the service records you as
      the grantor.
    </p>
    <label>
      Portfolios
      <input
        v-model="portfoliosText"
        autocomplete="off"
        spellcheck="false"
        placeholder="comma-separated portfolio IDs"
      />
    </label>

    <div class="grant-preview" aria-live="polite">
      <span><strong>Subject:</strong> {{ subject || 'not entered' }}</span>
      <span><strong>Roles:</strong> {{ roles.length ? roles.join(', ') : 'none' }}</span>
      <span><strong>Portfolios:</strong> {{ portfolios.length ? portfolios.join(', ') : 'none' }}</span>
    </div>

    <button type="submit" :disabled="busy || !ready">
      {{ busy ? 'Creating…' : 'Create one-time invitation' }}
    </button>
  </form>

  <section v-if="created" class="secret-panel" aria-labelledby="created-invite-title">
    <h2 id="created-invite-title">Invitation created — copy it now</h2>
    <p>
      Send this activation link to <strong>{{ created.subject }}</strong> through an approved private
      channel. It is a bearer credential, expires {{ when(created.expires_at) }}, and cannot be
      recovered from Kanz after this panel is dismissed.
    </p>
    <label>
      One-time activation link
      <textarea :value="activationLink" readonly rows="4" spellcheck="false" />
    </label>
    <div class="actions">
      <button type="button" @click="copyLink">{{ copied ? 'Copied' : 'Copy activation link' }}</button>
      <button type="button" class="quiet" @click="dismissSecret">I have transferred it</button>
    </div>
  </section>

  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <p v-if="message" role="status">{{ message }}</p>
  <section v-if="review" class="panel" aria-labelledby="invitation-review-title">
    <h2 id="invitation-review-title">Review {{ review.action }}</h2>
    <p><strong>{{ review.invite.subject }}</strong> · {{ review.invite.tenant }} · {{ review.invite.invite_id }}</p>
    <p>Roles: {{ review.invite.roles.join(', ') }}. Portfolios: {{ review.invite.portfolios.join(', ') || 'none' }}.</p>
    <p v-if="review.action === 'reissue'">The old token will stop working. A new one-time link will carry the same authority and a new expiry. Transfer it through an approved private channel.</p>
    <p v-else>This prevents this invitation from activating an account. An already accepted invitation cannot be revoked; use account access controls instead.</p>
    <button type="button" :disabled="busy" @click="change">Confirm {{ review.action }}</button>
    <button type="button" class="quiet" :disabled="busy" @click="review = null">Cancel</button>
  </section>
  <button type="button" class="quiet" :disabled="busy || loading" @click="review = null; load()">Reload invitations</button>
  <p v-if="loading">Loading…</p>

  <table v-else-if="!error">
    <thead>
      <tr>
        <th>Subject</th>
        <th>Authority</th>
        <th>Created by</th>
        <th>Expires</th>
        <th>Status</th>
        <th>Actions</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="invite in entries" :key="invite.invite_id">
        <td><code>{{ invite.subject }}</code><br /><span class="muted">{{ invite.tenant }}</span></td>
        <td>
          {{ invite.roles.join(', ') }}
          <span class="muted"><br />portfolios: {{ invite.portfolios.join(', ') || 'none' }}</span>
        </td>
        <td>{{ invite.created_by }}</td>
        <td>{{ when(invite.expires_at) }}</td>
        <td :class="invite.redeemable ? 'state-warn' : 'state-unset'">
          {{ invite.state }}
          <span v-if="invite.revoked_by" class="muted"><br />revoked by {{ invite.revoked_by }}</span>
          <span v-if="invite.reissued_as" class="muted"><br />reissued as {{ invite.reissued_as }}</span>
        </td>
        <td>
          <template v-if="invite.state !== 'accepted' && !invite.reissued_as">
            <button v-if="invite.state !== 'revoked'" type="button" :aria-label="`Revoke invitation ${invite.invite_id}`" :disabled="busy" @click="prepare(invite, 'revoke')">Revoke</button>
            <button type="button" :aria-label="`Reissue invitation ${invite.invite_id}`" :disabled="busy" @click="prepare(invite, 'reissue')">Reissue</button>
          </template>
        </td>
      </tr>
      <tr v-if="entries.length === 0">
        <td colspan="6">No invitation records were returned for this tenant.</td>
      </tr>
    </tbody>
  </table>
</template>
