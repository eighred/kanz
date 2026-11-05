package bondshadow

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	quantpb "github.com/eighred/kanz/kanz-schemas-go/quant/v1"
	refpb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixture(t *testing.T) compute.BondRiskCapture {
	t.Helper()
	asof := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	maturity := asof.AddDate(2, 0, 0)
	c, e := curve.NewZeroCurve([]float64{1, 2}, []float64{.05, .05}, curve.Continuous, curve.LinearZero)
	if e != nil {
		t.Fatal(e)
	}
	b := pricing.Bond{Face: 1000, Issue: asof, Maturity: maturity, DayCount: pricing.Actual365Fixed}
	return compute.BondRiskCapture{InstrumentID: "bond", AsOf: asof, CapturedAt: asof.Add(time.Second), Bond: b, Curve: c, Terms: &refpb.BondTerms{FaceValue: &commonpb.Decimal{Coefficient: 1000}, CouponRate: &commonpb.Decimal{}, IssueDate: timestamppb.New(asof), MaturityDate: timestamppb.New(maturity), CurrencyCode: "USD", DayCount: refpb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_365F}, Result: b.CurveRisk(asof, c)}
}
func result(j *quantpb.BondRiskRequest) *quantpb.BondRiskResult {
	return &quantpb.BondRiskResult{CaptureId: j.CaptureId, CaptureDigest: j.CaptureDigest, TenantId: j.TenantId, ProducerVersion: "stamp", Status: quantpb.BondRiskStatus_BOND_RISK_STATUS_OK, Values: &quantpb.BondRiskValues{DirtyPrice: &commonpb.Decimal{Coefficient: 100}, Dv01: &commonpb.Decimal{Coefficient: 1}, EffectiveDuration: proto.Float64(2), Convexity: proto.Float64(3)}}
}
func TestCaptureAndResultBoundary(t *testing.T) {
	c := fixture(t)
	c.Result = pricing.CurveRisk{DirtyPrice: 100, DV01: 1, EffectiveDuration: 2, Convexity: 3}
	j, e := job(c, "tenant", "source")
	if e != nil {
		t.Fatal(e)
	}
	if !j.CapturedAt.AsTime().Equal(c.CapturedAt) {
		t.Fatal("capture time replaced during queued job construction")
	}
	if j.Coverage != nil || j.CurveAsOf != nil || j.CurveId != nil {
		t.Fatal("fabricated provenance")
	}
	c.Terms.FaceValue.Coefficient = 3
	if j.Terms.FaceValue.Coefficient != 1000 {
		t.Fatal("capture aliases terms")
	}
	if s := compare(j, c, result(j), "stamp"); s != "matched" {
		t.Fatal(s)
	}
	cases := map[string]func(*quantpb.BondRiskResult){"tenant": func(r *quantpb.BondRiskResult) { r.TenantId = "other" }, "id": func(r *quantpb.BondRiskResult) { r.CaptureId = "other" }, "digest": func(r *quantpb.BondRiskResult) { r.CaptureDigest = nil }, "version": func(r *quantpb.BondRiskResult) { r.ProducerVersion = "other" }, "missing": func(r *quantpb.BondRiskResult) { r.Values.DirtyPrice = nil }, "unknown nested": func(r *quantpb.BondRiskResult) { r.Values.DirtyPrice.ProtoReflect().SetUnknown([]byte{0x78, 1}) }, "unknown top": func(r *quantpb.BondRiskResult) { r.ProtoReflect().SetUnknown([]byte{0x78, 1}) }, "missing ratio": func(r *quantpb.BondRiskResult) { r.Values.Convexity = nil }, "nan": func(r *quantpb.BondRiskResult) { r.Values.Convexity = proto.Float64(math.NaN()) }, "exponent": func(r *quantpb.BondRiskResult) { r.Values.Dv01.Exponent = 100 }, "status": func(r *quantpb.BondRiskResult) { r.Status = 99 }, "failure values": func(r *quantpb.BondRiskResult) { r.Status = quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := result(j)
			mutate(r)
			if s := compare(j, c, r, "stamp"); s != "invalid_result" {
				t.Fatal(s)
			}
		})
	}
	r := result(j)
	r.Values.DirtyPrice.Coefficient = 101
	if s := compare(j, c, r, "stamp"); s != "disagreement" {
		t.Fatal(s)
	}
	r.Status = quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT
	r.Values = nil
	if s := compare(j, c, r, "stamp"); s != "worker_rejected" {
		t.Fatal(s)
	}
}
func TestQueueAndConfig(t *testing.T) {
	c := fixture(t)
	count := 0
	w := &Worker{done: make(chan struct{}), queue: make(chan compute.BondRiskCapture, 1), observe: func(s string) {
		if s != "queue_full" {
			t.Fatal(s)
		}
		count++
	}}
	w.Submit(c)
	c.Terms.FaceValue.Coefficient = 999
	queued := <-w.queue
	if queued.Terms.FaceValue.Coefficient != 1000 {
		t.Fatal("queued terms alias caller")
	}
	if !queued.CapturedAt.Equal(c.CapturedAt) {
		t.Fatal("queued capture time changed")
	}
	w.queue <- queued
	w.Submit(c)
	if count != 1 {
		t.Fatal(count)
	}
	close(w.done)
	w.Submit(c)
	if count != 1 {
		t.Fatal("closed queue")
	}
	if w, e := New(context.Background(), Config{}, "", "", nil); e != nil || w != nil {
		t.Fatal("default must be off")
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	good := Config{Enabled: true, Executable: exe, ExpectedVersion: "stamp", Timeout: time.Second}
	if e := good.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, cfg := range []Config{{Executable: exe}, {ExpectedVersion: "stamp"}, {Timeout: time.Second}, {Enabled: true}, {Enabled: true, Executable: exe, Timeout: time.Second}, {Enabled: true, Executable: exe, ExpectedVersion: "stamp", Timeout: time.Hour}} {
		if cfg.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
func TestRustBondShadowActualProcess(t *testing.T) {
	exe := os.Getenv("KANZ_BOND_RISK_BIN")
	if exe == "" {
		t.Skip("real Rust worker not configured")
	}
	version := os.Getenv("KANZ_BOND_RISK_VERSION")
	cfg := Config{Enabled: true, Executable: exe, ExpectedVersion: version, Timeout: 5 * time.Second}
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	c := fixture(t)
	if s := evaluate(context.Background(), cfg, "tenant", "go-test", c); s != "matched" {
		t.Fatal(s)
	}
	cfg.ExpectedVersion = "wrong-stamp"
	if s := evaluate(context.Background(), cfg, "tenant", "go-test", c); s != "invalid_result" {
		t.Fatal(s)
	}
}

func TestSubmitRefusesOversizedCanonicalBeforeCopy(t *testing.T) {
	c := fixture(t)
	unknown := protowire.AppendTag(nil, 100, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, make([]byte, pricing.MaxBondTermsBytes))
	c.Terms.ProtoReflect().SetUnknown(unknown)
	observed := 0
	w := &Worker{done: make(chan struct{}), queue: make(chan compute.BondRiskCapture, 1), observe: func(s string) {
		if s != "invalid_input" {
			t.Fatal(s)
		}
		observed++
	}}
	allocations := testing.AllocsPerRun(10, func() { w.Submit(c) })
	if observed == 0 || len(w.queue) != 0 {
		t.Fatal("oversized diagnostic enqueued")
	}
	if allocations != 0 {
		t.Fatalf("oversized Submit copied metadata: allocations=%v", allocations)
	}
	if c.Terms.FaceValue.Coefficient != 1000 || len(c.Terms.ProtoReflect().GetUnknown()) != len(unknown) {
		t.Fatal("input was altered or truncated")
	}
}
