package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestFillCorpusIndependentOracle(t *testing.T) {
	cases, err := corpus()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 22 {
		t.Fatalf("corpus shrank: %d", len(cases))
	}
	for _, e := range examples() {
		t.Run(e.name, func(t *testing.T) {
			a, err := checked(e)
			if err != nil {
				t.Fatal(err)
			}
			b, err := checked(e)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a.Request, b.Request) || !bytes.Equal(a.Candidate, b.Candidate) {
				t.Fatal("replay changed deterministic output")
			}
		})
	}
}

func TestOracleRoundingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value       string
		coefficient int64
		exponent    int32
	}{
		{"1/200000000", 1, -8}, {"-1/200000000", -1, -8},
		{"4/1000000000", 0, -8}, {"-4/1000000000", 0, -8},
		{"9223372036854775807/100000000", 9223372036854775807, -8},
		{"9223372036854775808/100000000", 922337203685477581, -7},
		{"-9223372036854775808/100000000", -9223372036854775808, -8},
		{"100000000000000000449", 1000000000000000005, 2},
	} {
		if got := quantize(tc.value); got.Coefficient != tc.coefficient || got.Exponent != tc.exponent {
			t.Fatalf("rounding oracle failed literal %s: %v", tc.value, got)
		}
	}
}

func TestFillSequenceUsesPreviouslyRoundedState(t *testing.T) {
	// Three unit fills at 1, 0, 0 give exact sequential VWAP 1/3; a fourth
	// uses the stored 0.33333333 rather than an invented unrounded cost ledger.
	state := base()
	state.OrderedQuantity = d(4, 0)
	state.LeavesQuantity = d(4, 0)
	expected := []string{"1", "1/2", "1/3", "99999999/400000000"}
	for i, avg := range expected {
		p := d(0, 0)
		if i == 0 {
			p = d(1, 0)
		}
		e := example{fmt.Sprintf("sequence-%d", i), state, fill(d(1, 0), p), fmt.Sprint(i + 1), fmt.Sprint(3 - i), avg, ""}
		result, err := checked(e)
		if err != nil {
			t.Fatal(err)
		}
		state = &orderpb.OrderState{}
		if err := proto.Unmarshal(result.Candidate, state); err != nil {
			t.Fatal(err)
		}
	}
	// Pure fold has no dedup store. A duplicate on a nonterminal state is
	// another fill, while replay from the SAME original state is deterministic.
	first, err := order.ApplyFill(base(), fill(d(1, 0), d(2, 0)), instant)
	if err != nil {
		t.Fatal(err)
	}
	second, err := order.ApplyFill(first, fill(d(1, 0), d(2, 0)), instant)
	if err != nil {
		t.Fatal(err)
	}
	if second.GetFilledQuantity().GetCoefficient() != 200000000 {
		t.Fatal("aggregate incorrectly deduplicated a fill")
	}
}

