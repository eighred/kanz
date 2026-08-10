<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, ApiError } from '../api/client'

// The page that started this: /venues answered "Login" because the TUI's device
// flow could not complete. It is the first screen wired end-to-end through the
// BFF proxy — browser -> /api/v1/control/venues -> gateway -> operator.
interface VenueRow {
  venue: string
  configured: boolean
  updated_at?: string
}

const rows = ref<VenueRow[]>([])
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    const res = await api.get<{ venues?: VenueRow[] }>('/api/v1/control/venues')
    rows.value = res.venues ?? []
  } catch (e) {
    // A 403 here is the gateway refusing a caller without authz.Operate, and it
    // is CORRECT — this page renders for anyone signed in, because hiding it
    // would be a courtesy and not a control. Say so plainly rather than showing
    // an empty table, which reads as "no venues configured".
    error.value =
      e instanceof ApiError && e.status === 403
        ? 'Your account does not carry operator authority for venue keys.'
        : e instanceof Error
          ? e.message
          : 'could not load venues'
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <h1>Venues</h1>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <table v-else>
    <thead><tr><th>Venue</th><th>Keys</th></tr></thead>
    <tbody>
      <tr v-for="r in rows" :key="r.venue">
        <td>{{ r.venue }}</td>
        <td :class="r.configured ? 'state-ok' : 'state-unset'">
          {{ r.configured ? 'configured' : 'not set' }}
        </td>
      </tr>
      <tr v-if="rows.length === 0"><td colspan="2">No venues returned.</td></tr>
    </tbody>
  </table>
</template>
