package okx

import (
	"context"
	"log/slog"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// OKXConnector bundles the OKX venue with its background workers — the OKX
// analog of BinanceConnector, giving the OKX leg operational parity: user-data
// websocket ingester, periodic REST reconciliation, and a ticker feed.
type OKXConnector struct {
	settings VenueSettings
	rest     *okxREST
	venue    *OKXVenue
	wsURL    string
}

// NewOKXConnector assembles the venue + shared REST client. wsURL is the private
// websocket origin (e.g. wss://ws.okx.com:8443/ws/v5/private).
func NewOKXConnector(settings VenueSettings, wsURL string) *OKXConnector {
	venue := NewOKXVenueFromSettings(settings)
	return &OKXConnector{settings: settings, rest: venue.rest, venue: venue, wsURL: wsURL}
}

// Venue returns the execution venue for the router.
func (c *OKXConnector) Venue() Venue { return c.venue }

// Start launches the reconciler, user-data ingester (with reconnect), and ticker
// feed as background goroutines until ctx is cancelled.
func (c *OKXConnector) Start(ctx context.Context, deps WorkerDeps) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Expected != nil || deps.Closes != nil {
		rec := newOKXReconciler(OKXReconcilerConfig{
			REST: c.rest, Symbols: StaticSymbolMap(c.settings.Symbols),
			Expected: deps.Expected, Balances: deps.Balances, Pub: deps.Publisher,
			Closes: deps.Closes, CloseTimeout: deps.CloseTimeout,
			Venue: c.settings.MIC, Tenant: deps.Tenant,
		})
		if deps.Expected != nil {
			go rec.Run(ctx, deps.ReconcileInterval)
		}
		if deps.Closes != nil {
			// In-Flight Certainty watchdog — force-clears stuck closes fast.
			go rec.RunHealing(ctx, deps.HealInterval)
		}
	}
	if deps.Lookup != nil {
		go c.runUserData(ctx, deps)
	}
	go c.runTicker(ctx, deps)
}

func (c *OKXConnector) runUserData(ctx context.Context, deps WorkerDeps) {
	ing := newOKXUserDataIngester(nil, deps.Lookup, deps.Publisher, c.settings.MIC, deps.Tenant)
	backoff := time.Second
	for ctx.Err() == nil {
		ws := newOKXUserDataWS(c.wsURL, c.settings.APIKey, string(c.rest.apiSecret), c.settings.Passphrase)
		stop, err := ws.Connect(ctx)
		if err != nil {
			deps.Logger.Warn("okx user-data connect failed; backing off", "err", err, "backoff", backoff)
			Sleep(ctx, backoff)
			backoff = CapDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		ing.stream = ws
		if err := ing.Run(ctx); err != nil && ctx.Err() == nil {
			deps.Logger.Warn("okx user-data stream ended; reconnecting", "err", err)
		}
		stop()
	}
}

func (c *OKXConnector) runTicker(ctx context.Context, deps WorkerDeps) {
	interval := deps.TickerInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pollTicker(ctx, deps.Publisher)
		}
	}
}

// pollTicker publishes a market.v1.MarketDataEvent per instrument, feeding
// tv-sync's MarkSource — the OKX analog of the Binance ticker feed.
func (c *OKXConnector) pollTicker(ctx context.Context, pub Publisher) {
	for instrument, instID := range c.settings.Symbols {
		px, err := c.rest.tickerPrice(ctx, instID)
		if err != nil {
			continue
		}
		ev := &marketpb.MarketDataEvent{
			InstrumentId: instrument, Symbol: instID, Mic: c.settings.MIC,
			EventTime: timestamppb.Now(),
			Data:      &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: ParseDec(px)}},
		}
		_ = pub.Publish(ctx, bus.Event{
			Subject: "market.crypto.trade", EventType: "market.crypto.trade",
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
			EventTime: time.Now().UTC(), PartitionKey: instrument, Payload: ev,
		})
	}
}
