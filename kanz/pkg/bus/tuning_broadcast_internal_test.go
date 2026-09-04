package bus

// THE BROADCAST PATH RESOLVES ITS DELIVERY CONTRACT BY SUBJECT (#1009).
//
// subscribeEphemeral read `tuning := controlTuning` for every subject a broadcast
// was ever taken on, and tuningForSubject — the resolver that gives market.* its
// own contract — was never consulted on that path.
//
// That is not a cosmetic inconsistency. Two production services subscribe to
// market.* through the broadcast path: compliance's price spine
// (COMPLIANCE_PRICE_SUBJECTS = market.*.trade,market.*.quote) and the OMS's. Both
// were getting MaxAckPending 16 where tuning.go reasoned the tick path needs 512,
// and UNBOUNDED redelivery where the same file reasoned that a quote half a
// minute stale must be given up on rather than retried into a risk fold.
//
// The existing guard could not see it: test/arch's
// TestJetStreamConsumersAreExplicitlyTuned states outright that it "deliberately
// does not check the VALUES", and tuning_internal_test.go asserts
// tuningForSubject("market.tick...") == tickTuning — which is true, and is not
// the resolver the broadcast path called.

import "testing"

// A tick is a tick on either path. This is the assertion the old code fails.
func TestTickTuningReachesTheBroadcastPath(t *testing.T) {
	for _, subj := range []string{
		"market.tick.binance.BTC-USDT",
		"market.BINANCE.trade",
		"market.OKX.quote",
	} {
		if got := tuningFor(subj, deliveryBroadcast); got != tickTuning {
			t.Errorf("tuningFor(%q, broadcast) = %+v, want tickTuning %+v.\n\n"+
				"compliance's price spine and the OMS's are BROADCAST subscriptions on market.*. "+
				"Resolving them to the control class gives them MaxAckPending 16 where this file "+
				"reasoned the tick path needs 512, and unbounded redelivery where it reasoned a stale "+
				"tick must be dropped after five attempts.", subj, got, tickTuning)
		}
	}
}

// THE BRAKE SIGNAL KEEPS ITS UNBOUNDED REDELIVERY, and that is the property this
// change must not cost.
//
// controlTuning's MaxDeliver -1 exists so that "I could not read the brake
// signal" can never stop being re-offered and thereby resolve to "carry on
// trading". Every subject that carries one is still control class here.
func TestTheControlClassStillCoversEverySubjectThatCarriesABrakeSignal(t *testing.T) {
	for _, subj := range []string{
		"platform.mode.changed",
		"compliance.mandate.changed.acme.pf-1",
		"risk.position.changed.acme.pf-1.BTC-USD",
		"accounting.cash.balance",
		"wealth.household.valued.acme.h-1",
	} {
		got := tuningFor(subj, deliveryBroadcast)
		if got != controlTuning {
			t.Errorf("tuningFor(%q, broadcast) = %+v, want controlTuning %+v", subj, got, controlTuning)
		}
		if got.MaxDeliver != -1 {
			t.Errorf("tuningFor(%q, broadcast).MaxDeliver = %d, want -1 — a bounded MaxDeliver lets the "+
				"broker stop re-offering a control message the pod could not apply, which is "+
				"\"I could not read the brake signal\" silently becoming \"carry on\"", subj, got.MaxDeliver)
		}
	}
}

// The queue-group path is unchanged: this must be a strictly additive resolution,
// not a re-tuning of every durable in the estate.
func TestTheGroupPathIsUnchangedByTheBroadcastResolution(t *testing.T) {
	if got := tuningFor("market.tick.binance.BTC-USDT", deliveryGroup); got != tickTuning {
		t.Errorf("tick on the group path = %+v, want tickTuning", got)
	}
	for _, subj := range []string{"order.order.submit", "execution.fill.recorded"} {
		if got := tuningFor(subj, deliveryGroup); got != workTuning {
			t.Errorf("tuningFor(%q, group) = %+v, want workTuning %+v", subj, got, workTuning)
		}
		// The same subject on a broadcast is CONTROL, not work — the split that is
		// genuinely a property of the path rather than of the subject.
		if got := tuningFor(subj, deliveryBroadcast); got != controlTuning {
			t.Errorf("tuningFor(%q, broadcast) = %+v, want controlTuning %+v", subj, got, controlTuning)
		}
	}
	// tuningForSubject is the group path by definition, and callers outside this
	// file still use it.
	if tuningForSubject("order.order.submit") != tuningFor("order.order.submit", deliveryGroup) {
		t.Error("tuningForSubject and tuningFor(_, deliveryGroup) disagree — two resolvers again")
	}
}

// NON-VACUITY. Every assertion above compares against one of three package-level
// values; if any two were equal the comparisons would hold while proving nothing
// about which class was chosen.
func TestTheThreeTuningClassesAreDistinct(t *testing.T) {
	for _, pair := range []struct {
		name string
		a, b ConsumerTuning
	}{
		{"tick vs work", tickTuning, workTuning},
		{"tick vs control", tickTuning, controlTuning},
		{"work vs control", workTuning, controlTuning},
	} {
		if pair.a == pair.b {
			t.Fatalf("%s are identical — the resolver cannot be shown to have chosen either", pair.name)
		}
	}
}
