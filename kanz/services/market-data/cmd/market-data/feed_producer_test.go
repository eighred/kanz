package main

// THE COMPOSITION ROOT IS WHERE THIS DEFECT LIVED (#245).
//
// services/market-data/internal/feed builds a correct envelope, and
// bussink_test.go proves it — with a producer the TEST configures
// (bussink_test.go:32 sets Tenant: "acme"). runFeed configured a different one:
// bus.ProducerConfig{Source, ProducerVersion, Metrics}, no Tenant. The package
// tests could not see it, and runFeed dials SPIFFE and a broker before it
// constructs anything, so the config it passed was untested by construction.
// feedProducerConfig exists to give that line a seam.
//
// A test producer that is healthier than the wired one is worse than no test:
// it reports the path green while the deployed path publishes nothing.

import (
	"context"
	"errors"
	"testing"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/market-data/internal/config"
	"github.com/eighred/kanz/services/market-data/internal/feed"
)

// recordingClient is a bus.Client — the MESSAGE-level double (#245 Tier-B). The
// real bus.Producer sits above it, so bus.Validate runs on every envelope.
type recordingClient struct{ sent []bus.Message }

func (c *recordingClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *recordingClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *recordingClient) Close() error { return nil }

// A TICK PUBLISHED THROUGH THE FEED WIRED THE WAY runFeed WIRES IT MUST REACH
// THE TRANSPORT.
//
// This is the whole bug in one assertion: build the producer from the real
// feedProducerConfig, publish through the real feed.BusSink, and require that
// bytes come out. Before the Tenant fallback was added this failed with
// "envelope validation: tenant_id required" and the feed published nothing while
// /readyz stayed 200.
func TestFeedPublisherWiredFromConfigEmitsAValidatedFact(t *testing.T) {
	cfg := config.Config{Source: "market-data", Tenant: "acme-fund", FeedAssetClass: "equity"}

	rc := &recordingClient{}
	producer, err := bus.NewProducer(rc, feedProducerConfig(cfg, nil))
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	sink, err := feed.NewBusSink(producer, cfg.FeedAssetClass)
	if err != nil {
		t.Fatalf("NewBusSink: %v", err)
	}

	et := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	m := feed.Meta{InstrumentID: "AAPL", Symbol: "AAPL", MIC: "XNAS", EventTime: et, SourceSequence: 1}
	tick, err := feed.Trade(m, feed.DecimalFromFloat(101.25, -2), feed.DecimalFromFloat(100, 0), "t1")
	if err != nil {
		t.Fatalf("build trade: %v", err)
	}

	if err := sink.Publish(context.Background(), tick); err != nil {
		t.Fatalf("a tick published through the wired feed producer was refused: %v\n"+
			"This is the composition root, not the sink: MARKET_DATA_FEED publishes nothing "+
			"and the service reports ready the whole time.", err)
	}
	if len(rc.sent) != 1 {
		t.Fatalf("transport received %d messages, want 1", len(rc.sent))
	}
	env, payload, err := bus.Unframe(rc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("wired envelope fails Validate: %v", err)
	}
	if env.GetTenantId() != "acme-fund" {
		t.Errorf("TenantId = %q, want the configured MARKET_DATA_TENANT (acme-fund)", env.GetTenantId())
	}
	if len(payload) == 0 {
		t.Error("framed payload is empty")
	}
	var got marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.GetInstrumentId() != "AAPL" {
		t.Errorf("instrument_id = %q, want AAPL", got.GetInstrumentId())
	}
}

// The feed producer's tenant must be the SERVICE's tenant. market-ingest stamps
// MARKET_INGEST_TENANT on the same market.v1 event type; a feed that stamped
// something else would publish marks no tenant-dedicated consumer accepts
// (bus.RequireTenantScope, #223).
func TestFeedProducerTenantIsTheServiceTenant(t *testing.T) {
	cfg := config.Config{Source: "market-data", Tenant: "beta-fund"}
	if got := feedProducerConfig(cfg, nil).Tenant; got != cfg.Tenant {
		t.Fatalf("feed producer tenant = %q, want the service tenant %q — every tick would be "+
			"refused tenant_id required, or scoped to the wrong book", got, cfg.Tenant)
	}
}
