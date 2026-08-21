package compliance

import (
	"context"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/position"
)

// BookSource adapts the OMS position book (OMS-01e) to the COMP-01 gate's
// BookSource: the pre-trade check projects an order onto the live holdings the
// projector has folded from fills, so the hypothetical book is the real one.
type BookSource struct {
	book position.Store
	cash CashSource
	risk RiskSource
	now  func() time.Time
}

// CashSource answers what a portfolio can spend, or ok=false when it is UNKNOWN
// (#450). Satisfied by cashview.View, which folds the book of record's
// announcements.
//
// A SEAM AND NOT A QUERY. This is read on the order-admission path; a call into
// accounting here would put an externally-owned latency in front of every order
// and turn a degraded accounting service into a trading outage.
//
// completeness is what the ANNOUNCING deployment said that total contains
// (#614), or nil when it said nothing — which is not the same as "nothing is
// missing" and must not be flattened into it. It is carried onto the Book so a
// buying-power refusal can name what the balance was short of.
type CashSource interface {
	Spendable(portfolioID string) (total *commonpb.Decimal, currency string, completeness *comp.CashCompleteness, ok bool)
}

// NewBookSource wraps the position book. cash may be nil, and then every Book
// carries UNKNOWN cash — which BuyingPowerRule fails closed on, so a mandate
// declaring a spending limit refuses rather than admits.
// RiskSource answers what the RISK ENGINE has computed for a portfolio, or
// ok=false when the measure is UNKNOWN (#438). Satisfied by riskview.View, which
// folds risk.portfolio.measures_computed with a freshness bound.
//
// AN INTERFACE, AND NOT A CLIENT. It is deliberately a lookup against local
// state rather than a call: #438 rules out a synchronous request into the risk
// engine because it would put an unbounded, externally-owned latency in front of
// every order and turn a degraded risk service into a trading outage.
type RiskSource interface {
	Measure(portfolioID, name string) (*big.Rat, bool)
}

func NewBookSource(book position.Store, cash CashSource, risk RiskSource) *BookSource {
	return &BookSource{book: book, cash: cash, risk: risk, now: time.Now}
}

var _ comp.BookSource = (*BookSource)(nil)

// Book returns the portfolio's current holdings as a compliance Book.
func (s *BookSource) Book(ctx context.Context, portfolioID string) (*comp.Book, error) {
	snap, err := s.book.Snapshot(ctx, portfolioID, s.now().UTC())
	if err != nil {
		// The gate must NOT admit an order against a book it could not read. An empty
		// book passes every concentration limit there is (EXEC-M18).
		return nil, err
	}
	b := comp.BookFromSnapshot(snap)

	// CASH COMES FROM THE BOOK OF RECORD, NOT FROM HERE (#450). The position book
	// folds fills and knows what the portfolio HOLDS; only accounting's ledger
	// knows what it can SPEND, because only that fold takes cash movements and
	// corporate actions bitemporally with restatements. The snapshot's own
	// CashBalance is left unset by position.Book for exactly this reason — its
	// Snapshot says NAV is "a funded-book proxy until a cash/equity source lands".
	//
	// UNKNOWN STAYS UNKNOWN. A nil source, a portfolio never announced, or a
	// balance too old to be shown current all leave Book.Cash nil, and
	// BuyingPowerRule refuses on that with a named reason. Substituting a zero
	// would read as an empty account — a breach rather than an unknown — and the
	// difference decides whether an order is refused for a reason or for a fiction.
	//
	// AND WHAT THAT NUMBER IS MISSING TRAVELS WITH IT (#614). accounting folds six
	// kinds of journal entry and nothing on this platform produces two of them, so
	// the announced balance omits every dividend, coupon and merger payment (#588).
	// BuyingPowerRule fails closed on the short number either way — the sign of the
	// error is not knowable, so inflating it would be a guess that admits orders
	// the fund cannot pay for — but the refusal now SAYS what it could not account
	// for instead of reading as a spending limit.
	if s.cash != nil {
		if total, currency, completeness, ok := s.cash.Spendable(portfolioID); ok {
			b.Cash = &commonpb.Money{Amount: total, CurrencyCode: currency}
			b.CashCompleteness = completeness
		}
	}
	// RISK COMES FROM THE RISK ENGINE, FOLDED LOCALLY (#438). Bound as a closure
	// rather than copied as a map because "unknown" must keep its three causes as
	// one answer — never announced, absent from the last announcement, or too old
	// to be current — and a map would make the third invisible.
	//
	// nil source ⇒ Book.Risk stays nil ⇒ RiskLimitRule refuses any declared risk
	// limit rather than passing it. Which is the correct posture for a deployment
	// that has not wired risk: a mandate declaring a VaR ceiling on a platform
	// that cannot compute VaR must not read as compliant.
	if s.risk != nil {
		pf := portfolioID
		b.Risk = func(measure string) (*big.Rat, bool) { return s.risk.Measure(pf, measure) }
	}
	return b, nil
}
