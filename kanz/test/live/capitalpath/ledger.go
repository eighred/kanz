package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/pg"
)

const ledgerEntryTrade = 1

type ledgerRow struct {
	entryID        string
	portfolioID    string
	venueAccountID string
	entryType      int
	instrumentID   string
	quantity       string
	price          string
	effective      time.Time
	sourceRef      string
}

// verifyLedger opens a tenant-pinned, RLS-enforced pool and performs only
// SELECTs. The credential is also inspected: a role capable of changing the
// journal is refused even when this process promises not to use that privilege.
func verifyLedger(ctx context.Context, cfg config, fills []*orderpb.Fill) error {
	if len(fills) == 0 {
		return errors.New("capitalpath: cannot verify an empty fill set")
	}
	pool, err := pg.NewTenantPool(ctx, cfg.ledgerDSN, cfg.tenant)
	if err != nil {
		return fmt.Errorf("capitalpath: open tenant-scoped ledger reader: %w", err)
	}
	defer pool.Close()

	var canMutate bool
	if err := pool.QueryRow(ctx, `
		SELECT has_table_privilege(current_user, 'ledger_entries', 'INSERT')
		    OR has_table_privilege(current_user, 'ledger_entries', 'UPDATE')
		    OR has_table_privilege(current_user, 'ledger_entries', 'DELETE')`).Scan(&canMutate); err != nil {
		return fmt.Errorf("capitalpath: establish ledger credential posture: %w", err)
	}
	if canMutate {
		return errors.New("capitalpath: ledger evidence credential can mutate ledger_entries; bind a SELECT-only NOSUPERUSER role")
	}

	for _, fill := range fills {
		var row ledgerRow
		err := pool.QueryRow(ctx, `
			SELECT entry_id, portfolio_id, venue_account_id, entry_type, instrument_id,
			       COALESCE(quantity, ''), COALESCE(price, ''), effective_time, source_ref
			  FROM ledger_entries
			 WHERE entry_id = $1`, "fill:"+fill.GetFillId()).Scan(
			&row.entryID, &row.portfolioID, &row.venueAccountID, &row.entryType,
			&row.instrumentID, &row.quantity, &row.price, &row.effective, &row.sourceRef,
		)
		if err != nil {
			return fmt.Errorf("capitalpath: read ledger row for fill %q: %w", fill.GetFillId(), err)
		}
		if err := matchLedgerRow(cfg, fill, row); err != nil {
			return err
		}
	}
	return nil
}

func matchLedgerRow(cfg config, fill *orderpb.Fill, row ledgerRow) error {
	qty := new(big.Rat).Set(dec.FromProto(fill.GetQuantity()))
	switch fill.GetSide() {
	case orderpb.Side_SIDE_BUY:
	case orderpb.Side_SIDE_SELL:
		qty.Neg(qty)
	default:
		return fmt.Errorf("capitalpath: fill %q has unspecified side", fill.GetFillId())
	}
	price := dec.FromProto(fill.GetPrice())

	if row.entryID != "fill:"+fill.GetFillId() {
		return fmt.Errorf("capitalpath: ledger entry identity %q does not match fill %q", row.entryID, fill.GetFillId())
	}
	if row.portfolioID != cfg.portfolio {
		return fmt.Errorf("capitalpath: ledger portfolio %q, want %q", row.portfolioID, cfg.portfolio)
	}
	if row.venueAccountID != cfg.account || row.venueAccountID != fill.GetVenueAccountId() {
		return fmt.Errorf("capitalpath: ledger venue account %q disagrees with intent/fill account %q", row.venueAccountID, cfg.account)
	}
	if row.entryType != ledgerEntryTrade {
		return fmt.Errorf("capitalpath: ledger entry type %d is not TRADE", row.entryType)
	}
	if row.instrumentID != cfg.instrument || row.instrumentID != fill.GetInstrumentId() {
		return fmt.Errorf("capitalpath: ledger instrument %q disagrees with intent/fill", row.instrumentID)
	}
	if row.quantity != qty.RatString() {
		return fmt.Errorf("capitalpath: ledger quantity %q, want exact %q", row.quantity, qty.RatString())
	}
	if row.price != price.RatString() {
		return fmt.Errorf("capitalpath: ledger price %q, want exact %q", row.price, price.RatString())
	}
	if row.sourceRef != fill.GetFillId() {
		return fmt.Errorf("capitalpath: ledger source %q, want fill %q", row.sourceRef, fill.GetFillId())
	}
	if !row.effective.Equal(fill.GetExecutedAt().AsTime()) {
		return fmt.Errorf("capitalpath: ledger effective time %s, want venue execution time %s", row.effective.UTC(), fill.GetExecutedAt().AsTime().UTC())
	}
	return nil
}
