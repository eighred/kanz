package compliance

import (
	"context"
	"time"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/services/oms/internal/position"
)

// BookSource adapts the OMS position book (OMS-01e) to the COMP-01 gate's
// BookSource: the pre-trade check projects an order onto the live holdings the
// projector has folded from fills, so the hypothetical book is the real one.
type BookSource struct {
	book *position.Book
	now  func() time.Time
}

// NewBookSource wraps the position book.
func NewBookSource(book *position.Book) *BookSource {
	return &BookSource{book: book, now: time.Now}
}

var _ comp.BookSource = (*BookSource)(nil)

// Book returns the portfolio's current holdings as a compliance Book.
func (s *BookSource) Book(_ context.Context, portfolioID string) (*comp.Book, error) {
	return comp.BookFromSnapshot(s.book.Snapshot(portfolioID, s.now().UTC())), nil
}
