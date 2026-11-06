package juliaconformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	researchpb "github.com/eighred/kanz/kanz-schemas-go/research/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ociDirectory = flag.String("julia-oci-dir", "", "Owned public fixture directory")
var ociPhase = flag.String("julia-oci-phase", "", "prepare or verify public OCI fixtures")

// This is a test-only wire oracle, never a risk-engine transport or curve owner.
// Expected discount factors are the existing independently derived rationals in
// kanz-jl/test/kernel.jl, not outputs of either numerical implementation.
func TestJuliaOCIOracle(t *testing.T) {
	dir, phase := *ociDirectory, *ociPhase
	if dir == "" && phase == "" {
		t.Skip("OCI qualification not configured")
	}
	if !filepath.IsAbs(dir) || (phase != "prepare" && phase != "verify") {
		t.Fatal("invalid OCI oracle configuration")
	}
	input := func(kind researchpb.RateQuoteKind, tenor, rate, span float64) *researchpb.CalibrationInput {
		return &researchpb.CalibrationInput{InstrumentId: "fixture", Kind: kind, TenorYears: tenor, SpanYears: span, Rate: &researchpb.CalibrationInput_ResolvedRate{ResolvedRate: rate}}
	}
	const deposit = researchpb.RateQuoteKind_RATE_QUOTE_KIND_DEPOSIT
	const future = researchpb.RateQuoteKind_RATE_QUOTE_KIND_FUTURE
	const swap = researchpb.RateQuoteKind_RATE_QUOTE_KIND_SWAP
	type pillar struct{ tenor, discount, tolerance float64 }
	type fixture struct {
		name    string
		inputs  []*researchpb.CalibrationInput
		pillars []pillar
		failure researchpb.CurveCalibrationFailure
	}
	fixtures := []fixture{
		{"deposit", []*researchpb.CalibrationInput{input(deposit, 1, .05, 0)}, []pillar{{1, 20.0 / 21, 1e-14}}, 1},
		{"forward", []*researchpb.CalibrationInput{input(future, 1, .04, .25), input(deposit, 1, .05, 0)}, []pillar{{1, 20.0 / 21, 1e-14}, {1.25, 2000.0 / 2121, 1e-14}}, 1},
		{"swap", []*researchpb.CalibrationInput{input(deposit, 1, .05, 0), input(swap, 2, .06, 0)}, []pillar{{1, 20.0 / 21, 1e-14}, {2, 330.0 / 371, 1e-13}}, 1},
		{"negative", []*researchpb.CalibrationInput{input(deposit, 1, -.05, 0)}, []pillar{{1, 20.0 / 19, 1e-14}}, 1},
		{"stub", []*researchpb.CalibrationInput{input(swap, .5, .04, 0)}, []pillar{{.5, 50.0 / 51, 1e-13}}, 1},
		{"missing", []*researchpb.CalibrationInput{input(deposit, 1, .05, 0), {InstrumentId: "missing", Kind: swap, TenorYears: 2}}, []pillar{{1, 20.0 / 21, 1e-14}}, 1},
		{"zero", []*researchpb.CalibrationInput{input(deposit, 1, 0, 0)}, []pillar{{1, 1, 1e-14}}, 1},
		{"unsupported", []*researchpb.CalibrationInput{input(deposit, 1, .05, 0)}, nil, 3},
		{"invalid", []*researchpb.CalibrationInput{input(deposit, 101, .05, 0)}, nil, 2},
		{"numerical", []*researchpb.CalibrationInput{input(deposit, 1, -1, 0)}, nil, 4},
	}
	type manifestCase struct {
		Name     string `json:"name"`
		ExitZero bool   `json:"exitZero"`
	}
	manifest := make([]manifestCase, 0, len(fixtures)+3)
	for _, f := range fixtures {
		manifest = append(manifest, manifestCase{f.name, true})
		t.Run(f.name, func(t *testing.T) {
			digest := sha256.Sum256([]byte(f.name))
			stamp := timestamppb.New(time.Unix(1700000000, 0))
			job := &researchpb.CurveCalibrationJob{CaptureId: f.name, CaptureDigest: digest[:], Currency: "USD", RequestedAsOf: stamp, CapturedAt: stamp, Interpolation: referencepb.Interpolation_INTERPOLATION_LINEAR_ZERO, Inputs: f.inputs, LatestObservations: true}
			if f.name == "unsupported" {
				job.Interpolation = 0
			}
			if phase == "prepare" {
				wire, err := proto.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, f.name+".input"), wire, 0600); err != nil {
					t.Fatal(err)
				}
				return
			}
			wire, err := os.ReadFile(filepath.Join(dir, f.name+".output"))
			if err != nil || len(wire) == 0 || len(wire) > 1<<20 {
				t.Fatal("missing or oversized response", err)
			}
			var result researchpb.CurveCalibrationResult
			if err := proto.Unmarshal(wire, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.ProtoReflect().GetUnknown()) != 0 || result.CaptureId != f.name || !bytes.Equal(result.CaptureDigest, digest[:]) || result.Failure != f.failure || len(result.Pillars) != len(f.pillars) {
				t.Fatal("response binding/failure/pillar mismatch")
			}
			for i, expected := range f.pillars {
				p := result.Pillars[i]
				if p == nil || len(p.ProtoReflect().GetUnknown()) != 0 || p.TenorYears != expected.tenor || math.IsNaN(p.ZeroRate) || math.IsInf(p.ZeroRate, 0) || math.Abs(math.Exp(-p.ZeroRate*p.TenorYears)-expected.discount) > expected.tolerance {
					t.Fatal("independent rational golden mismatch")
				}
			}
		})
	}
	for _, f := range []struct {
		name string
		wire []byte
	}{{"empty", nil}, {"malformed", []byte{0x0a, 0x7f}}, {"oversize", make([]byte, (1<<20)+1)}} {
		manifest = append(manifest, manifestCase{f.name, false})
		if phase == "prepare" {
			if err := os.WriteFile(filepath.Join(dir, f.name+".input"), f.wire, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if phase == "prepare" {
		b, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cases.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
