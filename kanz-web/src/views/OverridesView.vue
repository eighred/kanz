<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  classify,
  describeApproveFailure,
  describeOverrideQueue,
  overrides,
  signable,
  type PendingOverride,
  type Reading,
} from '../api/overrides'
import { samePerson } from '../api/dualcontrol'
import { formatRational } from '../api/decimal'
import { useSession } from '../stores/session'

// ACT ONE OF DUAL CONTROL (#371, one slice — the pricing-override queue).
//
// A pricing-oversight exception is a mark this platform does not trust. Overriding
// one substitutes a human's price for the book's, into an append-only compliance
// record, and #410 named it the FIRST of the three acts that must take two people
// — chosen because its actor is authenticated (#444) and its trail is already
// append-only. datamaster has implemented the whole workflow since #495/#498 and
// #539 opened the gateway routes, and still nobody could reach it: there was no
// client. An approver's options were curl, or leaving the exception unresolved.
//
// This is the sibling of ApprovalsView. Its patterns are reused deliberately —
// two dual-control screens that behave differently is the divergence this
// repository keeps paying for — and it differs in exactly three places, each
// forced by the server rather than chosen. See ../api/overrides for all three.
//
// # THE FOUR THINGS THIS SCREEN MUST NOT DO
//
// 1. IT MUST NOT RENDER A LAPSED PROPOSAL AS UNTOUCHED WORK, and it must not
//    offer to sign one. #563 made a proposal nobody signed VISIBLE rather than
//    absent, precisely so a proposer could tell "I never proposed it" from
//    "somebody is still considering it" from "it died unsigned". Drawn as
//    pending, that repair is undone at the last step: the approver sees work,
//    presses approve, and datamaster answers 409 — while the exception underneath
//    goes on looking like it has a resolution coming when it has none.
//
// 2. IT MUST NOT SHOW A PRICE IT CANNOT RENDER EXACTLY. The price and the reason
//    ARE the payload the signature covers — datamaster hashes them and re-checks
//    the digest at approval time — so a rounded or coerced price means a second
//    person signed a value nobody read. The wire form is a RATIONAL string and
//    both parseFloat and Number are unavailable on it: see formatRational.
//
// 3. IT MUST NOT COLLAPSE 404 AND 403. Absent is not forbidden (#535).
//
// 4. IT MUST NOT DRAW AN ERROR AS AN EMPTY QUEUE. Nor an empty queue as nothing:
//    on this surface an empty list ALSO means "dual control is not armed here",
//    and the screen says so rather than implying a control is running.
//
// # WHAT IT MAY SAY THAT THE ORDER SCREEN MAY NOT
//
// "APPLIED". datamaster approves over HTTP: the override takes effect inside the
// request and the reply is the updated exception. The order path publishes to the
// bus and answers 202, which is why that screen may only say "submitted". Copying
// its wording here would understate a completed act, and an approver who does not
// believe the override landed will sign it again.

const session = useSession()

const entries = ref<PendingOverride[]>([])
const error = ref('')
const loading = ref(true)
/** loaded is true once the queue has answered at least once, error or not. */
const loaded = ref(false)

/** confirming is the entry awaiting confirmation, or null when none is. */
const confirming = ref<PendingOverride | null>(null)
const actionError = ref('')
const notice = ref('')
const busy = ref(false)

const me = computed(() => session.identity?.subject ?? '')

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    entries.value = await overrides.list()
    error.value = ''
  } catch (e) {
    // AN ERROR IS NEVER AN EMPTY QUEUE. The entries are cleared as well, so a
    // stale list from a previous successful read cannot be mistaken for current
    // — the alert says the queue is unknown and the table must not contradict it.
    entries.value = []
    error.value = describeOverrideQueue(e)
  } finally {
    loading.value = false
    loaded.value = true
  }
}

onMounted(() => void load(true))

/** state reads one entry, denying by default — see classify(). */
function state(p: PendingOverride): Reading {
  return classify(p)
}

/** needsAttention is every entry that is not clean, signable work. */
const needsAttention = computed(() => entries.value.filter((p) => state(p) !== 'pending'))

/** mine is true when this account proposed the entry, by the server's own rule. */
function mine(p: PendingOverride): boolean {
  return samePerson(p.proposer, me.value)
}

/**
 * expired reports that the window closed while this page was open.
 *
 * SEPARATE FROM "lapsed", AND BOTH ARE NEEDED. datamaster decides lapsed at read
 * time; the list is a SNAPSHOT and the deadline keeps moving. An entry that
 * arrived pending can age out while an approver reads it, and saying so before
 * the click beats a 409 after it.
 */
