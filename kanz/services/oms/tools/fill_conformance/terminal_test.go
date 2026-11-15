package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type terminalObservation struct {
	name      string
	request   *omspb.TerminalFoldRequest
	candidate *orderpb.OrderState
	quantity  *commonpb.Decimal
	rejection string
}

func terminalCorpus(t *testing.T) []terminalObservation {
	t.Helper()
	var result []terminalObservation
	statuses := make([]int, 0, len(orderpb.OrderStatus_name))
	for value := range orderpb.OrderStatus_name {
		statuses = append(statuses, int(value))
	}
	sort.Ints(statuses)
	quantities := []*commonpb.Decimal{nil, d(0, 0), d(8, 0), d(-7, -2), d(math.MaxInt64, math.MaxInt32), d(math.MinInt64, math.MinInt32)}
	for _, operation := range []omspb.TerminalFoldOperation{omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_CANCEL, omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_EXPIRE} {
		for _, status := range statuses {
			for index, quantity := range quantities {
				s := base()
				s.Status = orderpb.OrderStatus(status)
				s.FilledQuantity = d(2, 0)
				s.LeavesQuantity = quantity
				before := proto.Clone(s)
				var got *orderpb.OrderState
				var remaining *commonpb.Decimal
				var err error
				target := orderpb.OrderStatus_ORDER_STATUS_CANCELLED
				if operation == omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_CANCEL {
					got, remaining, err = order.Cancel(s, instant)
				} else {
					target = orderpb.OrderStatus_ORDER_STATUS_EXPIRED
					got, remaining, err = order.Expire(s, instant)
				}
				// Independent specified transition table: terminal 4..7 refuse; every
				// other declared status changes exactly status/time, preserving wire leaves.
				name := fmt.Sprintf("%s/status-%d/quantity-%d", operation, status, index)
				observation := terminalObservation{name: name}
				if status >= 4 && status <= 7 {
					var rejection *order.RejectError
					if !errors.As(err, &rejection) || rejection.Code != "ORDER_TERMINAL" || got != nil || remaining != nil {
						t.Fatalf("%s: terminal refusal mismatch", name)
					}
					observation.rejection = "ORDER_TERMINAL"
				} else {
					expected := proto.Clone(s).(*orderpb.OrderState)
					expected.Status = target
					expected.AsOf = timestamppb.New(instant)
					if err != nil || !proto.Equal(got, expected) || !proto.Equal(remaining, quantity) {
						t.Fatalf("%s: state/quantity mismatch", name)
					}
					observation.candidate = got
					observation.quantity = remaining
				}
				if !proto.Equal(s, before) {
					t.Fatal("Go transition mutated input")
				}
				request := &omspb.TerminalFoldRequest{CaptureId: name, TenantId: "tenant", SourceRevision: "canonical-go-corpus", ExpectedVersion: proto.Int64(17), State: s, EventTime: timestamppb.New(instant), CapturedAt: timestamppb.New(instant), Operation: operation, Path: "reconcile"}
				wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(request)
				if e != nil {
					t.Fatal(e)
				}
				digest := sha256.Sum256(wire)
				request.CaptureDigest = digest[:]
				observation.request = request
				result = append(result, observation)
			}
		}
	}
	if len(result) != 108 {
		t.Fatalf("transition corpus changed: %d cases; review state inventory", len(result))
	}
	return result
}
func TestTerminalCorpusIndependentState(t *testing.T) { _ = terminalCorpus(t) }

func TestRustFillThenTerminalReplay(t *testing.T) {
	bin := os.Getenv("KANZ_OMS_FOLD_BIN")
	if bin == "" {
		t.Skip("actual Rust terminal binary unavailable")
	}
	version := os.Getenv("KANZ_OMS_FOLD_VERSION")
	if !filepath.IsAbs(bin) || version == "" {
		t.Fatal("absolute stamped worker required")
	}
	e := example{name: "fill-before-terminal", state: base(), fill: fill(d(2, 0), d(125, 0)), filled: "2", leaves: "8", average: "125"}
	expected, err := checked(e)
	if err != nil {
		t.Fatal(err)
	}
	jfill := request(e)
	raw, err := runWorker(t, bin, expected.Request)
	if err != nil {
		t.Fatal("fill worker failed")
	}
	filled := new(omspb.FillFoldResult)
	if proto.Unmarshal(raw, filled) != nil || matchResult(jfill, filled, expected, version) != nil {
		t.Fatal("fill result not bound")
	}
	for _, op := range []omspb.TerminalFoldOperation{omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_CANCEL, omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_EXPIRE} {
		state := filled.Candidate
		for step := 0; step < 2; step++ {
			j := &omspb.TerminalFoldRequest{CaptureId: fmt.Sprintf("sequence-%d-%d", op, step), TenantId: "tenant", SourceRevision: "corpus", ExpectedVersion: proto.Int64(int64(8 + step)), State: state, EventTime: timestamppb.New(instant), CapturedAt: timestamppb.New(instant), Operation: op, Path: "reconcile"}
			unsigned, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(j)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			digest := sha256.Sum256(unsigned)
			j.CaptureDigest = digest[:]
			wire, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(j)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			raw, runErr := runWorker(t, bin, wire, "--terminal")
			if runErr != nil {
				t.Fatal("terminal worker failed")
			}
			r := new(omspb.TerminalFoldResult)
			if proto.Unmarshal(raw, r) != nil || r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || r.TenantId != j.TenantId || r.Operation != op || r.ProducerVersion != version || r.ExpectedVersion == nil || r.GetExpectedVersion() != j.GetExpectedVersion() {
				t.Fatal("sequence result binding")
			}
			if step == 0 {
				want := proto.Clone(state).(*orderpb.OrderState)
				want.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
				if op == omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_EXPIRE {
					want.Status = orderpb.OrderStatus_ORDER_STATUS_EXPIRED
				}
				want.AsOf = j.EventTime
				if r.Status != omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_OK || r.RejectionCode != "" || !proto.Equal(r.Candidate, want) || !proto.Equal(r.UnfilledQuantity, state.LeavesQuantity) {
					t.Fatal("partial fill lost during terminal transition")
				}
				state = r.Candidate
			} else if r.Status != omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_REJECTED || r.RejectionCode != "ORDER_TERMINAL" || r.Candidate != nil || r.UnfilledQuantity != nil {
				t.Fatal("terminal replay resurrected order")
			}
		}
	}
}

