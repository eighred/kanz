package publish_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/pkg/bus"
)

// THE RECORD OF WHAT PRICED THE BOOK, PROVED TO SURVIVE THE PROCESS (#1039).
//
// These are round-trip tests in the strong sense: the artifact is turned into
// the FACT that will be archived, the FACT is unframed and decoded exactly as a
// lake reader would, and the RECONSTRUCTED artifact is required to answer the
// same questions the original did. A translation that loses a pillar, a
// loading, or the interpolation still publishes a plausible-looking message and
// is only found by re-deriving a number from it.

func fittedModel(t *testing.T, asOf time.Time) *factormodel.Model {
	t.Helper()
	m, err := factormodel.Fit(context.Background(),
		factormodel.Config{Type: factormodel.Statistical, StatFactors: 2, Window: 8},
		[]string{"AAA", "BBB", "CCC"}, asOf,
		factormodel.Providers{Returns: fixedReturns{}})
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	return m
}

// fixedReturns is a deterministic return panel — three instruments with genuinely
// different variance, so the fitted loadings and covariance are non-degenerate
// and a translation that dropped one would be visible.
type fixedReturns struct{}

func (fixedReturns) Returns(_ context.Context, id string, _ time.Time, n int) ([]float64, error) {
	base := map[string]float64{"AAA": 0.010, "BBB": -0.004, "CCC": 0.021}[id]
	out := make([]float64, n)
	for i := range out {
		out[i] = base * math.Sin(float64(i+1)) // deterministic, mean-reverting, unequal
	}
	return out, nil
}

func calibratedCurve(t *testing.T) *curve.Curve {
	t.Helper()
	c, err := curve.Calibrate([]curve.RateQuote{
		{Kind: curve.Deposit, Tenor: 0.25, Value: 0.0431},
		{Kind: curve.Swap, Tenor: 2, Value: 0.0402},
		{Kind: curve.Swap, Tenor: 10, Value: 0.0455},
	}, curve.LogLinearDF)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	return c
}

// decodeCalibratedCurve decodes a risk.curve.calibrated payload exactly as a lake
// reader would — from the bytes, not from the struct the producer held.
func decodeCalibratedCurve(t *testing.T, payload []byte) *domainpb.CalibratedCurve {
	t.Helper()
	var cc domainpb.CalibratedCurve
	if err := proto.Unmarshal(payload, &cc); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	return &cc
}

func TestEmitCalibratedCurve_ReconstructsTheCurveFromTheFact(t *testing.T) {
	pub, cc := newPublisher(t)
	c := calibratedCurve(t)
	asOf := baseTime.Add(9 * time.Hour)

	if err := pub.EmitCalibratedCurve(context.Background(), "USD", asOf, c); err != nil {
		t.Fatalf("EmitCalibratedCurve: %v", err)
	}
	if got := len(cc.sent); got != 1 {
		t.Fatalf("messages=%d want 1", got)
	}
	msg := cc.sent[0]
	if msg.Subject != publish.EventTypeCurveCalibrated {
		t.Errorf("Subject=%q want %q", msg.Subject, publish.EventTypeCurveCalibrated)
	}
	if string(msg.Key) != "USD" {
		t.Errorf("Key=%q want USD — the curve's versions must stay ordered per currency", msg.Key)
	}

	env, payloadBytes, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}
	// A FACT's idempotency key IS its event id; a COMMAND's is supplied.
	if env.IdempotencyKey != env.EventId {
		t.Errorf("IdempotencyKey=%q want event_id=%q (FACT)", env.IdempotencyKey, env.EventId)
	}

	decoded := decodeCalibratedCurve(t, payloadBytes)
	yc := decoded.GetCurve()
	if yc.GetCurrencyCode() != "USD" || yc.GetCurveId() != "USD" {
		t.Errorf("currency=%q curve_id=%q want USD/USD", yc.GetCurrencyCode(), yc.GetCurveId())
	}
	if !yc.GetAsOf().AsTime().Equal(asOf) {
		t.Errorf("as_of=%v want %v — the point-in-time key is half the record", yc.GetAsOf().AsTime(), asOf)
	}
	if yc.GetInterpolation() != referencepb.Interpolation_INTERPOLATION_LOG_LINEAR_DF {
		t.Errorf("interpolation=%v want LOG_LINEAR_DF — a curve rebuilt under the wrong "+
			"interpolation disagrees everywhere between its pillars", yc.GetInterpolation())
	}
	if yc.GetCompounding() != referencepb.Compounding_COMPOUNDING_CONTINUOUS {
		t.Errorf("compounding=%v want CONTINUOUS", yc.GetCompounding())
	}
	if decoded.GetMethod() != publish.CurveMethodMixedStrip {
		t.Errorf("method=%q want %q", decoded.GetMethod(), publish.CurveMethodMixedStrip)
	}

	// THE ROUND TRIP. Rebuild the curve out of the FACT alone and require it to
	// discount the same as the original — that, and not field equality, is what
	// "the curve survived" means.
	wantTenors, wantZeros := c.Tenors(), c.Zeros()
	if len(decoded.GetCurve().GetPoints()) != len(wantTenors) {
		t.Fatalf("points=%d want %d", len(decoded.GetCurve().GetPoints()), len(wantTenors))
	}
	gotTenors := make([]float64, 0, len(wantTenors))
	gotZeros := make([]float64, 0, len(wantZeros))
	for _, p := range decoded.GetCurve().GetPoints() {
		gotTenors = append(gotTenors, p.GetTenorYears())
		r, ok := dec.Float64(p.GetRate())
		if !ok {
			t.Fatalf("pillar rate at %v is not decodable", p.GetTenorYears())
		}
		gotZeros = append(gotZeros, r)
	}
	rebuilt, err := curve.NewZeroCurve(gotTenors, gotZeros, curve.Continuous, curve.LogLinearDF)
	if err != nil {
		t.Fatalf("NewZeroCurve from the published FACT: %v", err)
	}
	for _, tau := range []float64{0.5, 1, 3, 7, 10} {
		want, got := c.Discount(tau), rebuilt.Discount(tau)
		if math.Abs(want-got) > 1e-9 {
			t.Errorf("DF(%.1fy) rebuilt from the FACT = %.12f, original %.12f — the record "+
				"does not reproduce the curve it claims to be", tau, got, want)
		}
	}
}

