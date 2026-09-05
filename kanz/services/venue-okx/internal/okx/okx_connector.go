package okx

import (
	"context"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/accountproof"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/internal/venuemargin"
)

// OKXConnector bundles the OKX venue with its background workers — the OKX
// analog of BinanceConnector, giving the OKX leg operational parity: user-data
// websocket ingester, periodic REST reconciliation, and a ticker feed.
type OKXConnector struct {
	settings VenueSettings
	rest     *okxREST
	venue    *OKXVenue
	wsURL    string
	// dialUserData opens ONE private user-data websocket, or fails. A FIELD for
	// the reason venue-binance's is (#1047): the reconnect loop's re-dial rate is
	// what stands between a store outage and an exchange rate-limit or IP ban, and
	// a loop that can only be driven against a real venue cannot be asserted.
	dialUserData func(ctx context.Context) (UserDataStream, func(), error)
	// newUserDataBackoff builds the reconnect policy for one run of the user-data
	// loop. A FIELD for the reason dialUserData is (#1047): the loop's growth
	// curve is a venue-safety property, and on the production bounds a test cannot
	// tell an exponential delay from one pinned at the base.
	newUserDataBackoff func() *UserDataBackoff
}

// NewOKXConnector assembles the venue + shared REST client. wsURL is the private
// websocket origin (e.g. wss://ws.okx.com:8443/ws/v5/private).
func NewOKXConnector(settings VenueSettings, wsURL string, mode exchangeauth.OKXTradingMode) *OKXConnector {
	venue := NewOKXVenueFromSettings(settings, mode)
	c := &OKXConnector{settings: settings, rest: venue.rest, venue: venue, wsURL: wsURL}
	c.dialUserData = func(ctx context.Context) (UserDataStream, func(), error) {
		ws := newOKXUserDataWS(c.wsURL, c.settings.APIKey, string(c.rest.apiSecret), c.settings.Passphrase)
		stop, err := ws.Connect(ctx)
		if err != nil {
			return nil, nil, err
		}
		return ws, stop, nil
	}
	c.newUserDataBackoff = NewUserDataBackoff
	return c
}

// Venue returns the execution venue for the router.
func (c *OKXConnector) Venue() Venue { return c.venue }

// Exchange is the seam that asks OKX which account this adapter's API credential
// actually belongs to (SOV-02a) — accountproof.Exchange, satisfied by the same signed
// REST client the reconciler already uses. The composition root calls it ONCE, at
// startup, and refuses to serve orders if OKX names a different account than this
// deployment claims to be.
func (c *OKXConnector) Exchange() accountproof.Exchange { return c.rest }

// MarginSource is the seam that reads OKX's OWN margin state for this adapter's
// account (#408, control 1) — satisfied by the same signed REST client the
// reconciler and the account proof already use.
//
// EXPOSED SO THE COMPOSITION ROOT ANNOUNCES IT RATHER THAN THE CONNECTOR
// ASSUMING IT. Start could have reached for c.rest directly and always had a
// source; then "this adapter observes margin" would be true by construction and
// unobservable, and the day an OKX account was in a mode that reports nothing it
// would look identical to one being watched. Handing it out makes the wiring a
// line in the composition root that venuemargin.Announce turns into a gauge.
func (c *OKXConnector) MarginSource() VenueMarginSource { return c.rest }

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
			// In-Flight Certainty watchdog — force-clears stuck closes fast.
			go rec.RunHealing(ctx, deps.HealInterval)
		}
	}
	if deps.Orders != nil {
		go c.runUserData(ctx, deps)
	}
	// MARGIN IS READ FROM OKX, NOT DERIVED FROM OUR BOOK (#408, control 1). A nil
	// source starts nothing and is NOT silent — venuemargin.Announce has already
	// set the posture gauge to 0 and warned at the composition root, which is the
	// one place an operator can tell "this venue reports no margin" apart from
	// "nothing has looked".
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
				// NOT FATAL AND NOT SILENT. A failed observation ages the account out
				// to UNKNOWN, which every #408 control fails closed on — so the
				// symptom an operator meets first is orders being refused, and this
				// line is what tells them why.
				deps.Logger.Warn("okx: margin observation failed — the exchange's own margin state for this "+
					"account is going UNKNOWN, and every margin control on it fails closed",
					"err", err, "account", c.settings.Account, "subject", venuemargin.Subject)
			},
			OnUncovered: func(reason string, n int) {
				deps.Logger.Warn("okx: the exchange did not report part of its margin state — those "+
					"quantities are UNKNOWN, not zero",
					"reason", reason, "count", n, "account", c.settings.Account)
			},
		}).Run(ctx, deps.MarginInterval)
	}
	go c.runTicker(ctx, deps)
}

