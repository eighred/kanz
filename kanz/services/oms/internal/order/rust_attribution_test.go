package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/execution/tca"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/schedule"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type attrTime struct {
	Seconds int64 `json:"seconds"`
	Nanos   int32 `json:"nanos"`
}
type attrObservation struct {
	Instrument  string  `json:"instrument"`
	Venue       string  `json:"venue"`
	From        string  `json:"from_nanos"`
	To          string  `json:"to_nanos"`
	Numerator   *string `json:"numerator"`
	Denominator *string `json:"denominator"`
}
type attrRequest struct {
	Operation string            `json:"operation"`
	State     []byte            `json:"state,omitempty"`
	Children  [][]byte          `json:"children,omitempty"`
	At        *attrTime         `json:"at,omitempty"`
	Tape      []attrObservation `json:"tape,omitempty"`
	Parent    string            `json:"parent_id,omitempty"`
	Index     int64             `json:"index,omitempty"`
	Slices    int64             `json:"slices,omitempty"`
	From      *string           `json:"from_nanos,omitempty"`
	To        *string           `json:"to_nanos,omitempty"`
}
type attrExact struct {
	Quantity  string   `json:"quantity"`
	Notional  string   `json:"notional"`
	Average   string   `json:"average"`
	Arrival   string   `json:"arrival"`
	Shortfall string   `json:"shortfall"`
	Legs      []string `json:"legs"`
	Slices    int      `json:"slices"`
	Measured  int      `json:"measured"`
}
type attrPart struct {
	Overall   *string `json:"overall"`
	Maximum   *string `json:"maximum"`
	Index     int     `json:"index"`
	Venue     string  `json:"venue"`
	Intervals int     `json:"intervals"`
	Measured  int     `json:"measured"`
}
type attrResult struct {
	Version       string     `json:"version"`
	Status        string     `json:"status"`
	Code          string     `json:"code"`
	Outcome       string     `json:"outcome"`
	Payload       []byte     `json:"payload"`
	Attribution   *attrExact `json:"attribution"`
	Participation *attrPart  `json:"participation"`
	ChildID       string     `json:"child_id"`
	Interval      []string   `json:"interval"`
}
type attrChildren struct {
	Store
	children []*orderpb.OrderState
}

func (s *attrChildren) ListByParent(context.Context, string) ([]*orderpb.OrderState, error) {
	return s.children, nil
}

type attrTape []attrObservation