func TestEmitCalibratedCurve_CurveWithNoStripMakesNoCoverageClaim(t *testing.T) {
	pub, cc := newPublisher(t)
	// Calibrate sets no StripCoverage; only Calibrator.Refresh does, from the
	// source's own report.
	if err := pub.EmitCalibratedCurve(context.Background(), "USD", baseTime, calibratedCurve(t)); err != nil {
		t.Fatalf("EmitCalibratedCurve: %v", err)
	}
	_, payloadBytes, _ := bus.Unframe(cc.sent[0].Body)
	if cov := decodeCalibratedCurve(t, payloadBytes).GetStripCoverage(); cov != nil {
		t.Errorf("strip_coverage=%v want absent — a curve built from no declared strip makes "+
			"no coverage claim, and a zero-valued message would read as 'nothing was configured'", cov)
	}
}

func TestEmitCalibratedCurve_RefusesANilCurve(t *testing.T) {
	pub, cc := newPublisher(t)
	if err := pub.EmitCalibratedCurve(context.Background(), "USD", baseTime, nil); !errors.Is(err, publish.ErrNoCurve) {
		t.Fatalf("err=%v want ErrNoCurve", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("published %d message(s) after refusing", len(cc.sent))
	}
}

func TestEmitFactorModel_ReconstructsTheModelFromTheFact(t *testing.T) {
	pub, cc := newPublisher(t)
	asOf := baseTime.Add(30 * time.Hour)
	m := fittedModel(t, asOf)

	if err := pub.EmitFactorModel(context.Background(), m); err != nil {
		t.Fatalf("EmitFactorModel: %v", err)
	}
	if got := len(cc.sent); got != 1 {
		t.Fatalf("messages=%d want 1", got)
	}
	msg := cc.sent[0]
	if msg.Subject != publish.EventTypeFactorModelFitted {
		t.Errorf("Subject=%q want %q", msg.Subject, publish.EventTypeFactorModelFitted)
	}
	if string(msg.Key) != m.ModelID {
		t.Errorf("Key=%q want %q — successive instances of one model must stay ordered",
			msg.Key, m.ModelID)
	}

	env, payloadBytes, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}
	if env.IdempotencyKey != env.EventId {
		t.Errorf("IdempotencyKey=%q want event_id=%q (FACT)", env.IdempotencyKey, env.EventId)
	}

	var snap factorpb.FactorModelSnapshot
	if err := proto.Unmarshal(payloadBytes, &snap); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if snap.GetModel().GetModelId() != m.ModelID {
		t.Errorf("model_id=%q want %q", snap.GetModel().GetModelId(), m.ModelID)
	}
	if !snap.GetModel().GetAsOf().AsTime().Equal(asOf) {
		t.Errorf("as_of=%v want %v", snap.GetModel().GetAsOf().AsTime(), asOf)
	}
	if len(snap.GetModel().GetFactors()) != len(m.Factors) {
		t.Fatalf("factors=%d want %d", len(snap.GetModel().GetFactors()), len(m.Factors))
	}
	for i, f := range snap.GetModel().GetFactors() {
		if f.GetName() != m.Factors[i].Name {
			t.Errorf("factor[%d]=%q want %q — the factor ORDER is the column order of the "+
				"loadings and the covariance", i, f.GetName(), m.Factors[i].Name)
		}
		if f.GetType() != factorpb.FactorType_FACTOR_TYPE_STATISTICAL {
			t.Errorf("factor[%d].type=%v want STATISTICAL", i, f.GetType())
		}
	}

	// EVERY INSTRUMENT'S LOADINGS AND SPECIFIC VARIANCE. A snapshot short one row
	// reconstructs a model that reports zero systematic AND zero specific risk for
	// that holding — the direction that makes a limit pass.
	if len(snap.GetExposures()) != len(m.Instruments) {
		t.Fatalf("exposures=%d want %d", len(snap.GetExposures()), len(m.Instruments))
	}
	for i, e := range snap.GetExposures() {
		id := m.Instruments[i]
		if e.GetInstrumentId() != id {
			t.Fatalf("exposure[%d].instrument_id=%q want %q", i, e.GetInstrumentId(), id)
		}
		if e.GetModelId() != m.ModelID || !e.GetAsOf().AsTime().Equal(asOf) {
			t.Errorf("exposure[%s] does not name the instance it came from (%q/%v)",
				id, e.GetModelId(), e.GetAsOf().AsTime())
		}
		want, _ := m.Loading(id)
		if len(e.GetLoadings()) != len(want) {
			t.Fatalf("exposure[%s].loadings=%d want %d", id, len(e.GetLoadings()), len(want))
		}
		for k := range want {
			if math.Abs(e.GetLoadings()[k]-want[k]) > 1e-12 {
				t.Errorf("exposure[%s].loadings[%d]=%v want %v", id, k, e.GetLoadings()[k], want[k])
			}
		}
		if math.Abs(e.GetSpecificVariance()-m.SpecificVar[id]) > 1e-15 {
			t.Errorf("exposure[%s].specific_variance=%v want %v", id,
				e.GetSpecificVariance(), m.SpecificVar[id])
		}
	}

	// THE COVARIANCE, ROW-MAJOR AND SQUARE. values[i*k+j] is F[i][j]; a transposed
	// or short matrix produces a different systematic risk in silence.
	k := len(m.Factors)
	if int(snap.GetCovariance().GetDimension()) != k {
		t.Fatalf("covariance.dimension=%d want %d", snap.GetCovariance().GetDimension(), k)
	}
	if len(snap.GetCovariance().GetValues()) != k*k {
		t.Fatalf("covariance.values=%d want %d", len(snap.GetCovariance().GetValues()), k*k)
	}
	for i := 0; i < k; i++ {
		for j := 0; j < k; j++ {
			if got, want := snap.GetCovariance().GetValues()[i*k+j], m.FactorCov[i][j]; math.Abs(got-want) > 1e-15 {
				t.Errorf("covariance[%d][%d]=%v want %v", i, j, got, want)
			}
		}
	}
}

