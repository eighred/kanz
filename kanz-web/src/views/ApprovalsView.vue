<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  approvals,
  classify,
  describeApprovals,
  describeApproveFailure,
  describeRefusal,
  samePerson,
  type PendingApproval,
  type Reading,
} from '../api/approvals'
import { formatDecimal } from '../api/decimal'
import { useSession } from '../stores/session'

// THE SECOND SIGNATURE (#371, one slice — the approvals screen).
//
// Until this screen existed an approver worked from curl, and not for want of a
// server. Dual control has been complete on the server side for some time:
// GET /v1/orders/pending-approvals lists what is held (#557), POST
// /v1/orders/{id}/approve releases it (#546), and a refusal is recorded on the
// queue with a state and a reason (#575). What was missing was any client at
// all, and the gap is not cosmetic — a held order is DELIBERATELY absent from
// the orders table, so no other screen shows it, and the approve route refuses
// an empty digest with 400. Without this queue an approver could not discover
// the order id, and could not have signed it if they had.
//
// # THE THREE THINGS THIS SCREEN MUST NOT DO
//
// 1. IT MUST NOT RENDER A REFUSED ENTRY AS UNTOUCHED WORK. "refused" never
//    means finished — a decided proposal is off the queue entirely — it means
//    the last person who tried was turned away and the order is STILL waiting.
//    Drawn identically to a fresh proposal, that fact is invisible exactly when
//    somebody needs it: the approver who was refused sees their own signature
//    silently absent, and the next approver has no idea a rule already fired.
//
// 2. IT MUST NOT REPORT THE 202 AS AN APPROVAL. The approve route publishes to
//    the bus and answers immediately; the OMS decides afterwards and can refuse.
//    A "done" message here would claim an outcome nothing has decided, which is
//    precisely the failure dual control exists to prevent — a person believing
//    capital was released when their signature was refused. The screen says
//    SUBMITTED, and then re-reads the queue, which is the only place the answer
//    ever appears.
//
// 3. IT MUST NOT COLLAPSE 404 AND 403. They describe different deployments:
//    absent versus forbidden (#535). See describeApprovals.
//
// # WHAT IS SHOWN, AND WHY THE DIGEST IS NOT AN IMPLEMENTATION DETAIL
//
// The digest is the value a signature covers, and the approve route requires it.
// It exists nowhere else a person can look — the FACT carrying it goes to NATS
// — so this queue is its only human-readable home. It is rendered in FULL and
// never truncated: two proposals differing in one field differ somewhere in
// those 64 characters, and an abbreviation is how an approver confirms they are
// signing the thing they read about.
//
// # THE APPROVE BUTTON IS A COURTESY, NOT A CONTROL
//
// It is withheld from the proposer because the OMS would refuse them anyway and
// discovering a rule by pressing a button and waiting for an asynchronous
// refusal is a poor way to learn it. The comparison matches the server's —
// case- and space-insensitive — so the two agree. The GATEWAY and the OMS remain
// the only things deciding anything; this store is consulted for a name.

const session = useSession()

const entries = ref<PendingApproval[]>([])
const ownerTenant = ref('')
const error = ref('')
const loading = ref(true)
/** loaded is true once the queue has answered at least once, error or not. */
const loaded = ref(false)

/** pending is the entry awaiting confirmation, or null when none is. */
const confirming = ref<PendingApproval | null>(null)
const actionError = ref('')
const notice = ref('')
const busy = ref(false)

const me = computed(() => session.identity?.subject ?? '')

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    const resp = await approvals.list()
    entries.value = resp.pending ?? []
    ownerTenant.value = resp.owner_tenant ?? ''
    error.value = ''
  } catch (e) {
    // AN ERROR IS NEVER AN EMPTY QUEUE. The gateway's own handler says so: a
    // caller that rendered a failure as "nothing to do" would hide a broken
    // control, and the entries are cleared so a stale list cannot be read as
    // current.
    entries.value = []
    error.value = describeApprovals(e)
  } finally {
    loading.value = false
    loaded.value = true
  }
}

onMounted(() => void load(true))

/** state reads one entry, denying by default — see classify(). */
function state(p: PendingApproval): Reading {
  return classify(p)
}

/** needsAttention is every entry whose state is not a clean "pending". */
const needsAttention = computed(() => entries.value.filter((p) => state(p) !== 'pending'))

/** mine is true when this account proposed the entry, by the server's own rule. */
function mine(p: PendingApproval): boolean {
  return samePerson(p.proposer, me.value)
}

/** refusedMe is true when THIS account's signature was the one turned away. */
function refusedMe(p: PendingApproval): boolean {
  return state(p) === 'refused' && samePerson(p.last_refused_by, me.value)
}

