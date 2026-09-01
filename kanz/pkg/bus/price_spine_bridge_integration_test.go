package bus_test

// #955: A PRICE PUBLISHED IN __system__ REACHES A TENANT'S OMS, AND NOTHING
// WIDER THAN THE PRICE SPINE DOES.
//
// This is the market-data twin of TestAnOrderReachesOnlyItsOwnTenantsOMS, and it
// runs against the same REAL committed accounts — test/mtls/up.sh extracts
// tenants.conf straight out of infra/nats/tenancy.yaml — with real SVIDs mapped
// by verify_and_map. What is proven is the configuration that ships.
//
// THE DEFECT IT COVERS. `acme` granted oms-acme a SUBSCRIBE on market.*.trade and
// market.*.quote, contained no market producer, and imported none. NATS accounts
// are isolated by construction, so the pod authenticated, reported Ready,
// subscribed to both subjects and folded an EMPTY mark source for the life of the
// deployment: every MARKET and STOP order refused PRICE_UNAVAILABLE, no arrival
// price stamped on anything it did accept, and both #875 coverage gauges at zero
// — which is the one reading OMSQuoteCoverageAbsent structurally cannot fire on.
// A denied subscription and a quiet feed are the same observable state (#787).
//
// A FAKE BUS CANNOT PROVE ANY OF IT, for the same reason the order bridge cannot
// be faked: account isolation IS the property, and a double has no accounts. It
// would deliver every publish to every subscriber and report success — which is
// indistinguishable from the passing case and from the broken one.
//
// THE TEST IS A PAIR, and the second half is the one that keeps the fix honest.
// Delivery alone is equally satisfied by importing `market.>`, which would carry
// book snapshots, bars, the ingestion-coverage FACT and the volume profile across
// a tenant boundary as a side effect of fixing the order path. So the negative
// half publishes market.book.snapshot — the same producer, the same account, one
// subject outside the exported spine — and requires that it does NOT arrive.

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/pkg/bus"
)

// theInstrument is the one this test quotes. Distinctive enough that a match in
// the received bytes cannot come from anything else the broker is carrying.
const theInstrument = "PRICE-SPINE-955-BTCUSDT"

func TestThePriceSpineReachesATenantsOMS(t *testing.T) {
	url, certDir := mtlsEnv(t)

	// mark.DefaultSubjects, not a retyped pair. These are the subjects the OMS
	// folds (OMS_PRICE_SUBJECTS' default), the subjects tenancy.yaml exports and
	// imports, and the subjects test/arch/tenant_bridge_test.go derives its
	// expectation from — so a change to the fold reaches this test rather than
	// leaving it asserting yesterday's spine.
	if len(mark.DefaultSubjects) == 0 {
		t.Fatal("mark.DefaultSubjects is empty — this test would assert nothing")
	}

	// THREE IDENTITIES, THREE ROLES, EXACTLY AS PRODUCTION WIRES THEM.
	// market-ingest publishes in __system__ and is the only minted identity
	// permitted to publish a market subject at all; risk-engine subscribes
	// `market.>` in __system__ and is the control; oms-acme subscribes in `acme`
	// and is the subject of the test.
	platform := dialAs(t, url, certDir, "client")
	tenantOMS := dialAs(t, url, certDir, "tenantoms")

	// The tenant subscribes the UNCHANGED logical subject, exactly as
	// cmd/oms does — no `to:` remap exists on this import, because a public
	// price has the same name in every account.
	const quoteSubject = "market.crypto.quote"
	platformQuotes := subscribeLogical(t, platform, quoteSubject)
	tenantQuotes := subscribeLogical(t, tenantOMS, quoteSubject)

	// The wider subject the tenant must NOT receive. Subscribed BEFORE the
	// publish, so a delivery cannot be missed by timing and read as isolation.
	const snapshotSubject = "market.book.snapshot"
	platformSnapshots := subscribeLogical(t, platform, snapshotSubject)
	tenantSnapshots := subscribeLogical(t, tenantOMS, snapshotSubject)

	producer := producerAs(t, url, certDir, "marketingest", "market-ingest")

	// --- the price spine ------------------------------------------------------
	publishQuote(t, producer, theInstrument)

	if got, ok := received(t, platformQuotes, 5*time.Second); !ok || !strings.Contains(got, theInstrument) {
		t.Fatalf("the PLATFORM did not receive the quote (got %q, ok=%v).\n\n"+
			"This is the control half: without it a broker dropping everything would satisfy "+
			"every assertion below. Check that MARKET is provisioned in __system__ and that "+
			"market-ingest holds publish on %s.", got, ok, quoteSubject)
	}
	if got, ok := received(t, tenantQuotes, 5*time.Second); !ok || !strings.Contains(got, theInstrument) {
		t.Fatalf("oms-acme did not receive the quote (got %q, ok=%v).\n\n"+
			"This is the defect #955 exists to close. market-data and market-ingest publish the "+
			"price spine in __system__ and NO workload in a tenant account publishes a market "+
			"subject, so without the export on __system__ and the import on `acme` the tenant's "+
			"OMS subscribes, receives nothing, and refuses every MARKET and STOP order under "+
			"PRICE_UNAVAILABLE — with every probe Ready and no error on either side.", got, ok)
	}

	// --- and nothing wider than it -------------------------------------------
	publishSnapshot(t, producer, theInstrument)

	if got, ok := received(t, platformSnapshots, 5*time.Second); !ok || !strings.Contains(got, theInstrument) {
		t.Fatalf("the PLATFORM did not receive the book snapshot (got %q, ok=%v).\n\n"+
			"The negative assertion below is worthless without this: a publish that never "+
			"happened is not evidence of a narrow import.", got, ok)
	}
	if got, ok := received(t, tenantSnapshots, 500*time.Millisecond); ok {
		t.Fatalf("oms-acme received %s: %q.\n\n"+
			"The import was widened past the price spine. `market.>` also carries book "+
			"snapshots, bars, the ingestion-coverage FACT and the intraday volume profile, and "+
			"a tenant account holding all of it is a decision (#959) rather than a side effect "+
			"of repairing the order path. The exported set is mark.DefaultSubjects and nothing "+
			"else.", snapshotSubject, got)
	}
}