// TestEmitFactorModel_RefusesAModelThatNamesNoInstance asserts THIS refusal, by
// sentinel, and includes the case nothing else catches.
//
// THE MIDDLE CASE IS THE WHOLE TEST. A model with a zero as_of is refused a
// second time by the producer ("Event.EventTime required"), so a test built only
// on the zero value passes with this refusal switched off behind `false &&` — it
// asserts that SOME error happened. A model with a real as_of and an empty
// model_id publishes cleanly and lands on a 30-day topic under an EMPTY
// partition key, as an artifact no measure's provenance can ever join to. That
// is the arm this refusal exists for and the arm the mutation found.
func TestEmitFactorModel_RefusesAModelThatNamesNoInstance(t *testing.T) {
	pub, cc := newPublisher(t)
	for name, m := range map[string]*factormodel.Model{
		"nil":               nil,
		"zero value":        {},
		"dated but unnamed": {AsOf: baseTime},
		"named but undated": {ModelID: "STATISTICAL-2F-8D"},
	} {
		err := pub.EmitFactorModel(context.Background(), m)
		if !errors.Is(err, publish.ErrModelNamesNoInstance) {
			t.Errorf("%s: err=%v want ErrModelNamesNoInstance — an artifact nothing can cite "+
				"must be refused HERE, not left to whatever the envelope validator happens to "+
				"notice", name, err)
		}
	}
	if len(cc.sent) != 0 {
		t.Errorf("published %d message(s) after refusing", len(cc.sent))
	}
}