func (s attrTape) Volume(instrument, venue string, from, to time.Time) (*big.Rat, bool) {
	for _, row := range s {
		if row.Instrument == instrument && row.Venue == venue && row.From == attrNanos(from) && row.To == attrNanos(to) {
			if row.Numerator == nil || row.Denominator == nil {
				return nil, false
			}
			q, ok := new(big.Rat).SetString(*row.Numerator + "/" + *row.Denominator)
			return q, ok
		}
	}
	return nil, false
}
func attrNanos(t time.Time) string {
	n := new(big.Int).Mul(big.NewInt(t.Unix()), big.NewInt(1_000_000_000))
	return n.Add(n, big.NewInt(int64(t.Nanosecond()))).String()
}
func attrEpoch(t *testing.T, s string) time.Time {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatal("invalid test nanos")
	}
	sec, rem := new(big.Int), new(big.Int)
	sec.QuoRem(n, big.NewInt(1_000_000_000), rem)
	if !sec.IsInt64() {
		t.Fatal("test seconds overflow")
	}
	at := time.Unix(sec.Int64(), rem.Int64()).UTC()
	if !timestamppb.New(at).IsValid() {
		t.Fatal("test timestamp out of canonical domain")
	}
	return at
}
func attrWire(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, e := (proto.MarshalOptions{Deterministic: true}).Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func attrRead(t *testing.T, b []byte) *orderpb.OrderState {
	t.Helper()
	var st orderpb.OrderState
	if e := proto.Unmarshal(b, &st); e != nil {
		t.Fatal(e)
	}
	return &st
}
func attrString(r *big.Rat) *string {
	if r == nil {
		return nil
	}
	s := r.RatString()
	return &s
}

func attrGo(t *testing.T, q attrRequest) attrResult {
	t.Helper()
	out := attrResult{Status: "ok"}
	if q.Operation == "Schedule" {
		p := algo.Plan{Start: attrEpoch(t, *q.From), End: attrEpoch(t, *q.To), Slices: int(q.Slices)}
		out.ChildID = schedule.ChildID(q.Parent, int(q.Index))
		from, to, ok := p.Interval(int(q.Index))
		if ok {
			out.Interval = []string{attrNanos(from), attrNanos(to)}
		}
		return out
	}
	st := attrRead(t, q.State)
	children := make([]*orderpb.OrderState, 0, len(q.Children))
	for _, raw := range q.Children {
		children = append(children, attrRead(t, raw))
	}
	svc := &Service{store: &attrChildren{Store: NewMemoryStore(), children: children}, emitter: NewEmitter(&fakeBus{}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if q.Tape != nil {
		svc.realisedVolume = attrTape(q.Tape)
	}
	now := time.Unix(q.At.Seconds, int64(q.At.Nanos)).UTC()
	records := svc.attributionRecords(bus.WithTenantID(context.Background(), "attribution-fixture"), st, now)
	if st.ParentOrderId != "" || !IsTerminal(st) {
		out.Outcome = "Ignored"
		return out
	}
	slices, actualChildren, ok := svc.attributionSlices(context.Background(), st)
	if !ok {
		t.Fatal("oracle child read failed")
	}
	a, err := tca.Attribute(st.Side, dec.FromProto(st.ArrivalPrice), slices)
	if err != nil {
		switch {
		case errors.Is(err, tca.ErrNoArrivalMark):
			out.Outcome = "NoArrival"
		case errors.Is(err, tca.ErrNoFills):
			out.Outcome = "NoFills"
		default:
			out.Outcome = "MissingFillPrice"
		}
		if len(records) != 0 {
			t.Fatal("unmeasurable Go owner emitted record")
		}
		return out
	}
	p := svc.measureParticipation(st, actualChildren)
	out.Attribution = &attrExact{Quantity: a.FilledQuantity.RatString(), Notional: a.NotionalTraded.RatString(), Average: a.AveragePrice.RatString(), Arrival: a.ArrivalPrice.RatString(), Shortfall: a.ShortfallBps.RatString(), Slices: a.Slices, Measured: a.Measured}
	if a.Quality == tca.QualityDecomposed {
		out.Attribution.Legs = []string{a.SpreadBps.RatString(), a.ImpactBps.RatString(), a.TimingBps.RatString()}
	}
	out.Participation = &attrPart{Overall: attrString(p.Overall), Maximum: attrString(p.MaxSlice), Index: p.MaxSliceIndex, Venue: p.MaxSliceVenue, Intervals: p.Intervals, Measured: p.Measured}
	if len(records) == 0 {
		out.Outcome = "Unrepresentable"
	} else {
		if len(records) != 1 {
			t.Fatal("duplicate Go attribution")
		}
		out.Outcome = "Captured"
		out.Payload = records[0].Payload
		var candidate orderpb.ExecutionAttributionRecorded
		if err := proto.Unmarshal(out.Payload, &candidate); err != nil {
			t.Fatal(err)
		}
		if _, valid := dec.InDomainDeep(&candidate); !valid {
			out.Outcome = "CapturedOutsideCanonicalDomain"
		}
	}
	return out
}

type attrOutput struct {
	bytes.Buffer
	limit int
}

func (b *attrOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("bounded fixture output")
	}
	return b.Buffer.Write(p)
}
func attrRust(t *testing.T, q attrRequest) attrResult {
	t.Helper()
	binary, version := os.Getenv("KANZ_ORDER_ATTRIBUTION_BIN"), os.Getenv("KANZ_ORDER_ATTRIBUTION_VERSION")
	if binary == "" {
		t.Skip("actual Rust attribution fixture unavailable")
	}
	if len(version) != 40 {
		t.Fatal("exact source stamp required")
	}
	body, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(body)
	for _, key := range []string{"PATH", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	stdout, stderr := &attrOutput{limit: 2 << 20}, &attrOutput{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("Rust attribution process failure (%T); raw diagnostics withheld", err)
	}
	var got attrResult
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&got) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("invalid bounded attribution response")
	}
	if got.Version != version {
		t.Fatal("source stamp differs")
	}
	got.Version = ""
	return got
}

func attrCases(t *testing.T) map[string]attrRequest {
	t.Helper()
	at := &attrTime{Seconds: 1_770_092_706, Nanos: 7}
	base := &orderpb.OrderState{OrderId: "decision", PortfolioId: "pf", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, OrderedQuantity: d(3, 0), FilledQuantity: d(3, 0), AverageFillPrice: d(103, 0), ArrivalPrice: d(100, 0), ReleasePrice: d(101, 0), ReleaseBid: d(100, 0), ReleaseAsk: d(102, 0)}
	cases := map[string]attrRequest{}
	add := func(name string, change func(*orderpb.OrderState)) {
		st := proto.Clone(base).(*orderpb.OrderState)
		if change != nil {
			change(st)
		}
		cases[name] = attrRequest{Operation: "Measure", State: attrWire(t, st), At: at}
	}
	add("buy-decomposed", nil)
	add("sell-decomposed", func(s *orderpb.OrderState) { s.Side = orderpb.Side_SIDE_SELL })
	add("future-side-preserved", func(s *orderpb.OrderState) { s.Side = 99 })
	add("missing-arrival", func(s *orderpb.OrderState) { s.ArrivalPrice = nil })
	add("zero-arrival", func(s *orderpb.OrderState) { s.ArrivalPrice = d(0, 0) })
	add("no-fills", func(s *orderpb.OrderState) { s.FilledQuantity = nil })
	add("missing-price", func(s *orderpb.OrderState) { s.AverageFillPrice = nil })
	add("unquoted-total-only", func(s *orderpb.OrderState) { s.ReleaseAsk = nil })
	add("crossed-quote-total-only", func(s *orderpb.OrderState) { s.ReleaseAsk = d(99, 0) })
	add("child-ignored", func(s *orderpb.OrderState) { s.ParentOrderId = "parent" })
	add("nonterminal-ignored", func(s *orderpb.OrderState) { s.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED })
	add("cancelled-partial", func(s *orderpb.OrderState) { s.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED })
	add("expired-partial", func(s *orderpb.OrderState) { s.Status = orderpb.OrderStatus_ORDER_STATUS_EXPIRED })
	add("small-value-quantization", func(s *orderpb.OrderState) { s.AverageFillPrice = d(1, -10); s.ReleaseAsk = nil })
	add("go-wire-exponent114-outside-domain", func(s *orderpb.OrderState) {
		s.AverageFillPrice = d(1, 64)
		s.ArrivalPrice = d(1, -64)
		s.ReleaseAsk = nil
	})
	add("sell-go-wire-exponent114-outside-domain", func(s *orderpb.OrderState) {
		s.Side = orderpb.Side_SIDE_SELL
		s.AverageFillPrice = d(1, 64)
		s.ArrivalPrice = d(1, -64)
		s.ReleaseAsk = nil
	})
	add("copied-unknown-metadata", func(s *orderpb.OrderState) {
		s.OrderedQuantity.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x81, 0})
		s.ArrivalAt = &timestamppb.Timestamp{Seconds: 1}
		s.ArrivalAt.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 2})
		s.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 3})
	})
	add("copied-known-number-wrong-wire", func(s *orderpb.OrderState) {
		s.OrderedQuantity.ProtoReflect().SetUnknown([]byte{0x0a, 1, 7})
		s.ArrivalAt = &timestamppb.Timestamp{Seconds: 1}
		s.ArrivalAt.ProtoReflect().SetUnknown([]byte{0x0a, 1, 9})
	})
	duplicate := cases["buy-decomposed"]
	field := base.ProtoReflect().Descriptor().Fields().ByName("ordered_quantity")
	duplicate.State = protowire.AppendBytes(protowire.AppendTag(bytes.Clone(duplicate.State), protowire.Number(field.Number()), protowire.BytesType), []byte{0xf8, 0x07, 0x81, 0})
	cases["duplicate-copied-message-unknown-only"] = duplicate
	parent := proto.Clone(base).(*orderpb.OrderState)
	parent.ExecutionSchedule = &orderpb.ExecutionSchedule{Algo: orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, SliceCount: 3, WindowStart: &timestamppb.Timestamp{Seconds: 10}, WindowEnd: &timestamppb.Timestamp{Seconds: 10, Nanos: 10}, MaxParticipationRate: d(2, -1)}
	children := []*orderpb.OrderState{}
	for _, i := range []int{2, 0} {
		c := proto.Clone(base).(*orderpb.OrderState)
		c.ParentOrderId = parent.OrderId
		c.OrderId = schedule.ChildID(parent.OrderId, i)
		c.Venue = "venue"
		children = append(children, c)
	}
	q := attrRequest{Operation: "Measure", State: attrWire(t, parent), At: at}
	for _, c := range children {
		q.Children = append(q.Children, attrWire(t, c))
	}
	cases["scheduled-holes-no-tape"] = q
	n, den := "30", "1"
	q.Tape = []attrObservation{{Instrument: parent.InstrumentId, Venue: "venue", From: "10000000006", To: "10000000010", Numerator: &n, Denominator: &den}}
	cases["scheduled-partial-tape"] = q
	q.Tape = append(q.Tape, attrObservation{Instrument: parent.InstrumentId, Venue: "venue", From: "10000000000", To: "10000000003", Numerator: &n, Denominator: &den})
	cases["scheduled-tie-first-child"] = q
	return cases
}

