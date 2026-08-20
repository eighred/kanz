<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  classify,
  describeApproveFailure,
  describeMandateQueue,
  describeVisibility,
  mandates,
  renderRuleCount,
  renderVersion,
  signable,
  type PendingMandateChange,
  type Reading,
} from '../api/mandates'
import { samePerson } from '../api/dualcontrol'
import { useSession } from '../stores/session'

// ACT TWO OF DUAL CONTROL (#371, one slice — the mandate-change queue).
//
// A mandate is the constraint every order in a portfolio is checked against.
// #410 named changing one the SECOND of the three acts that must take two people,
// and PR #595 built the surface: two separately authenticated requests through
// the gateway, behind authz.Mandate. Before it, cmd/kanz-mandate's two steps both
// ran on one operator's machine under one SVID — the proposal file was a carrier
// and not a signature, and its own doc said so. A unilateral change was
// detectable and never preventable.
//
// And then no client reached it. This screen is the client.
//
// It is the sibling of ApprovalsView (#580) and OverridesView (#590), and its
// patterns are theirs deliberately — three dual-control screens that behave
// differently is the divergence this repository keeps paying for. Every place it
// differs is forced by compliance rather than chosen; ../api/mandates lists them.
//
// # THE FIVE THINGS THIS SCREEN MUST NOT DO
//
// 1. IT MUST NOT RENDER A LAPSED OR UNSTATED PROPOSAL AS UNTOUCHED WORK, and it
//    must not offer to sign one. A proposal nobody signed is LISTED rather than
//    erased (#563) so a proposer can tell "I never proposed it" from "somebody is
//    still considering it" from "it died unsigned". Drawn as pending, that repair
//    is undone at the last step: compliance answers 409, and meanwhile the
//    portfolio goes on looking like its constraint is about to change when it is
//    still governed by the old one.
//
// 2. IT MUST NOT LET A COUNT OF ZERO RULES DISAPPEAR. An empty ruleset is legal
//    and means "governed by a mandate that constrains nothing" — every order
//    passes every check. It is the single most consequential thing this queue can
//    carry, and `v-if="p.rule_count"` hides it. See renderRuleCount.
//
// 3. IT MUST NOT PRINT A NUMBER THAT DID NOT SURVIVE THE WIRE. `version` is a
//    uint64 arriving as a JSON number; above 2^53 JSON.parse has already rounded
//    it. Printing the rounded one as fact, on the field that pins which
//    constraint an order is audited against, is the coercion defect this app
//    keeps finding in new places.
//
// 4. IT MUST NOT COLLAPSE 404 AND 403. Absent is not forbidden (#535). And
//    authz.Mandate is NOT authz.Approve — an order approver who cannot read this
//    queue is seeing the control work, not a misconfiguration.
//
// 5. IT MUST NOT DRAW AN ERROR AS AN EMPTY QUEUE.
//
// # AND THE ONE THING IT MUST SAY THAT NEITHER SIBLING HAS TO
//
// THE QUEUE DOES NOT CARRY THE MANDATE. compliance serves the digest and not the
// rules, and there is no route to fetch one proposal's mandate — propose, approve
// and this queue are all three that exist. So a signatory can see the SHAPE of
// the change (portfolio, mandate, version, rule count, reason, digest) and not
// its CONTENT, and no diff against today's mandate exists anywhere on the
// platform. That is stated in front of the person at the moment they sign, by
// describeVisibility(), rather than left for them to assume otherwise. An
// approver signing a change they cannot read is the forgeable-actor problem in a
// new costume: two real names on the record, neither of whom read the rule.

const session = useSession()

const entries = ref<PendingMandateChange[]>([])
const error = ref('')
const loading = ref(true)
/** loaded is true once the queue has answered at least once, error or not. */
const loaded = ref(false)

/** confirming is the entry awaiting confirmation, or null when none is. */
const confirming = ref<PendingMandateChange | null>(null)
const actionError = ref('')
const notice = ref('')
const busy = ref(false)

const me = computed(() => session.identity?.subject ?? '')

async function load(initial = false) {
  if (initial) loading.value = true
  try {
    entries.value = await mandates.list()
    error.value = ''
  } catch (e) {
    // AN ERROR IS NEVER AN EMPTY QUEUE. The entries are cleared too, so a stale
    // list from a previous successful read cannot be mistaken for current — the
    // alert says the queue is unknown and the table must not contradict it.
    entries.value = []
    error.value = describeMandateQueue(e)
  } finally {
    loading.value = false
    loaded.value = true
  }
}