func TestRustTerminalFoldProcessConformance(t *testing.T) {
	bin := os.Getenv("KANZ_OMS_FOLD_BIN")
	if bin == "" {
		t.Skip("actual Rust terminal binary unavailable")
	}
	version := os.Getenv("KANZ_OMS_FOLD_VERSION")
	if !filepath.IsAbs(bin) || version == "" {
		t.Fatal("absolute stamped worker required")
	}
	for _, caseValue := range terminalCorpus(t) {
		t.Run(caseValue.name, func(t *testing.T) {
			j := caseValue.request
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(j)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := runWorker(t, bin, wire, "--terminal")
			if err != nil {
				t.Fatal("actual terminal worker failed")
			}
			r := new(omspb.TerminalFoldResult)
			if proto.Unmarshal(raw, r) != nil {
				t.Fatal("invalid result protobuf")
			}
			if r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || r.TenantId != j.TenantId || r.ProducerVersion != version || r.Operation != j.Operation || r.ExpectedVersion == nil || r.GetExpectedVersion() != j.GetExpectedVersion() || len(r.ProtoReflect().GetUnknown()) != 0 {
				t.Fatal("result identity mismatch")
			}
			if caseValue.rejection != "" {
				if r.Status != omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_REJECTED || r.RejectionCode != caseValue.rejection || r.Candidate != nil || r.UnfilledQuantity != nil {
					t.Fatal("refusal mismatch")
				}
			} else if r.Status != omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_OK || r.RejectionCode != "" || !proto.Equal(r.Candidate, caseValue.candidate) || !proto.Equal(r.UnfilledQuantity, caseValue.quantity) {
				t.Fatal("complete state/optional quantity mismatch")
			}
		})
	}
	original := terminalCorpus(t)[0].request
	for _, where := range []string{"request", "state", "quantity"} {
		t.Run("unknown-"+where, func(t *testing.T) {
			j := proto.Clone(original).(*omspb.TerminalFoldRequest)
			switch where {
			case "request":
				j.ProtoReflect().SetUnknown([]byte{0xf8, 0x3e, 1})
			case "state":
				j.State.ProtoReflect().SetUnknown([]byte{0xf8, 0x3e, 1})
			case "quantity":
				j.State.LeavesQuantity = d(0, 0)
				j.State.LeavesQuantity.ProtoReflect().SetUnknown([]byte{0x78, 1})
			}
			wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = runWorker(t, bin, wire, "--terminal"); e == nil {
				t.Fatal("unknown wire silently discarded")
			}
		})
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runWorker(t, bin, wire, "--unsupported"); err == nil {
		t.Fatal("unsupported mode accepted")
	}
	for _, mutate := range []func(*omspb.TerminalFoldRequest){func(j *omspb.TerminalFoldRequest) { j.Operation = 99 }, func(j *omspb.TerminalFoldRequest) { j.ExpectedVersion = nil }, func(j *omspb.TerminalFoldRequest) { j.State.Status = 99 }} {
		j := proto.Clone(original).(*omspb.TerminalFoldRequest)
		mutate(j)
		wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := runWorker(t, bin, wire, "--terminal")
		if e != nil {
			t.Fatal("closed input should return typed refusal")
		}
		r := new(omspb.TerminalFoldResult)
		if proto.Unmarshal(raw, r) != nil || r.Status != omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_UNSUPPORTED_INPUT || r.Candidate != nil || r.UnfilledQuantity != nil {
			t.Fatal("unsupported input not closed")
		}
	}
}
