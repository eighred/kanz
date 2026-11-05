package pricing

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	quantpb "github.com/eighred/kanz/kanz-schemas-go/quant/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BondRiskCaptureMeta identifies a local shadow calculation. It grants no
// financial authority and does not assert a historical curve-record version.
type BondRiskCaptureMeta struct {
	CaptureID, InstrumentID, TenantID, SourceRevision string
	AsOf, CapturedAt                                  time.Time
}

// ProjectBondRisk detaches exactly the payment schedule and working zero pillars
// used by the existing pricer. It does not re-query terms or round coupon amounts.
func ProjectBondRisk(terms *referencepb.BondTerms, bond Bond, c *curve.Curve, meta BondRiskCaptureMeta) (*quantpb.BondRiskRequest, error) {
	if terms == nil || c == nil || meta.CaptureID == "" || meta.InstrumentID == "" || meta.TenantID == "" || meta.SourceRevision == "" || meta.AsOf.IsZero() || meta.CapturedAt.IsZero() {
		return nil, errors.New("bond shadow capture identity or inputs missing")
	}
	// The authoritative terms provider bounds frequencies. Refuse unsupported
	// direct callers before the existing backwards schedule could fail to advance.
	if bond.Frequency != 0 && bond.Frequency != 1 && bond.Frequency != 2 && bond.Frequency != 4 && bond.Frequency != 12 {
		return nil, errors.New("bond shadow frequency unsupported")
	}
	if !bond.Maturity.After(bond.Issue) || bond.Maturity.After(bond.Issue.AddDate(100, 0, 0)) {
		return nil, errors.New("bond shadow schedule outside bounded horizon")
	}
	r := &quantpb.BondRiskRequest{
		CaptureId: meta.CaptureID, InstrumentId: meta.InstrumentID, TenantId: meta.TenantID,
		AsOf: timestamppb.New(meta.AsOf), CapturedAt: timestamppb.New(meta.CapturedAt),
		Terms: proto.Clone(terms).(*referencepb.BondTerms), SourceRevision: meta.SourceRevision,
		ScheduleConvention: "go_bond_schedule_v1",
	}
	if r.AsOf.CheckValid() != nil || r.CapturedAt.CheckValid() != nil {
		return nil, errors.New("bond shadow timestamp invalid")
	}
	for _, payment := range bond.PaymentsAfter(meta.AsOf) {
		r.Payments = append(r.Payments, &quantpb.BondPayment{PaymentDate: timestamppb.New(payment.Date), Coupon: payment.Coupon, Redemption: payment.Redemption})
	}
	tenors, zeros := c.Tenors(), c.Zeros()
	if len(tenors) == 0 || len(tenors) > 256 || len(tenors) != len(zeros) || len(r.Payments) > 1201 {
		return nil, errors.New("bond shadow projection exceeds bounds")
	}
	for i, tenor := range tenors {
		r.CurvePoints = append(r.CurvePoints, &quantpb.ResolvedCurvePoint{TenorYears: tenor, ContinuousZero: proto.Float64(zeros[i])})
	}
	switch c.Interp() {
	case curve.LinearZero:
		r.Interpolation = referencepb.Interpolation_INTERPOLATION_LINEAR_ZERO
	case curve.LogLinearDF:
		r.Interpolation = referencepb.Interpolation_INTERPOLATION_LOG_LINEAR_DF
	default:
		return nil, errors.New("bond shadow interpolation unsupported")
	}
	if cov, known := c.StripCoverage(); known {
		if cov.Configured <= 0 || cov.Configured > 256 || cov.Quoted < 0 || cov.Quoted > cov.Configured || len(cov.Missing) > 256 {
			return nil, errors.New("bond shadow coverage invalid")
		}
		r.Coverage = &domainpb.CalibrationCoverage{Configured: uint32(cov.Configured), Quoted: uint32(cov.Quoted)}
		for _, missing := range cov.Missing {
			r.Coverage.Missing = append(r.Coverage.Missing, &domainpb.MissingQuote{InstrumentId: missing.InstrumentID, Reason: missing.Reason})
		}
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
	if err != nil || len(body) > (1<<20)-64 {
		return nil, errors.New("bond shadow request cannot be encoded within bounds")
	}
	digest := sha256.Sum256(body)
	r.CaptureDigest = digest[:]
	return r, nil
}
