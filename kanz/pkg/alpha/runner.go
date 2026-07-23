package alpha

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	signalpb "github.com/kanz-eng/kanz-schemas-go/signal/v1"

	"github.com/kanz-eng/kanz/internal/marketedge/book"
	"github.com/kanz-eng/kanz/internal/marketedge/depth"
	"github.com/kanz-eng/kanz/internal/marketedge/ingest"
	"github.com/kanz-eng/kanz/internal/marketedge/trades"
	"github.com/kanz-eng/kanz/internal/signal/translate"
)

// Feed is one (instrument, venue) pair's live inputs: the L2 depth stream and the
// trade tape. Depth is required; a nil Trades source simply leaves the volume
// side of the view empty (an engine that reads Volumes gets zeroes rather than a
// fabricated number).
type Feed struct {
	InstrumentID string
	MIC          string
	Depth        depth.DepthSource
	Trades       trades.TradeSource
}

// Config configures a Runner.
type Config struct {
	// Feeds are the (instrument, venue) inputs to fold. One book + one tape each.
	Feeds []Feed
	// Engines are the alpha engines to tick. EMPTY IS VALID and is what Kanz's own
	// open binary runs: it folds the books and publishes snapshots, and decides
	// nothing. The restricted layer supplies the engines.
	Engines []Engine

	// Publisher is the bus producer (snapshots, signals, order commands).
	Publisher translate.Publisher
	// Prices / Equity / Positions / Alloc bind the shared signal translator to live
	// platform state — the same seams the TradingView path resolves sizes against,
	// so both brains size identically.
	Prices    translate.PriceSource
	Equity    translate.EquitySource
	Positions translate.PositionSource
	Alloc     translate.AllocationPolicy
	// Gate is the kill-switch — the SAME *translate.Gate the webhook perimeter
	// holds, so one trip stops both brains at once. Required IF engines are
	// registered: an autonomous loop firing every 100ms with no operational brake
	// leaves `kill -9` as the only risk control, which is not one.
	Gate *translate.Gate
	// TenantOf maps a fund to its tenant; nil ⇒ the fund is the tenant.
	TenantOf func(fundID string) string

	// MarketDataTenant is the tenant stamped on BOOK SNAPSHOTS, which are shared
	// reference data and belong to no fund — every fund sees the same BTC-USD
	// book — so TenantOf cannot answer for them: there is no fund to ask about.
	//
	// It is separate from TenantOf for that reason, not by oversight. Snapshots
	// publish off a ticker with no inbound envelope, so leaving this empty makes
	// them depend entirely on the caller's ProducerConfig.Tenant; a caller that
	// leaves that empty and supplies tenants per-event will have every snapshot
	// rejected as "tenant_id required" while its fund-scoped signals keep
	// flowing, because those carry their own tenant.
	MarketDataTenant string

	// TickInterval is how often the engines are evaluated. <=0 ⇒ 100ms.
	TickInterval time.Duration
	// TradeRetention bounds the trade tape. <=0 ⇒ 1m.
	TradeRetention time.Duration
	// SnapshotInterval / SnapshotDepth govern the bounded book snapshots published
	// for durable replay and audit (the only depth that reaches the bus).
	SnapshotInterval time.Duration
	SnapshotDepth    int

	Logger *slog.Logger
	Now    func() time.Time
}

// Runner owns the native-alpha edge: it folds depth and trades into memory,
// publishes the bounded book snapshots, ticks the engines against the live state,
// and emits whatever they decide through the shared translator.
type Runner struct {
	cfg    Config
	tr     *translate.Translator
	views  []MarketView
	books  []*book.Book
	tapes  []*trades.Tape
	logger *slog.Logger
	// halted is the last observed gate state, for edge-triggered logging. Owned by
	// the tick goroutine.
	halted bool
}

