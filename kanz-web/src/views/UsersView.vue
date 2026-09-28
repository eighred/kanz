<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { users, type UserAccess, type IdentityPermissions, type GatewayPermissions } from '../api/users'

const authority = ref<IdentityPermissions | null>(null)
const gateway = ref<GatewayPermissions | null>(null)
const gatewayError = ref('')
const rows = ref<UserAccess[]>([])
const next = ref('')
const selected = ref<UserAccess | null>(null)
const roles = ref('')
const portfolios = ref('')
const busy = ref(false)
const error = ref('')
const message = ref('')
const split = (value: string) => value.split(',').map(x => x.trim()).filter(Boolean)
async function load(after = '') {
  const page = await users.list(after)
  rows.value = after ? [...rows.value, ...page.users] : page.users
  next.value = page.next_cursor
}
function edit(user: UserAccess) {
  selected.value = user
  roles.value = user.roles.join(', ')
  portfolios.value = user.portfolios.join(', ')
  message.value = ''; error.value = ''
}
async function run(action: () => Promise<unknown>) {
  if (busy.value) return
  busy.value = true; error.value = ''; message.value = ''
  try { await action() } catch (e) { error.value = e instanceof Error ? e.message : 'Account access is unavailable.' }
  finally { busy.value = false }
}
async function save() {
  if (!selected.value) return
  const user = selected.value
  await run(async () => {
    await users.update(user, split(roles.value), split(portfolios.value))
    selected.value = null
    message.value = 'Access updated and previous sessions revoked. The account holder must sign in again.'
    await load()
  })
}
async function setStatus(user: UserAccess) {
  await run(async () => {
    await users.status(user.subject, user.status === 'active' ? 'disabled' : 'active')
    selected.value = null
    message.value = 'Account status updated. Re-enabling an account does not restore previous sessions.'
    await load()
  })
}
onMounted(() => run(async () => {
  authority.value = await users.permissions()
  try { gateway.value = await users.gatewayPermissions() }
  catch (e) { gatewayError.value = e instanceof Error ? e.message : 'Gateway permissions are unavailable.' }
  if (authority.value.identity_admin) await load()
}))
</script>

<template>
  <section aria-labelledby="users-title">
    <h1 id="users-title">Users and permissions</h1>
    <p v-if="busy" role="status">Loading account access…</p>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="message" role="status">{{ message }}</p>
    <template v-if="authority">
      <h2>Your effective permissions</h2>
      <p>{{ authority.subject }} · Tenant {{ authority.tenant }}</p>
      <p>Identity administration: {{ authority.identity_admin ? 'permitted' : 'not permitted' }}</p>
      <p v-if="gatewayError" role="alert">Gateway permissions unavailable: {{ gatewayError }}</p>
      <template v-if="gateway">
        <p>Gateway capabilities: {{ gateway.capabilities.join(', ') || 'none' }}</p>
        <p>Portfolio claims: {{ gateway.portfolios.join(', ') || 'none asserted' }}</p>
        <p>Capital actions require explicit portfolio entitlement. Read scope follows each endpoint’s policy. Individual actions remain subject to current account authority and domain controls.</p>
        <details><summary>Permitted API routes</summary><ul><li v-for="route in gateway.routes" :key="route.Pattern">{{ route.Pattern }} · {{ route.Capability }}</li></ul></details>
      </template>
      <template v-if="authority.identity_admin">
        <h2>Tenant users</h2>
        <button :disabled="busy" @click="run(async () => { selected = null; await load() })">Reload users</button>
        <table>
          <thead><tr><th>Subject</th><th>Status</th><th>Roles</th><th>Portfolio claims</th><th>Created by</th><th>Created</th><th>Last account update</th><th>Actions</th></tr></thead>
          <tbody><tr v-for="user in rows" :key="user.subject">
            <td>{{ user.subject }}</td><td>{{ user.status }}</td><td>{{ user.roles.join(', ') }}</td><td>{{ user.portfolios.join(', ') || 'none' }}</td>
            <td>{{ user.created_by || 'unavailable in historical record' }}</td><td>{{ user.created_at }}</td><td>{{ user.updated_at }}</td>
            <td><button :disabled="busy" :aria-label="`Edit access for ${user.subject}`" @click="edit(user)">Edit access</button>
              <button :disabled="busy || (user.subject === authority.subject && user.status === 'active')" :aria-label="`${user.status === 'active' ? 'Disable' : 'Enable'} ${user.subject}`" @click="setStatus(user)">{{ user.status === 'active' ? 'Disable' : 'Enable' }}</button></td>
          </tr></tbody>
        </table>
        <p v-if="!rows.length && !busy">No users found.</p>
        <button v-if="next" :disabled="busy" @click="run(() => load(next))">Load more users</button>
        <form v-if="selected" @submit.prevent="save">
          <h2>Edit access for {{ selected.subject }}</h2>
          <p>Saving revokes all previous sessions for this account. Identity administrators may only combine their role with kanz-user.</p>
          <label for="access-roles">Roles, separated by commas</label><input id="access-roles" v-model="roles" required :disabled="busy">
          <label for="access-portfolios">Portfolio IDs, separated by commas</label><input id="access-portfolios" v-model="portfolios" :disabled="busy">
          <button :disabled="busy" type="submit">Save access and revoke sessions</button>
          <button :disabled="busy" type="button" @click="selected = null">Cancel</button>
        </form>
      </template>
    </template>
  </section>
</template>
