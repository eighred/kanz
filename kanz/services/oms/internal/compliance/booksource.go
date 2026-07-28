package compliance

import (
	"context"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/position"
)

// BookSource adapts the OMS position book (OMS-01e) to the COMP-01 gate's
// BookSource: the pre-trade check projects an order onto the live holdings the
// projector has folded from fills, so the hypothetical book is the real one.
type BookSource struct {
	book position.Store
	now  func() time.Time
}

// NewBookSource wraps the position book.
func NewBookSource(book position.Store) *BookSource {
	return &BookSource{book: book, now: time.Now}
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
	return comp.BookFromSnapshot(snap), nil
}
