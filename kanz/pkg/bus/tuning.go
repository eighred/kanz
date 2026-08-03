package bus

import (
	"fmt"
	"strings"
	"time"
)

// ConsumerTuning is the DELIVERY CONTRACT for one JetStream durable: how long
// the broker waits for an ack before it assumes the handler died, how many
// times it will re-offer a message before giving up, and how much work it lets
// one consumer hold un-acked at once.
//
// WHY THIS TYPE EXISTS AT ALL. Until #237 every durable in this estate was
// created with three fields — Durable, AckPolicy, FilterSubject — and nothing
// else, so all three of these ran on JetStream server defaults: AckWait 30s,
// MaxDeliver -1 (redeliver forever), MaxAckPending 1000. None of those numbers
// was chosen; they were simply what was left when nobody set anything. Two of
// them were actively wrong for this platform:
//
//   - MaxDeliver -1 means a message the handler can neither process nor park
//     (the archiver NAKs when its DLQ produce fails) is re-offered every AckWait
//     FOREVER. The only symptom is load and a consumer lag that never clears,
//     and nothing alerts on lag yet (#230).
//
//   - MaxAckPending 1000 is incoherent with AckWait once you notice WHEN the
//     ack clock starts. JetStream marks a message delivered — and starts its
//     AckWait — at FETCH time, not at callback time (this is the same fact
//     Subscribe's drain comment turns on). One bus subject is dispatched by ONE
//     goroutine, so 1000 fetched messages are a queue in front of a single
//     handler: with a 30s AckWait, anything slower than 30ms per message means
//     the tail of that queue times out and is redelivered WHILE the head is
//     still being worked. A backlog therefore manufactured its own duplicate
//     deliveries. The rule this type is sized by is
//
//     MaxAckPending × worst-case per-message handling  <  AckWait
//
//     and it is why MaxAckPending is small on the slow classes and large only
//     where the handler is a cheap in-memory fold.
//
// The zero value is not usable: validate rejects it, and Subscribe never
// constructs one. "Nothing configured" must not be able to look like "checked,
// and fine".
type ConsumerTuning struct {
	// AckWait is how long the broker waits for an ack before redelivering. It
	// must comfortably exceed the WORST-CASE handler for the subjects it covers,
	// including any in-handler retry backoff (RetryConfig), because a redelivery
	// that overtakes a still-running handler is a second concurrent dispatch of
	// the same event — on this platform, a second trade.
	AckWait time.Duration
	// MaxDeliver caps total delivery attempts. -1 is unbounded and is a decision
	// only the control-plane class is allowed to make (see controlTuning).
	MaxDeliver int
	// MaxAckPending caps messages delivered-but-unacked on this consumer. See
	// the type comment for the arithmetic that binds it to AckWait.
	MaxAckPending int
}

// Per-class AckWait values. These are CONSTANTS, not fields of the structs
// below, so that maxTunedAckWait / minTunedAckWait can also be constants and
// the lease ordering in dedup.go can be asserted BY THE COMPILER rather than by
// a comment somebody has to remember to re-read. See dedupClaimLease.
const (
	// workAckWait covers order commands, execution FACTs, accounting, audit,
	// compliance — every subject whose handler can make a network call.
	//
	// Sized from the slowest real path this estate has: the OMS submit handler
	// waits up to defaultClaimWait (5s, services/oms/internal/order/orderlock.go)
	// for the goroutine already working that order, then calls venue.Execute — a
	// REST round-trip to an exchange, which the venue adapters bound at tens of
	// seconds, not milliseconds — and then writes through a row that a concurrent
	// amend may hold FOR UPDATE. 5s + a slow venue + a contended write is the
	// worst case, and 60s leaves headroom over it rather than sitting on top of
	// it. The 30s server default did NOT: a venue that took 26s put the redelivery
	// inside the first dispatch.
	//
	// The cost of raising it is that a HARD crash (no drain, no NAK) leaves that
	// pod's in-flight messages invisible for up to 60s instead of 30s. That cost
	// is paid only on a crash — Subscribe drains and NAKs on an ordinary
	// shutdown, which is the case that actually happens on every rolling deploy.
	workAckWait = 60 * time.Second

	// tickAckWait covers market.> — the 12-partition tick fan-in. Handlers here
	// are in-memory folds into a quote cache, microseconds each, and a tick that
	// is 15s late is worthless anyway. A short AckWait is what lets a genuinely
	// stuck tick consumer redeliver and recover fast.
	tickAckWait = 15 * time.Second

	// controlAckWait covers the ephemeral broadcast consumers (SubscribeBroadcast):
	// the halt FACT, mode changes, mandates. Rare messages, fast handlers; the
	// only reason it is not tickAckWait is that these handlers arm in-process
	// state (a gate, a registry) and a slow arm must not be raced by a redelivery.
	controlAckWait = 30 * time.Second

	// maxTunedAckWait / minTunedAckWait are the extremes over the three classes
	// above. They are hand-written rather than computed because the ordering
	// guards that depend on them are compile-time constant expressions; the unit
	// test TestTunedAckWaitBoundsCoverEveryClass fails if a class is ever added
	// or changed without updating them, so they cannot drift silently.
	maxTunedAckWait = workAckWait
	minTunedAckWait = tickAckWait
)

