package calibrationshadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	researchpb "github.com/eighred/kanz/kanz-schemas-go/research/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixture() curve.CalibrationCapture {
	rate := 0.05
	now := time.Now().UTC()
	return curve.CalibrationCapture{Currency: "USD", RequestedAsOf: now, CapturedAt: now, Interp: curve.LinearZero, Tenors: []float64{1}, Zeros: []float64{math.Log1p(rate)}, Strip: curve.Strip{Capture: &curve.InputCapture{CapturedAt: now, Inputs: []curve.CapturedInput{{InstrumentID: "deposit-1", Kind: curve.Deposit, Tenor: 1, ResolvedRate: &rate}}}}}
}

func TestJobDigestAndValidation(t *testing.T) {
	c := fixture()
	j, e := job(c)
	if e != nil {
		t.Fatal(e)
	}
	digest := append([]byte(nil), j.CaptureDigest...)
	j.CaptureDigest = nil
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if e != nil {
		t.Fatal(e)
	}
	want := sha256.Sum256(b)
	if !bytes.Equal(digest, want[:]) {
		t.Fatal("capture digest does not bind full job")
	}
	mutations := map[string]func(*curve.CalibrationCapture){"capture": func(c *curve.CalibrationCapture) { c.Strip.Capture = nil }, "nan": func(c *curve.CalibrationCapture) { c.Zeros[0] = math.NaN() }, "timestamp": func(c *curve.CalibrationCapture) { c.CapturedAt = time.Time{} }, "currency": func(c *curve.CalibrationCapture) { c.Currency = "usd" }, "tenor": func(c *curve.CalibrationCapture) { c.Tenors[0] = 0 }, "rate": func(c *curve.CalibrationCapture) { *c.Strip.Capture.Inputs[0].ResolvedRate = math.Inf(1) }, "bounds": func(c *curve.CalibrationCapture) { c.Strip.Capture.Inputs = make([]curve.CapturedInput, 257) }}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := fixture()
			mutate(&c)
			if _, e := job(c); e == nil {
				t.Fatal("invalid capture accepted")
			}
		})
	}
}

func TestRatePresenceSurvivesProtobuf(t *testing.T) {
	c := fixture()
	*c.Strip.Capture.Inputs[0].ResolvedRate = 0
	c.Strip.Capture.Inputs = append(c.Strip.Capture.Inputs, curve.CapturedInput{InstrumentID: "missing", Kind: curve.Swap, Tenor: 2})
	j, e := job(c)
	if e != nil {
		t.Fatal(e)
	}
	wire, e := proto.Marshal(j)
	if e != nil {
		t.Fatal(e)
	}
	decoded := new(researchpb.CurveCalibrationJob)
	if e := proto.Unmarshal(wire, decoded); e != nil {
		t.Fatal(e)
	}
	zero, ok := decoded.Inputs[0].Rate.(*researchpb.CalibrationInput_ResolvedRate)
	if !ok || zero.ResolvedRate != 0 {
		t.Fatal("observed zero lost presence")
	}
	if decoded.Inputs[1].Rate != nil {
		t.Fatal("missing rate fabricated as zero")
	}
}

func TestComparisonRefusesUnboundAndInvalidResults(t *testing.T) {
	c := fixture()
	j, e := job(c)
	if e != nil {
		t.Fatal(e)
	}
	good := func() *researchpb.CurveCalibrationResult {
		return &researchpb.CurveCalibrationResult{CaptureId: j.CaptureId, CaptureDigest: append([]byte(nil), j.CaptureDigest...), Failure: researchpb.CurveCalibrationFailure_CURVE_CALIBRATION_FAILURE_NONE, Pillars: []*researchpb.CurveCalibrationPillar{{TenorYears: 1, ZeroRate: c.Zeros[0]}}}
	}
	if s := compare(j, c, good()); s != "matched" {
		t.Fatal(s)
	}
	for name, mutate := range map[string]func(*researchpb.CurveCalibrationResult){"digest": func(r *researchpb.CurveCalibrationResult) { r.CaptureDigest[0] ^= 1 }, "id": func(r *researchpb.CurveCalibrationResult) { r.CaptureId = "wrong" }, "shape": func(r *researchpb.CurveCalibrationResult) { r.Pillars = nil }, "nan": func(r *researchpb.CurveCalibrationResult) { r.Pillars[0].ZeroRate = math.NaN() }, "zero_tenor": func(r *researchpb.CurveCalibrationResult) { r.Pillars[0].TenorYears = 0 }, "overflow": func(r *researchpb.CurveCalibrationResult) { r.Pillars[0].ZeroRate = -1000 }, "failure": func(r *researchpb.CurveCalibrationResult) { r.Failure = 0 }, "disagreement": func(r *researchpb.CurveCalibrationResult) { r.Pillars[0].ZeroRate += 0.001 }} {
		t.Run(name, func(t *testing.T) {
			r := good()
			mutate(r)
			if compare(j, c, r) == "matched" {
				t.Fatal("bad result matched")
			}
		})
	}
}

// This helper is a real subprocess, not a mocked exec implementation. Production
// never accepts arbitrary arguments; execute constructs its fixed Julia argv.
func TestProcessHelper(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if !strings.HasPrefix(mode, "shadow-helper-") {
		return
	}
	switch mode {
	case "shadow-helper-timeout":
		time.Sleep(5 * time.Second)
	case "shadow-helper-out":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), MaxBytes+1))
	case "shadow-helper-err":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("x"), (32<<10)+1))
	case "shadow-helper-env":
		if os.Getenv("SHADOW_TEST_SECRET") != "" || os.Getenv("JULIA_LOAD_PATH") != "@"+string(os.PathListSeparator)+"@stdlib" {
			os.Exit(2)
		}
		_, _ = os.Stdout.Write([]byte("scrubbed"))
	case "shadow-helper-echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "shadow-helper-fail":
		os.Exit(3)
	}
	os.Exit(0)
}

