package bus

import (
	"strings"
	"testing"
	"time"
)

// allTuningClasses is every built-in delivery contract. A class added to
// tuning.go and not added here is caught by TestTuningClassCountIsPinned below,
// so this list cannot quietly fall behind the table it is meant to police.
var allTuningClasses = map[string]ConsumerTuning{
	"work":    workTuning,
	"tick":    tickTuning,
	"control": controlTuning,
}

// The core invariant of #237, asserted against the real values rather than
// against a comment. dedupClaimLease is what stops a redelivery from dispatching
// an event whose first copy is still running; if any class's AckWait reaches it,
// the redelivery arrives to find the key already free and the handler runs twice
// CONCURRENTLY — a double trade on this platform.
//
// The compile-time assertion in dedup.go covers maxTunedAckWait specifically.
// This covers every class, so a new class with a long AckWait that someone
// forgot to fold into maxTunedAckWait still fails.
func TestEveryTuningClassAckWaitIsShorterThanTheDedupClaimLease(t *testing.T) {
	for name, tune := range allTuningClasses {
		if tune.AckWait >= dedupClaimLease {
			t.Errorf("%s: AckWait %s >= dedup claim lease %s — the in-process dedup claim expires before "+
				"the broker's redelivery lands, so Claim succeeds on the redelivery and the handler runs a "+
				"second time concurrently with the first. This is the exact inversion #237 removed: raise "+
				"claimLeaseMargin, or lower this AckWait",
				name, tune.AckWait, dedupClaimLease)
		}
	}
}

// The OPPOSITE invariant, for the cross-pod deduper. Its claims outlive the pod
// that took them, so a lease reaching AckWait means a pod that crashes holding a
// claim causes its own redelivery to be refused on another pod — and the
// Consumer ACKS a refused claim, so the event is silently, permanently lost. See
// redisClaimLease.
func TestRedisClaimLeaseExpiresBeforeTheEarliestRedelivery(t *testing.T) {
	for name, tune := range allTuningClasses {
		if redisClaimLease >= tune.AckWait {
			t.Errorf("%s: cross-pod claim lease %s >= AckWait %s — a pod that crashes holding a claim leaves "+
				"a ghost claim that is still live when the broker redelivers, the surviving pod is refused, "+
				"and consumer.go acks a message nobody handled. The event is lost with no error anywhere",
				name, redisClaimLease, tune.AckWait)
		}
	}
}

// maxTunedAckWait / minTunedAckWait are hand-written constants (they have to be,
// for the compile-time assertions to be constant expressions). This is what
// stops them drifting from the table they claim to bound.
func TestTunedAckWaitBoundsCoverEveryClass(t *testing.T) {
	for name, tune := range allTuningClasses {
		if tune.AckWait > maxTunedAckWait {
			t.Errorf("%s: AckWait %s exceeds maxTunedAckWait %s — dedupClaimLease is derived from "+
				"maxTunedAckWait, so this class's redeliveries land after the claim has already expired. "+
				"Update maxTunedAckWait in tuning.go", name, tune.AckWait, maxTunedAckWait)
		}
		if tune.AckWait < minTunedAckWait {
			t.Errorf("%s: AckWait %s is below minTunedAckWait %s — redisClaimLease is pinned below "+
				"minTunedAckWait for crash-safety, so this class's redeliveries can land while a dead pod's "+
				"claim is still live. Update minTunedAckWait in tuning.go", name, tune.AckWait, minTunedAckWait)
		}
	}
}

// Non-vacuity for the three tests above: they iterate a map, and a map that
// silently lost its entries would pass all of them. The count is pinned so
// adding a fourth class is a deliberate act that updates allTuningClasses too.
func TestTuningClassCountIsPinned(t *testing.T) {
	if len(allTuningClasses) != 3 {
		t.Fatalf("allTuningClasses has %d entries, expected 3 (work, tick, control) — a class added to "+
			"tuning.go must be added here or the ordering guards silently stop covering it", len(allTuningClasses))
	}
}

