package translate

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/internal/alpha/score"
)

// THE SCORE HAS TO REACH THE FACT, OR IT IS NOT FALSIFIABLE (#416 C2).
//
// A probability that lives only inside the engine can never be scored against
// what happened. These pin that it survives to the audit root, that a malformed
// one is refused before it gets there, and that its absence stays a legitimate
// state rather than becoming a defaulted zero.

func scoredIntent(t *testing.T, s *score.Score) Intent {
	t.Helper()
	in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1),
		signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
	in.Score = s
	return in
}

func mustScore(t *testing.T, p float64) *score.Score {
	t.Helper()
	s, err := score.New(p, 0.005, 4*time.Hour, "obi-v1")
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

// THE CLAIM SURVIVES ONTO THE AUDIT ROOT, whole.
func TestEmit_TheScoreReachesTheFact(t *testing.T) {
	tr, rec := boundedTranslator(t, Qty{})
	if _, err := tr.Emit(context.Background(), scoredIntent(t, mustScore(t, 0.73))); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	facts := rec.facts()
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	got := facts[0].GetAlphaScore()
	if got == nil {
		t.Fatal("the StrategySignal carries no alpha_score — the probability died inside the " +
			"engine, and a score nobody recorded can never be scored against what happened")
	}
	// READ BACK THROUGH THE SAME CONTRACT the calibration path will use, rather
	// than field by field: that is what proves the round trip, not just the write.
	back, err := score.FromProto(got)
	if err != nil {
		t.Fatalf("the recorded score does not read back: %v", err)
	}
	if back.Probability() != 0.73 || back.Threshold() != 0.005 ||
		back.Horizon() != 4*time.Hour || back.ModelID() != "obi-v1" {
		t.Errorf("the claim changed on the way to the FACT: %v", back)
	}
}

// NO SCORE IS A LEGITIMATE STATE, and it must not become a defaulted zero.
//
// A TradingView alert is a rule with no probabilistic claim, and a CLOSE that
// flattens a position forecasts nothing. A zero written here would say "P=0",
// which is a confident bearish call rather than silence.
func TestEmit_AnIntentWithNoScoreRecordsNoScore(t *testing.T) {
	tr, rec := boundedTranslator(t, Qty{})
	if _, err := tr.Emit(context.Background(), scoredIntent(t, nil)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	facts := rec.facts()
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].GetAlphaScore() != nil {
		t.Errorf("an unscored intent recorded a score: %v — absence must stay absence, because "+
			"a defaulted zero reads as a confident short", facts[0].GetAlphaScore())
	}
}

// A MALFORMED SCORE IS REFUSED BEFORE THE FACT IS WRITTEN.
//
// The audit root is immutable: a score that states nothing, recorded once, sits
// there forever as a datapoint no calibration run can interpret. So the refusal
// has to happen at admission, and nothing may be published.
func TestEmit_AMalformedScoreIsRefusedAndNothingIsPublished(t *testing.T) {
	tr, rec := boundedTranslator(t, Qty{})

	// score.Score's fields are unexported, so a caller cannot build a malformed
	// one through New — the only way to hold one is the zero value, which is
	// exactly the case a consumer would otherwise treat as P=0.
	var zero score.Score
	_, err := tr.Emit(context.Background(), scoredIntent(t, &zero))
	if err == nil {
		t.Fatal("an intent carrying the zero Score was admitted")
	}
	if !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("error = %v, want ErrInvalidIntent", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("%d event(s) published on the refusal path — the FACT is immutable, so a bad "+
			"score must never be written at all", len(rec.events))
	}
}
