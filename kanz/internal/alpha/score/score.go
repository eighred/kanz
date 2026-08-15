// Package score is the alpha-score contract: a probability that carries the
// claim it is a probability of, and the statistics that judge it (#416 C2).
//
// # The rule this package exists to make unavoidable
//
// #416's owner ruling, 2026-08-12:
//
//	"9.8 out of 10" is unfalsifiable — nobody can say whether a given 9.8 was
//	right, so the number cannot be backtested, attributed, or debugged when it
//	loses money. Every score this platform acts on must state its target.
//
// A bare float cannot be wrong. P(return >= 0.5% within 4h) = 0.7 can: run it a
// thousand times and it should happen about seven hundred. That difference is the
// entire package, and it is enforced by the type rather than by review — Score
// has no exported fields and New refuses a claim that states nothing, so an
// engine cannot emit a naked confidence even by accident.
//
// # Calibration is not accuracy, and confusing them is the expensive mistake
//
// A model that says 0.55 every day for a coin flip is PERFECTLY CALIBRATED and
// useless. A model that says 0.99 and is right 70% of the time is SHARP and
// dangerous — it will be sized as if it were nearly certain. The two are
// different axes, and a platform that sizes positions by score needs both
// measured:
//
//	CALIBRATION   when it says 0.7, does it happen 70% of the time?
//	SHARPNESS     does it ever say anything other than the base rate?
//
// Brier() moves with both and cannot separate them; ECE() isolates calibration;
// Resolution() isolates sharpness. Reporting only one is how a useless model
// passes — which is why Report returns all three and the test helper asserts on
// more than one.
//
// # What this package does NOT do
//
// It does not decide how a score becomes a size. That is #416's E — "constraints
// gate, scores size" — and it belongs where the constraints are, not here. This
// package answers only "is this claim well-formed" and "was it any good".
package score

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Score is a probability together with the event it is about.
//
// UNEXPORTED FIELDS, so the only way to hold one is to have passed New. The
// zero value is inert and Validate refuses it — Go permits score.Score{}
// anywhere, and a consumer that merely accepted the type would read "no claim at
// all" as a claim of zero probability, which is a confident bearish call.
type Score struct {
	probability float64
	threshold   float64
	horizon     time.Duration
	modelID     string
}

// New builds a score for "P(return >= threshold within horizon) = probability",
// attributed to modelID.
//
// threshold is a FRACTION of the current price: 0.005 is "up 0.5%". It may be
// negative — P(return >= -2%) is a real and useful claim, the one a stop-loss
// model makes — but it may not be zero: "P(return >= 0)" is a coin-flip
// restatement that no sizing rule can act on, and accepting it would let a model
// look calibrated by predicting nothing.
func New(probability, threshold float64, horizon time.Duration, modelID string) (Score, error) {
	s := Score{probability: probability, threshold: threshold, horizon: horizon, modelID: modelID}
	if err := s.Validate(); err != nil {
		return Score{}, err
	}
	return s, nil
}

// Validate refuses a claim that states nothing.
func (s Score) Validate() error {
	switch {
	case math.IsNaN(s.probability) || s.probability < 0 || s.probability > 1:
		// NOT CLAMPED. A probability of 1.4 is a defect in the model, and clamping
		// it to 1 turns that defect into maximum confidence — which, under
		// "scores size", is the largest position the constraints allow.
		return fmt.Errorf("score: probability %v is outside [0,1] — refusing rather than "+
			"clamping, because clamping a broken model yields a maximally confident one", s.probability)
	case s.horizon <= 0:
		return fmt.Errorf("score: horizon must be positive — P(up 0.5%%) is near 1 over a year " +
			"and near 0.5 over a second, so a probability without one states nothing")
	case s.threshold == 0:
		return fmt.Errorf("score: return_threshold of 0 is a coin-flip restatement — a model " +
			"that predicts P(return >= 0) can look calibrated while predicting nothing")
	case math.IsNaN(s.threshold) || math.IsInf(s.threshold, 0):
		return fmt.Errorf("score: return_threshold %v is not a finite fraction", s.threshold)
	case s.modelID == "":
		// Calibration is a property of a VERSION. A score with no model id joins to
		// no calibration record, so its history cannot be kept apart from anyone
		// else's.
		return fmt.Errorf("score: model_id is required — calibration is a property of a model " +
			"version, and an unattributed score pools its record with every other model's")
	}
	return nil
}

// Probability, Threshold, Horizon and ModelID read the claim.
func (s Score) Probability() float64   { return s.probability }
func (s Score) Threshold() float64     { return s.threshold }
func (s Score) Horizon() time.Duration { return s.horizon }
func (s Score) ModelID() string        { return s.modelID }
func (s Score) IsZero() bool           { return s == Score{} }
func (s Score) String() string {
	return fmt.Sprintf("P(return>=%.4f within %s)=%.4f [%s]",
		s.threshold, s.horizon, s.probability, s.modelID)
}

