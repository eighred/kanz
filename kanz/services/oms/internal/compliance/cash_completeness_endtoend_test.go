package compliance

import (
	"context"
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
)

// THE WHOLE CHAIN AGAIN, FOR THE HALF #614 IS ABOUT.
//
// TestAnnouncedCashReachesTheGate proves the NUMBER travels: accounting → the
// cashview → BookSource → a refusal. This proves what the number CONTAINS
// travels with it, all the way into the refusal's evidence.
//
// Without it, an order refused because the fund's dividend was never counted is
// recorded identically to one refused because the fund is out of money, and
// nobody reading the trail afterwards can tell which happened. That is the
// defect: the balance is short by every corporate action (#588), the gate fails
// closed on it, and the refusal was spelled as a spending limit.

func announceWithPosture(t *testing.T, v *cashview.View, total int64, at time.Time,
	produced, unproduced []string) {
	t.Helper()
	msg := &accountingpb.PortfolioCashBalance{
		PortfolioId:   "PF1",
		BaseCurrency:  "USD",
		Total:         &commonpb.Decimal{Coefficient: total},
		AsOf:          timestamppb.New(at),
		KnowledgeTime: timestamppb.New(at),
	}
	if len(produced) > 0 || len(unproduced) > 0 {
		msg.Completeness = &accountingpb.BalanceCompleteness{
			ProducedEntryTypes:   produced,
			UnproducedEntryTypes: unproduced,
		}
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("fold announcement: %v", err)
	}
}

// buyingPowerViolation returns the single violation from a refused decision.
func buyingPowerViolation(t *testing.T, v *cashview.View) map[string]string {
	t.Helper()
	res, err := gateOver(t, v).Evaluate(context.Background(), buyOrder(3))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Allowed {
		t.Fatal("3 x 100 = 300 against an announced 250 was admitted — the gate is not reading " +
			"the announcement at all")
	}
	vs := res.Result.GetViolations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1: %v", len(vs), vs)
	}
	// 250 announced − 300 spent = −50, derived from the definition rather than
	// read back off the implementation.
	if got := vs[0].GetEvidence()["cash_after"]; got != "-50.000000" {
		t.Fatalf("evidence cash_after = %q, want -50.000000 (250 − 3 x 100)", got)
	}
	return vs[0].GetEvidence()
}

// THE REFUSAL NAMES THE FEED THAT WAS NEVER WIRED. This is the posture every
// deployment on this platform is actually in.
func TestARefusalNamesWhatTheBalanceOmits(t *testing.T) {
	v := cashview.New()
	announceWithPosture(t, v, 250, time.Now().UTC(),
		[]string{"cash", "fee", "trade"}, []string{"accrual", "corporate_action"})

	ev := buyingPowerViolation(t, v)
	if got := ev[comp.EvidenceBalanceCompleteness]; got != "incomplete" {
		t.Errorf("evidence %s = %q, want incomplete — accounting announced that two entry types "+
			"have no producer and the gate reported the refusal as a plain spending limit",
			comp.EvidenceBalanceCompleteness, got)
	}
	if got := ev[comp.EvidenceBalanceOmits]; !strings.Contains(got, "corporate_action") {
		t.Errorf("evidence %s = %q, want it to name corporate_action — a reader of this refusal "+
			"has to be able to go and look at the missing feed", comp.EvidenceBalanceOmits, got)
	}
}

// AND AN ANNOUNCEMENT FROM A PRODUCER THAT SAID NOTHING lands on "unstated", not
// on "complete". The two are different states with different next actions, and
// collapsing them is what made this silent in the first place.
func TestARefusalOnAnUnstatedBalanceIsNotReportedAsComplete(t *testing.T) {
	v := cashview.New()
	announceWithPosture(t, v, 250, time.Now().UTC(), nil, nil)

	ev := buyingPowerViolation(t, v)
	if got := ev[comp.EvidenceBalanceCompleteness]; got != "unstated" {
		t.Errorf("evidence %s = %q, want unstated", comp.EvidenceBalanceCompleteness, got)
	}
}

// AND A FULLY FED PRODUCER GETS TO SAY SO, so the estate is not permanently
// stuck reporting every refusal as suspect. Without this arm the chain could
// satisfy both tests above by never reading the field.
func TestARefusalOnACompleteBalanceIsReportedAsALimit(t *testing.T) {
	v := cashview.New()
	announceWithPosture(t, v, 250, time.Now().UTC(),
		[]string{"accrual", "cash", "corporate_action", "fee", "trade"}, nil)

	ev := buyingPowerViolation(t, v)
	if got := ev[comp.EvidenceBalanceCompleteness]; got != "complete" {
		t.Errorf("evidence %s = %q, want complete", comp.EvidenceBalanceCompleteness, got)
	}
	if _, ok := ev[comp.EvidenceBalanceOmits]; ok {
		t.Errorf("a complete balance carried %s", comp.EvidenceBalanceOmits)
	}
}

// AND NONE OF IT MOVES THE LINE. An affordable order is admitted under every
// posture, and an unaffordable one refused under every posture. If marking a
// balance incomplete ever became a refusal of its own it would refuse EVERY
// order on EVERY deployment, since corporate_action is unproduced in all of
// them; if it ever became an admission it would let through orders the fund
// cannot pay for. Both are worse than the bug.
func TestCompletenessMovesNoOrderEitherWay(t *testing.T) {
	postures := map[string][]string{
		"unstated":   nil,
		"complete":   {},
		"incomplete": {"corporate_action"},
	}
	for name, unproduced := range postures {
		v := cashview.New()
		produced := []string{"cash", "fee", "trade"}
		if name == "unstated" {
			produced = nil
		}
		announceWithPosture(t, v, 250, time.Now().UTC(), produced, unproduced)
		g := gateOver(t, v)

		res, err := g.Evaluate(context.Background(), buyOrder(2)) // 200 of 250
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", name, err)
		}
		if !res.Allowed {
			t.Errorf("%s: an affordable order (200 of 250) was REFUSED: %v", name,
				res.Result.GetViolations())
		}
		res, err = g.Evaluate(context.Background(), buyOrder(3)) // 300 of 250
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", name, err)
		}
		if res.Allowed {
			t.Errorf("%s: an unaffordable order (300 of 250) was ADMITTED", name)
		}
	}
}
