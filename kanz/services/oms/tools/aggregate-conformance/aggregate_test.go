package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
)

type timestamp struct {
	Seconds int64 `json:"seconds"`
	Nanos   int32 `json:"nanos"`
}

func at() timestamp { return timestamp{eventTime.Unix(), int32(eventTime.Nanosecond())} }

type request struct {
	Operation string    `json:"operation"`
	State     []byte    `json:"state,omitempty"`
	Command   []byte    `json:"command,omitempty"`
	Fill      []byte    `json:"fill,omitempty"`
	At        timestamp `json:"at"`
	View      string    `json:"view,omitempty"`
}
type result struct {
	Version  string  `json:"version"`
	Status   string  `json:"status"`
	Code     string  `json:"code"`
	State    []byte  `json:"state"`
	Quantity *string `json:"quantity"`
	Action   string  `json:"action"`
	Reason   string  `json:"reason"`
	Terminal bool    `json:"terminal"`
}
type sample struct {
	name                 string
	request              request
	want                 *orderpb.OrderState
	quantity             *commonpb.Decimal
	code, action, reason string
	terminal             bool
	numerical            bool
}

var eventTime = time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.UTC)

func wire(m proto.Message) []byte {
	b, e := (proto.MarshalOptions{Deterministic: true}).Marshal(m)
	if e != nil {
		panic(e)
	}
	return b
}
func read[T proto.Message](b []byte, m T) T {
	if e := proto.Unmarshal(b, m); e != nil {
		panic(e)
	}
	return m
}
func copyState(s *orderpb.OrderState) *orderpb.OrderState {
	return proto.Clone(s).(*orderpb.OrderState)
}
func goResult(s sample) result {
	st := read(s.request.State, &orderpb.OrderState{})
	command := append([]byte(nil), s.request.Command...)
	state := wire(st)
	now := time.Unix(s.request.At.Seconds, int64(s.request.At.Nanos)).UTC()
	inputs := []proto.Message{}
	snapshots := [][]byte{}
	remember := func(m proto.Message) { inputs = append(inputs, m); snapshots = append(snapshots, wire(m)) }
	var next *orderpb.OrderState
	var qty *commonpb.Decimal
	var err error
	r := result{Status: "ok"}
	switch s.request.Operation {
	case "Accept":
		cmd := read(command, &orderpb.SubmitOrder{})
		remember(cmd)
		next, err = order.Accept(cmd, now)
	case "Route":
		next, err = order.Route(st, now)
	case "Reject":
		next, err = order.Reject(st, now)
	case "ApplyFill":
		fill := read(s.request.Fill, &orderpb.Fill{})
		remember(fill)
		next, err = order.ApplyFill(st, fill, now)
	case "Cancel":
		next, qty, err = order.Cancel(st, now)
	case "Expire":
		next, qty, err = order.Expire(st, now)
	case "Amend":
		cmd := read(command, &orderpb.AmendOrder{})
		remember(cmd)
		next, err = order.Amend(st, cmd, now)
	case "IsTerminal":
		r.Terminal = order.IsTerminal(st)
	case "Reconcile":
		views := map[string]execution.OrderViewState{"Indeterminate": execution.OrderViewIndeterminate, "Unknown": execution.OrderViewUnknown, "Working": execution.OrderViewWorking, "PartiallyFilled": execution.OrderViewPartiallyFilled, "Filled": execution.OrderViewFilled, "Rejected": execution.OrderViewRejected, "Cancelled": execution.OrderViewCancelled, "Expired": execution.OrderViewExpired, "Future": execution.OrderViewState(99)}
		action, reason := order.Reconcile(st, execution.OrderView{State: views[s.request.View], Reason: "fixture-remote-reason"})
		r.Action = action.String()
		if reason != "" {
			switch {
			case order.IsTerminal(st):
				r.Reason = "TerminalOrder"
			case s.request.View == "Unknown" && st.VenueAckAt != nil:
				r.Reason = "AcknowledgementContradiction"
			default:
				r.Reason = "IndeterminateVenue"
			}
		}
	default:
		panic("unknown oracle operation")
	}
	for i, m := range inputs {
		if !bytes.Equal(wire(m), snapshots[i]) {
			panic("Go mutated command/fill input")
		}
	}
	if !bytes.Equal(wire(st), state) || !bytes.Equal(command, s.request.Command) {
		panic("Go mutated aggregate input")
	}
	if err != nil {
		var rejected *order.RejectError
		if errors.As(err, &rejected) {
			r.Status = "rejected"
			r.Code = rejected.Code
		} else {
			r.Status = "numerical"
		}
		return r
	}
	if next != nil {
		r.State = wire(next)
	}
	if qty != nil {
		b, _ := json.Marshal(wire(qty))
		var v string
		if e := json.Unmarshal(b, &v); e != nil {
			panic(e)
		}
		r.Quantity = &v
	}
	return r
}
func assertIndependent(t *testing.T, s sample, r result) {
	t.Helper()
	wantStatus := "ok"
	if s.numerical {
		wantStatus = "numerical"
	}
	if s.code != "" {
		wantStatus = "rejected"
	}
	if r.Status != wantStatus || r.Code != s.code || r.Action != s.action || r.Reason != s.reason || r.Terminal != s.terminal {
		t.Fatalf("independent decision mismatch: %+v", r)
	}
	if s.want != nil && !bytes.Equal(r.State, wire(s.want)) {
		t.Fatal("independent full state mismatch")
	}
	if s.want == nil && len(r.State) != 0 {
		t.Fatal("unexpected state on refusal/decision")
	}
	if s.quantity == nil {
		if r.Quantity != nil {
			t.Fatal("missing quantity became present")
		}
	} else {
		b, _ := json.Marshal(wire(s.quantity))
		var q string
		_ = json.Unmarshal(b, &q)
		if r.Quantity == nil || *r.Quantity != q {
			t.Fatal("terminal quantity not exact")
		}
	}
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("fixture output capacity")
	}
	return b.Buffer.Write(p)
}
func rust(t *testing.T, q request) result {
	t.Helper()
	binary, version := os.Getenv("KANZ_ORDER_AGGREGATE_BIN"), os.Getenv("KANZ_ORDER_AGGREGATE_VERSION")
	if binary == "" {
		t.Skip("KANZ_ORDER_AGGREGATE_BIN unavailable; actual Rust process not run")
	}
	if version == "" {
		t.Fatal("missing exact Rust build stamp")
	}
	input, e := json.Marshal(q)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.WaitDelay = time.Second
	for _, key := range []string{"PATH", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stdin = bytes.NewReader(input)
	out, diagnostics := &boundedOutput{limit: 3 << 20}, &boundedOutput{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = diagnostics
	if e = cmd.Run(); e != nil {
		t.Fatalf("Rust fixture process failed (%T); no raw diagnostic emitted", e)
	}
	var r result
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&r); e != nil {
		t.Fatal("malformed Rust fixture result")
	}
	if dec.Decode(new(any)) != io.EOF {
		t.Fatal("trailing Rust fixture output")
	}
	if r.Version != version {
		t.Fatal("Rust fixture stamp mismatch")
	}
	return r
}
func TestAggregateIndependentOracle(t *testing.T) {
	for _, s := range corpus() {
		t.Run(s.name, func(t *testing.T) { assertIndependent(t, s, goResult(s)) })
	}
}
func TestRustOrderAggregateDifferential(t *testing.T) {
	for _, s := range corpus() {
		t.Run(s.name, func(t *testing.T) {
			g := goResult(s)
			assertIndependent(t, s, g)
			r := rust(t, s.request)
			r.Version = ""
			gb, _ := json.Marshal(g)
			rb, _ := json.Marshal(r)
			if !bytes.Equal(gb, rb) {
				t.Fatalf("Rust/Go complete result differs: operation=%s status=%s/%s code=%s/%s", s.request.Operation, g.Status, r.Status, g.Code, r.Code)
			}
		})
	}
}
func TestRustOrderAggregateRefusesMalformedWire(t *testing.T) {
	for _, op := range []string{"Accept", "Route", "ApplyFill", "Amend"} {
		t.Run(op, func(t *testing.T) {
			q := request{Operation: op, State: wire(baseState()), Command: []byte{0x80}, Fill: []byte{0x80}, At: at()}
			if op == "Route" {
				q.State = []byte{0x80}
			}
			r := rust(t, q)
			if r.Status != "unsupported" || len(r.State) != 0 || r.Quantity != nil {
				t.Fatal("malformed wire was not refused without output")
			}
		})
	}
}

func TestAggregateCanonicalBoundary(t *testing.T) {
	if baseState().ProtoReflect().Descriptor().Fields().ByNumber(33) != nil {
		t.Fatal("field33 is no longer unknown; qualify the newly authoritative cancellation contract explicitly")
	}
	if len(baseState().ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("unknown preservation fixture absent")
	}
}
func TestRustOrderAggregateBoundedRefusals(t *testing.T) {
	for _, name := range []string{"oversized-state", "invalid-nanos", "invalid-seconds", "out-of-domain-fill"} {
		t.Run(name, func(t *testing.T) {
			s := baseState()
			q := rq("Route", s)
			want := "unsupported"
			switch name {
			case "oversized-state":
				s.OrderId = strings.Repeat("x", (1<<20)+1)
				q.State = wire(s)
				want = "capacity"
			case "invalid-nanos":
				q.At.Nanos = -1
			case "invalid-seconds":
				q.At.Seconds = 253402300800
			case "out-of-domain-fill":
				q.Operation = "ApplyFill"
				q.Fill = wire(&orderpb.Fill{Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: 65}})
			}
			r := rust(t, q)
			if r.Status != want || len(r.State) != 0 || r.Quantity != nil {
				t.Fatal("bounded refusal did not retain no-output boundary")
			}
		})
	}
}
