package arch

import (
	"sort"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution/algo"
)

// THE WIRE VOCABULARY AND THE REGISTRY'S VOCABULARY ARE ONE VOCABULARY (#869).
//
// # The claim this holds up
//
// services/oms/internal/order.algoNameOf turns order.v1's ExecutionAlgo into an
// algorithm name by STRIPPING THE ENUM PREFIX rather than by a switch, and its own
// doc gives the reason: a switch is a second list, so adding an enum value and
// implementing the algorithm would still leave the order path refusing it until
// somebody remembered a third edit. Deriving the name makes the two vocabularies
// the same vocabulary.
//
// That argument only holds if the two SETS agree. Derivation guarantees the
// spelling, not the membership — and a mismatch in either direction is silent:
//
//  1. AN ALGORITHM WITH NO ENUM VALUE CANNOT BE SELECTED BY ANY ORDER. It is
//     registered, algo.Lookup resolves it, its own tests pass, and there is no way
//     to ask for it over the wire — the same failure
//     every_algo_is_reachable_test.go catches one layer in, arriving through the
//     schema instead of through the registry.
//
//  2. AN ENUM VALUE NOTHING IMPLEMENTS IS A SCHEMA THAT ADVERTISES AN ALGORITHM
//     THE PLATFORM REFUSES. A client generates the SDK, sees EXECUTION_ALGO_IS,
//     submits it, and is told this build implements something else. The refusal is
//     correct and loud — algo.Lookup never falls back — but the order was
//     unplaceable from the moment the schema said it existed, and nothing in the
//     schema says so.
//
// # Both sets are DERIVED
//
// The registry's set comes from algo.Registered() through algo.Names(). The wire's
// set comes from the generated enum's own descriptor. NEITHER IS WRITTEN HERE, and
// that is the point: a guard carrying a hand-written list of algorithm names is a
// third copy of the thing that drifts, and it would go stale on the same commit
// that broke the property.
//
// # What it cannot check
//
// That the enum's NUMBER is stable (buf breaking owns that), that the algorithm is
// correct (its own tests), or that an order naming it can actually be worked —
// VWAP and POV are refused at admission today because the OMS has no volume
// profile, which is a wiring gap this guard is deliberately blind to. It answers
// one question: can every implemented algorithm be named, and does every name mean
// something.

// enumPrefix is the prefix algoNameOf strips. It is stated once here and asserted
// against the enum's zero value below, so a schema that renamed the enum makes
// this guard fail rather than silently match nothing.
const enumPrefix = "EXECUTION_ALGO_"

// algoWithoutEnumExempt maps a registered algorithm to an argued reason it may
// have no wire enum value, and the issue that retires the entry.
//
// EMPTY. An entry here is a decision that this build contains an execution
// algorithm no order can ask for, which is the dark-capability shape one layer
// down: it compiles, it is registered, and it is unreachable from outside.
var algoWithoutEnumExempt = map[string]string{}

// enumWithoutAlgoExempt maps a wire enum value to an argued reason nothing
// implements it.
//
// ALSO EMPTY. An entry here is a decision that the published schema names an
// algorithm every order using it will be refused for — which is defensible during
// a staged rollout, and is exactly the kind of thing that must be visible in a
// diff rather than discovered by a client.
var enumWithoutAlgoExempt = map[string]string{}

