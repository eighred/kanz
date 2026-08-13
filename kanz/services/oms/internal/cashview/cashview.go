// Package cashview holds what each portfolio can spend, folded from the book of
// record's announcements (#450).
//
// THE OMS MUST NOT COMPUTE A BALANCE. accounting's ledger is the only component
// that folds trade legs, cash movements and corporate actions bitemporally with
// restatements; a second computation here would drift from it, and the drift
// would surface as a pre-trade control that refuses or admits wrongly. So this
// package only REMEMBERS a level somebody else computed.
//
// It is read on the order-admission path, so it is an in-memory map and never a
// query: a call into accounting from the pre-trade gate would put an
// externally-owned latency in front of every order and turn a degraded
// accounting service into a trading outage.
package cashview

import (
	"context"
	"sync"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
)

// Subject is where the book of record announces a portfolio's spendable cash.
//
// DECLARED HERE AND IN accounting's consume.SubjectPortfolioCash, because Go's
// internal-package rule forbids one service importing the other's internals —
// the service-isolation invariant test/arch enforces. Two literals that must
// agree is a drift risk, so test/arch/cash_subject_agreement_test.go fails the
// build if they ever disagree. That is the same trade the fill subjects already
// make, with the check the fill subjects do not have.
const Subject = "accounting.balance.portfolio"

// DefaultMaxAge bounds how old an announcement may be and still be treated as
// the current balance.
//
// A BALANCE THAT CANNOT BE SHOWN TO BE CURRENT IS NOT A BALANCE. Announcements
// are derived state, published after the ledger write they describe, and a
// publish failure must never fail that write (see consume.Folder.announce) — so
// one can be lost. Without a bound, the last balance received would be served
// forever, and a portfolio that had since spent everything would keep passing a
// buying-power check against a number from before the spending.
//
// Aging out yields "unknown", which BuyingPowerRule fails closed on: the order
// is refused with a reason rather than admitted on stale evidence. Same shape as
// the signal-freshness bound (#416) — an input that cannot be shown to be
// current is refused, not trusted.
const DefaultMaxAge = 5 * time.Minute

// Balance is one portfolio's spendable cash as the book of record last announced
// it.
type Balance struct {
	Total    *commonpb.Decimal
	Currency string
	AsOf     time.Time
	// ByVenueAccount is cash per (venue_account_id, asset) — what
	// execution.ExpectedBalances needs (#418). Keyed account → asset → amount.
	ByVenueAccount map[string]map[string]*commonpb.Decimal
}

// View is the OMS's memory of the announced balances. Safe for concurrent use:
// the bus handler writes it while the pre-trade gate reads it.
type View struct {
	mu      sync.RWMutex
	byPF    map[string]Balance
	maxAge  time.Duration
	now     func() time.Time
	stale   func(portfolioID string, age time.Duration)
	updated func(portfolioID string)
}

// Option customizes a View.
type Option func(*View)

// WithMaxAge overrides DefaultMaxAge. A non-positive value means NO BOUND, which
// is only ever right in a test: in production it restores exactly the "serve a
// balance from before the spending" failure the bound exists to prevent.
func WithMaxAge(d time.Duration) Option { return func(v *View) { v.maxAge = d } }

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(v *View) {
		if now != nil {
			v.now = now
		}
	}
}

// WithOnStale is called when a lookup finds a balance too old to use. An
// operator needs to know announcements stopped, rather than inferring it from
// orders being refused.
func WithOnStale(fn func(portfolioID string, age time.Duration)) Option {
	return func(v *View) { v.stale = fn }
}

// WithOnUpdate is called when an announcement is folded.
func WithOnUpdate(fn func(portfolioID string)) Option {
	return func(v *View) { v.updated = fn }
}

// New returns an empty View. Empty means UNKNOWN for every portfolio, not zero —
// see Lookup.
func New(opts ...Option) *View {
	v := &View{byPF: map[string]Balance{}, maxAge: DefaultMaxAge, now: time.Now}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Handle folds an accounting.v1.PortfolioCashBalance announcement. It is a
// bus.EventHandler.
//
// A LEVEL, NOT A DELTA, so this is a replace and not an accumulate: a
// redelivery is a no-op and a gap self-corrects on the next announcement. That
// is what makes a lost publish survivable, and it is why accounting sends the
// whole balance rather than the change.
func (v *View) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var msg accountingpb.PortfolioCashBalance
	if err := proto.Unmarshal(payload, &msg); err != nil {
		// A malformed announcement is a permanent defect, not a transient one:
		// returning it would nack and redeliver the same bad bytes forever. The
		// balance simply ages out to unknown, which fails closed downstream.
		return nil
	}
	if msg.GetPortfolioId() == "" || msg.GetTotal() == nil {
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95).
	// dec.FromProto materialises 10^abs(exponent), so a hostile or corrupt
	// announcement could hang the handler rather than post a wrong balance.
	if _, in := dec.InDomainDeep(&msg); !in {
		return nil
	}

	b := Balance{
		Total:          msg.GetTotal(),
		Currency:       msg.GetBaseCurrency(),
		AsOf:           msg.GetAsOf().AsTime(),
		ByVenueAccount: map[string]map[string]*commonpb.Decimal{},
	}
	for _, e := range msg.GetByVenueAccount() {
		if e.GetVenueAccountId() == "" || e.GetAsset() == "" {
			continue
		}
		byAsset := b.ByVenueAccount[e.GetVenueAccountId()]
		if byAsset == nil {
			byAsset = map[string]*commonpb.Decimal{}
			b.ByVenueAccount[e.GetVenueAccountId()] = byAsset
		}
		byAsset[e.GetAsset()] = e.GetAmount()
	}

	v.mu.Lock()
	v.byPF[msg.GetPortfolioId()] = b
	v.mu.Unlock()
	if v.updated != nil {
		v.updated(msg.GetPortfolioId())
	}
	return nil
}

// Lookup returns the portfolio's spendable cash, or ok=false when it is UNKNOWN
// — never announced, or announced too long ago to be shown current.
//
// ok=false IS NOT ZERO. A portfolio with no balance has not been shown to be
// empty; it has not been shown at all. Returning a zero here would read as an
// account with nothing in it, which is a breach rather than an unknown, and the
// difference decides whether an order is refused for a reason or refused for a
// fiction.
func (v *View) Lookup(portfolioID string) (Balance, bool) {
	v.mu.RLock()
	b, ok := v.byPF[portfolioID]
	v.mu.RUnlock()
	if !ok {
		return Balance{}, false
	}
	if v.maxAge > 0 {
		if age := v.now().UTC().Sub(b.AsOf); age > v.maxAge {
			if v.stale != nil {
				v.stale(portfolioID, age)
			}
			return Balance{}, false
		}
	}
	return b, true
}

// Spendable is the compliance BookSource's CashSource seam: the portfolio's
// total in its base currency, or ok=false when UNKNOWN.
//
// A narrower signature than Lookup on purpose. The gate needs one number, and a
// seam that handed it the whole Balance would invite a caller to read the
// per-account map for a decision the total is the right basis for — an exchange
// account's holding is not what a portfolio may spend.
func (v *View) Spendable(portfolioID string) (*commonpb.Decimal, string, bool) {
	b, ok := v.Lookup(portfolioID)
	if !ok {
		return nil, "", false
	}
	return b.Total, b.Currency, true
}
