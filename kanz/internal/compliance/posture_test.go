package compliance

import (
	"context"
	"log/slog"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// THE GATE MUST REPORT WHAT IT HOLDS, NOT WHAT IT WAS BUILT TO HOLD (#643).
//
// Posture is the mechanism the composition-root repair rests on: a builder that
// returns a gate is only testable because the gate can be asked which seams it
// carries. If Posture reported a constant, every assertion downstream of it —
// the OMS's startup gauge, its builder test — would pass over a gate with no
// classifier and no recorder, which is precisely the state #640 shipped in for
// months.
//
// So each field is proven to move with its seam, individually. A test that built
// one fully wired gate and checked Complete() would pass against a Posture that
// hardcoded true.

type postureBooks struct{}

func (postureBooks) Book(context.Context, string) (*Book, error) { return &Book{}, nil }

type postureMandates struct{}

func (postureMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, bool, error) {
	return nil, false, nil
}

type postureClassifier struct{}

func (postureClassifier) Classify(context.Context, string, time.Time) (Attributes, bool) {
	return Attributes{}, false
}

type postureRecorder struct{}

func (postureRecorder) Record(context.Context, DecisionRecord) error { return nil }

type postureMargin struct{}

func (postureMargin) Margin(string, string, string) (MarginState, bool) {
	return MarginState{}, false
}

// wiredGate is a gate with every seam present — the baseline each case below
// removes exactly one thing from.
func wiredGate() *PreTradeGate {
	return NewPreTradeGate(
		NewEngine(nil), postureBooks{}, postureMandates{}, postureClassifier{}, postureRecorder{},
		slog.Default(), WithMarginSource(postureMargin{}),
	)
}

func TestPostureReportsEverySeamIndividually(t *testing.T) {
	full := wiredGate().Posture()
	if !full.Complete() {
		t.Fatalf("a fully wired gate reports %+v — the baseline every case below subtracts from is "+
			"already wrong", full)
	}

	for _, tc := range []struct {
		name  string
		gate  *PreTradeGate
		field func(GatePosture) bool
	}{
		{
			"books",
			NewPreTradeGate(NewEngine(nil), nil, postureMandates{}, postureClassifier{}, postureRecorder{},
				slog.Default(), WithMarginSource(postureMargin{})),
			func(p GatePosture) bool { return p.Books },
		},
		{
			"mandates",
			NewPreTradeGate(NewEngine(nil), postureBooks{}, nil, postureClassifier{}, postureRecorder{},
				slog.Default(), WithMarginSource(postureMargin{})),
			func(p GatePosture) bool { return p.Mandates },
		},
		{
			"classifier",
			NewPreTradeGate(NewEngine(nil), postureBooks{}, postureMandates{}, nil, postureRecorder{},
				slog.Default(), WithMarginSource(postureMargin{})),
			func(p GatePosture) bool { return p.Classifier },
		},
		{
			"recorder",
			NewPreTradeGate(NewEngine(nil), postureBooks{}, postureMandates{}, postureClassifier{}, nil,
				slog.Default(), WithMarginSource(postureMargin{})),
			func(p GatePosture) bool { return p.Recorder },
		},
		{
			"margin",
			NewPreTradeGate(NewEngine(nil), postureBooks{}, postureMandates{}, postureClassifier{},
				postureRecorder{}, slog.Default()),
			func(p GatePosture) bool { return p.Margin },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.gate.Posture()
			if tc.field(p) {
				t.Errorf("Posture reports a %s on a gate built without one (%+v) — every control "+
					"downstream of this method would then vouch for a seam that is not there",
					tc.name, p)
			}
			if p.Complete() {
				t.Errorf("Complete() = true with no %s", tc.name)
			}
		})
	}
}

// A NIL GATE REPORTS NOTHING WIRED, which is the honest answer for a gate that
// does not exist — and it must not panic, because the caller asking is a startup
// path deciding what to announce.
func TestPostureOfANilGateIsEmptyRatherThanAPanic(t *testing.T) {
	var g *PreTradeGate
	p := g.Posture()
	if p.Books || p.Mandates || p.Classifier || p.Recorder || p.Margin || p.Complete() {
		t.Errorf("a nil gate reports %+v, want nothing wired", p)
	}
}
