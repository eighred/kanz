package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	quantpb "github.com/eighred/kanz/kanz-schemas-go/quant/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
)

func TestBondCorpusIndependentAnalytics(t *testing.T) {
	a, e := build()
	if e != nil {
		t.Fatal(e)
	}
	b, e := build()
	if e != nil {
		t.Fatal(e)
	}
	aa, e := json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	bb, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(aa, bb) || len(a.Cases) != 14 {
		t.Fatal("corpus changed or is nondeterministic")
	}
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			req, want, e := capture(f)
			if e != nil {
				t.Fatal(e)
			}
			if e := compare(want, analytic(f)); e != nil {
				t.Fatal(e)
			}
			if len(req.Payments) != len(f.payments) {
				t.Fatal("schedule count differs from explicit oracle")
			}
			for i, p := range req.Payments {
				if !p.PaymentDate.AsTime().Equal(midnight(f.payments[i].Date)) {
					t.Fatal("payment differs from explicit calendar oracle")
				}
			}
			if req.CurveAsOf != nil || req.CurveId != nil || req.Coverage != nil {
				t.Fatal("unknown metadata fabricated")
			}
			digest := append([]byte(nil), req.CaptureDigest...)
			req.CaptureDigest = nil
			wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(req)
			if e != nil {
				t.Fatal(e)
			}
			sum := sha256.Sum256(wire)
			if !bytes.Equal(digest, sum[:]) {
				t.Fatal("capture digest mismatch")
			}
		})
	}
}

type bounded struct {
	buffer bytes.Buffer
	limit  int
	cancel context.CancelFunc
}

func (b *bounded) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.cancel()
		return 0, errors.New("worker output bound")
	}
	return b.buffer.Write(p)
}
func runWorker(binary string, wire []byte) (*quantpb.BondRiskResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = nil
	// Preserve only Windows executable loader's system location, never appenv.
	cmd.Env = []string{}
	if root := os.Getenv("SystemRoot"); root != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+root)
	}
	cmd.Stdin = bytes.NewReader(wire)
	out := &bounded{limit: 1 << 20, cancel: cancel}
	stderr := &bounded{limit: 32 << 10, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if e := cmd.Run(); e != nil {
		return nil, errors.New("worker failed or exceeded deadline/output bounds")
	}
	r := new(quantpb.BondRiskResult)
	if e := proto.Unmarshal(out.buffer.Bytes(), r); e != nil {
		return nil, errors.New("invalid protobuf response")
	}
	return r, nil
}
func binding(req *quantpb.BondRiskRequest, r *quantpb.BondRiskResult, version string) error {
	if r.CaptureId != req.CaptureId || r.TenantId != req.TenantId || !bytes.Equal(r.CaptureDigest, req.CaptureDigest) || r.ProducerVersion != version || len(r.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("result identity/version binding failed")
	}
	return nil
}
func measured(r *quantpb.BondRiskResult) (result, error) {
	v := r.Values
	if r.Status != quantpb.BondRiskStatus_BOND_RISK_STATUS_OK || v == nil || v.DirtyPrice == nil || v.Dv01 == nil || v.EffectiveDuration == nil || v.Convexity == nil || len(v.ProtoReflect().GetUnknown()) != 0 {
		return result{}, errors.New("successful result requires all measured values")
	}
	price, ok := dec.Float64(v.DirtyPrice)
	if !ok {
		return result{}, errors.New("invalid price decimal")
	}
	dv, ok := dec.Float64(v.Dv01)
	if !ok {
		return result{}, errors.New("invalid DV01 decimal")
	}
	out := result{price, dv, *v.EffectiveDuration, *v.Convexity}
	if e := compare(out, out); e != nil {
		return result{}, e
	}
	if price < 0 || dv < 0 {
		return result{}, errors.New("negative price/DV01")
	}
	return out, nil
}
func redigest(t *testing.T, r *quantpb.BondRiskRequest) []byte {
	t.Helper()
	r.CaptureDigest = nil
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	d := sha256.Sum256(b)
	r.CaptureDigest = d[:]
	b, e = proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// Required in Linux CI with a freshly built, stamped binary. An absent local
// dependency is an explicit skip, never actual worker proof.
func TestRustBondRiskProcessConformance(t *testing.T) {
	binary := os.Getenv("KANZ_BOND_RISK_BIN")
	if binary == "" {
		t.Skip("set KANZ_BOND_RISK_BIN and KANZ_BOND_RISK_VERSION for real Rust process proof")
	}
	version := os.Getenv("KANZ_BOND_RISK_VERSION")
	if !filepath.IsAbs(binary) || version == "" {
		t.Fatal("absolute binary and expected immutable build stamp required")
	}
	if info, e := os.Stat(binary); e != nil || !info.Mode().IsRegular() {
		t.Fatal("configured worker missing")
	}
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			req, want, e := capture(f)
			if e != nil {
				t.Fatal(e)
			}
			wire, e := proto.Marshal(req)
			if e != nil {
				t.Fatal(e)
			}
			r, e := runWorker(binary, wire)
			if e != nil {
				t.Fatal(e)
			}
			if e := binding(req, r, version); e != nil {
				t.Fatal(e)
			}
			got, e := measured(r)
			if e != nil {
				t.Fatal(e)
			}
			if e := compare(got, want); e != nil {
				t.Fatal(e)
			}
			if e := compare(got, analytic(f)); e != nil {
				t.Fatal(e)
			}
		})
	}
	invalid := []struct {
		name   string
		mutate func(*quantpb.BondRiskRequest)
		status quantpb.BondRiskStatus
	}{
		{"missing-rate", func(r *quantpb.BondRiskRequest) { r.CurvePoints[0].ContinuousZero = nil }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"unknown-interpolation", func(r *quantpb.BondRiskRequest) { r.Interpolation = referencepb.Interpolation(99) }, quantpb.BondRiskStatus_BOND_RISK_STATUS_UNSUPPORTED_CONVENTION},
		{"unknown-daycount", func(r *quantpb.BondRiskRequest) { r.Terms.DayCount = referencepb.DayCountConvention(99) }, quantpb.BondRiskStatus_BOND_RISK_STATUS_UNSUPPORTED_CONVENTION},
		{"missing-face", func(r *quantpb.BondRiskRequest) { r.Terms.FaceValue = nil }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"decimal-domain", func(r *quantpb.BondRiskRequest) { r.Terms.FaceValue.Exponent = 65 }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"nonfinite-rate", func(r *quantpb.BondRiskRequest) { r.CurvePoints[0].ContinuousZero = proto.Float64(math.Inf(1)) }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"payment-on-settle", func(r *quantpb.BondRiskRequest) { r.Payments[0].PaymentDate = r.AsOf }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"missing-redemption", func(r *quantpb.BondRiskRequest) { r.Payments[0].Redemption = false }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"future-capture", func(r *quantpb.BondRiskRequest) { r.CapturedAt.Seconds = r.AsOf.Seconds - 1 }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
		{"empty-active-schedule", func(r *quantpb.BondRiskRequest) { r.Payments = nil }, quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			req, _, e := capture(fixtures()[0])
			if e != nil {
				t.Fatal(e)
			}
			tc.mutate(req)
			wire := redigest(t, req)
			r, e := runWorker(binary, wire)
			if e != nil {
				t.Fatal(e)
			}
			if e := binding(req, r, version); e != nil {
				t.Fatal(e)
			}
			if r.Status != tc.status || r.Values != nil {
				t.Fatal("invalid input did not produce closed status without measured values")
			}
		})
	}
	for _, wire := range [][]byte{{0xff}, make([]byte, (1<<20)+1)} {
		if _, e := runWorker(binary, wire); e == nil {
			t.Fatal("malformed/oversized wire accepted")
		}
	}
}

