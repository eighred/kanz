// Command fill-conformance exports fresh canonical Go fold observations after
// checking them against separately specified rational expectations.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type observation struct {
	Name      string `json:"name"`
	Request   []byte `json:"request"`
	Candidate []byte `json:"candidate,omitempty"`
	Rejection string `json:"rejection,omitempty"`
}
type example struct {
	name                               string
	state                              *orderpb.OrderState
	fill                               *orderpb.Fill
	filled, leaves, average, rejection string
}

var instant = time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.UTC)

func d(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }
func base() *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: "order", PortfolioId: "portfolio", InstrumentId: "instrument", Side: 1, OrderType: 4, TimeInForce: 5, OrderedQuantity: d(10, 0), LeavesQuantity: d(10, 0), FilledQuantity: d(0, 0), Status: 2, LimitPrice: d(123, -2), StopPrice: d(122, -2), ExpireAt: timestamppb.New(instant.Add(time.Hour)), AsOf: timestamppb.New(instant.Add(-time.Minute)), Venue: "fixture", VenueAccountId: "account", VenueAckAt: timestamppb.New(instant.Add(-time.Second)), Quarantine: &orderpb.OrderQuarantine{Reason: "retained", At: timestamppb.New(instant)}, CancelAnnouncedAt: timestamppb.New(instant), OutcomeAnnouncedAt: timestamppb.New(instant), AcceptedAnnouncedAt: timestamppb.New(instant), ArrivalPrice: d(100, 0), ArrivalAt: timestamppb.New(instant), ParentOrderId: "parent", Leverage: d(2, 0), MarginMode: 2, ReleasePrice: d(101, 0), ReleaseAt: timestamppb.New(instant), ReleaseBid: d(99, 0), ReleaseAsk: d(102, 0), ExecutionSchedule: &orderpb.ExecutionSchedule{Algo: 1, SliceCount: 2, WindowStart: timestamppb.New(instant), WindowEnd: timestamppb.New(instant.Add(time.Hour))}}
}
func fill(q, p *commonpb.Decimal) *orderpb.Fill {
	return &orderpb.Fill{FillId: "fill", OrderId: "order", InstrumentId: "instrument", Side: 1, Quantity: q, Price: p, Venue: "fixture", VenueAccountId: "account", VenueExecutionId: "execution", ExecutedAt: timestamppb.New(instant), Fee: &commonpb.Money{Amount: d(1, -3), CurrencyCode: "USD"}}
}

