<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { control, describe, ready, type Cluster, type Node } from '../api/control'

// THE ESTATE, AS THE GATEWAY REPORTS IT.
//
// Read-only on purpose. Cordon, drain and region are POSTs that change a
// running cluster; they belong behind a confirmation step and an audit trail,
// not on the screen somebody lands on. This one answers "what is out there and
// is it healthy", which is what an operator opens first.

const nodes = ref<Node[]>([])
const clusters = ref<Cluster[]>([])
const error = ref('')
const loading = ref(true)
let timer: ReturnType<typeof setInterval> | undefined

// POLLING, BECAUSE THERE IS NO STREAM. The gateway deliberately does not proxy
// SSE, so a live screen polls or it lies. Thirty seconds is slow enough to cost
// nothing and fast enough that a drained node does not sit green for a shift.
const REFRESH_MS = 30_000

const notReady = computed(() => nodes.value.filter((n) => !ready(n)).length)
const unschedulable = computed(() => nodes.value.filter((n) => n.schedulable !== true).length)

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    const [n, c] = await Promise.all([control.nodes(), control.clusters()])
    nodes.value = n
    clusters.value = c
    error.value = ''
  } catch (e) {
    // The message distinguishes "no control plane configured" from "no nodes" —
    // drawn as an empty table, the first reads as a healthy estate.
    error.value = describe(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load(true)
  timer = setInterval(() => void load(), REFRESH_MS)
})
onUnmounted(() => clearInterval(timer))

/** age renders createdAt as a coarse duration; the server never sends one. */
function age(iso?: string): string {
  if (!iso) return '—'
  const ms = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const days = Math.floor(ms / 86_400_000)
  if (days >= 1) return `${days}d`
  const hours = Math.floor(ms / 3_600_000)
  if (hours >= 1) return `${hours}h`
  return `${Math.max(1, Math.floor(ms / 60_000))}m`
}
</script>

<template>
  <h1>Estate</h1>
  <p class="muted">Nodes and regions as the control plane reports them. Refreshes every 30s.</p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p class="muted">
      {{ nodes.length }} node{{ nodes.length === 1 ? '' : 's' }} ·
      <span :class="notReady ? 'state-bad' : 'state-ok'">{{ notReady }} not ready</span> ·
      <span :class="unschedulable ? 'state-warn' : 'state-ok'">{{ unschedulable }} unschedulable</span>
    </p>

    <table>
      <thead>
        <tr><th>Region</th><th>Online</th><th>Offline</th></tr>
      </thead>
      <tbody>
        <tr v-for="c in clusters" :key="c.region ?? '(none)'">
          <td>{{ c.region || '(no region label)' }}</td>
          <td class="state-ok">{{ c.online ?? 0 }}</td>
          <td :class="(c.offline ?? 0) > 0 ? 'state-bad' : 'state-unset'">{{ c.offline ?? 0 }}</td>
        </tr>
        <tr v-if="clusters.length === 0"><td colspan="3">No regions reported.</td></tr>
      </tbody>
    </table>

    <table>
      <thead>
        <tr><th>Node</th><th>Status</th><th>Scheduling</th><th>Roles</th><th>Version</th><th>Age</th></tr>
      </thead>
      <tbody>
        <tr v-for="n in nodes" :key="n.name">
          <td>{{ n.name }}</td>
          <!-- Anything that is not an explicit READY renders as not-ready. -->
          <td :class="ready(n) ? 'state-ok' : 'state-bad'">{{ ready(n) ? 'ready' : 'not ready' }}</td>
          <td :class="n.schedulable === true ? 'state-ok' : 'state-warn'">
            {{ n.schedulable === true ? 'schedulable' : 'cordoned' }}
            <span v-if="n.evictablePods" class="muted">· {{ n.evictablePods }} evictable</span>
          </td>
          <td>{{ n.roles?.join(', ') || '—' }}</td>
          <td>{{ n.kubeletVersion || '—' }}</td>
          <td>{{ age(n.createdAt) }}</td>
        </tr>
        <tr v-if="nodes.length === 0"><td colspan="6">No nodes reported.</td></tr>
      </tbody>
    </table>
  </template>
</template>