func (c *OKXConnector) runUserData(ctx context.Context, deps WorkerDeps) {
	ing := newOKXUserDataIngester(OKXUserDataConfig{
		Orders: deps.Orders, Pub: deps.Publisher, Venue: c.settings.MIC, Tenant: deps.Tenant,
		OnRefused: deps.OnFillRefused, OnDropped: deps.OnFillDropped, Logger: deps.Logger,
	})
	// THE RE-DIAL RATE IS THE THING THIS LOOP DECIDES, AND IT IS A VENUE-SAFETY
	// DECISION (#1047).
	//
	// It backed off only when Connect FAILED, and reset the delay on every
	// successful connect. That covers an exchange being down and covers nothing
	// else. When the exchange is HEALTHY and the adapter's own order view is not,
	// every session connects, the ingester refuses the first execution report it
	// cannot resolve, Run returns, the delay is reset, and the loop re-dials with
	// no pause at all — measured at 38,688 dials in 300ms. Binance and OKX
	// rate-limit and then IP-BAN that, which turns a recoverable database blip
	// into a venue-level lockout of the whole account and stops every order on it.
	//
	// So BOTH failures back off through one policy, and the delay is reset only on
	// evidence the session actually resolved a report — see UserDataBackoff.
	// The seam defaults to the PRODUCTION policy rather than to nothing: an unset
	// field must never mean "no back-off", which is the defect itself.
	newBackoff := c.newUserDataBackoff
	if newBackoff == nil {
		newBackoff = NewUserDataBackoff
	}
	backoff := newBackoff()
	for ctx.Err() == nil {
		stream, stop, err := c.dialUserData(ctx)
		if err != nil {
			deps.Logger.Warn("okx user-data connect failed; backing off",
				"err", err, "backoff", backoff.Delay())
			backoff.Wait(ctx)
			continue
		}
		ing.stream = stream
		runErr := ing.Run(ctx)
		stop()
		if ctx.Err() != nil {
			return
		}
		// THE RESET IS NOT ON CONNECTING. A session that published a fill proved
		// the whole path works and has earned the base delay back; one that died
		// without resolving anything has proved nothing, whatever killed it.
		resolved := ing.resolvedAReport()
		if resolved {
			backoff.Reset()
		}
		deps.Logger.Warn("okx user-data stream ended; backing off before re-dialling — a stream "+
			"that never resolves a report must not be re-dialled at full speed, or the exchange "+
			"rate-limits and then bans this account",
			"err", runErr, "backoff", backoff.Delay(), "resolved_a_report_this_session", resolved)
		backoff.Wait(ctx)
	}
}

func (c *OKXConnector) runTicker(ctx context.Context, deps WorkerDeps) {
	interval := deps.TickerInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticks := NewMarkTickPublisher(deps.Publisher, deps.Logger, c.settings.MIC, deps.OnMarkTickDropped)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pollTicker(ctx, ticks)
		}
	}
}

// pollTicker publishes a market.v1.MarketDataEvent per instrument, feeding
// tv-sync's MarkSource — the OKX analog of the Binance ticker feed.
//
// It takes a *MarkTickPublisher rather than a bare Publisher because the
// envelope and the failure handling are shared with Binance (#673): a tick that
// does not reach the bus is counted and named there, once, for both venues. This
// used to end in a discarded publish error, which made a subject the broker was
// refusing indistinguishable from an exchange with nothing to say.
func (c *OKXConnector) pollTicker(ctx context.Context, ticks *MarkTickPublisher) {
	for instrument, instID := range c.settings.Symbols {
		px, err := c.rest.tickerPrice(ctx, instID)
		if err != nil {
			continue
		}
		// Skipped, not published, when the price will not convert (#94) — the same
		// stance as the fetch error just above. ParseDec answered an unparseable
		// price with ZERO before, and a zero mark does not look wrong downstream,
		// it looks free. One missed tick is recoverable; a fabricated one is folded.
		price, ok := ParseDec(px)
		if !ok {
			continue
		}
		ticks.PublishTrade(ctx, instrument, instID, price)
	}
}
