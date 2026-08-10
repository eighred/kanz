<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { control, describe, type VenueKeyStatus } from '../api/control'

// The page that started all this: /venues answered "Login" because the TUI's
// device flow could not complete against an SSO that does not exist.
//
// PRESENCE ONLY, NEVER KEY MATERIAL. The gateway returns a `configured` flag and
// nothing else, and this screen must never grow a way to read a key back — a
// surface that can display one is a surface that can leak one.
const rows = ref<VenueKeyStatus[]>([])
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    rows.value = await control.venues()
  } catch (e) {
    // A 403 here is the gateway refusing a caller without operator authority,
    // and it is CORRECT: this page renders for anyone signed in, because hiding
    // it would be a courtesy and not a control. A 404 means the gateway fronts
    // no control plane — which must not be drawn as "no venues configured".
    error.value = describe(e)
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <h1>Venues</h1>
  <p class="muted">Whether trading credentials are present. Keys themselves are never returned.</p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <table v-else>
    <thead><tr><th>Venue</th><th>Keys</th></tr></thead>
    <tbody>
      <tr v-for="r in rows" :key="r.venue">
        <td>{{ r.venue }}</td>
        <!-- `configured` is omitted by protojson when false, so only an
             explicit true counts as configured. -->
        <td :class="r.configured === true ? 'state-ok' : 'state-unset'">
          {{ r.configured === true ? 'configured' : 'not set' }}
        </td>
      </tr>
      <tr v-if="rows.length === 0"><td colspan="2">No venues returned.</td></tr>
    </tbody>
  </table>
</template>
