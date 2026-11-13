package binance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func priceDomainReport(id, status, price string) []byte {
	return []byte(fmt.Sprintf(`{"e":"executionReport","s":"BTCUSDT","c":%q,"x":"TRADE","X":%q,"l":"1","L":%q,"z":"1","t":7,"T":1700000000000}`, id, status, price))
}

func TestExecutedPriceUserDataRefusesBeforeEffects(t *testing.T) {
	for _, status := range []string{"PARTIALLY_FILLED", "FILLED"} {
		for _, price := range []string{"0", "-1", "-0.000000001"} {
			t.Run(status+"/"+price, func(t *testing.T) {
				view := newFakeOrders("bad", "good")
				capture := &reconCapture{}
				refused := 0
				ing := newUserDataIngester(UserDataConfig{
					Stream: &fakeStream{frames: [][]byte{
						priceDomainReport("bad", status, price),
						priceDomainReport("good", "FILLED", "0.000000001"),
					}}, Orders: view, Pub: capture, Venue: "BINANCE", Tenant: "fund-alpha",
					OnRefused: func(mic, id, reason string) {
						refused++
						if mic != "BINANCE" || id != "bad" || reason != "invalid_price" {
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
	v := &BinanceVenue{mic: "BINANCE", now: time.Now}
	for _, price := range []string{"0", "-1", "-0.000000001"} {
		t.Run(price, func(t *testing.T) {
			fills, err := v.fills(&orderResponse{Symbol: "BTCUSDT", Fills: []orderFill{
				{TradeID: 1, Qty: "1", Price: "100"},
				{TradeID: 2, Qty: "1", Price: price},
			}}, kanzOrder("o1"))
			if err == nil || fills != nil {
				t.Fatalf("bad second price leaked partial fills: %v, %v", fills, err)
			}
		})
	}
}

func TestExecutedPriceExactPositiveControls(t *testing.T) {
	v := &BinanceVenue{mic: "BINANCE", now: time.Now}
	for text, want := range map[string]*big.Rat{
		"0.000000001":   big.NewRat(1, 1000000000),
		"1000000000000": big.NewRat(1000000000000, 1),
	} {
		t.Run(text, func(t *testing.T) {
			fills, err := v.fills(&orderResponse{Symbol: "BTCUSDT", Fills: []orderFill{{TradeID: 1, Qty: "1", Price: text}}}, kanzOrder("o1"))
			if err != nil || len(fills) != 1 || dec.FromProto(fills[0].GetPrice()).Cmp(want) != 0 {
				t.Fatalf("exact positive price: %v, %v", fills, err)
			}
			capture := &reconCapture{}
			if err := ingesterOver(nil, capture).handle(context.Background(), priceDomainReport("o1", "FILLED", text)); err != nil || len(capture.events) != 1 {
				t.Fatalf("positive WS control: %v, %v", capture.events, err)
			}
			if fill := capture.events[0].Payload.(*orderpb.OrderFilled).GetFill(); dec.FromProto(fill.GetPrice()).Cmp(want) != 0 {
				t.Fatalf("WS price = %v, want %s", fill.GetPrice(), want)
			}
		})
	}
	// An early NEW notification's empty execution fields are not a price.
	capture := &reconCapture{}
	if err := ingesterOver(nil, capture).handle(context.Background(), []byte(`{"e":"executionReport","c":"o1","x":"NEW","X":"NEW","l":"0","L":"0"}`)); err != nil || len(capture.events) != 0 {
		t.Fatalf("early notification = %v, %v", capture.events, err)
	}
}

func TestExecutedPriceRefusalReachesNoBroker(t *testing.T) {
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
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, executionStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := info.State.LastSeq
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: "binance-price-domain-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", ProducerVersion: "test", Tenant: "fund-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	identity := time.Now().UnixNano()
	zero, negative, good := fmt.Sprintf("price-zero-%d", identity), fmt.Sprintf("price-negative-%d", identity), fmt.Sprintf("price-good-%d", identity)
	view := newFakeOrders(zero, negative, good)
	refused := 0
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{priceDomainReport(zero, "FILLED", "0"), priceDomainReport(negative, "FILLED", "-1"), priceDomainReport(good, "FILLED", "1")}},
		Orders: view, Pub: producer, Venue: "BINANCE", Tenant: "fund-alpha", OnRefused: func(string, string, string) { refused++ },
	})
	if err := ing.Run(ctx); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	seen := fillFactsOnTheStream(ctx, t, stream, start)
	if seen[zero] || seen[negative] || !seen[good] || refused != 2 || view.get(t, zero).GetQuarantine() == nil || view.get(t, negative).GetQuarantine() == nil {
		t.Fatalf("broker facts zero/negative/good=%v/%v/%v; refusals=%d, want false/false/true and two quarantines", seen[zero], seen[negative], seen[good], refused)
	}
}