// Quantization oracle uses decimal text rounding, then decimal digit removal;
// it calls neither dec.ToProtoScaled nor the aggregate arithmetic. Sequential
// rescaling (including double rounding) is the existing representation rule.
func quantize(value string) *commonpb.Decimal {
	r, ok := new(big.Rat).SetString(value)
	if !ok {
		panic("bad oracle literal")
	}
	whole, fraction, found := strings.Cut(r.FloatString(8), ".")
	if !found || len(fraction) != 8 {
		panic("bad oracle fixed-scale decimal")
	}
	text := whole + fraction
	n, ok := new(big.Int).SetString(text, 10)
	if !ok {
		panic("bad oracle decimal")
	}
	exponent := int32(-8)
	for !n.IsInt64() {
		negative := n.Sign() < 0
		s := new(big.Int).Abs(n).String()
		round := s[len(s)-1] >= '5'
		n.SetString(s[:len(s)-1], 10)
		if round {
			n.Add(n, big.NewInt(1))
		}
		if negative {
			n.Neg(n)
		}
		exponent++
	}
	return d(n.Int64(), exponent)
}
func examples() []example {
	var out []example
	add := func(name string, s *orderpb.OrderState, f *orderpb.Fill, a, b, c, code string) {
		out = append(out, example{name, s, f, a, b, c, code})
	}
	add("partial", base(), fill(d(3, 0), d(20, 0)), "3", "7", "20", "")
	s := base()
	s.FilledQuantity = d(3, 0)
	s.LeavesQuantity = d(7, 0)
	s.AverageFillPrice = d(20, 0)
	s.Status = 3
	add("weighted-full", s, fill(d(7, 0), d(30, 0)), "10", "0", "27", "")
	for _, p := range []int64{4, 5, 6, -4, -5, -6} {
		add(fmt.Sprintf("half-boundary-%d", p), base(), fill(d(1, 0), d(p, -9)), "1", "9", fmt.Sprintf("%d/1000000000", p), "")
	}
	add("missing-price-is-legacy-zero", base(), fill(d(1, 0), nil), "1", "9", "0", "")
	s = base()
	s.FilledQuantity = d(2, 0)
	s.LeavesQuantity = d(8, 0)
	s.AverageFillPrice = nil
	add("missing-prior-average", s, fill(d(1, 0), d(1, 0)), "3", "7", "1/3", "")
	s = base()
	s.OrderedQuantity = d(9223372036854775807, 0)
	s.LeavesQuantity = proto.Clone(s.OrderedQuantity).(*commonpb.Decimal)
	add("large-representability", s, fill(d(9223372036854775807, 0), d(9223372036854775807, 0)), "9223372036854775807", "0", "9223372036854775807", "")
	s = base()
	s.FilledQuantity = d(1, 0)
	s.LeavesQuantity = d(9, 0)
	s.AverageFillPrice = d(2, 20)
	add("double-rounding", s, fill(d(1, 0), d(898, 0)), "2", "8", "100000000000000000449", "")
	add("tiny-quantity-rounds-zero", base(), fill(d(1, -9), d(10, 0)), "1/1000000000", "9999999999/1000000000", "10", "")
	for _, status := range []orderpb.OrderStatus{4, 5, 6, 7} {
		s = base()
		s.Status = status
		add(fmt.Sprintf("terminal-%d", status), s, fill(d(1, 0), d(1, 0)), "", "", "", "ORDER_TERMINAL")
	}
	for _, q := range []*commonpb.Decimal{nil, d(0, 0), d(-1, 0)} {
		add(fmt.Sprintf("invalid-quantity-%d", len(out)), base(), fill(q, d(1, 0)), "", "", "", "INVALID_FILL")
	}
	add("overfill", base(), fill(d(1000000001, -8), d(1, 0)), "", "", "", "OVERFILL")
	s = base()
	s.LeavesQuantity = nil
	add("missing-leaves", s, fill(d(1, 0), d(1, 0)), "", "", "", "OVERFILL")
	for _, status := range []orderpb.OrderStatus{0, 1, 8} {
		s = base()
		s.Status = status
		add(fmt.Sprintf("nonterminal-%d", status), s, fill(d(1, 0), d(2, 0)), "1", "9", "2", "")
	}
	s = base()
	s.FilledQuantity = nil
	add("missing-filled-is-legacy-zero", s, fill(d(1, 0), d(2, 0)), "1", "9", "2", "")
	// Build each sequential input from analytic expectations, never Go output.
	s = base()
	s.OrderedQuantity = d(4, 0)
	s.LeavesQuantity = d(4, 0)
	for i, average := range []string{"1", "1/2", "1/3", "99999999/400000000"} {
		price := d(0, 0)
		if i == 0 {
			price = d(1, 0)
		}
		add(fmt.Sprintf("rounded-sequence-%d", i), s, fill(d(1, 0), price), fmt.Sprint(i+1), fmt.Sprint(3-i), average, "")
		s = proto.Clone(s).(*orderpb.OrderState)
		s.FilledQuantity = quantize(fmt.Sprint(i + 1))
		s.LeavesQuantity = quantize(fmt.Sprint(3 - i))
		s.AverageFillPrice = quantize(average)
		s.Status = 3
	}
	return out
}
func request(e example) *omspb.FillFoldRequest {
	r := &omspb.FillFoldRequest{CaptureId: e.name, TenantId: "tenant", SourceRevision: "fixture-source", ExpectedVersion: proto.Int64(7), State: e.state, Fill: e.fill, EventTime: timestamppb.New(instant), CapturedAt: timestamppb.New(instant.Add(time.Second)), Path: "live"}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	r.CaptureDigest = h[:]
	return r
}
func checked(e example) (observation, error) {
	before := proto.Clone(e.state)
	beforeFill := proto.Clone(e.fill)
	actual, err := order.ApplyFill(e.state, e.fill, instant)
	if !proto.Equal(before, e.state) || !proto.Equal(beforeFill, e.fill) {
		return observation{}, errors.New("aggregate mutated input")
	}
	var code string
	if err != nil {
		var rejected *order.RejectError
		if !errors.As(err, &rejected) {
			return observation{}, err
		}
		code = rejected.Code
	}
	if code != e.rejection {
		return observation{}, fmt.Errorf("%s rejection %q want %q", e.name, code, e.rejection)
	}
	if code == "" {
		want := proto.Clone(e.state).(*orderpb.OrderState)
		want.FilledQuantity = quantize(e.filled)
		want.LeavesQuantity = quantize(e.leaves)
		want.AverageFillPrice = quantize(e.average)
		want.AsOf = timestamppb.New(instant)
		want.Status = 3
		remaining, _ := new(big.Rat).SetString(e.leaves)
		if remaining.Sign() == 0 {
			want.Status = 4
		}
		if !proto.Equal(actual, want) {
			return observation{}, fmt.Errorf("%s independent full-state oracle disagreement", e.name)
		}
	} else if actual != nil {
		return observation{}, errors.New("refusal returned candidate")
	}
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request(e))
	if err != nil {
		return observation{}, err
	}
	var candidate []byte
	if actual != nil {
		candidate, err = (proto.MarshalOptions{Deterministic: true}).Marshal(actual)
	}
	return observation{e.name, wire, candidate, code}, err
}
func corpus() ([]observation, error) {
	var out []observation
	for _, e := range examples() {
		v, err := checked(e)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func main() {
	values, err := corpus()
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(struct {
			Version int           `json:"version"`
			Cases   []observation `json:"cases"`
		}{1, values})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
