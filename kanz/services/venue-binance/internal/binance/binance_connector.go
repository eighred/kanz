package binance

import (
	"context"
	"log/slog"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/venueadapter/accountproof"
	"github.com/eighred/kanz/pkg/bus"
)

// BinanceConnector bundles the venue with its background workers (M3.6): the
// user-data websocket ingester, the periodic REST reconciliation loop, and the
// ticker feed that drives tv-sync's mark source. The composition root builds it,
// takes Venue() for the router, and calls Start to run the workers.
type BinanceConnector struct {
	settings VenueSettings
	rest     *binanceREST
	venue    *BinanceVenue
	wsBase   string
}

// NewBinanceConnector assembles the venue + shared REST client from settings.
// wsBase is the websocket origin (e.g. wss://testnet.binance.vision).
func NewBinanceConnector(settings VenueSettings, wsBase string) *BinanceConnector {
	venue := NewBinanceVenueFromSettings(settings)
	return &BinanceConnector{settings: settings, rest: venue.rest, venue: venue, wsBase: wsBase}
}

// Venue returns the execution venue for the router.
func (c *BinanceConnector) Venue() Venue { return c.venue }

// Exchange is the seam that asks Binance which account this adapter's API credential
// actually belongs to (SOV-02a) — accountproof.Exchange, satisfied by the same signed
// REST client the reconciler already uses. The composition root calls it ONCE, at
// startup, and refuses to serve orders if the exchange names a different account than
// this deployment claims to be.
func (c *BinanceConnector) Exchange() accountproof.Exchange { return c.rest }

// Start launches the reconciler, the user-data ingester (with reconnect), and
// the ticker feed as background goroutines until ctx is cancelled.
func (c *BinanceConnector) Start(ctx context.Context, deps WorkerDeps) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	// Reconciliation loop (secondary audit; emits correcting FACTs) + the
	// In-Flight Certainty healing watchdog, which runs on its own fast tick.
	if deps.Expected != nil || deps.Closes != nil {
		rec := newReconciler(ReconcilerConfig{
			REST: c.rest, Symbols: StaticSymbolMap(c.settings.Symbols),
			Expected: deps.Expected, Balances: deps.Balances, Pub: deps.Publisher,
			Closes: deps.Closes, CloseTimeout: deps.CloseTimeout,
			Venue: c.settings.MIC, Tenant: deps.Tenant,
		})
		if deps.Expected != nil {
			go rec.Run(ctx, deps.ReconcileInterval)
		}
		if deps.Closes != nil {
			go rec.RunHealing(ctx, deps.HealInterval)
		}
	}
	// User-data stream ingester with resilient reconnect.
	if deps.Lookup != nil {
		go c.runUserData(ctx, deps)
	}
	// Ticker feed → market price FACTs → tv-sync MarkSource.
	go c.runTicker(ctx, deps)
}

func (c *BinanceConnector) runUserData(ctx context.Context, deps WorkerDeps) {
	ing := newUserDataIngester(UserDataConfig{
		Lookup: deps.Lookup, Pub: deps.Publisher, Venue: c.settings.MIC, Tenant: deps.Tenant,
	})
	backoff := time.Second
	for ctx.Err() == nil {
		ws := newBinanceUserDataWS(c.settings.BaseURL, c.wsBase, c.settings.APIKey, nil)
		stop, err := ws.Connect(ctx)
		if err != nil {
			deps.Logger.Warn("binance user-data connect failed; backing off", "err", err, "backoff", backoff)
			sleep(ctx, backoff)
			backoff = capDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		ing.stream = ws
		if err := ing.Run(ctx); err != nil && ctx.Err() == nil {
			deps.Logger.Warn("binance user-data stream ended; reconnecting", "err", err)
		}
		stop()
	}
}

func (c *BinanceConnector) runTicker(ctx context.Context, deps WorkerDeps) {
	interval := deps.TickerInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	feed := &binanceTickerFeed{rest: c.rest, symbols: c.settings.Symbols, mic: c.settings.MIC, pub: deps.Publisher, logger: deps.Logger}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			feed.pollOnce(ctx)
		}
	}
}

// binanceTickerFeed polls the last price per instrument and publishes a
// market.v1.MarketDataEvent (a Trade), which tv-sync's MarkSource folds into
// live unrealized P&L.
//
// This used to say "Price is universal market fact — not tenant-scoped", and
// that reading of the domain is defensible but it was not a description of the
// wire: bus.Validate REQUIRES tenant_id on every envelope, so a mark that named
// no tenant was not published as universal, it was refused. Nothing here stamps
// Event.TenantID and a time.Ticker loop has no inbound delivery to inherit one
// from, which left ProducerConfig.Tenant as the only source — and it was empty.
// It is now cfg.Tenant (venueProducerConfig in cmd/venue-binance), i.e. the
// tenant this dedicated adapter serves, which is also what
// bus.RequireTenantScope needs a tenant-dedicated consumer to see.
type binanceTickerFeed struct {
	rest    *binanceREST
	symbols map[string]string // instrument_id -> exchange symbol
	mic     string
	pub     Publisher
	logger  *slog.Logger
}

func (f *binanceTickerFeed) pollOnce(ctx context.Context) {
	for instrument, symbol := range f.symbols {
		px, err := f.rest.tickerPrice(ctx, symbol)
		if err != nil {
			continue // transient / rate-limited — skip this tick, don't fabricate
		}
		// Skipped, not published, when the price will not convert (#94) — the same
		// stance as the fetch error above, and for the same reason: parseDec used to
		// answer an unparseable price with ZERO, and a zero mark does not read as
		// wrong downstream, it reads as free.
		price, ok := parseDec(px)
		if !ok {
			continue
		}
		ev := &marketpb.MarketDataEvent{
			InstrumentId: instrument, Symbol: symbol, Mic: f.mic,
			EventTime: timestamppb.Now(),
			Data:      &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: price}},
		}
		_ = f.pub.Publish(ctx, bus.Event{
			Subject: "market.crypto.trade", EventType: "market.crypto.trade",
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
			EventTime: time.Now().UTC(), PartitionKey: instrument, Payload: ev,
		})
	}
}

// --- exported constructors for the composition root ---

// NewReconciler builds a reconciliation worker (exported wrapper).
func NewReconciler(cfg ReconcilerConfig) *Reconciler { return newReconciler(cfg) }
