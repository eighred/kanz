package score

import (
	"math"
	"testing"
	"time"
)

const model = "obi-v1"

func mustScore(t *testing.T, p float64) Score {
	t.Helper()
	s, err := New(p, 0.005, 4*time.Hour, model)
	if err != nil {
		t.Fatalf("New(%v): %v", p, err)
	}
	return s
}

// ===== THE CONTRACT: a score cannot be a bare number =====

// THE ZERO VALUE STATES NOTHING AND IS REFUSED.
//
// Go permits score.Score{} anywhere. A consumer that merely accepted the type
// would read "no claim at all" as a claim of probability zero — a confident
// bearish call — so the zero value has to be rejected explicitly rather than
// assumed away by the constructor's existence.
func TestZeroScoreIsRefused(t *testing.T) {
	var s Score
	if err := s.Validate(); err == nil {
		t.Fatal("the zero Score validated — it would enter a sizing rule as P=0, which is not " +
			"'no opinion', it is a confident short")
	}
	if !s.IsZero() {
		t.Error("IsZero is false for the zero value")
	}
}

// A PROBABILITY OUTSIDE [0,1] IS REFUSED, NOT CLAMPED.
//
// Clamping 1.4 to 1 turns a defective model into a maximally confident one —
// and under "constraints gate, scores size" that is the largest position the
// constraints allow. The bug becomes the biggest trade.
func TestProbabilityOutsideTheUnitIntervalIsRefusedNotClamped(t *testing.T) {
	for _, p := range []float64{-0.1, 1.4, math.NaN()} {
		s, err := New(p, 0.005, time.Hour, model)
		if err == nil {
			t.Errorf("New(%v) succeeded, giving probability %v", p, s.Probability())
		}
		if s.Probability() != 0 || !s.IsZero() {
			t.Errorf("New(%v) returned a non-zero Score alongside its error — a caller that "+
				"ignores err must get something inert, not a clamped claim", p)
		}
	}
}

