// Command bond-risk-conformance exports captured canonical requests and the
// existing Go result. It has no live providers, credentials or pricing authority.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/termsource"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	quantpb "github.com/eighred/kanz/kanz-schemas-go/quant/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type result struct {
	DirtyPrice float64 `json:"dirty_price"`
	DV01       float64 `json:"dv01"`
	Duration   float64 `json:"effective_duration"`
	Convexity  float64 `json:"convexity"`
}
type tolerance struct {
	Absolute float64 `json:"absolute"`
	Relative float64 `json:"relative"`
}

// Fixed before seeing any Rust results. Convexity's central difference loses
// precision through subtraction at a 1bp bump; do not tune these to a candidate.
var tolerances = map[string]tolerance{"dirty_price": {1e-9, 1e-11}, "dv01": {1e-9, 1e-9}, "effective_duration": {1e-7, 1e-9}, "convexity": {2e-5, 1e-8}}

type entry struct {
	Name     string `json:"name"`
	Request  []byte `json:"request"`
	Expected result `json:"expected"`
}
type corpus struct {
	Version    int                  `json:"version"`
	Tolerances map[string]tolerance `json:"tolerances"`
	Cases      []entry              `json:"cases"`
}
type payment struct {
	Date   string
	Amount float64
	Zero   float64
}
type fixture struct {
	name          string
	terms         *referencepb.BondTerms
	settle        time.Time
	tenors, zeros []float64
	interp        curve.Interpolation
	payments      []payment
}

func date(s string) time.Time {
	v, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return v
}
func midnight(s string) time.Time { return date(s + "T00:00:00Z") }
func terms(issue, maturity string, coupon int64, frequency referencepb.PaymentFrequency) *referencepb.BondTerms {
	return &referencepb.BondTerms{FaceValue: &commonpb.Decimal{Coefficient: 1000}, CouponRate: &commonpb.Decimal{Coefficient: coupon, Exponent: -4}, CouponFrequency: frequency, IssueDate: timestamppb.New(midnight(issue)), MaturityDate: timestamppb.New(midnight(maturity)), DayCount: referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_365F, CurrencyCode: "USD"}
}

func fixtures() []fixture {
	annual := referencepb.PaymentFrequency_PAYMENT_FREQUENCY_ANNUAL
	zero := referencepb.PaymentFrequency_PAYMENT_FREQUENCY_UNSPECIFIED
	flat := func(name string, t *referencepb.BondTerms, settle string, z float64, p []payment) fixture {
		for i := range p {
			p[i].Zero = z
		}
		return fixture{name, t, midnight(settle), []float64{0.25, 30}, []float64{z, z}, curve.LinearZero, p}
	}
	out := []fixture{
		flat("zero-flat", terms("2024-01-01", "2027-01-01", 0, zero), "2025-01-01", .03, []payment{{Date: "2027-01-01", Amount: 1000}}),
		flat("zero-negative-rate", terms("2024-01-01", "2027-01-01", 0, zero), "2025-01-01", -.01, []payment{{Date: "2027-01-01", Amount: 1000}}),
		flat("observed-zero-rate", terms("2024-01-01", "2027-01-01", 0, zero), "2025-01-01", 0, []payment{{Date: "2027-01-01", Amount: 1000}}),
		flat("annual-coupons", terms("2024-01-01", "2027-01-01", 600, annual), "2025-01-01", .03, []payment{{Date: "2026-01-01", Amount: 60}, {Date: "2027-01-01", Amount: 1060}}),
		flat("coupon-date-excluded", terms("2024-01-01", "2027-01-01", 600, annual), "2026-01-01", .03, []payment{{Date: "2027-01-01", Amount: 1060}}),
		flat("initial-stub-full-coupon", terms("2024-02-15", "2025-01-01", 600, annual), "2024-03-01", .03, []payment{{Date: "2025-01-01", Amount: 1060}}),
		flat("monthly-normalized-month-end", terms("2023-12-15", "2024-05-31", 600, referencepb.PaymentFrequency_PAYMENT_FREQUENCY_MONTHLY), "2024-01-01", .03, []payment{{Date: "2024-02-01", Amount: 5}, {Date: "2024-03-01", Amount: 5}, {Date: "2024-04-01", Amount: 5}, {Date: "2024-05-01", Amount: 5}, {Date: "2024-05-31", Amount: 1005}}),
		flat("leap-month-normalization", terms("2023-08-31", "2024-08-31", 600, referencepb.PaymentFrequency_PAYMENT_FREQUENCY_SEMIANNUAL), "2023-09-02", .03, []payment{{Date: "2024-03-02", Amount: 30}, {Date: "2024-08-31", Amount: 1030}}),
		flat("matured-measured-zero", terms("2024-01-01", "2025-01-01", 600, annual), "2025-01-01", .03, nil),
	}
	for _, dc := range []referencepb.DayCountConvention{referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_360, referencepb.DayCountConvention_DAY_COUNT_CONVENTION_THIRTY_360, referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_ACT} {
		f := flat("curve-time-"+dc.String(), terms("2023-01-01", "2025-01-01", 0, zero), "2024-01-01", .03, []payment{{Date: "2025-01-01", Amount: 1000}})
		f.terms.DayCount = dc
		out = append(out, f)
	}
	for _, ip := range []curve.Interpolation{curve.LinearZero, curve.LogLinearDF} {
		z := .03
		name := "linear-zero-interior"
		if ip == curve.LogLinearDF {
			z = .035
			name = "log-linear-df-interior"
		}
		out = append(out, fixture{name, terms("2024-01-01", "2027-01-01", 0, zero), midnight("2025-01-01"), []float64{1, 3}, []float64{.02, .04}, ip, []payment{{Date: "2027-01-01", Amount: 1000, Zero: z}}})
	}
	return out
}