onMounted(() => void load(true))

/** state reads one entry, denying by default — see classify(). */
function state(p: PendingMandateChange): Reading {
  return classify(p)
}

/** needsAttention is every entry that is not clean, signable work. */
const needsAttention = computed(() => entries.value.filter((p) => state(p) !== 'pending'))

/** mine is true when this account proposed the entry, by the server's own rule. */
function mine(p: PendingMandateChange): boolean {
  return samePerson(p.proposer, me.value)
}

/**
 * expired reports that the window closed while this page was open.
 *
 * SEPARATE FROM "lapsed", AND BOTH ARE NEEDED. compliance decides lapsed at read
 * time; the list is a SNAPSHOT and the deadline keeps moving. An entry that
 * arrived pending can age out while a signatory reads it, and saying so before
 * the click beats a 409 after it.
 */
function expired(p: PendingMandateChange): boolean {
  const t = Date.parse(p.expires_at ?? '')
  return Number.isFinite(t) && t <= Date.now()
}

/** when renders a wire timestamp, or a dash — never a fabricated one. */
function when(ts: string | undefined): string {
  if (!ts) return '—'
  const t = Date.parse(ts)
  return Number.isFinite(t) ? new Date(t).toISOString().replace('T', ' ').replace(/\.\d+Z$/, 'Z') : ts
}

/** rules is the rule count as text, or null when it was not stated — never 0. */
function rules(p: PendingMandateChange): string | null {
  return renderRuleCount(p)
}

/** version is the target version as text, or null when it did not survive. */
function version(p: PendingMandateChange): string | null {
  return renderVersion(p)
}

/** visibility states what is and is not readable about this change. */
function visibility(p: PendingMandateChange): string {
  return describeVisibility(p)
}

