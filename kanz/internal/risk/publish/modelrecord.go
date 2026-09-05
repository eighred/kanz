package publish

import (
	"context"
	"errors"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/pkg/bus"
)

// THE MODEL ARTIFACTS THE ENGINE COMPUTED FROM, not the numbers it computed
// (#1039).
//
// # The state this ends
//
// This platform had a bitemporal spine and it stopped at the ledger and the
// mark. The accounting book replays on both axes and spot marks resolve at an
// as-of out of Postgres — and everything the risk engine DERIVED lived in RAM
// and was thrown away. The discount curve sat in an internal/pit-bounded
// in-memory store whose own package doc says it "cannot be" the book of record
// "because it does not survive to the 4th". The factor model was fitted per
// evaluation and discarded when the call returned. Meanwhile the OUTPUT — the
// VaR, the DV01 — lived 30 days on risk.portfolio.
//
// So a number outlived every input that produced it. It could be quoted and it
// could not be reproduced, challenged or explained, and SR 11-7 model governance
// asks for the model INSTANCE, not the model code.
//
// # Why FACTs and not a table
//
// A calibrated curve and a fitted model are observed-and-committed state, which
// is what a FACT is. Publishing them puts them on the path that already ends in
// durable long-horizon storage — the single-writer NATS to Kafka to lake archive
// — instead of standing up a second retention mechanism beside it, and it makes
// the artifact available to every consumer that already reads this engine's
// output. The point-in-time READ (resolving a curve as of an instant out of
// durable storage rather than out of the map) is the next piece and is
// deliberately not here: honouring an as-of before the inputs are durable
// answers a historical query out of an empty store, and a missing curve is a
// critical unknown that makes a bond book report zero rate risk.
//
// # Neither carries an IdempotencyKey
//
// These are FACTs. A FACT is observed; the key that makes a retry safe belongs
// to the COMMAND that requested the work, and bus.Producer refuses a FACT that
// sets one.
const (
	// EventTypeCurveCalibrated carries one calibrated discount curve, keyed
	// (currency, as_of) — the pair the in-memory store already versions by.
	EventTypeCurveCalibrated = "risk.curve.calibrated"

	// EventTypeFactorModelFitted carries one fitted factor-model instance, keyed
	// (model_id, as_of) — the pair factor.v1 keys every one of its messages by.
	EventTypeFactorModelFitted = "risk.factor.model_fitted"
)

// Payload schema-refs registered with the schema registry (EVT-16).
const (
	schemaRefCalibratedCurve = "domain.v1.CalibratedCurve:1"
	schemaRefFactorSnapshot  = "factor.v1.FactorModelSnapshot:1"
	schemaVersionCurve       = 1
	schemaVersionFactorModel = 1
)

// curveRateExp is the scale calibrated zero rates are carried at on the wire.
//
// TEN DECIMAL PLACES, and it is not cosmetic. A zero rate is a small number
// whose LAST digits are the curve: two curves that agree to 1e-6 still disagree
// on a 30-year DV01 by an amount a desk would notice, and a record of a curve
// exists precisely so somebody can re-derive the number that was published from
// it. The coefficient at this scale is ~5e8 for a 5% rate, nowhere near int64.
const curveRateExp int32 = -10

// CurveMethodMixedStrip is the one method this engine calibrates by: deposits,
// rate futures and par swaps solved sequentially in ascending maturity
// (curve.Calibrate). A closed vocabulary of one, named rather than left empty,
// because a curve that names no method is a curve nobody can re-solve.
const CurveMethodMixedStrip = "mixed_strip_sequential"

// ErrModelNamesNoInstance refuses a factor model carrying no model_id or no
// as_of.
//
// A SENTINEL, BECAUSE THE ONE ARM THAT MATTERS IS NOT COVERED BY ANYTHING ELSE.
// A model with a zero as_of is refused a second time downstream — bus.Producer
// answers "Event.EventTime required" — but a model with a REAL as_of and an
// EMPTY model_id publishes cleanly, with an empty partition key, and lands on a
// topic with 30-day retention as an artifact no measure's provenance can ever
// join to. That case was found by switching this refusal off behind `false &&`
// and watching the publish succeed, and the sentinel is what lets the test
// assert THIS refusal rather than any error at all.
var ErrModelNamesNoInstance = errors.New("publish: factor model names no instance " +
	"(model_id/as_of unset) — nothing could cite it")

// ErrNoCurve refuses a nil curve, for the same reason and with the same shape:
// without it the translation dereferences nil and the failure is a panic in the
// calibration scheduler rather than a reported refusal.
var ErrNoCurve = errors.New("publish: curve is nil")