func TestEveryExecutionAlgoNamesAWireEnumValue(t *testing.T) {
	// ===== THE WIRE SET, FROM THE DESCRIPTOR =====
	values := orderpb.ExecutionAlgo(0).Descriptor().Values()
	wire := map[string]bool{}
	unspecified := 0
	for i := range values.Len() {
		full := string(values.Get(i).Name())
		if !strings.HasPrefix(full, enumPrefix) {
			t.Errorf("enum value %q does not carry the %q prefix — algoNameOf strips that prefix "+
				"to derive an algorithm name, so this value resolves to something nobody meant",
				full, enumPrefix)
			continue
		}
		name := strings.TrimPrefix(full, enumPrefix)
		if name == "UNSPECIFIED" {
			// THE ZERO VALUE IS DELIBERATELY UNIMPLEMENTED. An ExecutionSchedule
			// carrying it is refused at admission rather than defaulted, which is
			// #868's ruling and not a gap.
			unspecified++
			continue
		}
		wire[name] = true
	}

	// ===== NON-VACUITY =====
	if values.Len() == 0 {
		t.Fatal("order.v1.ExecutionAlgo has no values — the schema was renamed and both sets " +
			"below are empty, so every comparison passes by matching nothing")
	}
	if unspecified != 1 {
		t.Errorf("found %d UNSPECIFIED values in ExecutionAlgo, want exactly 1 — the zero value is "+
			"what makes an unset algorithm refusable rather than defaulted", unspecified)
	}
	registered := algo.Names()
	if len(registered) == 0 {
		t.Fatal("algo.Registered() is empty — this build can work no order at all")
	}
	if len(wire) == 0 {
		t.Fatal("ExecutionAlgo names no algorithm but UNSPECIFIED — no order on this platform " +
			"can be worked as a schedule")
	}

	// ===== (1) EVERY REGISTERED ALGORITHM CAN BE NAMED ON THE WIRE =====
	var (
		unnamed    []string
		seenExempt = map[string]bool{}
	)
	for _, n := range registered {
		if wire[string(n)] {
			continue
		}
		if reason, ok := algoWithoutEnumExempt[string(n)]; ok {
			seenExempt[string(n)] = true
			t.Logf("%s: exempt — %s", n, reason)
			continue
		}
		unnamed = append(unnamed, string(n))
	}
	if len(unnamed) > 0 {
		sort.Strings(unnamed)
		t.Errorf("%d registered algorithm(s) have no %s value: %s\n\n"+
			"algo.Lookup resolves them and no order can ask for one, because an order names its "+
			"algorithm through order.v1.ExecutionSchedule.algo and nothing else. The code is in the "+
			"binary and unreachable from outside.\n"+
			"Add the enum value to kanz-schemas/proto/order/v1/order.proto, or add an argued entry "+
			"to algoWithoutEnumExempt.",
			len(unnamed), enumPrefix+"<NAME>", strings.Join(unnamed, ", "))
	}
	for name, reason := range algoWithoutEnumExempt {
		if !seenExempt[name] {
			t.Errorf("exemption for %q (%s) matches nothing — the enum value was added or the "+
				"algorithm renamed; remove the entry", name, reason)
		}
	}

	// ===== (2) EVERY WIRE NAME RESOLVES TO AN IMPLEMENTATION =====
	//
	// Through algo.Lookup rather than by comparing the two sets, so this arm also
	// covers the resolution itself: a name present in both sets that Lookup still
	// refuses would pass a set comparison and fail every order.
	var (
		unimplemented []string
		seenEnumEx    = map[string]bool{}
	)
	for name := range wire {
		if _, err := algo.Lookup(algo.Name(name)); err == nil {
			continue
		}
		if reason, ok := enumWithoutAlgoExempt[name]; ok {
			seenEnumEx[name] = true
			t.Logf("%s%s: exempt — %s", enumPrefix, name, reason)
			continue
		}
		unimplemented = append(unimplemented, name)
	}
	if len(unimplemented) > 0 {
		sort.Strings(unimplemented)
		t.Errorf("%d %s value(s) resolve to no implementation: %s\n\n"+
			"A client generating this SDK can name them, and every order that does is refused with "+
			"\"this build implements %v\". The refusal is correct; the schema promising the "+
			"algorithm is not.\n"+
			"Implement and register it in internal/execution/algo, or add an argued entry to "+
			"enumWithoutAlgoExempt.",
			len(unimplemented), enumPrefix+"<NAME>", strings.Join(unimplemented, ", "), registered)
	}
	for name, reason := range enumWithoutAlgoExempt {
		if !seenEnumEx[name] {
			t.Errorf("exemption for %q (%s) matches nothing — the algorithm was implemented or the "+
				"enum value removed; remove the entry", name, reason)
		}
	}
}