function ask(p: PendingMandateChange) {
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
    // BOTH IDS. compliance matches the path's portfolio against the proposal's
    // stored subject, so approving on the proposal id alone would make the
    // portfolio on screen decorative — a signatory could be shown one portfolio
    // and sign for another.
    await mandates.approve(p.portfolio_id, p.proposal_id)
    // "IN FORCE", AND NOT THE ORDER SCREEN'S "submitted". This route decides
    // synchronously and publishes inside the request: a 2xx means the
    // ConfigChanged FACT is on the bus and every OMS and compliance replica arms
    // with this mandate, including one that boots tomorrow. Understating it
    // would leave a signatory expecting a later confirmation that never comes,
    // and the predictable repair for that is signing it a second time.
    notice.value =
      `Mandate change PUBLISHED for portfolio ${p.portfolio_id}. It is now the constraint every ` +
      `order in that portfolio is checked against, and every replica arms with it — including one ` +
      `that boots tomorrow. Two people are on the record: ${p.proposer} proposed it and you ` +
      `approved it. Nothing further is pending on it.`
    confirming.value = null
    await load()
  } catch (e) {
    // Several of these are the CONTROL FIRING rather than a transport failure,
    // and one (502) means the approval was valid and the mandate is NOT in force.
    // describeApproveFailure says which. Keeping the panel open puts the reason
    // beside what was being attempted.
    actionError.value = describeApproveFailure(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <h1>Mandate changes</h1>
  <p class="muted">
    Proposed changes to the constraint set a portfolio's orders are checked against,
    held for a second signature. Approving one publishes it to every replica, so it
    takes two different people.
  </p>

  <p v-if="loading">Loading…</p>

  <!-- 404 AND 403 ARRIVE HERE AS DIFFERENT SENTENCES. Absent is not forbidden,
       and this capability is not the order-approval one. -->
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>

  <template v-else>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <!-- UNMISSABLE, AND ABOVE THE TABLE. A lapsed proposal is listed rather than
         erased (#563), so it sits in the same list as live work and would
         otherwise be found only by reading every row. -->
    <p v-if="needsAttention.length" class="error" role="alert">
      {{ needsAttention.length }} of {{ entries.length }} entr{{ entries.length === 1 ? 'y' : 'ies' }}
      below can no longer be signed, or carr{{ needsAttention.length === 1 ? 'ies' : 'y' }} a state
      this screen cannot read. A lapsed mandate change died unsigned — the portfolio it names is
      still governed by its PREVIOUS mandate and somebody has to propose the change again. Do not
      read these as work waiting for you.
    </p>

    <table>
      <thead>
        <tr>
          <th>Portfolio</th>
          <th>Proposed by</th>
          <th>State</th>
          <th>Becomes</th>
          <th>Reason</th>
          <th>Expires</th>
          <th>Action</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="p in entries" :key="p.proposal_id">
          <tr>
            <td>
              <code>{{ p.portfolio_id || '—' }}</code>
              <span class="muted">
                <br />mandate <code>{{ p.mandate_id || '(none stated)' }}</code>
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

            <!-- THE SHAPE OF THE CHANGE, which is all this surface carries.
                 A ZERO RULE COUNT IS SHOWN AND NAMED, never hidden by a falsy
                 check: a mandate with no rules constrains nothing, and that is
                 the most consequential thing that can be on this queue.
                 A VERSION THAT DID NOT SURVIVE IS SAID, not printed. -->
            <td :class="rules(p) === null || version(p) === null ? 'state-bad' : ''">
              <template v-if="version(p) !== null">
                version <code>{{ version(p) }}</code>
              </template>
              <template v-else>
                <strong>VERSION NOT RENDERABLE</strong>
                <span class="muted"
                  ><br />compliance sent
                  <code>{{ p.version === undefined ? 'nothing' : String(p.version) }}</code
                  >, which did not survive as an exact integer</span
                >
              </template>
              <br />
              <template v-if="rules(p) === '0'">
                <strong>NO RULES</strong>
                <span class="muted"><br />constrains nothing</span>
              </template>
              <template v-else-if="rules(p) !== null">
                {{ rules(p) }} rule{{ rules(p) === '1' ? '' : 's' }}
              </template>
              <template v-else>
                <strong>RULE COUNT NOT STATED</strong>
                <span v-if="p.rule_count !== undefined" class="muted"
                  ><br />compliance sent <code>{{ String(p.rule_count) }}</code></span
                >
              </template>
            </td>

            <!-- IN FULL, NEVER ABBREVIATED: it is hashed into the digest
                 compliance re-checks, so it is part of what is being signed —
                 and on a surface that cannot show the rules it is very nearly
                 the only human-readable statement of what is changing. -->
            <td>{{ p.reason || '(no reason given)' }}</td>

            <td :class="expired(p) ? 'state-bad' : ''">
              {{ when(p.expires_at) }}
              <span v-if="expired(p) && state(p) === 'pending'"><br /><strong>window closed</strong></span>
            </td>

            <td class="actions">
              <!-- A LAPSED PROPOSAL MUST NOT LOOK APPROVABLE. compliance refuses
                   it with 409, so a button here would be an offer the server has
                   already decided against. Same for a state nobody stated: the
                   honest answer to "can this be signed?" is that we do not know. -->
              <button v-if="!signable(p)" class="quiet" disabled>
                {{ state(p) === 'lapsed' ? 'Lapsed — cannot be signed' : 'Not signable' }}
              </button>
              <!-- WITHHELD FROM THE PROPOSER AS A COURTESY, NOT AS A CONTROL.
                   compliance refuses a self-approval whatever this button does,
                   case- and space-insensitively, and logs it at WARN as the
                   control firing. samePerson folds the same way so the disabled
                   button and the eventual refusal say the same thing. -->
              <button v-else-if="mine(p)" class="quiet" disabled>You proposed this</button>
              <button v-else @click="ask(p)">Approve</button>
            </td>
          </tr>

          <!-- THE DETAIL ROW, and it exists only when the state cell cannot say
               it in one word. role="alert" because it is a finding. -->
          <tr v-if="state(p) !== 'pending'">
            <td colspan="7" class="error" role="alert">
              <template v-if="state(p) === 'lapsed'">
                <strong>This mandate change lapsed unsigned</strong> — its window closed at
                {{ when(p.expires_at) }} and nobody gave the second signature. It is listed rather
                than erased so this is not mistaken for "never proposed" (#563). The mandate did NOT
                take effect: portfolio {{ p.portfolio_id }} is still governed by its previous
                constraint set, and the change has to be proposed again.
              </template>
              <template v-else>
                compliance did not state this proposal's state ({{
                  p.state === '' || p.state === undefined ? 'the field was empty' : `it said "${p.state}"`
                }}). Treat it as unknown rather than as work waiting for you — it may already have
                lapsed. Ask whoever runs compliance before acting on it.
              </template>
            </td>
          </tr>
        </template>

        <tr v-if="entries.length === 0">
          <td colspan="7">
            <!-- AN EMPTY QUEUE IS AN ANSWER, and this is only reached when the
                 read SUCCEEDED — a failure renders as the alert above. -->
            No mandate change is awaiting a second signature. The queue answered; this is
            "nothing is held", not "nothing could be read".
            <span class="muted">
              <br />This queue lists live AND lapsed proposals for your tenant, so an empty one also
              means nothing has lapsed unsigned recently. It says nothing about mandate changes made
              before this surface existed: cmd/kanz-mandate could publish one on a single operator's
              authority, and those never appeared here.
            </span>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-if="loaded && !me" class="hint">
      This browser has no subject, so this screen cannot tell which of these you proposed.
      compliance still refuses a self-approval — you would learn it from a 403 instead.
    </p>
  </template>

  <!-- THE CONFIRMATION. What is being signed belongs in front of the person at
       the moment they sign, not in a row they scrolled past — and on this act
       that includes what CANNOT be seen, which is most of it. -->
  <div
    v-if="confirming"
    class="confirm card"
    role="dialog"
    aria-modal="true"
    aria-labelledby="approve-mandate-title"
  >
    <h2 id="approve-mandate-title">Give the second signature</h2>

    <p class="muted">
      portfolio <code>{{ confirming.portfolio_id }}</code
      ><br />
      mandate <code>{{ confirming.mandate_id || '(none stated)' }}</code
      ><br />
      proposal <code>{{ confirming.proposal_id }}</code
      ><br />
      proposed by <strong>{{ confirming.proposer }}</strong
      >, {{ when(confirming.created_at) }}
    </p>

    <p>
      Put a
      <strong v-if="version(confirming) !== null">version {{ version(confirming) }}</strong>
      <strong v-else class="error">version this screen cannot render exactly</strong>
      mandate in force over portfolio <code>{{ confirming.portfolio_id }}</code
      >, carrying
      <strong v-if="rules(confirming) === '0'" class="error">NO RULES AT ALL</strong>
      <strong v-else-if="rules(confirming) !== null"
        >{{ rules(confirming) }} rule{{ rules(confirming) === '1' ? '' : 's' }}</strong
      >
      <strong v-else class="error">an unstated number of rules</strong>
      <br />
      because: {{ confirming.reason || '(no reason given)' }}
    </p>

    <!-- THE THING THIS ACT HAS TO SAY AND ITS SIBLINGS DO NOT. The queue serves
         a digest, not a mandate, and no route exists to expand it. A signatory
         who believes this screen showed them the change is exactly the failure
         two signatures are supposed to prevent. -->
    <p class="error" role="alert">
      <strong>You cannot read this change here.</strong> {{ visibility(confirming) }}
    </p>

    <p class="muted">
      digest your signature covers:<br />
      <code>{{ confirming.digest || '(none served — report this)' }}</code>
    </p>

    <p v-if="!confirming.digest" class="error" role="alert">
      compliance served no digest for this proposal. It re-derives one from what it stores and does
      not need yours, so the approval may still succeed — but with no digest on screen there is
      nothing tying what you just read to what will be published. Report this rather than signing.
    </p>

    <p v-if="rules(confirming) === '0'" class="error" role="alert">
      This mandate carries no rules. Approving it means every order in portfolio
      {{ confirming.portfolio_id }} passes every mandate check from the moment it publishes. That is
      legal and it is not the same as having no mandate — but it is the change most worth being
      certain about, and this screen cannot show you that it is what was intended.
    </p>

    <p v-if="expired(confirming)" class="error" role="alert">
      This proposal's window has closed. compliance will refuse the signature with a 409 and the
      mandate will not publish — it has to be proposed again.
    </p>

    <p class="hint">
      Unlike an order approval, this decides now: a success means the mandate is PUBLISHED and every
      OMS and compliance replica arms with it, including one that boots tomorrow. A refusal — you
      are the proposer, or the proposal expired — comes back on this same request and nothing is
      published.
    </p>

    <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>

    <div class="actions">
      <button :disabled="busy" @click="sign">
        {{ busy ? 'Approving…' : 'Approve the mandate change' }}
      </button>
      <button class="quiet" :disabled="busy" @click="cancel">Cancel</button>
    </div>
  </div>
</template>