func TestAttributionIndependentPureOracle(t *testing.T) {
	for name, q := range attrCases(t) {
		t.Run(name, func(t *testing.T) {
			before, _ := json.Marshal(q)
			got := attrGo(t, q)
			after, _ := json.Marshal(q)
			if !bytes.Equal(before, after) {
				t.Fatal("oracle mutated input")
			}
			if name == "buy-decomposed" && (got.Attribution.Shortfall != "300" || !reflect.DeepEqual(got.Attribution.Legs, []string{"100", "100", "100"})) {
				t.Fatal("independent arrival-notional golden differs")
			}
			if name == "scheduled-partial-tape" && (got.Participation.Overall == nil || *got.Participation.Overall != "1/10" || got.Participation.Measured != 1) {
				t.Fatal("independent partial tape golden differs")
			}
		})
	}
}
func TestRustAttributionDifferential(t *testing.T) {
	for name, q := range attrCases(t) {
		t.Run(name, func(t *testing.T) {
			want := attrGo(t, q)
			got := attrRust(t, q)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("attribution differs: status=%s outcome=%s", got.Status, got.Outcome)
			}
		})
	}
}
func TestRustAttributionScheduleEdges(t *testing.T) {
	t.Run("long-parent-last-supported-index", func(t *testing.T) {
		from, to := "0", "100000"
		q := attrRequest{Operation: "Schedule", Parent: strings.Repeat("parent:", 60_000), Index: 99_999, Slices: 100_000, From: &from, To: &to}
		want := attrGo(t, q)
		got := attrRust(t, q)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("long canonical parent identity/interval differs")
		}
	})
	for _, s := range []struct {
		name, from, to string
		slices, index  int64
	}{{"floor", "0", "10", 3, 1}, {"negative-epoch", "-10", "0", 3, 1}, {"duration-saturation", "-62135596800000000000", "253402300799999999999", 3, 2}, {"zero-count", "0", "10", 0, 0}, {"last-index", "0", "10", 3, 2}, {"outside-index", "0", "10", 3, 3}, {"negative-index", "0", "10", 3, -1}, {"empty-window", "10", "10", 3, 0}} {
		t.Run(s.name, func(t *testing.T) {
			q := attrRequest{Operation: "Schedule", Parent: "parent:unicode-α", Index: s.index, Slices: s.slices, From: &s.from, To: &s.to}
			want := attrGo(t, q)
			got := attrRust(t, q)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("canonical schedule identity/interval differs")
			}
		})
	}
}

