package okx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/tca"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

func priceDomainReport(id, status, price, fee string) []byte {
	return []byte(fmt.Sprintf(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":%q,"state":%q,"fillSz":"1","fillPx":%q,"accFillSz":"1","tradeId":"7","fillFee":%q,"fillFeeCcy":"USDT","uTime":"1700000000000"}]}`, id, status, price, fee))
}

func TestExecutedPriceUserDataRefusesBeforeEffects(t *testing.T) {
	for _, status := range []string{"partially_filled", "filled"} {
		for _, price := range []string{"0", "-1", "-0.000000001"} {
			t.Run(status+"/"+price, func(t *testing.T) {
				view := newOKXOrders("bad", "good")
				capture := &okxCapture{}
				refused := 0
				ing := newOKXUserDataIngester(OKXUserDataConfig{
					Stream: &okxStream{frames: [][]byte{
						priceDomainReport("bad", status, price, ""),
						priceDomainReport("good", "filled", "0.000000001", ""),
					}}, Orders: view, Pub: capture, Venue: "OKX", Tenant: "fund-alpha",
					OnRefused: func(mic, id, reason string) {
						refused++
						if mic != "OKX" || id != "bad" || reason != "invalid_price" {
							t.Errorf("refusal = %s/%s/%s", mic, id, reason)
						}
					},
				})
				if err := ing.Run(context.Background()); !errors.Is(err, io.EOF) {
					t.Fatalf("refusal ended the session: %v", err)
				}
				if refused != 1 || len(capture.events) != 1 {
					t.Fatalf("refusals=%d events=%d, want one refusal and the other order's fill", refused, len(capture.events))
				}
				fill := capture.events[0].Payload.(*orderpb.OrderFilled).GetFill()
				if fill.GetOrderId() != "good" || dec.FromProto(fill.GetPrice()).Cmp(big.NewRat(1, 1000000000)) != 0 {
					t.Fatalf("positive control = %v", fill)
				}
				st := view.get(t, "bad")
				if st.GetQuarantine() == nil || st.GetQuarantine().GetReason() == "" || st.GetAsOf() != nil || dec.FromProto(st.GetFilledQuantity()).Sign() != 0 {
					t.Fatalf("refused report changed progress or omitted quarantine: %v", st)
				}
				if len(view.errs) != 0 {
					t.Fatal(view.errs)
				}
			})
		}
	}
}

func TestExecutedPriceRESTRefusesWholeConversion(t *testing.T) {
	v := &OKXVenue{mic: "OKX", now: time.Now}
	for _, price := range []string{"0", "-1", "-0.000000001"} {
		t.Run(price, func(t *testing.T) {
			fills, err := v.fills([]okxFill{
				{TradeID: "1", FillSz: "1", FillPx: "100"},
				{TradeID: "2", FillSz: "1", FillPx: price},
			}, okxKanzOrder("o1"), "BTC-USDT")
			if err == nil || fills != nil {
				t.Fatalf("bad second price leaked partial fills: %v, %v", fills, err)
			}
		})
	}
}

func TestExecutedPriceExactPositiveAndNonExecutionControls(t *testing.T) {
	v := &OKXVenue{mic: "OKX", now: time.Now}
	for text, want := range map[string]*big.Rat{
		"0.000000001":   big.NewRat(1, 1000000000),
		"1000000000000": big.NewRat(1000000000000, 1),
	} {
		t.Run(text, func(t *testing.T) {
			fills, err := v.fills([]okxFill{{TradeID: "fee-only", FillSz: "0", FillPx: ""}, {TradeID: "1", FillSz: "1", FillPx: text}}, okxKanzOrder("o1"), "BTC-USDT")
			if err != nil || len(fills) != 1 || dec.FromProto(fills[0].GetPrice()).Cmp(want) != 0 {
				t.Fatalf("exact positive price after fee-only row: %v, %v", fills, err)
			}
			capture := &okxCapture{}
			if err := okxIngesterOver(nil, capture).handle(context.Background(), priceDomainReport("o1", "filled", text, "")); err != nil || len(capture.events) != 1 {
				t.Fatalf("positive WS control: %v, %v", capture.events, err)
			}
			if fill := capture.events[0].Payload.(*orderpb.OrderFilled).GetFill(); dec.FromProto(fill.GetPrice()).Cmp(want) != 0 {
				t.Fatalf("WS price = %v, want %s", fill.GetPrice(), want)
			}
		})
	}
	for _, frame := range []string{
		`{"arg":{"channel":"orders"},"data":[{"clOrdId":"o1","state":"live"}]}`,
		`{"arg":{"channel":"orders"},"data":[{"clOrdId":"o1","state":"filled","fillSz":"0","fillPx":"","tradeId":"fee-only","fillFee":"0.01"}]}`,
	} {
		capture := &okxCapture{}
		if err := okxIngesterOver(nil, capture).handle(context.Background(), []byte(frame)); err != nil || len(capture.events) != 0 {
			t.Fatalf("nonexecution notification = %v, %v", capture.events, err)
		}
	}
}

func TestOKXFeeZeroCurrencyAndUnsupportedControls(t *testing.T) {
	for _, raw := range []string{"", "0"} {
		if money, ok := okxFee(raw, "USDT"); !ok || money != nil {
			t.Fatalf("absent/zero fee = %v, %v", money, ok)
		}
	}
	for _, raw := range []string{"-0", "0.000"} {
		money, ok := okxFee(raw, "BTC")
		if !ok || money == nil || money.GetCurrencyCode() != "BTC" || dec.FromProto(money.GetAmount()).Sign() != 0 {
			t.Fatalf("explicit exact zero = %v, %v", money, ok)
		}
	}
	for _, raw := range []string{"-9223372036854775808", "1.1234567890123456789", "not-a-number"} {
		t.Run(raw, func(t *testing.T) {
			v := &OKXVenue{mic: "OKX", now: time.Now}
			fills, err := v.fills([]okxFill{{TradeID: "1", FillSz: "1", FillPx: "100"}, {TradeID: "2", FillSz: "1", FillPx: "100", Fee: raw, FeeCcy: "USDT"}}, okxKanzOrder("o1"), "BTC-USDT")
			if err == nil || fills != nil {
				t.Fatalf("unsupported fee returned partial REST output: %v, %v", fills, err)
			}
			capture := &okxCapture{}
			if err := okxIngesterOver(nil, capture).handle(context.Background(), priceDomainReport("o1", "filled", "100", raw)); err == nil || len(capture.events) != 0 {
				t.Fatalf("unsupported fee published WS output: %v, %v", capture.events, err)
			}
		})
	}
}

func TestExecutedPriceAndSignedFeesOnRealBroker(t *testing.T) {
	broker := os.Getenv("TEST_NATS_URL")
	if broker == "" {
		t.Skip("requires TEST_NATS_URL real JetStream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.StreamInfo("EXECUTION")
	if err != nil {
		t.Fatal(err)
	}
	start := info.State.LastSeq
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: "okx-fill-domain-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-okx", ProducerVersion: "test", Tenant: "fund-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	identity := time.Now().UnixNano()
	view := newOKXOrders()
	var frames [][]byte
	ids := make([]string, 4)
	for i, fields := range []struct{ price, fee string }{{"0", ""}, {"-1", ""}, {"100", "-0.01"}, {"100", "0.01"}} {
		ids[i] = fmt.Sprintf("okx-domain-%d-%d", identity, i)
		if err := view.store.Record(ctx, okxKanzOrder(ids[i])); err != nil {
			t.Fatal(err)
		}
		frame := priceDomainReport(ids[i], "filled", fields.price, fields.fee)
		frame = []byte(strings.Replace(string(frame), `"tradeId":"7"`, fmt.Sprintf(`"tradeId":"%d"`, identity+int64(i)), 1))
		frames = append(frames, frame)
	}
	refused := 0
	ing := newOKXUserDataIngester(OKXUserDataConfig{Stream: &okxStream{frames: frames}, Orders: view, Pub: producer, Venue: "OKX", Tenant: "fund-alpha", OnRefused: func(string, string, string) { refused++ }})
	if err := ing.Run(ctx); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	info, err = js.StreamInfo("EXECUTION")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*orderpb.Fill{}
	for seq := start + 1; seq <= info.State.LastSeq; seq++ {
		msg, err := js.GetMsg("EXECUTION", seq)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(env.GetPartitionKey(), fmt.Sprintf("okx-domain-%d-", identity)) {
			continue
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var event orderpb.OrderFilled
		if env.GetEventType() != "order.order.filled" || env.GetTenantId() != "fund-alpha" || proto.Unmarshal(payload, &event) != nil {
			t.Fatalf("unexpected own fill envelope: %v", env)
		}
		if seen[event.GetOrderId()] != nil {
			t.Fatal("duplicate fill FACT")
		}
		seen[event.GetOrderId()] = event.GetFill()
	}
	if len(seen) != 2 || seen[ids[0]] != nil || seen[ids[1]] != nil || refused != 2 || view.get(t, ids[0]).GetQuarantine() == nil || view.get(t, ids[1]).GetQuarantine() == nil {
		t.Fatalf("bad prices reached broker or omitted refusal: facts=%v refusals=%d", seen, refused)
	}
	for i, want := range []*big.Rat{big.NewRat(1, 100), big.NewRat(-1, 100)} {
		fill := seen[ids[i+2]]
		if fill == nil || fill.GetFee().GetCurrencyCode() != "USDT" || dec.FromProto(fill.GetFee().GetAmount()).Cmp(want) != 0 {
			t.Fatalf("broker fee = %v, want %s USDT", fill, want)
		}
		st := okxKanzOrder(fill.GetOrderId())
		st.ArrivalPrice = &commonpb.Decimal{Coefficient: 100}
		result, err := tca.Measure(st, []*orderpb.Fill{fill})
		// A +/-0.01 USDT fee on 100 USDT notional is exactly +/-1 bps.
		wantBps := big.NewRat(1-int64(2*i), 1)
		if err != nil || result.ShortfallBps.Cmp(wantBps) != 0 {
			t.Fatalf("broker-to-TCA = %+v, %v; want %s bps", result, err, wantBps)
		}
	}
}

func TestOKXSignedFeeReachesWSRESTAndTCA(t *testing.T) {
	maxRebateBps, ok := new(big.Rat).SetString("-922337203685477580700")
	if !ok {
		t.Fatal("invalid independent maximum-rebate oracle")
	}
	for _, tc := range []struct {
		name, raw string
		want      *big.Rat
		bps       *big.Rat
	}{
		{"charge", "-0.000000003", big.NewRat(3, 1000000000), big.NewRat(3, 10000000)},
		{"rebate", "0.000000003", big.NewRat(-3, 1000000000), big.NewRat(-3, 10000000)},
		{"largest_representable_rebate", "9223372036854775807", big.NewRat(-9223372036854775807, 1), maxRebateBps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &OKXVenue{mic: "OKX", now: time.Now}
			rest, err := v.fills([]okxFill{{TradeID: "7", FillSz: "1", FillPx: "100", Fee: tc.raw, FeeCcy: "USDT"}}, okxKanzOrder("o1"), "BTC-USDT")
			if err != nil || len(rest) != 1 {
				t.Fatalf("REST = %v, %v", rest, err)
			}
			capture := &okxCapture{}
			if err := okxIngesterOver(nil, capture).handle(context.Background(), priceDomainReport("o1", "filled", "100", tc.raw)); err != nil || len(capture.events) != 1 {
				t.Fatalf("WS = %v, %v", capture.events, err)
			}
			ws := capture.events[0].Payload.(*orderpb.OrderFilled).GetFill()
			for source, fill := range map[string]*orderpb.Fill{"REST": rest[0], "WS": ws} {
				t.Run(source, func(t *testing.T) {
					if fill.GetFee().GetCurrencyCode() != "USDT" || dec.FromProto(fill.GetFee().GetAmount()).Cmp(tc.want) != 0 {
						t.Fatalf("canonical expense = %v, want %s USDT", fill.GetFee(), tc.want)
					}
					st := okxKanzOrder("o1")
					st.ArrivalPrice = &commonpb.Decimal{Coefficient: 100}
					result, err := tca.Measure(st, []*orderpb.Fill{fill})
					// One unit at the arrival price costs only the fee: fee / 100 * 10,000 bps.
					if err != nil || result.Fees.Cmp(tc.want) != 0 || result.ShortfallBps.Cmp(tc.bps) != 0 {
						t.Fatalf("TCA = %+v, %v; want fee=%s, bps=%s", result, err, tc.want, tc.bps)
					}
				})
			}
		})
	}
}
