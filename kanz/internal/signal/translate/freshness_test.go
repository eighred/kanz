package translate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE BUG THIS CLOSES (#416).
//
// A TradingView alert carries the time the strategy fired it, and nothing
// compared that to now. The 5-minute replay window at the webhook perimeter is
// NONCE DEDUP — it stops the same alert arriving twice and says nothing about a
// first delivery that took thirty minutes. So an alert delayed by a webhook
// retry, a network partition or a paused pod was acted on as if it were
// current: at the size the strategy chose, for a price that has since moved.
//
// These test freshEnough directly rather than through Emit, because Emit needs
// prices, equity, an allocation policy and a publisher — none of which bear on
// whether a decision is too old to act on.
func agedTranslator(t *testing.T, max time.Duration, now time.Time, allowUnstamped bool) *Translator {
	t.Helper()
	return &Translator{opt: Options{
		MaxSignalAge:         max,
		Authority:            boundAuthority(t),
		AllowUnstampedSignal: allowUnstamped,
		Now:                  func() time.Time { return now },
	}}
}

var fired = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func TestASignalOlderThanTheBoundIsRefused(t *testing.T) {
	// The owner's case exactly: a thirty-minute-old alert.
	tr := agedTranslator(t, 2*time.Minute, fired.Add(30*time.Minute), false)

	err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired)})
	if err == nil {
		t.Fatal("a 30-minute-old alert was accepted. It executes at the size the strategy chose " +
			"for a price that has since moved, and nothing on the path notices.")
	}
	if !errors.Is(err, ErrStaleSignal) {
		t.Errorf("err = %v, want ErrStaleSignal", err)
	}
	// The refusal must name both numbers, or an operator cannot tell a delayed
	// delivery from a bound set too tight.
	for _, want := range []string{"30m", "2m"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// A FRESH SIGNAL IS UNAFFECTED. Without this the test above is satisfied by a
// bound that refuses everything, which would be a trading outage wearing the
// shape of a fix.
func TestAFreshSignalPasses(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired.Add(10*time.Second), false)

	if err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired)}); err != nil {
		t.Fatalf("a 10-second-old alert was refused: %v", err)
	}
}

// EXACTLY AT THE BOUND IS STILL ACTED ON. The boundary belongs on the permissive
// side: a bound is a statement about what is too old, and "exactly 2 minutes"
// was chosen as acceptable.
func TestTheBoundaryIsInclusive(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired.Add(2*time.Minute), false)

	if err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired)}); err != nil {
		t.Fatalf("an alert exactly at the bound was refused: %v", err)
	}
}

// A FUTURE-DATED SIGNAL IS REFUSED TOO, and this is the arm that a one-sided
// check would miss: "now minus then" is NEGATIVE for a future timestamp, so it
// passes any "older than X" comparison — forever. A sender whose clock is wrong
// is a sender whose age we cannot judge at all.
func TestAFutureDatedSignalIsRefused(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired, false)

	err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired.Add(time.Hour))})
	if err == nil {
		t.Fatal("an alert timestamped an hour in the FUTURE was accepted. It would never expire, " +
			"so this sender's flow is permanently exempt from the bound.")
	}
	if !errors.Is(err, ErrStaleSignal) {
		t.Errorf("err = %v, want ErrStaleSignal", err)
	}
	if !strings.Contains(err.Error(), "FUTURE") {
		t.Errorf("the refusal does not distinguish a future timestamp from an old one: %v", err)
	}
}

// Small forward skew is normal and must not refuse: clocks differ, and a bound
// that trips on a two-second difference is one an operator turns off.
func TestSmallForwardSkewIsTolerated(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired, false)

	if err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired.Add(3 * time.Second))}); err != nil {
		t.Fatalf("3 seconds of forward clock skew was refused: %v", err)
	}
}

// NO BOUND MEANS NO CHECK — the behaviour every caller had before this existed.
// Adding the field must not silently start refusing live traffic; a deployment
// opts in, and webhook-ingest's config defaults it to a real bound.
func TestAnUnboundedTranslatorChecksNothing(t *testing.T) {
	tr := agedTranslator(t, 0, fired.Add(365*24*time.Hour), false)

	if err := tr.freshEnough(Intent{SourceTS: timestamppb.New(fired)}); err != nil {
		t.Fatalf("a translator with no bound refused a signal: %v", err)
	}
}

// AN UNSTAMPED SIGNAL IS REFUSED, AND THE ZERO VALUE IS WHAT REFUSES IT.
//
// A signal whose age nothing can establish cannot be shown to be current, which
// is the only question the bound asks — so a rule a sender opts out of by
// omitting a field is not a rule.
//
// The zero value mattering is the point of this test. A caller who never thinks
// about the field gets the refusal; the opposite arrangement would mean
// forgetting one line silently exempts a whole deployment from the bound.
//
// Owner ruling, 2026-08-12: no strategies were live, so `ts` was made mandatory
// BEFORE onboarding rather than tightened underneath running traffic.
func TestAnUnstampedSignalIsRefused(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired, false) // false = the ZERO value

	err := tr.freshEnough(Intent{SourceTS: nil})
	if err == nil {
		t.Fatal("an alert with no ts was accepted. Its age cannot be established, so the freshness " +
			"bound has nothing to judge.")
	}
	if !errors.Is(err, ErrStaleSignal) {
		t.Errorf("err = %v, want ErrStaleSignal", err)
	}
	if !strings.Contains(err.Error(), "ts") {
		t.Errorf("the refusal does not tell the sender which field to add: %v", err)
	}
}

// It can be relaxed EXPLICITLY, for onboarding a sender that cannot stamp its
// alerts yet — which is better than disabling the whole bound for one sender.
func TestAnUnstampedSignalPassesWhenExplicitlyAllowed(t *testing.T) {
	tr := agedTranslator(t, 2*time.Minute, fired, true)

	if err := tr.freshEnough(Intent{SourceTS: nil}); err != nil {
		t.Fatalf("AllowUnstampedSignal is set and an unstamped alert was still refused: %v", err)
	}
}
