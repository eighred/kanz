package capital

import (
	"context"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/jackc/pgx/v5"
)

// ReserveMany funds every debit currency of one order, including fees paid in
// another asset. No FX substitution is made. The caller supplies authoritative
// worst-case debits; an absent fee currency is not proof that fees are zero.
// Currencies lock in lexical order across replicas. Call once per admission,
// before taking any order/parent locks, and commit with the order and outbox.
func ReserveMany(ctx context.Context, tx pgx.Tx, portfolio, orderID string, debits []*commonpb.Money, now time.Time, maxAge time.Duration) error {
	if len(debits) == 0 || len(debits) > 32 {
		return ErrInvalid
	}
	byCurrency := make(map[string]dec.Exact, len(debits))
	currencies := make([]string, 0, len(debits))
	for _, debit := range debits {
		if debit == nil || debit.Amount == nil || !validID(debit.CurrencyCode) {
			return ErrInvalid
		}
		if _, ok := dec.InDomainDeep(debit); !ok {
			return ErrInvalid
		}
		value := dec.FromProto(debit.Amount)
		if value.Sign() < 0 {
			return ErrInvalid
		}
		if _, duplicate := byCurrency[debit.CurrencyCode]; duplicate {
			return ErrInvalid // caller must supply one complete bound per currency
		}
		currencies = append(currencies, debit.CurrencyCode)
		byCurrency[debit.CurrencyCode] = dec.Exact(value.RatString())
	}
	sort.Strings(currencies)
	// A failed later currency must undo the earlier reservations even if the
	// caller records a refusal and commits its enclosing transaction.
	batch, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = batch.Rollback(ctx) }()
	for _, currency := range currencies {
		if err := Reserve(ctx, batch, portfolio, currency, orderID, byCurrency[currency], now, maxAge); err != nil {
			return err
		}
	}
	return batch.Commit(ctx)
}