func capture(f fixture) (*quantpb.BondRiskRequest, result, error) {
	spec, ok := termsource.BondSpecFromTerms(f.terms)
	if !ok {
		return nil, result{}, fmt.Errorf("fixture terms refused: %s", f.name)
	}
	b := pricing.Bond{Face: spec.Face, CouponRate: spec.CouponRate, Frequency: spec.Frequency, Issue: spec.Issue, Maturity: spec.Maturity, DayCount: spec.DayCount}
	c, e := curve.NewZeroCurve(f.tenors, f.zeros, curve.Continuous, f.interp)
	if e != nil {
		return nil, result{}, e
	}
	req, e := pricing.ProjectBondRisk(f.terms, b, c, pricing.BondRiskCaptureMeta{CaptureID: "bond-corpus-" + f.name, InstrumentID: "BOND-FIXTURE", TenantID: "conformance", SourceRevision: "go-conformance-v1", AsOf: f.settle, CapturedAt: f.settle.Add(time.Second)})
	r := b.CurveRisk(f.settle, c)
	return req, result{r.DirtyPrice, r.DV01, r.EffectiveDuration, r.Convexity}, e
}

// Analytic central-1bp values over explicitly listed dates. This neither calls
// the production scheduler nor subtracts nearly equal bumped prices.
func analytic(f fixture) result {
	const h = 1e-4
	var price, duration, convexity, dv float64
	for _, p := range f.payments {
		years := midnight(p.Date).Sub(f.settle).Hours() / 24 / 365
		pv := p.Amount * math.Exp(-p.Zero*years)
		s := math.Sinh(h * years)
		half := math.Sinh(h * years / 2)
		price += pv
		duration += pv * s / h
		convexity += pv * 4 * half * half / (h * h)
		dv += pv * s
	}
	if price == 0 {
		return result{}
	}
	return result{price, math.Abs(dv), duration / price, convexity / price}
}
func closeEnough(name string, got, want float64) bool {
	tol := tolerances[name]
	return !math.IsNaN(got) && !math.IsInf(got, 0) && math.Abs(got-want) <= tol.Absolute+tol.Relative*math.Abs(want)
}
func compare(got, want result) error {
	for _, v := range []struct {
		name      string
		got, want float64
	}{{"dirty_price", got.DirtyPrice, want.DirtyPrice}, {"dv01", got.DV01, want.DV01}, {"effective_duration", got.Duration, want.Duration}, {"convexity", got.Convexity, want.Convexity}} {
		if !closeEnough(v.name, v.got, v.want) {
			return fmt.Errorf("%s differs: got %.17g want %.17g", v.name, v.got, v.want)
		}
	}
	return nil
}
func build() (corpus, error) {
	out := corpus{Version: 1, Tolerances: tolerances}
	for _, f := range fixtures() {
		req, want, e := capture(f)
		if e != nil {
			return out, e
		}
		if e := compare(want, analytic(f)); e != nil {
			return out, fmt.Errorf("Go analytic oracle %s: %w", f.name, e)
		}
		wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(req)
		if e != nil {
			return out, e
		}
		out.Cases = append(out.Cases, entry{f.name, wire, want})
	}
	return out, nil
}
func main() {
	out, e := build()
	if e == nil {
		e = json.NewEncoder(os.Stdout).Encode(out)
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