// NAK BACKOFF — the precondition that makes a finite MaxDeliver safe.
//
// jetstream.Msg.Nak() does NOT honour AckWait or a consumer BackOff: its own
// doc says it "triggers instant redelivery". Subscribe NAKs on every handler
// failure, so a handler that fails instantly against a downed dependency (the
// archiver's Kafka produce returns connection-refused in microseconds) spins:
// deliver, fail, NAK, redeliver, at thousands of iterations per second for as
// long as the dependency is down. That hot loop is the "load" symptom #237
// attributes to unbounded redelivery — and the issue's own wording, "redelivers
// every 30s", understates it by four orders of magnitude.
//
// It also makes a finite MaxDeliver actively DANGEROUS rather than protective.
// A delivery budget is only a budget if the deliveries are spread over time; at
// NAK speed, MaxDeliver 5 is exhausted in under a millisecond and the message is
// terminated by the broker while the outage is still in its first second. The
// existing archiver integration test proves it: TestArchiver_KafkaOutageLosesNothing
// holds Kafka down for 5 SECONDS and asserts nothing is lost. Bounding MaxDeliver
// without this backoff turns that test — and that production guarantee — red.
//
// So redelivery is backed off exponentially from the delivery count the broker
// already tracks: 2s, 4s, 8s, 16s, 32s, then 60s forever. Same shape as
// RetryConfig.Backoff, which handles the in-handler retry one layer up; this one
// covers the layer below, where the message goes back to the broker.
const (
	nakBackoffBase = 2 * time.Second
	nakBackoffMax  = 60 * time.Second
)

// nakDelay returns how long the broker should hold a failed message before
// re-offering it. numDelivered is the broker's own delivery count for this
// message (jetstream MsgMetadata.NumDelivered), 1 on the first delivery; 0 or
// anything unexpected falls back to the base delay rather than to instant
// redelivery, because instant is the failure mode this exists to remove.
func nakDelay(numDelivered uint64) time.Duration {
	d := nakBackoffBase
	for i := uint64(1); i < numDelivered; i++ {
		d *= 2
		if d >= nakBackoffMax {
			return nakBackoffMax
		}
	}
	return d
}

// workTuning is the default for every subject that is not a market tick.
//
// MaxDeliver 64: finite, but LARGE, and the size is the argument.
//
// A redelivery on a work subject is almost never a poison message. The Consumer
// parks a terminal handler failure in the DLQ and ACKS the original on the FIRST
// delivery (consumer.go), so a deterministically-unprocessable event never
// redelivers at all — that is the DLQ's job, not MaxDeliver's. A message that
// DOES come back is therefore an INFRASTRUCTURE failure: the DLQ publish itself
// failed, the pod crashed mid-dispatch, or the $JS.ACK.> grant is missing. Those
// are transient-until-someone-fixes-them, and the right behaviour is to keep
// trying while a human is paged, not to give up on the fifth attempt.
//
// With nakDelay's backoff, 64 deliveries span roughly an hour (2+4+8+16+32s,
// then 60s apiece). That is the real unit here: MaxDeliver is a TIME budget, and
// an hour outlives any dependency outage this platform should survive
// automatically while still bounding the pathology #237 named — a message
// churning against the broker forever with no terminal state.
//
// WHAT HAPPENS WHEN IT IS EXHAUSTED. JetStream stops offering the message and
// emits a $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES advisory. THE MESSAGE IS
// NOT DELETED — it stays in the stream until the stream's max-age (24h for
// EXECUTION), so it is recoverable by replay (EVT-20) — but no consumer will
// see it again on its own. Nothing subscribes to that advisory yet, and that is
// the honest gap in this change: the loud signal exists at the broker and this
// platform does not listen to it. #230 owns the alerting side.
//
// MaxAckPending 32: 32 × the ~1s a slow venue round-trip costs is 32s, inside
// the 60s AckWait, so a backlog cannot time out its own tail. The 1000 default
// would have been 1000s of queue in front of a 60s clock.
var workTuning = ConsumerTuning{
	AckWait:       workAckWait,
	MaxDeliver:    64,
	MaxAckPending: 32,
}