// EmitCalibratedCurve publishes a CalibratedCurve FACT for the currency's curve
// as it was calibrated at asOf.
//
// Partitioned by CURRENCY, not by (currency, as_of). The ordering that has to
// hold is "this currency's curve versions arrive in the order they were
// calibrated" — a consumer rebuilding a point-in-time store folds them in
// sequence — and keying on the as-of too would put every version of one curve
// on a different partition, which is exactly the ordering that would be lost.
func (p *Publisher) EmitCalibratedCurve(ctx context.Context, currency string, asOf time.Time, c *curve.Curve) error {
	if c == nil {
		return ErrNoCurve
	}
	if currency == "" {
		return errors.New("publish: curve currency is empty")
	}
	return p.producer.Publish(ctx, bus.Event{
		Subject:          EventTypeCurveCalibrated,
		EventType:        EventTypeCurveCalibrated,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionCurve,
		Domain:           "risk",
		EventTime:        asOf,
		PartitionKey:     currency,
		PayloadSchemaRef: schemaRefCalibratedCurve,
		Payload:          ToProtoCalibratedCurve(currency, asOf, c),
	})
}

// EmitFactorModel publishes a FactorModelSnapshot FACT for one fitted model
// instance.
//
// Partitioned by MODEL_ID for the same reason the curve is partitioned by
// currency: successive instances of one model must arrive in fit order, and the
// as-of is what distinguishes them rather than what routes them.
//
// It REFUSES an unnamed model rather than publishing one. A snapshot whose
// model_id or as_of is empty is an artifact nothing can cite — no measure's
// provenance can join to it — so it is not a record, it is noise on a topic
// with long retention. factormodel.Fit stamps both; a model that reaches here
// without them was assembled by something that bypassed Fit.
func (p *Publisher) EmitFactorModel(ctx context.Context, m *factormodel.Model) error {
	if m == nil {
		return ErrModelNamesNoInstance
	}
	if m.ModelID == "" || m.AsOf.IsZero() {
		return ErrModelNamesNoInstance
	}
	return p.producer.Publish(ctx, bus.Event{
		Subject:          EventTypeFactorModelFitted,
		EventType:        EventTypeFactorModelFitted,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionFactorModel,
		Domain:           "risk",
		EventTime:        m.AsOf,
		PartitionKey:     m.ModelID,
		PayloadSchemaRef: schemaRefFactorSnapshot,
		Payload:          ToProtoFactorModelSnapshot(m),
	})
}

// ToProtoCalibratedCurve translates a calibrated curve to its wire shape.
//
// The curve itself becomes a reference.v1.YieldCurve — the estate's one curve
// contract — rather than a second curve shape that would have to be kept in
// step with it. Rates are emitted as ZERO rates under CONTINUOUS compounding
// because that is what the internal curve stores; converting to any other
// convention here would make the record disagree with the object it records.
func ToProtoCalibratedCurve(currency string, asOf time.Time, c *curve.Curve) *domainpb.CalibratedCurve {
	tenors, zeros := c.Tenors(), c.Zeros()
	points := make([]*referencepb.CurvePoint, 0, len(tenors))
	for i, t := range tenors {
		points = append(points, &referencepb.CurvePoint{
			TenorYears: t,
			Rate:       rateDecimal(zeros[i]),
		})
	}
	out := &domainpb.CalibratedCurve{
		Curve: &referencepb.YieldCurve{
			CurveId:       currency,
			AsOf:          timestamppb.New(asOf),
			CurrencyCode:  currency,
			CurveType:     referencepb.CurveType_CURVE_TYPE_ZERO,
			Compounding:   referencepb.Compounding_COMPOUNDING_CONTINUOUS,
			Interpolation: toProtoInterpolation(c.Interp()),
			Points:        points,
		},
		Method: CurveMethodMixedStrip,
	}
	// ABSENT WHEN THE CURVE MAKES NO CLAIM. Curve.StripCoverage returns ok=false
	// for a curve not built from a declared strip, and a zero-valued coverage
	// message would render that as "nothing was configured" — the third value
	// collapsing into a verdict, which is the failure this estate names by cost.
	if cov, ok := c.StripCoverage(); ok {
		out.StripCoverage = toProtoCoverage(cov)
	}
	return out
}