// New validates the configuration and builds the Runner.
//
// The translator seams (Prices/Equity/Positions/Alloc) are required IF AND ONLY IF
// engines are registered. With no engines the Runner cannot emit anything, so
// demanding them would force Kanz's open binary to wire four dependencies it never
// reads — and the only way to satisfy that is with empty placeholders, which is
// exactly the fake configuration that rots. With engines they are mandatory, and a
// missing one fails HERE, at construction, rather than at the first live signal.
func New(cfg Config) (*Runner, error) {
	if cfg.Publisher == nil {
		return nil, errors.New("alpha: publisher is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 100 * time.Millisecond
	}
	if cfg.TradeRetention <= 0 {
		cfg.TradeRetention = time.Minute
	}

	r := &Runner{cfg: cfg, logger: cfg.Logger}
	if len(cfg.Engines) > 0 {
		tr, err := translate.New(translate.Options{
			Prices: cfg.Prices, Equity: cfg.Equity, Positions: cfg.Positions,
			Alloc: cfg.Alloc, Publisher: cfg.Publisher, Gate: cfg.Gate,
			TenantOf: cfg.TenantOf, Now: cfg.Now,
		})
		if err != nil {
			return nil, fmt.Errorf("alpha: engines are registered, so the signal path must be wired: %w", err)
		}
		r.tr = tr
	}
	for _, f := range cfg.Feeds {
		if f.Depth == nil {
			return nil, fmt.Errorf("alpha: feed %s/%s has no depth source", f.InstrumentID, f.MIC)
		}
		b := book.New(f.InstrumentID, f.InstrumentID, f.MIC)
		tp := trades.New(f.InstrumentID, f.MIC, cfg.TradeRetention)
		r.books = append(r.books, b)
		r.tapes = append(r.tapes, tp)
		r.views = append(r.views, &view{book: b, tape: tp})
	}
	return r, nil
}

// Run folds every feed, publishes snapshots, and ticks the engines until ctx is
// cancelled. It returns the first fatal feed error.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, len(r.cfg.Feeds)*2)
	var wg sync.WaitGroup

	for i, f := range r.cfg.Feeds {
		b, tp := r.books[i], r.tapes[i]

		// Depth fold + bounded snapshot publish (the delivered ingest engine).
		eng := ingest.New(ingest.Config{
			Book: b, Source: f.Depth, Publisher: r.cfg.Publisher, Logger: r.logger,
			SnapshotInterval: r.cfg.SnapshotInterval, SnapshotDepth: r.cfg.SnapshotDepth,
			Now: r.cfg.Now, Tenant: r.cfg.MarketDataTenant,
		})
		wg.Add(1)
		go func(inst, mic string) {
			defer wg.Done()
			if err := eng.Run(ctx); err != nil && ctx.Err() == nil {
				r.logger.Error("depth fold stopped", "instrument", inst, "venue", mic, "err", err)
				errc <- err
			}
		}(f.InstrumentID, f.MIC)

		// Trade tape fold. Optional: no source ⇒ the volume side stays empty.
		if f.Trades != nil {
			wg.Add(1)
			go func(src trades.TradeSource, inst, mic string) {
				defer wg.Done()
				if err := tp.Fold(ctx, src); err != nil && ctx.Err() == nil {
					r.logger.Error("trade fold stopped", "instrument", inst, "venue", mic, "err", err)
					errc <- err
				}
			}(f.Trades, f.InstrumentID, f.MIC)
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.tickLoop(ctx)
	}()

	// Surface the first fatal feed error; otherwise run until cancelled.
	var fatal error
	select {
	case <-ctx.Done():
	case fatal = <-errc:
		cancel()
	}
	wg.Wait()
	return fatal
}

// tickLoop evaluates the engines against the live views. It is a no-op when no
// engine is registered — Kanz's open binary folds and snapshots, and decides
// nothing.
func (r *Runner) tickLoop(ctx context.Context) {
	if len(r.cfg.Engines) == 0 {
		return
	}
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	// The brake, at the top of every cycle. Engines are not even evaluated while
	// halted: a halted system produces no decisions, not decisions it then declines
	// to act on. This is the same gate the webhook perimeter checks, so one trip
	// paralyzes both channels simultaneously.
	//
	// Logged on the EDGE, not the level: at a 100ms tick, logging the halted state
	// itself would emit ten lines a second for the whole outage and bury the cause
	// under the symptom. tick runs on one goroutine, so the flag needs no lock.
	if halted := r.cfg.Gate.Halted(); halted != r.halted {
		r.halted = halted
		_, reason, _ := r.cfg.Gate.State()
		if halted {
			r.logger.Warn("alpha execution halted: engines suppressed", "reason", reason)
		} else {
			r.logger.Info("alpha execution resumed", "reason", reason)
		}
	}
	if r.halted {
		return
	}
	for _, e := range r.cfg.Engines {
		for _, in := range e.Evaluate(ctx, r.views) {
			if err := r.emit(ctx, e.Name(), in); err != nil {
				// An engine's intent that cannot be emitted is logged and dropped, not
				// retried: the book has already moved on, and re-firing a stale
				// decision into a changed market is worse than not firing at all.
				r.logger.Error("native signal not emitted",
					"engine", e.Name(), "instrument", in.InstrumentID, "err", err)
			}
		}
	}
}

// emit turns an engine Intent into the StrategySignal FACT + the venue-allocated
// SubmitOrder commands, through the SAME translator the TradingView webhook path
// uses. Provenance is stamped NATIVE_ENGINE — never inferred downstream.
func (r *Runner) emit(ctx context.Context, engineName string, in Intent) error {
	strategyID := in.StrategyID
	if strategyID == "" {
		strategyID = engineName
	}
	if in.Nonce == "" {
		return fmt.Errorf("%w: engine %s produced an intent with no nonce (it would not dedup)",
			translate.ErrInvalidIntent, engineName)
	}
	_, err := r.tr.Emit(ctx, translate.Intent{
		// The nonce makes this deterministic per decision: a re-fired identical
		// intent dedups to one fan-out instead of double-trading.
		SignalID:     translate.DeterministicID(strategyID, in.InstrumentID, in.Nonce),
		StrategyID:   strategyID,
		FundID:       in.FundID,
		InstrumentID: in.InstrumentID,
		Action:       in.Action,
		Size:         in.Size,
		SizeType:     in.SizeType,
		Leverage:     big.NewRat(1, 1), // spot; native alpha runs unlevered in Phase 1
		OrderType:    in.OrderType,
		LimitPrice:   in.LimitPrice,
		TimeInForce:  in.TimeInForce,
		Source:       signalpb.SignalSource_SIGNAL_SOURCE_NATIVE_ENGINE,
	})
	return err
}

// Views exposes the live read-seam. It is here so a restricted engine can be
// driven and tested against a running Runner without reaching into the edge.
func (r *Runner) Views() []MarketView { return r.views }

// view adapts the in-memory book + trade tape to the MarketView read-seam.
type view struct {
	book *book.Book
	tape *trades.Tape
}

func (v *view) InstrumentID() string { return v.book.InstrumentID() }
func (v *view) MIC() string          { return v.book.MIC() }
func (v *view) BestBid() *big.Rat    { return v.book.BestBid() }
func (v *view) BestAsk() *big.Rat    { return v.book.BestAsk() }
func (v *view) LastPrice() *big.Rat  { return v.tape.LastPrice() }

func (v *view) Bids(n int) []Level { bids, _ := v.book.Top(n); return toLevels(bids) }
func (v *view) Asks(n int) []Level { _, asks := v.book.Top(n); return toLevels(asks) }

func (v *view) Volumes(window time.Duration) (buy, sell *big.Rat) { return v.tape.Volumes(window) }

func toLevels(in []book.Level) []Level {
	out := make([]Level, len(in))
	for i, l := range in {
		out[i] = Level{Price: l.Price, Size: l.Size}
	}
	return out
}

var _ MarketView = (*view)(nil)