// tickTuning covers market.>. MaxDeliver 5 — deliberately far below the work
// class's 64, and for the opposite reason. A tick's value IS its currency: with
// nakDelay's backoff the fifth delivery lands ~30s after the first, and a quote
// half a minute stale is not something to keep retrying into a risk fold. Giving
// up on it is the correct outcome, not a tolerated one. MaxAckPending 512
// because the handler is a microsecond-scale in-memory fold: 512 × ~10ms is 5s,
// comfortably inside the 15s AckWait, and the throughput headroom is what the
// tick path needs.
var tickTuning = ConsumerTuning{
	AckWait:       tickAckWait,
	MaxDeliver:    5,
	MaxAckPending: 512,
}

// controlTuning covers the ephemeral broadcast consumers.
//
// MaxDeliver IS DELIBERATELY UNBOUNDED HERE, and it is the one place in this
// file where -1 is the right answer. These carry the halt FACT and the trading
// mode. A bounded MaxDeliver means that after N failures to read the brake
// signal the broker stops offering it and the pod goes on running with whatever
// state it last had — "I could not read the brake signal" silently resolving to
// "carry on", which is the exact failure SubscribeBroadcast's own doc forbids.
// A control message that cannot be applied must keep being offered.
//
// It is written as an explicit -1 rather than left unset so that "unbounded
// because that is correct here" and "unbounded because nobody set anything" do
// not look the same in the consumer config.
var controlTuning = ConsumerTuning{
	AckWait:       controlAckWait,
	MaxDeliver:    -1,
	MaxAckPending: 16,
}

// tickSubjectPrefix is the one subject class that gets its own tuning. It is a
// prefix on the domain segment of the subject taxonomy
// (kanz-schemas/docs/subject-taxonomy.md §4), the same axis the stream layout in
// infra/nats/bootstrap-job.yaml is cut on.
const tickSubjectPrefix = "market."

// tuningForSubject resolves the delivery contract for a queue-group durable.
//
// WHY THE TABLE LIVES HERE AND NOT AT EACH COMPOSITION ROOT. Twenty-odd
// binaries build a bus.NATSConfig. A per-service knob would be twenty copies of
// the same three numbers, and the copies are how a fix stops spreading — the
// `secret()` lesson in CLAUDE.md, where 17 services each had their own and 15
// were wrong. Resolving from the subject means a service gets the right
// contract by subscribing, with nothing to wire and nothing to forget.
//
// WHY TWO CLASSES AND NOT SEVEN. Exactly two workloads on this platform have
// genuinely different delivery economics: high-rate, cheap, disposable ticks,
// and low-rate, slow, must-not-duplicate work. A third class would be a number
// invented for a workload nobody has measured. NATSConfig.ConsumerTuning is the
// escape hatch for a service that can show it needs different numbers, and
// DialNATS validates it rather than trusting it.
func tuningForSubject(subject string) ConsumerTuning {
	if strings.HasPrefix(subject, tickSubjectPrefix) {
		return tickTuning
	}
	return workTuning
}

// validate refuses a tuning that cannot work, at construction, rather than
// letting it become a delivery pathology nobody can trace back to a config
// field. The AckWait-vs-claim-lease rule is the one most likely to be got wrong
// later, so it is checked here as well as being arithmetically impossible for
// the built-in classes (see dedupClaimLease).
func (t ConsumerTuning) validate() error {
	if t.AckWait <= 0 {
		return fmt.Errorf("AckWait must be positive, got %s — an unset AckWait silently falls back to the "+
			"JetStream server default (30s), which is the condition #237 exists to remove", t.AckWait)
	}
	if t.MaxDeliver == 0 || t.MaxDeliver < -1 {
		return fmt.Errorf("MaxDeliver must be >= 1, or exactly -1 for a deliberately unbounded control-plane "+
			"consumer, got %d — 0 means the server default (-1, redeliver forever) and would reintroduce the "+
			"poison-message loop", t.MaxDeliver)
	}
	if t.MaxAckPending < 1 {
		return fmt.Errorf("MaxAckPending must be >= 1, got %d — 0 means the server default (1000), which at "+
			"one dispatch goroutine per subject queues far more work in front of the AckWait clock than the "+
			"clock allows", t.MaxAckPending)
	}
	if t.AckWait >= dedupClaimLease {
		return fmt.Errorf("AckWait (%s) must be shorter than the consumer dedup claim lease (%s): the lease is "+
			"what stops a redelivery from dispatching an event whose first copy is STILL RUNNING, and a lease "+
			"that expires before the redelivery arrives inverts the guard — Claim succeeds and the handler runs "+
			"twice concurrently. Raise claimLeaseMargin in dedup.go, or lower this AckWait", t.AckWait, dedupClaimLease)
	}
	return nil
}