func toProtoInterpolation(i curve.Interpolation) referencepb.Interpolation {
	switch i {
	case curve.LogLinearDF:
		return referencepb.Interpolation_INTERPOLATION_LOG_LINEAR_DF
	case curve.LinearZero:
		return referencepb.Interpolation_INTERPOLATION_LINEAR_ZERO
	default:
		// UNSPECIFIED rather than a guess. A wrong interpolation reproduces
		// different discount factors everywhere between the pillars, so an
		// unrecognised value must read as "this record does not say" and not as
		// the default the reader happens to prefer.
		return referencepb.Interpolation_INTERPOLATION_UNSPECIFIED
	}
}

func toProtoCoverage(cov curve.StripCoverage) *domainpb.CalibrationCoverage {
	missing := make([]*domainpb.MissingQuote, 0, len(cov.Missing))
	for _, m := range cov.Missing {
		missing = append(missing, &domainpb.MissingQuote{InstrumentId: m.InstrumentID, Reason: m.Reason})
	}
	return &domainpb.CalibrationCoverage{
		Configured: uint32(max(cov.Configured, 0)),
		Quoted:     uint32(max(cov.Quoted, 0)),
		Missing:    missing,
	}
}

// ToProtoFactorModelSnapshot translates a fitted model to its wire shape: the
// definition, every instrument's loadings, and the factor covariance, as one
// message. See factor.v1.FactorModelSnapshot for why the three travel together.
func ToProtoFactorModelSnapshot(m *factormodel.Model) *factorpb.FactorModelSnapshot {
	asOf := timestamppb.New(m.AsOf)

	factors := make([]*factorpb.Factor, 0, len(m.Factors))
	for _, f := range m.Factors {
		factors = append(factors, &factorpb.Factor{Name: f.Name, Type: toProtoFactorType(f.Type)})
	}

	exposures := make([]*factorpb.FactorExposure, 0, len(m.Instruments))
	for i, id := range m.Instruments {
		loadings := make([]float64, len(m.Loadings[i]))
		copy(loadings, m.Loadings[i])
		exposures = append(exposures, &factorpb.FactorExposure{
			InstrumentId: id,
			ModelId:      m.ModelID,
			AsOf:         asOf,
			Loadings:     loadings,
			// The map read is deliberate rather than defensive: a universe
			// instrument always has a specific variance, and a zero here would
			// say the instrument carries no idiosyncratic risk — the reading
			// that shrinks a book's measured risk in the direction that makes a
			// limit pass. Any absence is a defect in the estimator, and it is
			// carried onto the wire as the zero the estimator produced rather
			// than hidden behind a substitute.
			SpecificVariance: m.SpecificVar[id],
		})
	}

	k := len(m.Factors)
	values := make([]float64, 0, k*k)
	for i := 0; i < k; i++ {
		for j := 0; j < k; j++ {
			values = append(values, m.FactorCov[i][j])
		}
	}

	return &factorpb.FactorModelSnapshot{
		Model: &factorpb.FactorModel{
			ModelId:    m.ModelID,
			AsOf:       asOf,
			Factors:    factors,
			Estimation: m.ModelID,
		},
		Exposures: exposures,
		Covariance: &factorpb.FactorCovariance{
			ModelId:   m.ModelID,
			AsOf:      asOf,
			Dimension: uint32(k),
			Values:    values,
		},
	}
}

func toProtoFactorType(t factormodel.FactorType) factorpb.FactorType {
	switch t {
	case factormodel.FactorStyle:
		return factorpb.FactorType_FACTOR_TYPE_STYLE
	case factormodel.FactorIndustry:
		return factorpb.FactorType_FACTOR_TYPE_INDUSTRY
	case factormodel.FactorCountry:
		return factorpb.FactorType_FACTOR_TYPE_COUNTRY
	case factormodel.FactorMacro:
		return factorpb.FactorType_FACTOR_TYPE_MACRO
	case factormodel.FactorStatistical:
		return factorpb.FactorType_FACTOR_TYPE_STATISTICAL
	default:
		return factorpb.FactorType_FACTOR_TYPE_UNSPECIFIED
	}
}

// rateDecimal carries a continuously-compounded zero rate at curveRateExp. A
// non-finite rate is not representable and must not become a plausible-looking
// zero: Calibrate has already refused a NaN discount factor, so reaching here
// with one is a defect, and a zero rate is a FLAT curve — the shape that prices
// a bond book at no rate risk at all.
func rateDecimal(r float64) *commonpb.Decimal {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return nil
	}
	return &commonpb.Decimal{
		Coefficient: int64(math.Round(r * math.Pow10(int(-curveRateExp)))),
		Exponent:    curveRateExp,
	}
}
