<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { control, describe, ready, type Node } from '../api/control'

// NODE LIFECYCLE: the actions Estate deliberately does not carry (#371).
//
// EstateView answers "what is out there and is it healthy" and is read-only on
// purpose — its own comment says cordon, drain and region "belong behind a
// confirmation step, not on the screen somebody lands on". This is that screen.
// The split is the point: an operator opening the app to check on the estate
// cannot reach a drain by mis-clicking a row.
//
// THE GATEWAY IS THE AUTHORITY, NOT THIS PAGE. Every button below renders for
// anyone signed in, and a caller without authz.Operate is refused by the gateway
// with a 403 that describe() explains. Hiding the buttons would be a courtesy;
// it would not be a control, and building it as though it were is how a
// front-end starts being trusted with an authorisation decision.

type Action = 'cordon' | 'uncordon' | 'drain' | 'region'

/** pending is the action awaiting confirmation, or null when none is. */
const pending = ref<{ node: Node; action: Action } | null>(null)
/** typed is the drain confirmation input — see requiresTypedName. */
const typed = ref('')
/** region is the target label for a move. */
const region = ref('')

const nodes = ref<Node[]>([])
const error = ref('')
const actionError = ref('')
const notice = ref('')
const loading = ref(true)
const busy = ref(false)
let timer: ReturnType<typeof setInterval> | undefined

// Same cadence as Estate. A drain is asynchronous, so the table is the only
// place its progress becomes visible and it must keep moving on its own.
const REFRESH_MS = 30_000

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    nodes.value = await control.nodes()
    error.value = ''
  } catch (e) {
    // "No control plane configured" must never be drawn as an empty table: an
    // estate with no nodes and a gateway with no control plane look identical
    // that way, and only one of them is fine.
    error.value = describe(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load(true)
  // POLLING PAUSES WHILE A CONFIRMATION IS OPEN. A refresh that replaced the
  // node list under an open dialog could leave the operator confirming against
  // a row that has moved — the click was aimed at what was on screen.
  timer = setInterval(() => {
    if (!pending.value && !busy.value) void load()
  }, REFRESH_MS)
})
onUnmounted(() => clearInterval(timer))

function ask(n: Node, action: Action) {
  pending.value = { node: n, action }
  typed.value = ''
  region.value = n.region ?? ''
  actionError.value = ''
  notice.value = ''
}

function cancel() {
  pending.value = null
  typed.value = ''
}

/**
 * requiresTypedName is true for drain alone.
 *
 * WHY ONLY DRAIN. Cordon and uncordon are each other's inverse and touch no
 * running workload; a region change is a relabel, which the proto calls
 * non-destructive. Drain EVICTS PODS — it is the one action here that moves
 * work off a machine, and the one whose blast radius is other people's
 * services. Typing the name is the cheapest way to make "which row was I on"
 * an answered question rather than an assumed one.
 *
 * Applying it to all four would be worse, not safer: a confirmation that
 * appears everywhere is one an operator learns to clear without reading.
 */
function requiresTypedName(a: Action): boolean {
  return a === 'drain'
}

function confirmable(): boolean {
  const p = pending.value
  if (!p || busy.value) return false
  if (requiresTypedName(p.action)) return typed.value === p.node.name
  if (p.action === 'region') return region.value.trim() !== '' && region.value.trim() !== p.node.region
  return true
}