func TestBondResultBindingRejectsCrossedResponses(t *testing.T) {
	req, _, e := capture(fixtures()[0])
	if e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*quantpb.BondRiskResult){func(r *quantpb.BondRiskResult) { r.CaptureId = "other" }, func(r *quantpb.BondRiskResult) { r.CaptureDigest[0] ^= 1 }, func(r *quantpb.BondRiskResult) { r.TenantId = "other" }, func(r *quantpb.BondRiskResult) { r.ProducerVersion = "other" }} {
		r := &quantpb.BondRiskResult{CaptureId: req.CaptureId, CaptureDigest: append([]byte(nil), req.CaptureDigest...), TenantId: req.TenantId, ProducerVersion: "expected"}
		mutate(r)
		if binding(req, r, "expected") == nil {
			t.Fatal("crossed result accepted")
		}
	}
}

func TestBondResultRequiresMeasuredValues(t *testing.T) {
	req, _, e := capture(fixtures()[0])
	if e != nil {
		t.Fatal(e)
	}
	good := &quantpb.BondRiskResult{Status: quantpb.BondRiskStatus_BOND_RISK_STATUS_OK, Values: &quantpb.BondRiskValues{DirtyPrice: req.Terms.CouponRate, Dv01: req.Terms.CouponRate, EffectiveDuration: proto.Float64(0), Convexity: proto.Float64(0)}}
	if _, e := measured(good); e != nil {
		t.Fatal("measured zero refused")
	}
	for _, mutate := range []func(*quantpb.BondRiskResult){func(r *quantpb.BondRiskResult) { r.Status = quantpb.BondRiskStatus_BOND_RISK_STATUS_UNSPECIFIED }, func(r *quantpb.BondRiskResult) { r.Values = nil }, func(r *quantpb.BondRiskResult) { r.Values.DirtyPrice = nil }, func(r *quantpb.BondRiskResult) { r.Values.Dv01 = nil }, func(r *quantpb.BondRiskResult) { r.Values.EffectiveDuration = nil }, func(r *quantpb.BondRiskResult) { r.Values.Convexity = nil }, func(r *quantpb.BondRiskResult) { r.Values.Convexity = proto.Float64(math.NaN()) }} {
		r := proto.Clone(good).(*quantpb.BondRiskResult)
		mutate(r)
		if _, e := measured(r); e == nil {
			t.Fatal("unknown or invalid measurement accepted as zero")
		}
	}
}
