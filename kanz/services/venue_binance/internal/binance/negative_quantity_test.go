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

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestUserData_NegativeCumulativeIsRefusedAndQuarantined(t *testing.T) {
	for _, status := range []string{"PARTIALLY_FILLED", "FILLED"} {
		t.Run(status, func(t *testing.T) {
			view := newFakeOrders()
			if err := view.store.Record(context.Background(), negativeQuantityOrder("negative1620")); err != nil {
				t.Fatal(err)
			}
			capture := &reconCapture{}
			refused := 0
			ing := newUserDataIngester(UserDataConfig{
				Stream: &fakeStream{frames: [][]byte{[]byte(fmt.Sprintf(
					`{"e":"executionReport","s":"BTCUSDT","c":"negative1620","S":"BUY","x":"TRADE","X":%q,"l":"1","L":"1","z":"-1","t":7,"T":1700000000000}`, status))}},
				Orders: view, Pub: capture, Venue: "BINANCE", Tenant: "fund-alpha",
				OnRefused: func(mic, id, reason string) {
					refused++
					if mic != "BINANCE" || id != "negative1620" || reason != "unrepresentable" {
						t.Errorf("unexpected refusal identity/class: %s/%s/%s", mic, id, reason)
					}
				},
			})
			if err := ing.Run(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("report handling ended with %v, want the exhausted fixture stream", err)
			}
			if len(capture.events) != 0 || refused != 1 {
				t.Errorf("negative cumulative published %d events and counted %d refusals", len(capture.events), refused)
			}
			st := view.get(t, "negative1620")
			if st.GetQuarantine() == nil || st.GetQuarantine().GetReason() == "" {
				t.Error("the refused execution report did not quarantine the order with a reason")
			}
			if dec.FromProto(st.GetFilledQuantity()).Sign() != 0 || dec.FromProto(st.GetLeavesQuantity()).Cmp(big.NewRat(10, 1)) != 0 {
				t.Error("the refused cumulative changed the recorded fill or leaves")
			}
			if len(view.errs) != 0 {
				t.Errorf("order-view persistence reported %v", view.errs)
			}
		})
	}
}

func negativeQuantityOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, LimitPrice: &commonpb.Decimal{Coefficient: 1},
		OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, FilledQuantity: &commonpb.Decimal{}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10},
	}
}

func TestNegativeCumulativeReachesNoBroker(t *testing.T) {
	broker := os.Getenv("TEST_NATS_URL")
	if broker == "" {
		t.Skip("set TEST_NATS_URL to prove negative cumulative refusal on the real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	startSeq := info.State.LastSeq
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: "venue-binance-negative-quantity-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "venue-binance", ProducerVersion: "test", Tenant: "fund-alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := time.Now().UnixNano()
	bad, good := fmt.Sprintf("negative-%d", identity), fmt.Sprintf("positive-%d", identity)
	view := newFakeOrders()
	var frames [][]byte
	for i, report := range []struct{ id, cumulative string }{{bad, "-1"}, {good, "1"}} {
		if err := view.store.Record(ctx, negativeQuantityOrder(report.id)); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, []byte(fmt.Sprintf(
			`{"e":"executionReport","s":"BTCUSDT","c":%q,"S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED","l":"1","L":"1","z":%q,"t":%d,"T":1700000000000}`,
			report.id, report.cumulative, identity+int64(i))))
	}
	refused := 0
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: frames}, Orders: view, Pub: producer,
		Venue: "BINANCE", Tenant: "fund-alpha",
		OnRefused: func(string, string, string) { refused++ },
	})
	if err := ing.Run(ctx); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	seen := fillFactsOnTheStream(ctx, t, stream, startSeq)
	if seen[bad] || !seen[good] || refused != 1 || view.get(t, bad).GetQuarantine() == nil {
		t.Fatalf("negative FACT=%v, positive control FACT=%v, refusals=%d; want false/true/1 and quarantine",
			seen[bad], seen[good], refused)
	}
}