async function run() {
  const p = pending.value
  if (!p || !confirmable()) return
  busy.value = true
  actionError.value = ''
  try {
    switch (p.action) {
      case 'cordon':
        await control.cordon(p.node.name)
        notice.value = `${p.node.name} is cordoned. Running pods are untouched.`
        break
      case 'uncordon':
        await control.uncordon(p.node.name)
        notice.value = `${p.node.name} accepts new pods again.`
        break
      case 'drain':
        await control.drain(p.node.name)
        // DELIBERATELY NOT "drained". The gateway returns once the node is
        // cordoned; eviction runs in the background under PodDisruptionBudgets
        // and may take a while or stall on one. Claiming completion here would
        // be inventing a fact the platform did not report.
        notice.value = `${p.node.name} is cordoned and eviction has started. It proceeds in the background — watch the evictable count.`
        break
      case 'region':
        await control.setRegion(p.node.name, region.value.trim())
        notice.value = `${p.node.name} is labelled ${region.value.trim()}.`
        break
    }
    pending.value = null
    typed.value = ''
    await load()
  } catch (e) {
    // The action failed, so the node is unchanged. Keeping the dialog open
    // means the operator can read the reason next to what they were doing.
    actionError.value = describe(e)
  } finally {
    busy.value = false
  }
}

const titles: Record<Action, string> = {
  cordon: 'Cordon node',
  uncordon: 'Uncordon node',
  drain: 'Drain node',
  region: 'Move node to another region',
}
</script>

<template>
  <h1>Nodes</h1>
  <p class="muted">
    Maintenance actions on the estate. Read-only status lives on
    <RouterLink to="/estate">Estate</RouterLink>.
  </p>

  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <table>
      <thead>
        <tr>
          <th>Node</th><th>Status</th><th>Scheduling</th><th>Region</th><th>Actions</th>
        </tr>
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
          <td>{{ n.region || '(no region label)' }}</td>
          <td class="actions">
            <!-- Cordon and uncordon are offered by CURRENT STATE rather than
                 both always: uncordoning a schedulable node is a no-op that
                 still reads as an action having been taken. -->
            <button v-if="n.schedulable === true" class="quiet" @click="ask(n, 'cordon')">Cordon</button>
            <button v-else class="quiet" @click="ask(n, 'uncordon')">Uncordon</button>
            <button class="quiet" @click="ask(n, 'region')">Move</button>
            <button class="danger" @click="ask(n, 'drain')">Drain</button>
          </td>
        </tr>
        <tr v-if="nodes.length === 0"><td colspan="5">No nodes reported.</td></tr>
      </tbody>
    </table>
  </template>

  <!-- THE CONFIRMATION. One panel for all four actions, because four dialogs
       are four places for the wording to drift from what the button does. -->
  <div v-if="pending" class="confirm card" role="dialog" aria-modal="true" aria-labelledby="confirm-title">
    <h2 id="confirm-title">{{ titles[pending.action] }}</h2>

    <p class="muted">
      <strong>{{ pending.node.name }}</strong>
      <span v-if="pending.node.region"> · {{ pending.node.region }}</span>
    </p>

    <p v-if="pending.action === 'cordon'">
      It stops accepting new pods. Running pods are untouched, and Uncordon reverses it.
    </p>
    <p v-else-if="pending.action === 'uncordon'">It accepts new pods again.</p>
    <p v-else-if="pending.action === 'drain'">
      It is cordoned and its pods are <strong>evicted</strong> — this moves running work off the
      machine. Eviction honours PodDisruptionBudgets and continues in the background after this
      returns.
      <span v-if="pending.node.evictablePods">
        {{ pending.node.evictablePods }} pod{{ pending.node.evictablePods === 1 ? '' : 's' }}
        would be evicted.
      </span>
    </p>
    <p v-else>
      It is relabelled, which moves it between the region groupings Estate shows. Nothing is
      evicted.
    </p>

    <label v-if="pending.action === 'region'">
      Region
      <input v-model="region" name="region" autocomplete="off" />
    </label>

    <label v-if="requiresTypedName(pending.action)">
      Type <strong>{{ pending.node.name }}</strong> to confirm
      <input v-model="typed" name="confirm-name" autocomplete="off" />
    </label>

    <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>

    <div class="actions">
      <button :disabled="!confirmable()" :class="pending.action === 'drain' ? 'danger' : ''" @click="run">
        {{ busy ? 'Working…' : titles[pending.action] }}
      </button>
      <button class="quiet" :disabled="busy" @click="cancel">Cancel</button>
    </div>
  </div>
</template>