/**
 * expired reports that the window closed while this page was open.
 *
 * The OMS excludes expired proposals from the queue, so an entry arrives live —
 * but the list is a SNAPSHOT and the deadline keeps moving. Approving after it
 * passes is refused with `expired`, and saying so before the click beats
 * discovering it asynchronously afterwards.
 */
function expired(p: PendingApproval): boolean {
  const t = Date.parse(p.expires_at ?? '')
  return Number.isFinite(t) && t <= Date.now()
}

/** when renders a wire timestamp, or a dash — never a fabricated one. */
function when(ts: string | undefined): string {
  if (!ts) return '—'
  const t = Date.parse(ts)
  return Number.isFinite(t) ? new Date(t).toISOString().replace('T', ' ').replace(/\.\d+Z$/, 'Z') : ts
}

/**
 * amount renders a SubmitOrder Decimal, or "—".
 *
 * Never a 0 fallback: a quantity that cannot be rendered must look
 * unrenderable, because a zero here reads as a harmless order.
 */
function amount(d: Parameters<typeof formatDecimal>[0]): string {
  return formatDecimal(d) ?? '—'
}

/** terse strips the protobuf enum prefix for display only. */
function terse(v: string | undefined, prefix: string): string {
  if (!v) return '—'
  return v.replace(prefix, '').replaceAll('_', ' ').toLowerCase() || '—'
}

function ask(p: PendingApproval) {
  confirming.value = p
  actionError.value = ''
  notice.value = ''
}

function cancel() {
  confirming.value = null
  actionError.value = ''
}

