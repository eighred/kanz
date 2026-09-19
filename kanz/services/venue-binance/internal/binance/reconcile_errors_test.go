package binance

import (
	"bytes"
	"context"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/prometheus/client_golang/prometheus"
)

func TestReconcileErrorLoopsReport(t *testing.T) {
	for _, phase := range []string{"reconcile", "healing"} {
		for _, failure := range []string{"rate_limited", "invalid_evidence", "broker"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				f := newFakeBinance(t)
				f.queryBody = `{"status":"CANCELED","executedQty":"0"}`
				f.accountBody = `{"balances":[{"asset":"USD","free":"1","locked":"0"}]}`
				cap := &reconCapture{}
				r := reconOver(f, cap, staticOrders{}, staticBalances{"USD": big.NewRat(0, 1)})
				registry := prometheus.NewRegistry()
				var logs bytes.Buffer
				observer := execution.NewReconcileErrorObserver(registry, slog.New(slog.NewJSONHandler(&logs, nil)), "BINANCE")
				if phase == "healing" {
					closes := NewCloseRegistry()
					closes.Track(CloseIntent{OrderID: "o1", InstrumentID: "BTC-USD", RequestedAt: time.Now().Add(-time.Hour)})
					r.closes = closes
				}
				want := failure
				switch failure {
				case "rate_limited":
					r.rest.bucket = newWeightBucket(1, time.Hour, nil)
					// Healing deliberately retains its existing force-clear policy. Its
					// post-sweep balance pass returns the exhausted budget to the loop.
				case "invalid_evidence":
					if phase == "healing" {
						f.queryBody = `{"status":"CANCELED","executedQty":"invalid"}`
					} else {
						f.accountBody = `{"balances":[{"asset":"USD","free":"invalid","locked":"0"}]}`
					}
				case "broker":
					url := os.Getenv("TEST_NATS_URL")
					if url == "" {
						t.Skip("requires TEST_NATS_URL real JetStream")
					}
					client, err := bus.DialNATS(t.Context(), bus.NATSConfig{URL: url, Name: "reconcile-error-test"})
					if err != nil {
						t.Fatal(err)
					}
					producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", Tenant: "test", ProducerVersion: "test"})
					if err != nil {
						t.Fatal(err)
					}
					// An established real connection is lost before a discrepancy is sent.
					// The real producer error must reach the periodic loop observer.
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
					r.pub = producer
					want = "other"
				}
				ctx, cancel := context.WithCancel(t.Context())
				seen := make(chan struct{}, 1)
				r.onReconcileError = func(ctx context.Context, loop string, err error) {
					observer.Observe(ctx, loop, err)
					if err != nil {
						cancel()
						select {
						case seen <- struct{}{}:
						default:
						}
					}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					if phase == "healing" {
						r.RunHealing(ctx, time.Millisecond)
					} else {
						r.Run(ctx, time.Millisecond)
					}
				}()
				defer cancel()
				select {
				case <-seen:
				case <-time.After(3 * time.Second):
					cancel()
					<-done
					t.Fatal("loop swallowed reconciliation error")
				}
				<-done
				families, err := registry.Gather()
				if err != nil {
					t.Fatal(err)
				}
				var count float64
				for _, family := range families {
					for _, m := range family.Metric {
						labels := map[string]string{}
						for _, l := range m.Label {
							labels[l.GetName()] = l.GetValue()
						}
						if labels["loop"] == phase && labels["reason"] == want {
							count += m.GetCounter().GetValue()
						}
					}
				}
				if count != 1 || !strings.Contains(logs.String(), want) {
					t.Fatalf("failed pass missing: count=%v logs=%s", count, logs.String())
				}
				if f.posts != 0 {
					t.Fatal("observability unexpectedly placed an order")
				}
			})
		}
	}
}