// Proto renders the score for the StrategySignal FACT.
//
// The threshold becomes a Decimal because calibration BUCKETS BY IT: two
// thresholds differing in the last bits of a float are two buckets where there
// should be one. Fixed at 1e-6 of a fraction — a tenth of a basis point, finer
// than any threshold anyone sets and coarse enough to group exactly.
func (s Score) Proto() *signalpb.AlphaScore {
	return &signalpb.AlphaScore{
		Probability: s.probability,
		ReturnThreshold: &commonpb.Decimal{
			Coefficient: int64(math.Round(s.threshold * 1e6)),
			Exponent:    -6,
		},
		Horizon: durationpb.New(s.horizon),
		ModelId: s.modelID,
	}
}

// FromProto reads a score back off the FACT, validating it.
//
// It VALIDATES rather than trusting, because a stored score is read by the
// calibration path long after the engine that wrote it is gone — and a
// probability of 1.4 that was somehow persisted must not silently become a
// datapoint claiming certainty.
func FromProto(p *signalpb.AlphaScore) (Score, error) {
	if p == nil {
		return Score{}, fmt.Errorf("score: nil AlphaScore")
	}
	t := p.GetReturnThreshold()
	if t == nil {
		return Score{}, fmt.Errorf("score: AlphaScore carries no return_threshold, so its " +
			"probability states nothing")
	}
	return New(p.GetProbability(), decimalToFloat(t),
		p.GetHorizon().AsDuration(), p.GetModelId())
}

// ===== CALIBRATION =====

// Outcome pairs a score with what actually happened.
//
// Hit is whether the claim CAME TRUE — return >= threshold within horizon —
// resolved by whoever joins scores to realized prices. It is a bool rather than
// the realized return on purpose: the claim is binary, and storing the return
// here would invite a second, quieter definition of "hit" at each call site.
type Outcome struct {
	Score Score
	Hit   bool
}

// Report is what a calibration run answers.
type Report struct {
	// N is the number of outcomes. Reported because every statistic below is
	// meaningless at small n and nothing else says so.
	N int
	// BaseRate is how often the claim actually came true.
	BaseRate float64
	// MeanScore is the average probability asserted. Far from BaseRate means the
	// model is systematically over- or under-confident.
	MeanScore float64
	// Brier is the mean squared error of the probabilities, in [0,1]. LOWER IS
	// BETTER, and it moves with both calibration and sharpness, which is why it
	// cannot be the only number reported.
	Brier float64
	// BrierSkill is Brier measured against the base-rate forecaster: 0 means "no
	// better than always predicting the base rate", 1 is perfect, NEGATIVE means
	// WORSE THAN A CONSTANT. That sign is the single most useful fact in this
	// struct and a raw Brier hides it — 0.09 sounds excellent and is terrible when
	// the base rate makes 0.08 free.
	BrierSkill float64
	// ECE is the expected calibration error: the average gap between asserted
	// probability and observed frequency, weighted by bin population. LOWER IS
	// BETTER. It isolates calibration from sharpness.
	ECE float64
	// Resolution is how far the bins' outcomes spread from the base rate — the
	// sharpness axis. HIGHER IS BETTER, and a model with ECE near zero and
	// resolution near zero is perfectly calibrated and useless: it has learned the
	// base rate and nothing else.
	Resolution float64
	// Bins is the reliability table, for a diagram or an eye.
	Bins []Bin
}

// Bin is one reliability bucket.
type Bin struct {
	Lo, Hi    float64 // the probability range, [Lo,Hi)
	N         int
	MeanScore float64 // average asserted probability in the bin
	Frequency float64 // observed hit rate in the bin
}

// DefaultBins is 10 — deciles, the conventional reliability diagram.
const DefaultBins = 10

