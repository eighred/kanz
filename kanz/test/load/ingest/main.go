// Command ingest drives the risk-engine WRITE hot-path at a production tick
// rate (PARITY-05d): it publishes order-of-magnitude-realistic PositionState
// updates across a book of portfolios so the sharded recompute fan-out
// (PARITY-05a) and the durable-state apply path run under load — the write-side
// complement to the k6 read harness (config.js/baseline.js only exercise reads).
//
// Rate is an OPEN model (fixed events/sec regardless of downstream speed) so the
// number measured is "ingest rate the engine sustains", the input to the
// capacity model (capacity-model.md). Backpressure shows up as publish latency /
// the OBS-01c bus-pending gauge climbing — the KEDA scale signal.
//
//	RATE=5000 DURATION=2m PORTFOLIOS=2000 go run ./test/load/ingest
//
// Reuses the production bus.Producer (envelope stamped + validated exactly as a
// real publisher) and hardcodes the risk subject strings, like seed/, to respect
// the RISK-02 arch boundary (no import of kanz/internal/risk).
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	positionEventType = "risk.position.changed"
	positionDomain    = "risk"
	positionSchemaRef = "domain.v1.PositionState:1"
)

// instruments is the per-portfolio universe the ticks walk — matches the seed's
// book so an update lands on an existing position.
var instruments = []string{"AAPL", "MSFT", "TSLA", "NVDA"}

func main() {
	url := envOr("INGEST_NATS_URL", envOr("SEED_NATS_URL", "nats://localhost:4222"))
	prefix := envOr("PORTFOLIO", "PF1")
	tenant := envOr("SEED_TENANT", "load-test")
	rate := envInt("RATE", 1000)          // events/sec
	portfolios := envInt("PORTFOLIOS", 1) // book size (mirror the seed)
	dur := envDuration("DURATION", time.Minute)
	if rate < 1 {
		rate = 1
	}
	if portfolios < 1 {
		portfolios = 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "load-ingest"})
	if err != nil {
		log.Fatalf("ingest: dial nats %s: %v", url, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "load-ingest/ingest",
		ProducerVersion: "dev",
		Tenant:          tenant,
	})
	if err != nil {
		log.Fatalf("ingest: new producer: %v", err)
	}

	log.Printf("ingest: %d events/sec across %d portfolios for %s → %s", rate, portfolios, dur, url)

	// Open-model pacing: one tick per (1s / rate). A ticker keeps the target rate
	// independent of publish latency — a slow broker makes ticks queue, which is
	// the backpressure signal we want to observe, not hide.
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var published, failed int64
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	logEvery := time.NewTicker(10 * time.Second)
	defer logEvery.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("ingest: done — published=%d failed=%d", atomic.LoadInt64(&published), atomic.LoadInt64(&failed))
			return
		case <-logEvery.C:
			log.Printf("ingest: published=%d failed=%d", atomic.LoadInt64(&published), atomic.LoadInt64(&failed))
		case <-ticker.C:
			portfolio := prefix
			if portfolios > 1 {
				portfolio = fmt.Sprintf("%s-%04d", prefix, rng.Intn(portfolios))
			}
			ev := tick(tenant, portfolio, instruments[rng.Intn(len(instruments))], rng)
			if err := producer.Publish(ctx, ev); err != nil {
				atomic.AddInt64(&failed, 1)
			} else {
				atomic.AddInt64(&published, 1)
			}
		}
	}
}

// tick builds one PositionState update — a fresh mark on a random instrument of a
// random portfolio, partition-keyed by portfolio so per-aggregate ordering holds
// (RISK-05) and the shard fan-out (PARITY-05a) keys on the same id.
func tick(tenant, portfolio, instrument string, rng *rand.Rand) bus.Event {
	now := time.Now().UTC()
	qty := int64(100 + rng.Intn(900))
	px := int64(10_000 + rng.Intn(40_000)) // cents·100
	pos := &domainpb.PositionState{
		PortfolioId:  portfolio,
		InstrumentId: instrument,
		Quantity:     &commonpb.Decimal{Coefficient: qty, Exponent: 0},
		AveragePrice: &commonpb.Decimal{Coefficient: px, Exponent: -2},
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: qty * px, Exponent: -2}, CurrencyCode: "USD"},
		AsOf:         timestamppb.New(now),
	}
	return bus.Event{
		// ONE SUBJECT PER HOLDING (EXEC-M20). The flat subject is no longer carried by any
		// stream, and a JetStream publish to an unbound subject is a HARD ERROR — so a
		// seeder still using it would not degrade, it would fail outright.
		Subject:          subject.PositionFor(tenant, portfolio, instrument),
		EventType:        positionEventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           positionDomain,
		EventTime:        now,
		PartitionKey:     portfolio,
		PayloadSchemaRef: positionSchemaRef,
		Payload:          pos,
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
