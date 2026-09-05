// THE OVER-FILL REFUSAL, PROVEN AGAINST A REAL BROKER (#1045).
//
// Every other test on this path publishes into an event-level double that
// accepts whatever it is handed. That is the right tool for asserting WHICH
// events an adapter emits, and it is the wrong evidence for "nothing was
// published": a double cannot refuse an envelope, so an absence measured against
// one is an absence in the test's own memory, not on the spine. This drives the
// REAL bus.Producer over REAL NATS/JetStream into the PROVISIONED EXECUTION
// stream, and then asks the stream what it holds.
//
// THE ASSERTION IS ON IDENTITY, NOT ON A COUNT. The estate's streams have 24h
// retention and are reused across runs, so a test counting messages passes once
// and fails forever after with the count climbing. Both orders here carry a
// per-run unique id, and the test asks whether the stream holds a fill FACT for
// each of them.
//
// AND IT CARRIES ITS OWN POSITIVE CONTROL. A test that only asserts an absence
// is satisfied by a broken publish path, a wrong stream, or a producer that
// refused both events for an unrelated reason. So a second order fills normally
// in the same run: that one MUST be on the stream, from the same ingester, the
// same producer and the same connection.
//
//	TEST_NATS_URL=nats://localhost:4222 go test ./services/venue-binance/... -run Overfill
package binance

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/pkg/bus"
)

// executionStream is the provisioned stream that binds order.> in the estate's
// NATS topology. The test PUBLISHES INTO IT rather than creating a stream of its
// own: a second stream claiming an estate subject is refused by the server
// (overlapping subjects), and a test that invents its own subject would not be
// exercising the one the position book and the ledger consume.
const executionStream = "EXECUTION"

func TestOverfillReachesNoBroker(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to prove the over-fill refusal against a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("admin jetstream: %v", err)
	}
	stream, err := js.Stream(ctx, executionStream)
	if err != nil {
		t.Fatalf("the provisioned %s stream is not present on %s: %v — bootstrap the estate topology "+
			"before running this", executionStream, url, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	// Everything this test could have published starts after here. Reading from
	// this sequence bounds the scan to this run without counting anything.
	startSeq := info.State.LastSeq

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "venue-binance-overfill-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "venue-binance", ProducerVersion: "test", Tenant: "fund-alpha",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	run := time.Now().UnixNano()
	okOrder := fmt.Sprintf("ovf-ok-%d", run)
	badOrder := fmt.Sprintf("ovf-bad-%d", run)

	view := newFakeOrders()
	good := kanzOrder(okOrder)
	good.OrderedQuantity = bdec("10")
	bad := kanzOrder(badOrder)
	bad.OrderedQuantity = bdec("10")
	for _, st := range []*orderpb.OrderState{good, bad} {
		if rerr := view.store.Record(ctx, st); rerr != nil {
			t.Fatalf("seed view: %v", rerr)
		}
	}

	refused := 0
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{
			[]byte(fmt.Sprintf(`{"e":"executionReport","s":"BTCUSDT","c":%q,"S":"BUY","x":"TRADE",`+
				`"X":"FILLED","l":"10","L":"50000","z":"10","q":"10","t":11,"T":1700000000000}`, okOrder)),
			[]byte(fmt.Sprintf(`{"e":"executionReport","s":"BTCUSDT","c":%q,"S":"BUY","x":"TRADE",`+
				`"X":"FILLED","l":"4","L":"50000","z":"14","q":"10","t":12,"T":1700000000000}`, badOrder)),
		}},
		Orders: view, Pub: prod, Venue: "BINANCE", Tenant: "fund-alpha",
		OnRefused: func(string, string, string) { refused++ },
	})
	_ = ing.Run(ctx) // io.EOF at the end of the frames

	if refused != 1 {
		t.Fatalf("OnRefused fired %d times, want exactly 1 (the over-fill)", refused)
	}

	seen := fillFactsOnTheStream(ctx, t, stream, startSeq)
	if !seen[okOrder] {
		t.Fatalf("the ORDINARY fill for %s is not on the %s stream — the positive control failed, so "+
			"the absence below proves nothing about the refusal: this run's publish path did not work",
			okOrder, executionStream)
	}
	if seen[badOrder] {
		t.Fatalf("a fill FACT for the OVER-FILLED order %s reached the %s stream — the position "+
			"projector and the accounting ledger both consume it, and it carries a cumulative 14 "+
			"against an order of 10", badOrder, executionStream)
	}
}

// fillFactsOnTheStream reads every message the stream took after startSeq and
// returns the order ids that arrived on a fill subject. Direct GetMsg rather
// than a consumer: nothing is created, so nothing lingers into the next run to
// answer for a build that no longer exists.
func fillFactsOnTheStream(ctx context.Context, t *testing.T, stream jetstream.Stream, startSeq uint64) map[string]bool {
	t.Helper()
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	seen := map[string]bool{}
	for seq := startSeq + 1; seq <= info.State.LastSeq; seq++ {
		msg, gerr := stream.GetMsg(ctx, seq)
		if gerr != nil {
			continue // discarded or expired between the two Infos
		}
		if msg.Subject != fillfact.SubjectFilled && msg.Subject != fillfact.SubjectPartiallyFilled {
			continue
		}
		env, _, uerr := bus.Unframe(msg.Data)
		if uerr != nil {
			t.Fatalf("a message on %s does not unframe: %v", msg.Subject, uerr)
		}
		seen[env.GetPartitionKey()] = true
	}
	return seen
}
