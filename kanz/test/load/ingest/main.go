// Command ingest drives the risk-engine WRITE hot-path at a production tick
// rate (PARITY-05d): it publishes order-of-magnitude-realistic PositionState
// updates across a book of portfolios so the sharded recompute fan-out
// (PARITY-05a) and the durable-state apply path run under load — the write-side
// complement to the k6 read harness (config.js/baseline.js only exercise reads).
//
// RATE is the offered schedule. The generator publishes serially and reports
// unsent arrivals separately from JetStream acknowledgements and publish errors.
// The engine's recompute metrics and consumer lag measure downstream processing.
//
//	RATE=5000 DURATION=2m PORTFOLIOS=2000 go run ./test/load/ingest
//
// IT ALSO REPORTS THE ENGINE'S CAPACITY, WHICH IS WHY IT EXISTS (#1050). Publish
// rate and bus backlog describe the GENERATOR and the SPINE; the component this
// harness drives is the one whose working set is a function of estate size rather
// than a constant, and it had no capacity number at all. The run now polls the
// engine's own /metrics — INGEST_ENGINE_METRICS_URL, default
// http://localhost:8081/metrics, matching RISK_ENGINE_LISTEN's default — and
// reports peak goroutines, peak RSS, peak recompute fan-out width and backlog,
// and the recompute-latency distribution for the run. Set it empty to skip, and
// the report says in words that no capacity claim can be made. See capacity.go.
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
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/env"
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
	url := env.Or("INGEST_NATS_URL", env.Or("SEED_NATS_URL", "nats://localhost:4222"))
	prefix := env.Or("PORTFOLIO", "PF1")
	tenant := env.Or("SEED_TENANT", "load-test")
	// A MALFORMED VALUE STOPS THE RUN (#692). A load generator that silently
	// falls back to 1000 events/sec because RATE=1k did not parse produces a
	// number somebody will quote, measured against a rate nobody set.
	rate, err := env.Int("RATE", 1000) // events/sec
	if err != nil {
		log.Fatal(err)
	}
	portfolios, err := env.Int("PORTFOLIOS", 1) // book size (mirror the seed)
	if err != nil {
		log.Fatal(err)
	}
	dur, err := env.Duration("DURATION", time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	if rate < 1 || int64(rate) > int64(time.Second) {
		log.Fatal("RATE must be between 1 and 1000000000 events/sec")
	}
	if portfolios < 1 || dur <= 0 {
		log.Fatal("PORTFOLIOS and DURATION must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	// THE CAPACITY POLLER (#1050). Started before the first tick so the opening
	// scrape is the engine's pre-load baseline: the latency histogram is reported
	// as a DELTA across the run, and a process that has been up for a day carries
	// a distribution dominated by what it did yesterday.
	//
	// EMPTY IS A DELIBERATE OPT-OUT AND IT SAYS SO. Silently skipping would leave
	// a run that printed publish numbers and nothing else — indistinguishable from
	// a run whose engine was unreachable, which is the state that matters.
	metricsURL, configured := os.LookupEnv("INGEST_ENGINE_METRICS_URL")
	if !configured {
		metricsURL = "http://localhost:8081/metrics"
	}
	capacity := &capacityReport{}
	if metricsURL == "" {
		log.Printf("ingest: WARNING — INGEST_ENGINE_METRICS_URL is empty, so this run measures the " +
			"GENERATOR and the SPINE only. It produces no risk-engine capacity number and nothing " +
			"from it may be quoted into a resources: block (#231).")
	} else {
		pollCtx, stopPoll := context.WithCancel(ctx)
		capacity.pollOnce(pollCtx, metricsURL)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			capacity.pollEngine(pollCtx, metricsURL, time.Second)
		}()
		defer func() {
			stopPoll()
			wg.Wait()
			// Include recomputes visible at the final scrape. The engine may still
			// have pending work after the generator stops.
			last, cancelLast := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelLast()
			capacity.pollOnce(last, metricsURL)
			log.Print(capacity.render(metricsURL, portfolios))
		}()
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	report := driveArrivals(ctx, int64(rate), dur, func(ctx context.Context) error {
		portfolio := prefix
		if portfolios > 1 {
			portfolio = fmt.Sprintf("%s-%04d", prefix, rng.Intn(portfolios))
		}
		ev := tick(tenant, portfolio, instruments[rng.Intn(len(instruments))], rng)
		return producer.Publish(ctx, ev)
	})
	log.Printf("ingest: done %s", report)
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