// Every class must be a config JetStream will accept and that says something.
func TestEveryTuningClassValidates(t *testing.T) {
	for name, tune := range allTuningClasses {
		if err := tune.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Only the control plane may be unbounded, and it must be EXPLICITLY unbounded.
// A queue-group durable with MaxDeliver -1 is the poison-message loop #237
// found: the archiver NAKs when its DLQ produce fails, and the message then
// redelivers every AckWait forever with no symptom but load.
func TestOnlyTheControlPlaneHasUnboundedRedelivery(t *testing.T) {
	if controlTuning.MaxDeliver != -1 {
		t.Errorf("controlTuning.MaxDeliver = %d, want -1: a bounded MaxDeliver on the halt FACT means the "+
			"broker eventually stops offering the brake signal and the pod carries on with stale state",
			controlTuning.MaxDeliver)
	}
	for _, name := range []string{"work", "tick"} {
		if got := allTuningClasses[name].MaxDeliver; got < 1 {
			t.Errorf("%s: MaxDeliver = %d, want a finite count >= 1 — unbounded redelivery on a work "+
				"consumer is the poison-message loop: a message the handler can neither process nor park "+
				"is re-offered every AckWait forever", name, got)
		}
	}
}

// A finite MaxDeliver is only safe if redelivery is spread over time. Nak()
// triggers INSTANT redelivery, so without this backoff a handler failing fast
// against a downed dependency exhausts the whole delivery budget in under a
// millisecond and the broker terminates the message while the outage is still in
// its first second — turning TestArchiver_KafkaOutageLosesNothing's guarantee
// (5s of Kafka down, nothing lost) into event loss.
func TestNakDelayGrowsAndIsNeverZero(t *testing.T) {
	if got := nakDelay(0); got <= 0 {
		t.Errorf("nakDelay(0) = %s — a zero delay is instant redelivery, the hot loop this exists to "+
			"remove; an unexpected delivery count must fall back to the base delay, not to zero", got)
	}
	prev := time.Duration(0)
	for n := uint64(1); n <= 12; n++ {
		got := nakDelay(n)
		if got <= 0 {
			t.Fatalf("nakDelay(%d) = %s, must be positive", n, got)
		}
		if got > nakBackoffMax {
			t.Fatalf("nakDelay(%d) = %s exceeds the cap %s", n, got, nakBackoffMax)
		}
		if got < prev {
			t.Fatalf("nakDelay(%d) = %s went backwards from %s — the backoff must be monotonic", n, got, prev)
		}
		prev = got
	}
	if nakDelay(1) != nakBackoffBase {
		t.Errorf("first failure should wait nakBackoffBase (%s), got %s", nakBackoffBase, nakDelay(1))
	}
	if nakDelay(2) <= nakDelay(1) {
		t.Error("the backoff never grows — every redelivery would use the base delay, so MaxDeliver would " +
			"still be a budget measured in a handful of seconds rather than in an outage's worth of time")
	}
	if nakDelay(50) != nakBackoffMax {
		t.Errorf("nakDelay(50) = %s, want the cap %s — a runaway backoff would park a recoverable "+
			"message for hours", nakDelay(50), nakBackoffMax)
	}
}

// The delivery budget expressed in TIME, which is the unit that actually
// matters: MaxDeliver × the backoff schedule is how long an outage a consumer
// survives unaided. Asserting the count alone would let a "cleanup" that halves
// nakBackoffMax quietly cut the runway by half with every test still green.
func TestWorkClassSurvivesARealisticOutageBeforeGivingUp(t *testing.T) {
	var runway time.Duration
	for n := uint64(1); n < uint64(workTuning.MaxDeliver); n++ {
		runway += nakDelay(n)
	}
	const wantAtLeast = 30 * time.Minute
	if runway < wantAtLeast {
		t.Errorf("work consumers give up after %s of retrying (MaxDeliver %d over the nakDelay schedule), "+
			"want at least %s — a redelivery on a work subject is an INFRASTRUCTURE failure, not a poison "+
			"message (the DLQ acks those on the first delivery), so giving up early converts a recoverable "+
			"dependency outage into a manual stream replay",
			runway, workTuning.MaxDeliver, wantAtLeast)
	}
	// The tick class is the deliberate opposite: a quote must NOT be retried for
	// an hour, because a stale quote folded into risk is worse than a missing one.
	var tickRunway time.Duration
	for n := uint64(1); n < uint64(tickTuning.MaxDeliver); n++ {
		tickRunway += nakDelay(n)
	}
	if tickRunway >= time.Minute {
		t.Errorf("tick consumers retry for %s — a quote that stale has no business reaching a risk fold", tickRunway)
	}
}

// Market ticks and order commands must not resolve to the same contract — that
// they differ is the reason the resolver exists at all.
func TestTuningForSubjectSeparatesTicksFromWork(t *testing.T) {
	if got := tuningForSubject("market.tick.binance.BTC-USDT"); got != tickTuning {
		t.Errorf("market subject resolved to %+v, want tickTuning %+v", got, tickTuning)
	}
	for _, subj := range []string{
		"order.order.submit",
		"execution.fill.recorded",
		"risk.position.changed",
		"compliance.breach.raised",
	} {
		if got := tuningForSubject(subj); got != workTuning {
			t.Errorf("%s resolved to %+v, want workTuning %+v", subj, got, workTuning)
		}
	}
	if tickTuning == workTuning {
		t.Fatal("tickTuning and workTuning are identical — the resolver is a no-op and every assertion " +
			"above passes vacuously")
	}
}

// validate is the #242-pattern refusal: a bad override must stop the process at
// DialNATS, not become a delivery pathology nobody can trace to a config field.
func TestValidateRefusesTheConfigurationsThatMatter(t *testing.T) {
	good := workTuning
	tests := []struct {
		name string
		mut  func(*ConsumerTuning)
		want string
	}{
		{"unset AckWait", func(c *ConsumerTuning) { c.AckWait = 0 }, "AckWait must be positive"},
		{"unset MaxDeliver", func(c *ConsumerTuning) { c.MaxDeliver = 0 }, "MaxDeliver must be"},
		{"unset MaxAckPending", func(c *ConsumerTuning) { c.MaxAckPending = 0 }, "MaxAckPending must be"},
		{
			"AckWait at the claim lease",
			func(c *ConsumerTuning) { c.AckWait = dedupClaimLease },
			"must be shorter than the consumer dedup claim lease",
		},
		{
			"AckWait past the claim lease",
			func(c *ConsumerTuning) { c.AckWait = dedupClaimLease + time.Hour },
			"must be shorter than the consumer dedup claim lease",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			err := c.validate()
			if err == nil {
				t.Fatalf("validate accepted %+v — a misconfiguration that reaches production silently is "+
					"exactly what this refusal exists to prevent", c)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
	if err := good.validate(); err != nil {
		t.Fatalf("validate rejected the built-in work class: %v", err)
	}
}