// publishQuote emits one two-sided quote exactly as marketedge/ingest's
// publishQuote does: the FACT's tenant is the PLATFORM's, because a top of book
// belongs to the market and not to whoever reads it. That is the half of the
// arrangement worth stating — a tenant OMS folds an envelope stamped __system__,
// and mark.Source does not filter on tenant precisely because a price is the same
// fact for every fund (the same reason accounting's FX fold is exempt from
// test/arch's tenant-scope guard).
func publishQuote(t *testing.T, p *bus.Producer, instrument string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := p.Publish(ctx, bus.Event{
		Subject:        "market.crypto.quote",
		EventType:      "market.crypto.quote",
		EventClass:     envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:  1,
		Domain:         "market",
		EventTime:      time.Now().UTC(),
		PartitionKey:   instrument,
		IdempotencyKey: "idem-quote-" + instrument,
		TenantID:       bus.SystemTenant,
		Payload: &marketpb.MarketDataEvent{
			InstrumentId: instrument,
			Symbol:       "BTC-USDT",
			Mic:          "XBIT",
			EventTime:    timestamppb.New(time.Now().UTC()),
			// Money is common.v1.Decimal, never float: 6_400_000e-2 and
			// 6_400_100e-2, a one-cent width on a 64,000 book.
			Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
				BidPrice: &commonpb.Decimal{Coefficient: 6400000, Exponent: -2},
				BidSize:  &commonpb.Decimal{Coefficient: 1, Exponent: 0},
				AskPrice: &commonpb.Decimal{Coefficient: 6400100, Exponent: -2},
				AskSize:  &commonpb.Decimal{Coefficient: 1, Exponent: 0},
			}},
		},
	})
	if err != nil {
		t.Fatalf("publish %s: %v\n\n"+
			"`no response from stream` means __system__ has no stream bound to market.> — the "+
			"price spine would reach nobody at all, tenant or otherwise.", "market.crypto.quote", err)
	}
}

// publishSnapshot emits a book snapshot on the one market subject that is
// deliberately NOT exported to a tenant account.
func publishSnapshot(t *testing.T, p *bus.Producer, instrument string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := p.Publish(ctx, bus.Event{
		Subject:        "market.book.snapshot",
		EventType:      "market.book.snapshot",
		EventClass:     envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:  1,
		Domain:         "market",
		EventTime:      time.Now().UTC(),
		PartitionKey:   instrument,
		IdempotencyKey: "idem-snapshot-" + instrument,
		TenantID:       bus.SystemTenant,
		Payload: &marketpb.OrderBookSnapshot{
			InstrumentId: instrument,
			Symbol:       "BTC-USDT",
			Mic:          "XBIT",
			EventTime:    timestamppb.New(time.Now().UTC()),
		},
	})
	if err != nil {
		t.Fatalf("publish %s: %v", "market.book.snapshot", err)
	}
}