func TestProcessBoundaries(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("SHADOW_TEST_SECRET", "fixture-secret")
	cfg := Config{Executable: exe, Project: t.TempDir(), Depot: t.TempDir(), Timeout: 3 * time.Second}
	for _, tc := range []struct{ mode, want string }{{"out", "output_limit"}, {"err", "output_limit"}, {"timeout", "timeout"}, {"fail", "worker_failure"}, {"env", ""}, {"echo", ""}} {
		t.Run(tc.mode, func(t *testing.T) {
			local := cfg
			if tc.mode == "timeout" {
				local.Timeout = 200 * time.Millisecond
			}
			out, status := runProcess(context.Background(), local, []byte("fixture"), []string{"-test.run=^TestProcessHelper$", "shadow-helper-" + tc.mode})
			if status != tc.want {
				t.Fatalf("status=%q want=%q", status, tc.want)
			}
			if tc.mode == "echo" && string(out) != "fixture" {
				t.Fatal("stdin changed")
			}
			if tc.mode == "env" && string(out) != "scrubbed" {
				t.Fatal("environment not scrubbed")
			}
		})
	}
	if _, s := runProcess(context.Background(), cfg, make([]byte, MaxBytes+1), nil); s != "invalid_input" {
		t.Fatal(s)
	}
}

func TestEnqueueDoesNotWaitOrMutate(t *testing.T) {
	statuses := []string{}
	w := &Worker{done: make(chan struct{}), queue: make(chan curve.CalibrationCapture, 1), observe: func(s string) { statuses = append(statuses, s) }}
	c := fixture()
	w.Enqueue(c)
	w.Enqueue(c)
	if len(statuses) != 1 || statuses[0] != "queue_full" {
		t.Fatal(statuses)
	}
	if c.Zeros[0] != math.Log1p(0.05) {
		t.Fatal("authoritative capture changed")
	}
}

func TestConfigRefusals(t *testing.T) {
	if e := (Config{}).Validate(); e != nil {
		t.Fatal(e)
	}
	for _, c := range []Config{{Executable: "partial"}, {Enabled: true}, {Enabled: true, Timeout: time.Second, Executable: "relative"}} {
		if c.Validate() == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}

func TestJuliaIntegration(t *testing.T) {
	exe := os.Getenv("TEST_JULIA_EXECUTABLE")
	if exe == "" {
		t.Skip("real Julia not configured: set TEST_JULIA_EXECUTABLE, TEST_JULIA_PROJECT and TEST_JULIA_DEPOT")
	}
	project := os.Getenv("TEST_JULIA_PROJECT")
	cfg := Config{Enabled: true, Executable: exe, Project: project, Script: filepath.Join(project, "bin", "curve_worker.jl"), Depot: os.Getenv("TEST_JULIA_DEPOT"), Timeout: time.Minute}
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	if status := evaluate(context.Background(), cfg, fixture()); status != "matched" {
		t.Fatalf("real Julia comparison: %s", status)
	}

	quotes := []curve.RateQuote{{Kind: curve.Deposit, Tenor: 0.25, Value: 0.03}, {Kind: curve.Future, Tenor: 0.25, Span: 0.25, Value: 0.035}, {Kind: curve.Swap, Tenor: 1, Value: 0.04}, {Kind: curve.Swap, Tenor: 2, Value: 0.045}}
	authoritative, e := curve.Calibrate(quotes, curve.LinearZero)
	if e != nil {
		t.Fatal(e)
	}
	capture := fixture()
	capture.Tenors = authoritative.Tenors()
	capture.Zeros = authoritative.Zeros()
	capture.Strip.Capture.Inputs = nil
	for i, q := range quotes {
		rate := q.Value
		capture.Strip.Capture.Inputs = append(capture.Strip.Capture.Inputs, curve.CapturedInput{InstrumentID: []string{"dep", "future", "swap1", "swap2"}[i], Kind: q.Kind, Tenor: q.Tenor, Span: q.Span, ResolvedRate: &rate})
	}
	capture.Strip.Coverage.Configured = 5
	capture.Strip.Coverage.Quoted = 4
	capture.Strip.Coverage.Missing = []curve.MissingQuote{{InstrumentID: "missing", Reason: "no_quote"}}
	for i := range capture.Strip.Capture.Inputs {
		input := &capture.Strip.Capture.Inputs[i]
		input.Observation = &marketpb.MarketDataEvent{InstrumentId: input.InstrumentID, Symbol: input.InstrumentID, Mic: "TEST", EventTime: timestamppb.New(capture.CapturedAt), Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: &commonpb.Decimal{Coefficient: []int64{300, 965000, 400, 450}[i], Exponent: -4}, Size: &commonpb.Decimal{Coefficient: 1}}}}
	}
	capture.Strip.Capture.Inputs = append(capture.Strip.Capture.Inputs, curve.CapturedInput{InstrumentID: "missing", Kind: curve.Swap, Tenor: 3})
	if status := evaluate(context.Background(), cfg, capture); status != "matched" {
		t.Fatalf("real mixed-strip Julia comparison: %s", status)
	}
	negative := fixture()
	*negative.Strip.Capture.Inputs[0].ResolvedRate = -0.01
	negative.Zeros[0] = math.Log1p(-0.01)
	if status := evaluate(context.Background(), cfg, negative); status != "matched" {
		t.Fatalf("real negative-rate Julia comparison: %s", status)
	}

}