func TestRustAttributionBoundedRefusals(t *testing.T) {
	base := attrCases(t)["buy-decomposed"]
	for _, name := range []string{"malformed-state", "oversized-state", "invalid-timestamp", "duplicate-children", "foreign-child", "schedule-index-capacity", "duplicate-tape-observation", "oversized-tape-rational"} {
		t.Run(name, func(t *testing.T) {
			q := base
			code := "InvalidInput"
			switch name {
			case "malformed-state":
				q.State = []byte{0x0a, 0xff}
			case "oversized-state":
				q.State = bytes.Repeat([]byte{0}, (1<<20)+1)
				code = "Capacity"
			case "invalid-timestamp":
				q.At = &attrTime{Seconds: 1, Nanos: 1_000_000_000}
			case "duplicate-children", "foreign-child":
				st := attrRead(t, q.State)
				child := proto.Clone(st).(*orderpb.OrderState)
				child.ParentOrderId = st.OrderId
				child.OrderId = "child"
				if name == "foreign-child" {
					child.ParentOrderId = "other"
				}
				q.Children = [][]byte{attrWire(t, child)}
				if name == "duplicate-children" {
					q.Children = append(q.Children, q.Children[0])
				}
			case "schedule-index-capacity":
				q = attrCases(t)["scheduled-holes-no-tape"]
				st := attrRead(t, q.State)
				st.ExecutionSchedule.SliceCount = 100001
				q.State = attrWire(t, st)
				code = "Capacity"
			case "duplicate-tape-observation":
				q = attrCases(t)["scheduled-partial-tape"]
				q.Tape = append(q.Tape, q.Tape[0])
			case "oversized-tape-rational":
				q = attrCases(t)["scheduled-partial-tape"]
				huge := new(big.Int).Lsh(big.NewInt(1), 4096).String()
				q.Tape = append([]attrObservation(nil), q.Tape...)
				q.Tape[0].Numerator = &huge
				code = "Capacity"
			}
			got := attrRust(t, q)
			if got.Status != "error" || got.Code != code || got.Payload != nil || got.Attribution != nil || got.Participation != nil {
				t.Fatal("bounded refusal returned output or wrong category")
			}
		})
	}
}

