package binance

import (
	"context"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/accountproof"
	"github.com/eighred/kanz/internal/venuemargin"
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
			// Every close this watchdog drops without asking the exchange is counted
			// by the composition root (#1036). Nil here would put the seam back to
			// where it was found: unhealable closes discarded on the same line as
			// healed ones, with nothing an alert can reach.
			OnCloseUnhealable: deps.OnCloseUnhealable,
			Venue:             c.settings.MIC, Tenant: deps.Tenant,
		})
		if deps.Expected != nil {
			go rec.Run(ctx, deps.ReconcileInterval)
		}
		if deps.Closes != nil {
			go rec.RunHealing(ctx, deps.HealInterval)
		}
	}
	// User-data stream ingester with resilient reconnect.
	if deps.Orders != nil {
		go c.runUserData(ctx, deps)
	}
	// MARGIN IS READ FROM THE EXCHANGE, NOT DERIVED FROM OUR BOOK (#408,
	// control 1). This connector is SPOT — every signed call it makes is /api/v3
	// — and the spot account endpoint reports no maintenance margin, no margin
	// ratio and no liquidation price, so the composition root supplies no source
	// and this starts nothing. That is a stated absence, not an omission:
	// venuemargin.Announce has already set the posture gauge to 0 and warned, so
	// "Binance reports no margin here" is distinguishable from "nothing looked".
	// The branch stays because a margin-capable Binance adapter supplies a source
	// and needs no further edit here.
	if deps.Margin != nil {
		go venuemargin.NewReporter(venuemargin.ReporterConfig{
			Source: deps.Margin, Pub: deps.Publisher,
			// THE ADAPTER'S OWN MAP, so a venue-reported liquidation price carries
			// the instrument this platform calls it (#408 control 4). This is the
			// only place the inversion is definable: the table is
			// instrument -> symbol and lives here, and
			// execution.ParseSymbolMap refuses a many-to-one map precisely so
			// that inverting it has one answer.
			Symbols: StaticSymbolMap(c.settings.Symbols),
			Venue:   c.settings.MIC, Account: c.settings.Account, Tenant: deps.Tenant,
			OnError: func(err error) {
				deps.Logger.Warn("binance: margin observation failed — the exchange's own margin state for "+
					"this account is going UNKNOWN, and every margin control on it fails closed",
					"err", err, "account", c.settings.Account, "subject", venuemargin.Subject)
			},
			OnUncovered: func(reason string, n int) {
				deps.Logger.Warn("binance: the exchange did not report part of its margin state — those "+
					"quantities are UNKNOWN, not zero",
					"reason", reason, "count", n, "account", c.settings.Account)
			},
		}).Run(ctx, deps.MarginInterval)
	}
	// Ticker feed → market price FACTs → tv-sync MarkSource.
	go c.runTicker(ctx, deps)
}

func (c *BinanceConnector) runUserData(ctx context.Context, deps WorkerDeps) {
	ing := newUserDataIngester(UserDataConfig{
		Orders: deps.Orders, Pub: deps.Publisher, Venue: c.settings.MIC, Tenant: deps.Tenant,
		OnRefused: deps.OnFillRefused, OnDropped: deps.OnFillDropped, Logger: deps.Logger,
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
	feed := &binanceTickerFeed{
		rest:    c.rest,
		symbols: c.settings.Symbols,
		ticks:   newMarkTickPublisher(deps.Publisher, deps.Logger, c.settings.MIC, deps.OnMarkTickDropped),
	}
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
	// ticks builds and publishes the FACT, and is where a publish that does not
	// land becomes a counter and a WARN instead of a discarded error (#673). The
	// envelope is built there rather than here so this feed and the OKX one
	// cannot drift into two market.crypto.trade shapes.
	ticks *MarkTickPublisher
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
		f.ticks.PublishTrade(ctx, instrument, symbol, price)
	}
}

// --- exported constructors for the composition root ---

// NewReconciler builds a reconciliation worker (exported wrapper).
func NewReconciler(cfg ReconcilerConfig) *Reconciler { return newReconciler(cfg) }