async function sign() {
  const p = confirming.value
  if (!p || busy.value) return
  busy.value = true
  actionError.value = ''
  try {
    // THE ID AND THE DIGEST, both from the entry. Approving on the id alone
    // would let the stored command change after the signature — propose a
    // defensible order, collect the approval, apply a different one — which is
    // why the route refuses an approval carrying no digest at all.
    await approvals.approve(p.order_id, p.digest)
    // DELIBERATELY NOT "approved". Nothing has decided yet; the command was
    // published and the OMS rules on it out of band. Claiming release here
    // would be inventing the one fact this control exists to establish.
    notice.value =
      `Approval SUBMITTED for ${p.order_id}. It is not approved yet — the OMS rules on it after ` +
      `this, and it can still be refused. The queue below is the only place that answer appears.`
    confirming.value = null
    await load()
  } catch (e) {
    // The publish failed, so nothing was signed and the order is untouched.
    // Keeping the panel open puts the reason beside what was being attempted.
    actionError.value = describeApproveFailure(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <h1>Approvals</h1>
  <p class="muted">
    Orders held for a second signature. A held order is in no blotter and no order
    history — this queue is the only place it appears.
    <span v-if="ownerTenant">· {{ ownerTenant }}</span>
  </p>

  <p v-if="loading">Loading…</p>

  <!-- 404 AND 403 ARRIVE HERE AS DIFFERENT SENTENCES. Absent is not forbidden. -->
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <!-- UNMISSABLE, AND ABOVE THE TABLE. A refused entry is still awaiting a
         signature, so it sits in the same list as untouched work and would
         otherwise be found only by reading every row. -->
    <p v-if="needsAttention.length" class="error" role="alert">
      {{ needsAttention.length }} of {{ entries.length }} entr{{ entries.length === 1 ? 'y' : 'ies' }}
      below {{ needsAttention.length === 1 ? 'has' : 'have' }} a signature that was already turned
      away, or a state this screen cannot read. They are still awaiting approval — refused never
      means finished — but do not read them as untouched work.
    </p>

    <table>
      <thead>
        <tr>
          <th>Order</th>
          <th>Proposed by</th>
          <th>State</th>
          <th>Expires</th>
          <th>Digest</th>
          <th>Action</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="p in entries" :key="p.order_id">
          <tr>
            <td>
              <code>{{ p.order_id }}</code>
              <span class="muted">
                <br />{{ p.command?.instrument_id || '—' }}
                · {{ terse(p.command?.side, 'SIDE_') }}
                · {{ amount(p.command?.quantity) }}
                · {{ terse(p.command?.order_type, 'ORDER_TYPE_') }}
                <template v-if="p.command?.limit_price"> @ {{ amount(p.command?.limit_price) }}</template>
                <template v-if="p.command?.portfolio_id"><br />{{ p.command.portfolio_id }}</template>
                <template v-if="p.command?.venue"> · {{ p.command.venue }}</template>
              </span>
            </td>

            <td>
              {{ p.proposer || '—' }}
              <span v-if="mine(p)" class="muted"><br />that is you</span>
            </td>

            <!-- THREE READINGS FOR TWO SERVER STATES. "unstated" is not a
                 tidy-up: an entry whose state this client cannot read must not
                 be drawn as clean. -->
            <td :class="state(p) === 'pending' ? 'state-warn' : 'state-bad'">
              <template v-if="state(p) === 'pending'">pending</template>
              <template v-else-if="state(p) === 'refused'"><strong>REFUSED</strong></template>
              <template v-else><strong>STATE NOT STATED</strong></template>
            </td>

            <td :class="expired(p) ? 'state-bad' : ''">
              {{ when(p.expires_at) }}
              <span v-if="expired(p)"><br /><strong>window closed</strong></span>
            </td>

            <!-- IN FULL, NEVER ABBREVIATED. It is the value being signed, and
                 the approve route refuses a signature that carries none. -->
            <td><code class="digest">{{ p.digest || '(none — this entry cannot be approved)' }}</code></td>

            <td class="actions">
              <button v-if="mine(p)" class="quiet" disabled>You proposed this</button>
              <button v-else-if="!p.digest" class="quiet" disabled>No digest</button>
              <button v-else @click="ask(p)">Approve</button>
            </td>
          </tr>

          <!-- THE DETAIL ROW, and it exists only when there is something the
               state cell cannot say in one word: who was refused, why, and when.
               role="alert" because it is a finding, not a caption. -->
          <tr v-if="state(p) !== 'pending'">
            <td colspan="6" class="error" role="alert">
              <template v-if="state(p) === 'refused'">
                <strong v-if="refusedMe(p)">Your signature was refused</strong>
                <strong v-else>A signature was refused</strong>
                — {{ p.last_refused_by || 'an unnamed subject' }},
                {{ when(p.last_refused_at) }}:
                {{ describeRefusal(p.last_refusal_reason) }}
                This order is still held and still needs a second signature.
              </template>
              <template v-else>
                The OMS did not state this entry's approval state ({{ p.state === '' || p.state === undefined ? 'the field was empty' : `it said "${p.state}"` }}).
                Treat it as unknown rather than as untouched work: a signature may already have been
                refused here. Ask whoever runs the OMS before approving.
              </template>
            </td>
          </tr>
        </template>

        <tr v-if="entries.length === 0">
          <td colspan="6">
            <!-- AN EMPTY QUEUE IS AN ANSWER, and this is only reached when the
                 read SUCCEEDED — a failure renders as the alert above. -->
            Nothing is awaiting a second signature. The queue answered; this is
            "nothing is held", not "nothing could be read".
          </td>
        </tr>
      </tbody>
    </table>

    <p v-if="loaded && !me" class="hint">
      This browser has no subject, so this screen cannot tell which of these you proposed.
      The OMS still refuses a self-approval — you would learn it from a refusal instead.
    </p>
  </template>

  <!-- THE CONFIRMATION. Approving releases an order onto the capital path, and
       what is being signed — the terms AND the digest — belongs in front of the
       person at the moment they sign rather than in a row they scrolled past. -->
  <div
    v-if="confirming"
    class="confirm card"
    role="dialog"
    aria-modal="true"
    aria-labelledby="approve-title"
  >
    <h2 id="approve-title">Give the second signature</h2>

    <p class="muted">
      <code>{{ confirming.order_id }}</code><br />
      proposed by <strong>{{ confirming.proposer }}</strong>, {{ when(confirming.proposed_at) }}
    </p>

    <p>
      {{ terse(confirming.command?.side, 'SIDE_') }}
      {{ amount(confirming.command?.quantity) }}
      {{ confirming.command?.instrument_id || '(no instrument)' }}
      ({{ terse(confirming.command?.order_type, 'ORDER_TYPE_') }}<template
        v-if="confirming.command?.limit_price"
      >
        @ {{ amount(confirming.command?.limit_price) }}</template
      >)
    </p>

    <p class="muted">
      Signing covers this digest, and only it:<br />
      <code class="digest">{{ confirming.digest }}</code>
    </p>

    <p v-if="expired(confirming)" class="error" role="alert">
      This proposal's window has closed. The OMS will refuse the signature as expired — the order
      has to be proposed again.
    </p>

    <p v-if="state(confirming) === 'refused'" class="error" role="alert">
      A signature was already refused on this order:
      {{ describeRefusal(confirming.last_refusal_reason) }}
    </p>

    <p class="hint">
      The gateway only PUBLISHES this approval. The OMS decides afterwards and can still refuse it,
      so this screen will report the approval as submitted — never as approved.
    </p>

    <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>

    <div class="actions">
      <button :disabled="busy" @click="sign">{{ busy ? 'Submitting…' : 'Submit approval' }}</button>
      <button class="quiet" :disabled="busy" @click="cancel">Cancel</button>
    </div>
  </div>
</template>
