package compliance

import (
	"context"
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
	now  func() time.Time
}

// CashSource answers what a portfolio can spend, or ok=false when it is UNKNOWN
// (#450). Satisfied by cashview.View, which folds the book of record's
// announcements.
//
// A SEAM AND NOT A QUERY. This is read on the order-admission path; a call into
// accounting here would put an externally-owned latency in front of every order
// and turn a degraded accounting service into a trading outage.
type CashSource interface {
	Spendable(portfolioID string) (total *commonpb.Decimal, currency string, ok bool)
}

// NewBookSource wraps the position book. cash may be nil, and then every Book
// carries UNKNOWN cash — which BuyingPowerRule fails closed on, so a mandate
// declaring a spending limit refuses rather than admits.
func NewBookSource(book position.Store, cash CashSource) *BookSource {
	return &BookSource{book: book, cash: cash, now: time.Now}
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
	if s.cash != nil {
		if total, currency, ok := s.cash.Spendable(portfolioID); ok {
			b.Cash = &commonpb.Money{Amount: total, CurrencyCode: currency}
		}
	}
	return b, nil
}
