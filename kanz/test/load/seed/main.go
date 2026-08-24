// Command seed publishes one PortfolioSnapshot to the NATS spine so the
// LATENCY-01a load stack has queryable state. The risk-engine read path
// (Exposure/Measures/Scenario) returns ErrPortfolioNotFound → 404 for an
// unseeded portfolio, which would trip the smoke test's error budget; one
// snapshot makes the portfolio queryable.
//
// It is intentionally tiny and self-contained: it reuses the production
// bus.Producer (so the envelope is stamped + validated exactly as a real
// publisher would) but hardcodes the well-known risk subject/taxonomy strings
// rather than importing kanz/internal/risk (the RISK-02 arch boundary keeps the
// risk impl packages private to the risk composer). Run against the ephemeral
// compose stack:
//
//	go run ./test/load/seed                 # nats://localhost:4222, PF1
//	SEED_PORTFOLIO=PF9 go run ./test/load/seed
package main

import (
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

const (
	// snapshotEventType / snapshotSubject mirror ingest.EventTypePortfolioSnapshot
	// and the subject-taxonomy §1 mapping ({domain}.{entity}.{event_type}). The
	// RISK stream binds "risk.>", so this subject lands on it.
	snapshotEventType = "risk.portfolio.snapshot"
	snapshotSubject   = "risk.portfolio.snapshot"
	snapshotDomain    = "risk"
	// payloadSchemaRef is the registry ref form <schema-id>:<version> (EVT-16c).
	// The engine does not resolve it on ingest; Validate only requires it be
	// non-empty, so a static ref is sufficient for the load fixture.
	payloadSchemaRef = "domain.v1.PortfolioSnapshot:1"
)

func main() {
	url := env.Or("SEED_NATS_URL", "nats://localhost:4222")
	prefix := env.Or("SEED_PORTFOLIO", "PF1")
	tenant := env.Or("SEED_TENANT", "load-test")
	// SEED_PORTFOLIOS scales the seed to a production-shaped book count
	// (PARITY-05d): >1 publishes <prefix>-0000..<prefix>-NNNN so the read mix
	// and the sharded recompute (PARITY-05a) spread across many portfolios, not
	// one hot key. Default 1 keeps the single-portfolio smoke behavior (the
	// well-known PF1). At count 1 the id is exactly the prefix (back-compat).
	// A malformed value stops the run rather than seeding a book size nobody
	// chose (#692).
	count, err := env.Int("SEED_PORTFOLIOS", 1)
	if err != nil {
		log.Fatal(err)
	}
	if count < 1 {
		count = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "load-seed"})
	if err != nil {
		log.Fatalf("seed: dial nats %s: %v", url, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "load-seed/seed",
		ProducerVersion: "dev",
		Tenant:          tenant,
	})
	if err != nil {
		log.Fatalf("seed: new producer: %v", err)
	}

	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		portfolio := prefix
		if count > 1 {
			portfolio = fmt.Sprintf("%s-%04d", prefix, i)
		}
		if err := producer.Publish(ctx, snapshotEvent(portfolio, now)); err != nil {
			log.Fatalf("seed: publish snapshot for %s: %v", portfolio, err)
		}
	}
	log.Printf("seed: published %d PortfolioSnapshot(s) prefix=%s positions=%d tenant=%s",
		count, prefix, len(buildSnapshot(prefix, now).GetPositions()), tenant)
}

// snapshotEvent builds the envelope seed publishes, EXTRACTED FROM THE LOOP SO A
// TEST CAN REACH IT (#245).
//
// main dials a broker before it constructs anything, so this value was
// unreachable without one — the same seam kanz-altevent's altEvent and
// market-data's feedProducerConfig exist for.
//
// NOTE THE CLASS: STATE_SNAPSHOT, not FACT. It is the one publisher in this
// module that emits one, so it is the only place the snapshot arm of
// bus.Validate is exercised at all.
func snapshotEvent(portfolio string, now time.Time) bus.Event {
	return bus.Event{
		Subject:          snapshotSubject,
		EventType:        snapshotEventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT,
		SchemaVersion:    1,
		Domain:           snapshotDomain,
		EventTime:        now,
		PartitionKey:     portfolio,
		PayloadSchemaRef: payloadSchemaRef,
		Payload:          buildSnapshot(portfolio, now),
	}
}

// buildSnapshot is a small but non-degenerate portfolio: a few long/short
// equity positions with mark-to-market values, so Exposure/Measures return
// real (non-empty) numbers rather than a zero set.
func buildSnapshot(portfolio string, asOf time.Time) *domainpb.PortfolioSnapshot {
	ts := timestamp(asOf)
	positions := []*domainpb.PositionState{
		position(portfolio, "AAPL", 1000, dec(195_50, -2), money(195_500_00, -2), ts),
		position(portfolio, "MSFT", 500, dec(420_10, -2), money(210_050_00, -2), ts),
		position(portfolio, "TSLA", -300, dec(250_00, -2), money(-75_000_00, -2), ts),
		position(portfolio, "NVDA", 200, dec(120_25, -2), money(24_050_00, -2), ts),
	}
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolio,
			DisplayName:      "Load Test Portfolio",
			BaseCurrency:     "USD",
			CashBalance:      money(50_000_00, -2),
			TotalMarketValue: money(354_600_00, -2),
			PositionCount:    uint32(len(positions)),
			AsOf:             ts,
		},
		Positions: positions,
		// LogPosition is the durable-log resume coordinate. The in-memory load
		// stack has no Kafka log to resume from, so a synthetic position is fine —
		// the engine only persists/uses it for PERS-01d bootstrap, which is off here.
		LogPosition: &commonpb.LogPosition{Topic: "risk.portfolio", Partition: 0, Offset: 0},
	}
}

func position(portfolio, instrument string, qty int64, avgPrice *commonpb.Decimal, mv *commonpb.Money, asOf *timestamppb.Timestamp) *domainpb.PositionState {
	return &domainpb.PositionState{
		PortfolioId:  portfolio,
		InstrumentId: instrument,
		Quantity:     dec(qty, 0),
		AveragePrice: avgPrice,
		MarketValue:  mv,
		AsOf:         asOf,
	}
}

// dec builds a common.v1.Decimal (value = coefficient × 10^exponent).
func dec(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

// money builds a USD common.v1.Money from a Decimal coefficient/exponent.
func money(coefficient int64, exponent int32) *commonpb.Money {
	return &commonpb.Money{Amount: dec(coefficient, exponent), CurrencyCode: "USD"}
}

func timestamp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
