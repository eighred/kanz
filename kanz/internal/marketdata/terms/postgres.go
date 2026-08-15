// Package terms is the durable store for derivative contract terms (DERIV-01a,
// #345) — the static specification a market quote for a derivative joins to.
//
// reference.v1.ContractTerms has defined these terms for a while and nothing
// stored them, so compute.TermsProvider — the seam the Greeks layer and the
// revaluation path both consume — has had no production implementation at all.
// Every implementation in the tree is a test double. This is the store that
// makes a real one possible.
//
// # Not tenant-scoped
//
// Contract terms are universal market fact, not tenant-owned state: a listed
// option's strike is the same for everyone who trades it. So the table carries
// no tenant_id and no RLS — the same call price_observations made beside it.
// Readers open pg.NewGlobalPool with a stated justification. Tenant isolation
// lives on the portfolio state that consumes these terms.
//
// This is the line that separates it from datamaster's golden_records, which IS
// tenant-scoped: a golden record is a RESOLUTION across vendor feeds and can
// legitimately differ per tenant. A strike is not a resolution.
//
// # Point-in-time
//
// as_of versions each record, mirroring ContractTerms's own documented contract.
// A restatement is a new row at a newer as_of, never an overwrite — options are
// amended in practice (corporate actions adjust strikes and multipliers), and a
// store that overwrote would silently reprice history against terms that did not
// apply at the time.
package terms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

// Kind is the ContractTerms oneof case, lifted out of the blob so a query can
// filter without decoding every row.
type Kind string

const (
	KindOption Kind = "OPTION"
	KindSwap   Kind = "SWAP"
	KindFuture Kind = "FUTURE"
	// KindBond is a bond's terms (#509). A bond is not a derivative, and this
	// store's table comment lists only the three derivative kinds — the row shape
	// fits unchanged because `kind` is TEXT with no CHECK and `underlying_id` is
	// already nullable for swaps, which reference no single instrument either.
)

// ErrNoTerms reports that no terms record exists for an instrument at or before
// the requested as_of.
//
// WHAT IT DOES AND DOES NOT DISTINGUISH — the first version of this comment
// overstated it, so this is precise.
//
// It separates "no terms row" from "found terms". It does NOT separate a share
// (never had terms, correctly priced linearly) from a DERIVATIVE WHOSE TERMS
// WERE NEVER LOADED (which would then be priced as if it were a share). Both
// arrive here as no row, and this store cannot tell them apart because it holds
// no notion of what an instrument IS.
//
// That distinction needs reference.v1.InstrumentReference.asset_class — a
// derivative is ASSET_CLASS_DERIVATIVE, and contract.proto's own header says
// these terms are what "turn the ASSET_CLASS_DERIVATIVE tag into analytics". A
// caller holding both can tell them apart; a caller holding only this store
// cannot, and must not claim to.
//
// It still matters that this is an ERROR rather than a zero Record: the caller
// gets a signal it has to handle, instead of an empty OptionTerms that decodes
// as a contract with a zero strike. compute.TermsProvider's own signature
// collapses everything to ok=false — greeks.go says so ("isOption=false when the
// position is not an option OR its pricing inputs are unavailable") — so the
// place to make a missing load visible is the provider's observability, not this
// return value.
var ErrNoTerms = errors.New("terms: no contract terms for instrument at or before as_of")

// Postgres is the contract-terms store backed by 0002_contract_terms.sql.
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres returns a store over an existing pool. The caller owns the pool
// lifecycle, matching the other marketdata stores.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Record is one versioned terms row.
type Record struct {
	InstrumentID string
	AsOf         time.Time
	UnderlyingID string // empty for a swap, which references no single instrument
	Kind         Kind
	Terms        *referencepb.ContractTerms
}

// Put stores terms idempotently. The primary key is (instrument_id, as_of), so
// an exact re-put is ON CONFLICT DO NOTHING — a terms record at a given as_of is
// immutable, and an amendment is a new as_of row rather than an update. Written
// in one transaction so a chain load is all-or-nothing: a partially loaded chain
// would calibrate a surface from some of an underlying's strikes, which fits
// cleanly and is wrong.
func (p *Postgres) Put(ctx context.Context, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("terms: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, r := range recs {
		if r.InstrumentID == "" {
			return errors.New("terms: cannot store a record with no instrument_id")
		}
		if r.Terms == nil {
			return fmt.Errorf("terms: %s has no ContractTerms payload", r.InstrumentID)
		}
		blob, merr := proto.Marshal(r.Terms)
		if merr != nil {
			return fmt.Errorf("terms: marshal %s: %w", r.InstrumentID, merr)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO contract_terms (instrument_id, as_of, underlying_id, kind, terms)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (instrument_id, as_of) DO NOTHING`,
			r.InstrumentID, r.AsOf.UTC(), nullString(r.UnderlyingID), string(r.Kind), blob); err != nil {
			return fmt.Errorf("terms: insert %s: %w", r.InstrumentID, err)
		}
	}
	return tx.Commit(ctx)
}

// LatestAsOf returns the newest terms record for instrumentID effective at or
// before asOf — the point-in-time read compute.TermsProvider is built on.
func (p *Postgres) LatestAsOf(ctx context.Context, instrumentID string, asOf time.Time) (Record, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT instrument_id, as_of, underlying_id, kind, terms
		FROM contract_terms
		WHERE instrument_id = $1 AND as_of <= $2
		ORDER BY as_of DESC
		LIMIT 1`, instrumentID, asOf.UTC())

	rec, err := scanRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s at %s", ErrNoTerms, instrumentID, asOf.UTC().Format(time.RFC3339))
	}
	return rec, err
}

// ChainAsOf returns every derivative whose underlying is underlyingID, each at
// its own newest as_of at or before asOf — the reverse direction
// volsurface.QuoteProvider needs and compute.TermsProvider cannot express.
//
// DISTINCT ON gives ONE row per instrument: without it, an instrument amended
// three times contributes three rows, and the calibrator would fit a surface
// containing the same strike at three different multipliers. An empty result is
// not an error — an underlying with no listed derivatives is a real answer, and
// the caller distinguishes it from a missing load by whether the underlying
// itself is known.
func (p *Postgres) ChainAsOf(ctx context.Context, underlyingID string, asOf time.Time, kind Kind) ([]Record, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (instrument_id) instrument_id, as_of, underlying_id, kind, terms
		FROM contract_terms
		WHERE underlying_id = $1 AND as_of <= $2 AND kind = $3
		ORDER BY instrument_id, as_of DESC`, underlyingID, asOf.UTC(), string(kind))
	if err != nil {
		return nil, fmt.Errorf("terms: chain for %s: %w", underlyingID, err)
	}
	defer rows.Close()

	out := make([]Record, 0)
	for rows.Next() {
		rec, serr := scanRecord(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Ping checks store liveness for readiness.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(s scanner) (Record, error) {
	var (
		rec        Record
		underlying *string
		kind       string
		blob       []byte
	)
	if err := s.Scan(&rec.InstrumentID, &rec.AsOf, &underlying, &kind, &blob); err != nil {
		return Record{}, err
	}
	rec.UnderlyingID = derefString(underlying)
	rec.Kind = Kind(kind)
	rec.Terms = &referencepb.ContractTerms{}
	if err := proto.Unmarshal(blob, rec.Terms); err != nil {
		return Record{}, fmt.Errorf("terms: decode %s: %w", rec.InstrumentID, err)
	}
	return rec, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
