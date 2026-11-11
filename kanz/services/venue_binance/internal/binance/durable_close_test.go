package binance

import (
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/server"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/test/backing"
	"github.com/nats-io/nats.go"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDurableCloseRecoveryWithRealBroker(t *testing.T) {
	broker := os.Getenv("TEST_NATS_URL")
	if broker == "" {
		t.Skip("requires real JetStream")
	}
	open := backing.VenueStore(t, "../../migrations")
	store := open()
	ctx := context.Background()
	f := newFakeBinance(t)
	f.cancelStatus = 504
	f.queryBody = "{"
	id := fmt.Sprintf("close-%d", time.Now().UnixNano())
	st := &orderpb.OrderState{OrderId: id, InstrumentId: "BTC-USD"}
	srv := server.New(venueOverFake(f), store, store, execution.AccountProof{Verified: true, ExchangeAccountID: "test-account"}, halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := srv.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: st}); err == nil {
		t.Fatal("expected ambiguous cancellation")
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: "close-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", Tenant: "fund-alpha", ProducerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	js, err := admin.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.StreamInfo("EXECUTION")
	if err != nil {
		t.Fatal(err)
	}
	start := info.State.LastSeq
	countEvidence := func() int {
		t.Helper()
		info, err := js.StreamInfo("EXECUTION")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for seq := start + 1; seq <= info.State.LastSeq; seq++ {
			msg, err := js.GetMsg("EXECUTION", seq)
			if err != nil {
				continue
			}
			env, _, err := bus.Unframe(msg.Data)
			if err != nil {
				t.Fatal(err)
			}
			if env.GetPartitionKey() == id {
				count++
			}
		}
		return count
	}
	r := healReconOverBinance(f, &reconCapture{}, open()) // restart before the unanswered query
	r.pub = producer
	now := time.Now().Add(time.Minute)
	r.now = func() time.Time { return now }
	if err := r.HealClosures(ctx); err == nil {
		t.Fatal("unanswered query reported success")
	}
	if countEvidence() != 0 {
		t.Fatal("unknown query fabricated evidence")
	}
	// Redelivery/restart cannot erase the unresolved intent or refresh its age.
	restarted := open()
	if err := restarted.Track(ctx, execution.CloseIntent{OrderID: id, InstrumentID: "BTC-USD"}); err != nil {
		t.Fatal(err)
	}
	r.closes = restarted
	f.queryBody = strings.ReplaceAll(`{"clientOrderId":"o1","status":"CANCELED","executedQty":"0"}`, "o1", id)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := r.HealClosures(ctx); err == nil {
		t.Fatal("lost broker delivery reported success")
	}
	if countEvidence() != 0 {
		t.Fatal("failed delivery reached broker")
	}
	recovered, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: "close-recovered"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()
	r.pub, err = bus.NewProducer(recovered, bus.ProducerConfig{Source: "venue-binance", Tenant: "fund-alpha", ProducerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	r.closes = open()
	now = now.Add(time.Minute)
	if err := r.HealClosures(ctx); err != nil {
		t.Fatal(err)
	}
	due, err := open().DueCloses(ctx, now.Add(time.Hour), 0)
	if err != nil || len(due) != 0 || countEvidence() != 1 || f.posts != 0 {
		t.Fatalf("recovery mismatch: due=%v err=%v evidence=%d posts=%d", due, err, countEvidence(), f.posts)
	}
}
