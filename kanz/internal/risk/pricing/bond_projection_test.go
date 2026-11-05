package pricing

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBondProjectionDetachesInputsAndPreservesUnknownProvenance(t *testing.T) {
	at := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	b := Bond{Face: 1000, CouponRate: .05, Frequency: 2, Issue: at, Maturity: at.AddDate(2, 0, 0)}
	terms := &referencepb.BondTerms{FaceValue: &commonpb.Decimal{Coefficient: 1000}, CouponRate: &commonpb.Decimal{Coefficient: 5, Exponent: -2}, IssueDate: timestamppb.New(b.Issue), MaturityDate: timestamppb.New(b.Maturity)}
	c, err := curve.NewZeroCurve([]float64{1, 2}, []float64{.03123456789012345, -.01}, curve.Continuous, curve.LogLinearDF)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ProjectBondRisk(terms, b, c, BondRiskCaptureMeta{CaptureID: "capture", InstrumentID: "bond", TenantID: "tenant", SourceRevision: "test", AsOf: at, CapturedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage != nil || r.CurveAsOf != nil || r.CurveId != nil {
		t.Fatal("projection invented coverage or historical curve provenance")
	}
	if r.CurvePoints[0].ContinuousZero == nil || r.CurvePoints[0].GetContinuousZero() != c.Zeros()[0] || r.Interpolation != referencepb.Interpolation_INTERPOLATION_LOG_LINEAR_DF {
		t.Fatal("projection changed working curve values")
	}
	terms.FaceValue.Coefficient = 1
	terms.MaturityDate.Seconds = 0
	if r.Terms.FaceValue.Coefficient != 1000 || !r.Payments[len(r.Payments)-1].PaymentDate.AsTime().Equal(b.Maturity) {
		t.Fatal("canonical capture aliases input terms")
	}
	want := append([]byte(nil), r.CaptureDigest...)
	r.CaptureDigest = nil
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(wire)
	if !bytes.Equal(want, digest[:]) {
		t.Fatal("capture digest does not bind detached request")
	}
	r.CurvePoints[0].ContinuousZero = proto.Float64(100)
	if c.Zeros()[0] == 100 {
		t.Fatal("capture changed authoritative curve")
	}
}

func TestBondProjectionRefusesBeforeUnboundedScheduling(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := curve.NewZeroCurve([]float64{1}, []float64{.03}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	meta := BondRiskCaptureMeta{CaptureID: "capture", InstrumentID: "bond", TenantID: "tenant", SourceRevision: "test", AsOf: at, CapturedAt: at}
	for _, b := range []Bond{
		{Frequency: 13, CouponRate: .05, Issue: at, Maturity: at.AddDate(1, 0, 0)},
		{Frequency: 12, CouponRate: .05, Issue: at, Maturity: at.AddDate(101, 0, 0)},
		{Frequency: 12, CouponRate: .05, Issue: at, Maturity: at},
	} {
		if _, err := ProjectBondRisk(&referencepb.BondTerms{}, b, c, meta); err == nil {
			t.Fatal("unsupported schedule was projected")
		}
	}
}