// Calibrate scores a model's claims against what happened.
//
// SCORES FROM DIFFERENT CLAIMS MUST NOT BE POOLED, and this does not check that:
// P(up 0.5% in 1h) and P(up 5% in 1d) are different questions whose hit rates
// have no reason to agree, so mixing them produces a number that describes
// neither. Group by (model, threshold, horizon) before calling — Report carries
// no claim fields precisely so a caller cannot believe it did the grouping for
// them.
func Calibrate(outcomes []Outcome, bins int) (Report, error) {
	if len(outcomes) == 0 {
		return Report{}, fmt.Errorf("score: no outcomes to calibrate against")
	}
	if bins <= 0 {
		bins = DefaultBins
	}
	for i, o := range outcomes {
		if err := o.Score.Validate(); err != nil {
			return Report{}, fmt.Errorf("score: outcome %d: %w", i, err)
		}
	}

	r := Report{N: len(outcomes)}
	type acc struct {
		n        int
		sumScore float64
		hits     int
	}
	buckets := make([]acc, bins)
	var sumScore, sumBrier float64
	hits := 0

	for _, o := range outcomes {
		p := o.Score.Probability()
		sumScore += p
		outcome := 0.0
		if o.Hit {
			outcome = 1
			hits++
		}
		sumBrier += (p - outcome) * (p - outcome)

		// p == 1 belongs in the top bin, not in a bin that does not exist.
		idx := int(p * float64(bins))
		if idx >= bins {
			idx = bins - 1
		}
		buckets[idx].n++
		buckets[idx].sumScore += p
		buckets[idx].hits += int(outcome)
	}

	n := float64(len(outcomes))
	r.BaseRate = float64(hits) / n
	r.MeanScore = sumScore / n
	r.Brier = sumBrier / n

	// The base-rate forecaster's Brier is p(1−p) — the irreducible part. Skill is
	// how much of it the model removed.
	ref := r.BaseRate * (1 - r.BaseRate)
	if ref > 0 {
		r.BrierSkill = 1 - r.Brier/ref
	}

	for i, b := range buckets {
		if b.n == 0 {
			continue
		}
		bin := Bin{
			Lo:        float64(i) / float64(bins),
			Hi:        float64(i+1) / float64(bins),
			N:         b.n,
			MeanScore: b.sumScore / float64(b.n),
			Frequency: float64(b.hits) / float64(b.n),
		}
		r.Bins = append(r.Bins, bin)
		w := float64(b.n) / n
		r.ECE += w * math.Abs(bin.MeanScore-bin.Frequency)
		r.Resolution += w * (bin.Frequency - r.BaseRate) * (bin.Frequency - r.BaseRate)
	}
	sort.Slice(r.Bins, func(i, j int) bool { return r.Bins[i].Lo < r.Bins[j].Lo })
	return r, nil
}

// decimalToFloat reconstructs the threshold by DIVIDING by the power of ten
// rather than multiplying by its reciprocal.
//
// The two are not the same, and the difference is one ULP: math.Pow10(-6) is not
// exactly 1e-6, so coefficient*Pow10(-6) rounds twice — once into the reciprocal
// and once into the product. 100*Pow10(-6) is 9.999999999999999e-05, which is
// NOT the 0.0001 that was written. Dividing by the exactly-representable 1e6
// rounds once and returns the original.
//
// THE SIBLING CONVERSIONS ELSEWHERE MULTIPLY, AND THEY ARE FINE. internal/lake/
// dataset and internal/marketdata/indicator both do coefficient*Pow10(exp) for
// prices and features, where a final-ULP difference is irrelevant to an average
// or a standard deviation. It matters HERE and only here because this value is
// an IDENTITY: calibration groups scores by the claim they make, so two
// thresholds that differ in the last bit are two buckets where there should be
// one, and a model's record silently splits in half.
//
// Found by the round-trip test, which is why that test asserts equality of the
// whole Score rather than comparing formatted output — the %.4f in String()
// prints both as "0.0001".
func decimalToFloat(d *commonpb.Decimal) float64 {
	exp := int(d.GetExponent())
	if exp >= 0 {
		return float64(d.GetCoefficient()) * math.Pow10(exp)
	}
	return float64(d.GetCoefficient()) / math.Pow10(-exp)
}

// scoreJSON is the wire form for records that are compared or stored as JSON —
// the backtest's Decision slice is the reproduction contract, and a Score whose
// fields are unexported would serialise as {} and make two different claims
// compare equal.
type scoreJSON struct {
	Probability float64 `json:"probability"`
	Threshold   float64 `json:"return_threshold"`
	HorizonSecs float64 `json:"horizon_seconds"`
	ModelID     string  `json:"model_id"`
}

// MarshalJSON renders the whole claim.
func (s Score) MarshalJSON() ([]byte, error) {
	return json.Marshal(scoreJSON{
		Probability: s.probability, Threshold: s.threshold,
		HorizonSecs: s.horizon.Seconds(), ModelID: s.modelID,
	})
}

// UnmarshalJSON reads a claim back, VALIDATING it — the same argument as
// FromProto: a stored score is read long after the engine that wrote it is gone,
// and a probability of 1.4 must not silently become a datapoint claiming
// certainty.
//
// The zero JSON object is the "no score" case and is left as the zero value
// rather than refused, so a Decision that carries no claim round-trips.
func (s *Score) UnmarshalJSON(b []byte) error {
	var j scoreJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if j == (scoreJSON{}) {
		*s = Score{}
		return nil
	}
	got, err := New(j.Probability, j.Threshold, time.Duration(j.HorizonSecs*float64(time.Second)), j.ModelID)
	if err != nil {
		return err
	}
	*s = got
	return nil
}