function expired(p: PendingOverride): boolean {
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
 * price renders the chosen price EXACTLY, or null when it cannot be.
 *
 * Never a 0 and never a rounded figure: this is the number being signed for, and
 * a price that cannot be rendered must look unrenderable. The template shows the
 * raw wire value beside a stated anomaly in that case, because the exact string
 * datamaster holds is still evidence even when this client cannot decimalise it.
 */
function price(p: PendingOverride): string | null {
  return formatRational(p.chosen_price)
}

/**
 * rawPrice is shown only when it DIFFERS from the rendered decimal.
 *
 * A price of 130.5 rides the wire as "261/2", and that rational is what the audit
 * trail and the digest are computed over. Showing it alongside lets an approver
 * tie what they read to what was recorded, without putting "130" under "130" on
 * every integer row.
 */
function rawPrice(p: PendingOverride): string {
  const raw = p.chosen_price ?? ''
  return raw && raw !== price(p) ? raw : ''
}

function ask(p: PendingOverride) {
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
    // BOTH IDS. The exception id is checked against the proposal's subject
    // upstream, so approving on the proposal id alone would make the exception on
    // screen decorative — an approver could be shown one and sign for another.
    await overrides.approve(p.exception_id, p.proposal_id)
    // "APPLIED", AND THAT IS NOT THE ORDER SCREEN'S WORDING BY MISTAKE. This
    // route decides synchronously: a 2xx means the override is in the exception's
    // append-only trail, signed by two named people. Saying "submitted" would
    // leave an approver expecting a later confirmation that never comes, and the
    // predictable repair for that is signing it a second time.
    notice.value =
      `Override APPLIED to exception ${p.exception_id}. Two people are now on that record — ` +
      `${p.proposer} proposed it and you approved it. Nothing further is pending on it.`
    confirming.value = null
    await load()
  } catch (e) {
    // Some of these are the CONTROL FIRING rather than a transport failure, and
    // describeApproveFailure says which. Keeping the panel open puts the reason
    // beside what was being attempted.
    actionError.value = describeApproveFailure(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <h1>Pricing overrides</h1>
  <p class="muted">
    Human overrides of a pricing-oversight exception, held for a second signature.
    Approving one writes a price into an append-only compliance record, so it takes
    two different people.
  </p>

  <p v-if="loading">Loading…</p>

  <!-- 404 AND 403 ARRIVE HERE AS DIFFERENT SENTENCES. Absent is not forbidden. -->
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <!-- UNMISSABLE, AND ABOVE THE TABLE. A lapsed proposal is listed rather than
         erased (#563), so it sits in the same list as live work and would
         otherwise be found only by reading every row. -->
    <p v-if="needsAttention.length" class="error" role="alert">
      {{ needsAttention.length }} of {{ entries.length }} entr{{ entries.length === 1 ? 'y' : 'ies' }}
      below can no longer be signed, or carr{{ needsAttention.length === 1 ? 'ies' : 'y' }} a state
      this screen cannot read. A lapsed proposal died unsigned — the exception it names is still
      unresolved and somebody has to propose it again. Do not read these as work waiting for you.
    </p>

    <table>
      <thead>
        <tr>
          <th>Exception</th>
          <th>Proposed by</th>
          <th>State</th>
          <th>Chosen price</th>
          <th>Reason</th>
          <th>Expires</th>
          <th>Action</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="p in entries" :key="p.proposal_id">
          <tr>
            <td>
              <code>{{ p.exception_id || '—' }}</code>
              <span class="muted">
                <br />proposal <code>{{ p.proposal_id || '(none)' }}</code>
                <template v-if="p.act"><br />{{ p.act }}</template>
                <br />proposed {{ when(p.created_at) }}
              </span>
            </td>

            <td>
              {{ p.proposer || '—' }}
              <span v-if="mine(p)" class="muted"><br />that is you</span>
            </td>

            <!-- THREE READINGS FOR TWO SERVER STATES. "unstated" is not a
                 tidy-up: an entry whose state this client cannot read must not
                 be drawn as clean, signable work. -->
            <td :class="state(p) === 'pending' ? 'state-warn' : 'state-bad'">
              <template v-if="state(p) === 'pending'">pending</template>
              <template v-else-if="state(p) === 'lapsed'"><strong>LAPSED</strong></template>
              <template v-else><strong>STATE NOT STATED</strong></template>
            </td>

            <!-- EXACT OR NOT AT ALL. This is the figure being signed for; a
                 rounded one is a signature covering a value nobody read. -->
            <td :class="price(p) === null ? 'state-bad' : ''">
              <template v-if="price(p) !== null">
                <code>{{ price(p) }}</code>
                <span v-if="rawPrice(p)" class="muted"><br />sent as {{ rawPrice(p) }}</span>
              </template>
              <template v-else-if="p.chosen_price">
                <strong>PRICE NOT RENDERABLE</strong>
                <span class="muted"><br />datamaster sent <code>{{ p.chosen_price }}</code></span>
              </template>
              <template v-else><strong>NO PRICE STATED</strong></template>
            </td>

            <!-- IN FULL, NEVER ABBREVIATED: it is hashed into the digest
                 datamaster re-checks, so it is part of what is being signed. -->
            <td>{{ p.reason || '(no reason given)' }}</td>

            <td :class="expired(p) ? 'state-bad' : ''">
              {{ when(p.expires_at) }}
              <span v-if="expired(p) && state(p) === 'pending'"><br /><strong>window closed</strong></span>
            </td>

            <td class="actions">
              <!-- A LAPSED PROPOSAL MUST NOT LOOK APPROVABLE. datamaster refuses
                   it with 409, so a button here would be an offer the server has
                   already decided against. Same for a state nobody stated: the
                   honest answer to "can this be signed?" is that we do not know. -->
              <button v-if="!signable(p)" class="quiet" disabled>
                {{ state(p) === 'lapsed' ? 'Lapsed — cannot be signed' : 'Not signable' }}
              </button>
              <button v-else-if="mine(p)" class="quiet" disabled>You proposed this</button>
              <button v-else @click="ask(p)">Approve</button>
            </td>
          </tr>

          <!-- THE DETAIL ROW, and it exists only when the state cell cannot say
               it in one word. role="alert" because it is a finding. -->
          <tr v-if="state(p) !== 'pending'">
            <td colspan="7" class="error" role="alert">
              <template v-if="state(p) === 'lapsed'">
                <strong>This proposal lapsed unsigned</strong> — its window closed at
                {{ when(p.expires_at) }} and nobody gave the second signature. It is listed rather
                than erased so this is not mistaken for "never proposed" (#563). The override did
                NOT take effect and the exception is still unresolved: it has to be proposed again.
              </template>
              <template v-else>
                datamaster did not state this proposal's state ({{ p.state === '' || p.state === undefined ? 'the field was empty' : `it said "${p.state}"` }}).
                Treat it as unknown rather than as work waiting for you — it may already have
                lapsed. Ask whoever runs datamaster before acting on it.
              </template>
            </td>
          </tr>
        </template>

        <tr v-if="entries.length === 0">
          <td colspan="7">
            <!-- AN EMPTY QUEUE IS AN ANSWER, and this is only reached when the
                 read SUCCEEDED — a failure renders as the alert above.
                 IT IS ALSO AMBIGUOUS, AND SAYING SO IS THE POINT. datamaster
                 answers [] both when nothing is pending and when it holds no
                 proposal store at all, which is what an unarmed deployment looks
                 like from here. A screen that implied a running control would be
                 "nothing configured" and "checked, and fine" looking the same. -->
            No override is awaiting a second signature. The queue answered; this is
            "nothing is held", not "nothing could be read".
            <span class="muted">
              <br />Note that this is also what an UNARMED deployment looks like: with
              DATAMASTER_REQUIRE_DUAL_CONTROL off, an override applies on one person's authority
              and never appears here. An empty queue is not on its own evidence that the control
              is running.
            </span>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-if="loaded && !me" class="hint">
      This browser has no subject, so this screen cannot tell which of these you proposed.
      datamaster still refuses a self-approval — you would learn it from a 403 instead.
    </p>
  </template>

  <!-- THE CONFIRMATION. What is being signed — the price AND the reason, which
       are exactly what datamaster hashes and re-checks — belongs in front of the
       person at the moment they sign, not in a row they scrolled past. -->
  <div
    v-if="confirming"
    class="confirm card"
    role="dialog"
    aria-modal="true"
    aria-labelledby="approve-override-title"
  >
    <h2 id="approve-override-title">Give the second signature</h2>

    <p class="muted">
      exception <code>{{ confirming.exception_id }}</code><br />
      proposal <code>{{ confirming.proposal_id }}</code><br />
      proposed by <strong>{{ confirming.proposer }}</strong>, {{ when(confirming.created_at) }}
    </p>

    <p>
      Override the mark to
      <strong v-if="price(confirming) !== null"><code>{{ price(confirming) }}</code></strong>
      <strong v-else class="error">a price this screen cannot render ({{ confirming.chosen_price || 'none was sent' }})</strong>
      <br />
      because: {{ confirming.reason || '(no reason given)' }}
    </p>

    <p v-if="price(confirming) === null" class="error" role="alert">
      Do not sign this. The price is the value your signature covers and it did not render
      exactly — approving would put a figure into an append-only record that nobody has read.
    </p>

    <p v-if="expired(confirming)" class="error" role="alert">
      This proposal's window has closed. datamaster will refuse the signature with a 409 and the
      override will not apply — it has to be proposed again.
    </p>

    <p class="hint">
      Unlike an order approval, this decides now: a success means the override has been APPLIED to
      the exception and signed by both of you. A refusal — you are the proposer, or the proposal
      expired — comes back on this same request and nothing is written.
    </p>

    <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>

    <div class="actions">
      <button :disabled="busy" @click="sign">{{ busy ? 'Approving…' : 'Approve the override' }}</button>
      <button class="quiet" :disabled="busy" @click="cancel">Cancel</button>
    </div>
  </div>
</template>