// A CLAIM WITH NO TARGET IS REFUSED.
func TestAClaimThatStatesNothingIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		threshold float64
		horizon   time.Duration
		model     string
	}{
		{"no horizon", 0.005, 0, model},
		{"negative horizon", 0.005, -time.Hour, model},
		// P(return >= 0) is a coin-flip restatement: a model predicting it can look
		// calibrated while predicting nothing.
		{"zero threshold", 0, time.Hour, model},
		{"infinite threshold", math.Inf(1), time.Hour, model},
		// Calibration is a property of a model VERSION. Unattributed scores pool
		// their record with every other model's.
		{"no model id", 0.005, time.Hour, ""},
	}
	for _, c := range cases {
		if _, err := New(0.7, c.threshold, c.horizon, c.model); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// A NEGATIVE THRESHOLD IS A REAL CLAIM. P(return >= -2%) is what a stop-loss
// model asserts, and refusing it would push that model into inventing a positive
// threshold it does not believe.
func TestANegativeThresholdIsAValidClaim(t *testing.T) {
	if _, err := New(0.9, -0.02, time.Hour, model); err != nil {
		t.Fatalf("P(return >= -2%%) was refused: %v", err)
	}
}

// THE ROUND TRIP PRESERVES THE CLAIM EXACTLY.
//
// The threshold is what calibration BUCKETS BY, so it has to survive the wire
// bit-for-bit at the precision anyone sets it: two thresholds that differ after
// a round trip are two buckets where there should be one.
func TestProtoRoundTripPreservesTheClaim(t *testing.T) {
	for _, th := range []float64{0.005, -0.02, 0.0001, 0.25} {
		want, err := New(0.73, th, 90*time.Minute, model)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FromProto(want.Proto())
		if err != nil {
			t.Fatalf("FromProto: %v", err)
		}
		if got != want {
			t.Errorf("round trip changed the claim: %v -> %v", want, got)
		}
	}
}

// A MALFORMED STORED SCORE IS REFUSED ON THE WAY BACK IN.
//
// The calibration path reads these long after the engine that wrote them is
// gone, so FromProto validates rather than trusts. A persisted 1.4 must not
// quietly become a datapoint claiming certainty.
func TestFromProtoValidatesRatherThanTrusts(t *testing.T) {
	if _, err := FromProto(nil); err == nil {
		t.Error("a nil AlphaScore was accepted")
	}
	p := mustScore(t, 0.7).Proto()
	p.Probability = 1.4
	if _, err := FromProto(p); err == nil {
		t.Error("a stored probability of 1.4 was accepted")
	}
	p2 := mustScore(t, 0.7).Proto()
	p2.ReturnThreshold = nil
	if _, err := FromProto(p2); err == nil {
		t.Error("a stored score with no threshold was accepted — its probability states nothing")
	}
}

// ===== CALIBRATION =====

// perfect builds a PERFECTLY CALIBRATED dataset by construction: for each decile
// probability p, exactly p of the outcomes are hits. Deterministic, so the
// statistics below are exact rather than sampled.
func perfect(t *testing.T) []Outcome {
	t.Helper()
	var out []Outcome
	for k := 0; k < 10; k++ {
		p := (float64(k) + 0.5) / 10 // 0.05, 0.15, ... 0.95
		hits := int(math.Round(p * 100))
		for i := 0; i < 100; i++ {
			out = append(out, Outcome{Score: mustScore(t, p), Hit: i < hits})
		}
	}
	return out
}

// A PERFECTLY CALIBRATED MODEL HAS ZERO CALIBRATION ERROR — and skill well below
// 1, which is the honest part.
//
// Skill 1 would require the model to also be perfectly SHARP (every score 0 or
// 1). A model that says 0.7 and is right 70% of the time is doing everything
// right and still cannot beat 0.33 here. Anyone reading a skill of 0.33 as
// "mediocre" has confused calibration with clairvoyance.
func TestAPerfectlyCalibratedModelHasZeroECE(t *testing.T) {
	r, err := Calibrate(perfect(t), DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	if r.N != 1000 {
		t.Fatalf("N = %d, want 1000", r.N)
	}
	if r.ECE > 1e-12 {
		t.Errorf("ECE = %v on a dataset that is calibrated by construction, want 0", r.ECE)
	}
	if math.Abs(r.BaseRate-0.5) > 1e-12 {
		t.Errorf("BaseRate = %v, want 0.5", r.BaseRate)
	}
	if math.Abs(r.MeanScore-r.BaseRate) > 1e-12 {
		t.Errorf("MeanScore %v != BaseRate %v — a calibrated model asserts, on average, what "+
			"happens", r.MeanScore, r.BaseRate)
	}
	// Brier = mean p(1−p) = 0.1675; skill = 1 − 0.1675/0.25 = 0.33.
	if math.Abs(r.Brier-0.1675) > 1e-9 {
		t.Errorf("Brier = %v, want 0.1675", r.Brier)
	}
	if math.Abs(r.BrierSkill-0.33) > 1e-9 {
		t.Errorf("BrierSkill = %v, want 0.33", r.BrierSkill)
	}
	if r.Resolution <= 0 {
		t.Errorf("Resolution = %v — a model spanning 0.05 to 0.95 is sharp, and a zero here "+
			"would mean the bins all landed on the base rate", r.Resolution)
	}
	if len(r.Bins) != 10 {
		t.Errorf("got %d populated bins, want 10", len(r.Bins))
	}
}

// THE HEADLINE CASE: PERFECTLY CALIBRATED AND COMPLETELY USELESS.
//
// A model that always asserts the base rate has ECE 0 and skill 0. If calibration
// were the only thing reported, it would pass every check and size positions on a
// number that has learned nothing but the base rate. This is why Report carries
// Resolution, and why the doc says reporting one statistic is how a useless model
// gets through.
func TestAModelThatAlwaysPredictsTheBaseRateIsCalibratedAndUseless(t *testing.T) {
	var out []Outcome
	for i := 0; i < 1000; i++ {
		out = append(out, Outcome{Score: mustScore(t, 0.5), Hit: i%2 == 0})
	}
	r, err := Calibrate(out, DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	if r.ECE > 1e-12 {
		t.Errorf("ECE = %v, want 0 — this model IS calibrated", r.ECE)
	}
	if math.Abs(r.Resolution) > 1e-12 {
		t.Errorf("Resolution = %v, want 0 — it never says anything but the base rate, and that "+
			"is the fact a calibration-only report would hide", r.Resolution)
	}
	if math.Abs(r.BrierSkill) > 1e-12 {
		t.Errorf("BrierSkill = %v, want 0 — no better than a constant", r.BrierSkill)
	}
}

// AN OVERCONFIDENT MODEL IS CAUGHT BY ECE AND BY A NEGATIVE SKILL.
//
// It asserts 0.95 and is right half the time. Negative skill is the single most
// useful number in the report: it says the model is WORSE than always predicting
// the base rate, which a raw Brier of 0.4 does not obviously convey.
func TestAnOverconfidentModelIsCaught(t *testing.T) {
	var out []Outcome
	for i := 0; i < 1000; i++ {
		out = append(out, Outcome{Score: mustScore(t, 0.95), Hit: i%2 == 0})
	}
	r, err := Calibrate(out, DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.ECE-0.45) > 1e-9 {
		t.Errorf("ECE = %v, want 0.45 (asserts 0.95, delivers 0.5)", r.ECE)
	}
	if r.BrierSkill >= 0 {
		t.Errorf("BrierSkill = %v, want negative — this model is worse than a constant, and "+
			"that sign is the fact a raw Brier hides", r.BrierSkill)
	}
}

// BRIER ALONE CANNOT TELL THE TWO APART, WHICH IS THE ARGUMENT FOR THE REST.
//
// A calibrated-and-sharp model and a calibrated-and-useless one are different in
// every way that matters to sizing. Their Brier scores are BOTH lower than the
// overconfident model's, and comparing only Brier ranks the useless one above
// the sharp one on some datasets. Asserted here so the reason Report has four
// numbers survives someone deciding to simplify it.
func TestBrierAloneDoesNotSeparateCalibrationFromSharpness(t *testing.T) {
	sharp, err := Calibrate(perfect(t), DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	var flat []Outcome
	for i := 0; i < 1000; i++ {
		flat = append(flat, Outcome{Score: mustScore(t, 0.5), Hit: i%2 == 0})
	}
	useless, err := Calibrate(flat, DefaultBins)
	if err != nil {
		t.Fatal(err)
	}

	// Both are perfectly calibrated: ECE cannot separate them either.
	if sharp.ECE > 1e-12 || useless.ECE > 1e-12 {
		t.Fatalf("both should have ECE 0: %v and %v", sharp.ECE, useless.ECE)
	}
	// RESOLUTION IS THE ONLY ONE THAT DOES.
	if !(sharp.Resolution > useless.Resolution) {
		t.Errorf("resolution did not separate the sharp model (%v) from the useless one (%v) — "+
			"and nothing else in the report can", sharp.Resolution, useless.Resolution)
	}
	if !(sharp.BrierSkill > useless.BrierSkill) {
		t.Errorf("skill did not separate them: %v vs %v", sharp.BrierSkill, useless.BrierSkill)
	}
}

// A SCORE OF EXACTLY 1 LANDS IN THE TOP BIN, not in a bin that does not exist.
func TestAProbabilityOfOneIsBinned(t *testing.T) {
	out := []Outcome{{Score: mustScore(t, 1), Hit: true}, {Score: mustScore(t, 0), Hit: false}}
	r, err := Calibrate(out, DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Bins) != 2 {
		t.Fatalf("got %d bins, want 2 — a p of exactly 1.0 fell outside the table", len(r.Bins))
	}
	if r.ECE != 0 {
		t.Errorf("ECE = %v on two exactly-right calls, want 0", r.ECE)
	}
}

// AN EMPTY OR MALFORMED DATASET IS AN ERROR, not a clean report.
//
// A calibration run over nothing returning a zero-valued Report reads as
// "perfectly calibrated" — the same failure this platform refuses everywhere
// else, arriving through a statistic.
func TestCalibrateRefusesNothingRatherThanReportingPerfection(t *testing.T) {
	if _, err := Calibrate(nil, DefaultBins); err == nil {
		t.Error("calibrating an empty set returned a report — a zero-valued Report reads as " +
			"perfect calibration")
	}
	if _, err := Calibrate([]Outcome{{Hit: true}}, DefaultBins); err == nil {
		t.Error("an outcome carrying the zero Score was calibrated — its P=0 would enter the " +
			"statistics as a confident and wrong call")
	}
}