type limitedOutput struct {
	data  bytes.Buffer
	limit int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.data.Len() {
		return 0, errors.New("worker output bound exceeded")
	}
	return b.data.Write(p)
}
func runWorker(t *testing.T, bin string, input []byte, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"HOME=" + cmd.Dir, "TMPDIR=" + cmd.Dir, "TEMP=" + cmd.Dir, "TMP=" + cmd.Dir}
	if root := os.Getenv("SystemRoot"); root != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+root)
	}
	out := &limitedOutput{limit: 1 << 20}
	stderr := &limitedOutput{limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("worker exceeded deadline")
	}
	return out.data.Bytes(), err
}
func matchResult(r *omspb.FillFoldRequest, w *omspb.FillFoldResult, expected observation, version string) error {
	if w.CaptureId != r.CaptureId || !bytes.Equal(w.CaptureDigest, r.CaptureDigest) || w.TenantId != r.TenantId || w.ProducerVersion != version || w.ExpectedVersion == nil || r.ExpectedVersion == nil || *w.ExpectedVersion != *r.ExpectedVersion || len(w.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("result identity mismatch")
	}
	if expected.Rejection != "" {
		if w.Status != 2 || w.RejectionCode != expected.Rejection || w.Candidate != nil {
			return errors.New("refusal mismatch")
		}
		return nil
	}
	want := &orderpb.OrderState{}
	if err := proto.Unmarshal(expected.Candidate, want); err != nil {
		return err
	}
	if w.Status != 1 || w.RejectionCode != "" || !proto.Equal(w.Candidate, want) {
		return errors.New("full state mismatch")
	}
	return nil
}
func TestRustFillFoldProcessConformance(t *testing.T) {
	bin := os.Getenv("KANZ_OMS_FOLD_BIN")
	if bin == "" {
		t.Skip("actual Rust binary unavailable; set KANZ_OMS_FOLD_BIN and KANZ_OMS_FOLD_VERSION")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("binary must be absolute")
	}
	version := os.Getenv("KANZ_OMS_FOLD_VERSION")
	if version == "" {
		t.Fatal("expected immutable build version required")
	}
	cases, err := corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cases {
		t.Run(e.Name, func(t *testing.T) {
			raw, err := runWorker(t, bin, e.Request)
			if err != nil {
				t.Fatal("worker failed")
			}
			r := &omspb.FillFoldRequest{}
			w := &omspb.FillFoldResult{}
			if err := proto.Unmarshal(e.Request, r); err != nil {
				t.Fatal(err)
			}
			if err := proto.Unmarshal(raw, w); err != nil {
				t.Fatal(err)
			}
			if err := matchResult(r, w, e, version); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, where := range []string{"request", "state", "fill", "decimal"} {
		t.Run("unknown-"+where, func(t *testing.T) {
			r := request(examples()[0])
			unknown := protowire.AppendTag(nil, 999, protowire.VarintType)
			unknown = protowire.AppendVarint(unknown, 1)
			switch where {
			case "request":
				r.ProtoReflect().SetUnknown(unknown)
			case "state":
				r.State.ProtoReflect().SetUnknown(unknown)
			case "fill":
				r.Fill.ProtoReflect().SetUnknown(unknown)
			case "decimal":
				r.Fill.Quantity.ProtoReflect().SetUnknown(unknown)
			}
			raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = runWorker(t, bin, raw); err == nil {
				t.Fatal("unknown fields were silently discarded")
			}
		})
	}
	for _, name := range []string{"missing-version", "unknown-status", "large-exponent"} {
		t.Run(name, func(t *testing.T) {
			r := request(examples()[0])
			switch name {
			case "missing-version":
				r.ExpectedVersion = nil
			case "unknown-status":
				r.State.Status = 999
			case "large-exponent":
				r.Fill.Price.Exponent = 65
			}
			r.CaptureDigest = nil
			raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(raw)
			r.CaptureDigest = h[:]
			raw, err = (proto.MarshalOptions{Deterministic: true}).Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runWorker(t, bin, raw)
			if err != nil {
				t.Fatal("valid protobuf must return typed unsupported")
			}
			w := &omspb.FillFoldResult{}
			if err := proto.Unmarshal(out, w); err != nil {
				t.Fatal(err)
			}
			if w.Status != 3 || w.Candidate != nil {
				t.Fatal("unsupported input became a measured state")
			}
		})
	}
	for _, input := range [][]byte{nil, {0xff}, bytes.Repeat([]byte{0}, (1<<20)+1)} {
		if _, err := runWorker(t, bin, input); err == nil {
			t.Fatal("malformed/oversized input accepted")
		}
	}
}

func TestResultBindingAndPresence(t *testing.T) {
	e, err := checked(examples()[0])
	if err != nil {
		t.Fatal(err)
	}
	r := request(examples()[0])
	state := &orderpb.OrderState{}
	if err := proto.Unmarshal(e.Candidate, state); err != nil {
		t.Fatal(err)
	}
	good := &omspb.FillFoldResult{CaptureId: r.CaptureId, CaptureDigest: r.CaptureDigest, TenantId: r.TenantId, ProducerVersion: "stamp", ExpectedVersion: proto.Int64(7), Status: 1, Candidate: state}
	if err := matchResult(r, good, e, "stamp"); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*omspb.FillFoldResult){func(w *omspb.FillFoldResult) { w.CaptureId = "other" }, func(w *omspb.FillFoldResult) { w.CaptureDigest = []byte{1} }, func(w *omspb.FillFoldResult) { w.TenantId = "other" }, func(w *omspb.FillFoldResult) { w.ProducerVersion = "other" }, func(w *omspb.FillFoldResult) { w.ExpectedVersion = nil }, func(w *omspb.FillFoldResult) { w.ExpectedVersion = proto.Int64(8) }, func(w *omspb.FillFoldResult) { w.Candidate = nil }, func(w *omspb.FillFoldResult) { w.Candidate.FilledQuantity = nil }, func(w *omspb.FillFoldResult) { w.Candidate.ReleaseAsk = nil }, func(w *omspb.FillFoldResult) { w.Candidate.AsOf.Nanos++ }, func(w *omspb.FillFoldResult) { w.Status = 0 }, func(w *omspb.FillFoldResult) { w.RejectionCode = "unexpected" }}
	for i, mutate := range mutations {
		w := proto.Clone(good).(*omspb.FillFoldResult)
		mutate(w)
		if matchResult(r, w, e, "stamp") == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
}