func TestAttributionProjectionInputMetadataWitness(t *testing.T) {
	for name, q := range attrCases(t) {
		if !strings.Contains(name, "copied") && !strings.Contains(name, "exponent114") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			got := attrGo(t, q)
			var payload orderpb.ExecutionAttributionRecorded
			if proto.Unmarshal(got.Payload, &payload) != nil {
				t.Fatal("Go owner emitted invalid candidate")
			}
			if strings.Contains(name, "exponent114") {
				if got.Outcome != "CapturedOutsideCanonicalDomain" || payload.ShortfallBps.Exponent != 114 {
					t.Fatal("noncanonical candidate classification lost")
				}
				return
			}
			st := attrRead(t, q.State)
			if !proto.Equal(st.OrderedQuantity, payload.OrderedQuantity) || !proto.Equal(st.ArrivalAt, payload.ArrivalAt) {
				t.Fatal("Go copied metadata witness differs")
			}
		})
	}
}

// Actual Producer framing control, not a broker or downstream financial-domain
// acceptance claim. Never use this capture to claim real messaging integration.
type attributionFrameCapture struct{ message bus.Message }

func (c *attributionFrameCapture) Publish(_ context.Context, m bus.Message) error {
	c.message = m
	return nil
}
func (*attributionFrameCapture) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("unused subscription")
}
func (*attributionFrameCapture) Close() error { return nil }

func TestAttributionCanonicalOutputDomainWitness(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId: "attribution-domain", PortfolioId: "pf", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		Status:          orderpb.OrderStatus_ORDER_STATUS_FILLED,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 1}, FilledQuantity: &commonpb.Decimal{Coefficient: 1},
		AverageFillPrice: &commonpb.Decimal{Coefficient: 1, Exponent: 64}, ArrivalPrice: &commonpb.Decimal{Coefficient: 1, Exponent: -64},
	}
	if _, ok := dec.InDomainDeep(st); !ok {
		t.Fatal("input must be canonical-domain positive control")
	}
	a, err := tca.Attribute(st.Side, dec.FromProto(st.ArrivalPrice), []tca.Slice{sliceOf(st)})
	if err != nil {
		t.Fatal(err)
	}
	// Independently: ((10^64 - 10^-64) / 10^-64) * 10000.
	want := new(big.Int).Exp(big.NewInt(10), big.NewInt(132), nil)
	want.Sub(want, big.NewInt(10000))
	if a.ShortfallBps.Cmp(new(big.Rat).SetInt(want)) != 0 {
		t.Fatal("arrival-notional exact rational mismatch")
	}
	svc := &Service{store: NewMemoryStore(), emitter: NewEmitter(&fakeBus{}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	at := time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)
	ctx := bus.WithTenantID(context.Background(), "attribution-fixture")
	records := svc.attributionRecords(ctx, st, at)
	if len(records) != 1 {
		t.Fatal("actual Go owner did not capture the candidate")
	}
	var payload orderpb.ExecutionAttributionRecorded
	if err := proto.Unmarshal(records[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ShortfallBps.GetExponent() != 114 || payload.ShortfallBps.GetCoefficient() != 1_000_000_000_000_000_000 {
		t.Fatal("actual Go eight-place/fit candidate differs")
	}
	if _, ok := dec.InDomainDeep(&payload); ok {
		t.Fatal("candidate must explicitly fail canonical financial input domain")
	}
	event, err := records[0].Event()
	if err != nil {
		t.Fatal(err)
	}
	capture := &attributionFrameCapture{}
	producer, err := bus.NewProducer(capture, bus.ProducerConfig{Source: "oms/attribution-oracle", ProducerVersion: "source-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Publish(ctx, event); err != nil {
		t.Fatal("actual producer refused candidate before client boundary")
	}
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(capture.message.Body, &frame); err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(frame.Envelope); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame.Payload, records[0].Payload) {
		t.Fatal("missing framed payload")
	}
}
