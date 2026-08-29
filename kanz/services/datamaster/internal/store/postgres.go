package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// PostgresGolden is the durable GoldenStore backed by golden_records
// (0001_master.sql) — the resolved record as a replace-on-write JSONB blob.
// Reuses the risk-engine persist.Postgres stance (pgxpool, RLS via app.tenant_id).
type PostgresGolden struct {
	pool *pgxpool.Pool
}

// NewPostgresGolden returns a golden store over an existing pool.
func NewPostgresGolden(pool *pgxpool.Pool) *PostgresGolden { return &PostgresGolden{pool: pool} }

func (p *PostgresGolden) Put(ctx context.Context, rec master.SecurityMaster) error {
	if rec.InstrumentID == "" {
		return errors.New("store: cannot store golden record with empty instrument_id")
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode golden record %s: %w", rec.InstrumentID, err)
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO golden_records (tenant_id, instrument_id, record, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, now())
		ON CONFLICT (tenant_id, instrument_id) DO UPDATE SET
			record     = EXCLUDED.record,
			updated_at = now()
	`, rec.InstrumentID, blob)
	if err != nil {
		return fmt.Errorf("put golden record %s: %w", rec.InstrumentID, err)
	}
	return nil
}

func (p *PostgresGolden) Get(ctx context.Context, instrumentID string) (master.SecurityMaster, bool, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx, `
		SELECT record FROM golden_records WHERE instrument_id = $1
	`, instrumentID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return master.SecurityMaster{}, false, nil
	}
	if err != nil {
		return master.SecurityMaster{}, false, fmt.Errorf("get golden record %s: %w", instrumentID, err)
	}
	var rec master.SecurityMaster
	if err := json.Unmarshal(blob, &rec); err != nil {
		return master.SecurityMaster{}, false, fmt.Errorf("decode golden record %s: %w", instrumentID, err)
	}
	return rec, true, nil
}

// PostgresExceptions is the durable ExceptionStore backed by exceptions +
// exception_overrides (0001_master.sql). Add is idempotent on the deterministic
// exception id (ON CONFLICT DO NOTHING — the re-detected break keeps its review
// state); Override appends to the immutable audit trail and flips the status.
type PostgresExceptions struct {
	pool *pgxpool.Pool
	// tenant is the tenant this instance serves, taken at construction exactly as
	// NewPostgresCycleLock takes it.
	//
	// IT IS REQUIRED BY THE OVERRIDE FACT, and it cannot come from the context.
	// bus/outbox resolve a tenant from the INBOUND DELIVERY a handler is running
	// in; an override arrives over HTTP, so there is no delivery and no tenant on
	// the context. Without this, outbox.From refuses the record and the whole
	// override transaction fails — every override, on the durable path. That is
	// the same defect that crash-looped the OMS when it published outside a
	// delivery, arriving here by the same route.
	tenant string
}

// NewPostgresExceptions returns an exception store over an existing pool.
func NewPostgresExceptions(pool *pgxpool.Pool, tenant string) *PostgresExceptions {
	if strings.TrimSpace(tenant) == "" {
		panic("store: NewPostgresExceptions requires a tenant — the override FACT cannot be published without one")
	}
	return &PostgresExceptions{pool: pool, tenant: tenant}
}

func (p *PostgresExceptions) Add(ctx context.Context, e pricing.Exception) error {
	if e.ID == "" {
		return errors.New("store: cannot add exception with empty id")
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO exceptions
			(tenant_id, exception_id, kind, instrument_id, detail, status, detected_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, exception_id) DO NOTHING
	`, e.ID, string(e.Kind), e.InstrumentID, e.Detail, string(e.Status), e.DetectedAt)
	if err != nil {
		return fmt.Errorf("add exception %s: %w", e.ID, err)
	}
	return nil
}

func (p *PostgresExceptions) AddAll(ctx context.Context, exs []pricing.Exception) error {
	for _, e := range exs {
		if err := p.Add(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Override appends the audit record, flips the status, announces the FACT and
// consumes claim — in ONE transaction.
//
// THE CLAIM IS THE FIRST STATEMENT, AND IT IS IN HERE RATHER THAN BEFORE (#807).
//
// It used to be a separate call that committed on its own, and the approve
// handler ran the two in sequence. A process death between them consumed the
// proposal and applied nothing: the second signature was spent, the override
// never happened, and no row, FACT or log line anywhere said so. The exception
// went on reading as OPEN while the proposal that would have closed it read as
// decided, so the contradiction was only findable by a human who went looking —
// and until they did, the book was valued off a price the system had flagged.
//
// FIRST, not last, so a racing approver blocks on the proposal row before it
// takes the exception's — this transaction never waits on one lock while holding
// another. Two approvers acting on one proposal at the same moment serialise
// here: the loser's DELETE sees zero rows once the winner commits and it returns
// ErrProposalAlreadyDecided having written nothing, instead of both appending an
// override for one decision.
func (p *PostgresExceptions) Override(ctx context.Context, id string, o pricing.Override, claim Claim) error {
	// ONE implementation of what a storable override is, shared with the
	// in-memory queue. It carries the self-approval refusal, so the clause the
	// control rests on cannot be reached around by a caller that skips the
	// handler.
	if err := o.Validate(); err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin override tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if claim.Held() {
		claimed, err := claimProposal(ctx, tx, claim.ProposalID)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("%w: proposal %s", ErrProposalAlreadyDecided, claim.ProposalID)
		}
	}

	var status, instrumentID string
	err = tx.QueryRow(ctx,
		`SELECT status, instrument_id FROM exceptions WHERE exception_id = $1 FOR UPDATE`,
		id).Scan(&status, &instrumentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: unknown exception %q", id)
	}
	if err != nil {
		return fmt.Errorf("lock exception %s: %w", id, err)
	}
	// chosen_price is TEXT holding the rational's exact RatString (0002), the same
	// stance as the accounting ledger's money columns: a lossless round-trip, and
	// `double` is banned for a price. It was DOUBLE PRECISION, which rounded the
	// figure a named human chose on its way into the audit trail.
	// approver is '' for a single-signed override (0004). Stored rather than
	// inferred: whether dual control was armed at the time is not recoverable
	// from a config value that has since changed.
	if _, err := tx.Exec(ctx, `
		INSERT INTO exception_overrides (tenant_id, exception_id, actor, approver, reason, chosen_price, overridden_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6)
	`, id, o.Actor, o.Approver, o.Reason, o.ChosenPrice.RatString(), o.At); err != nil {
		return fmt.Errorf("append override %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE exceptions SET status = $2 WHERE exception_id = $1
	`, id, string(pricing.StatusOverridden)); err != nil {
		return fmt.Errorf("mark overridden %s: %w", id, err)
	}
	// THE FACT IS ENQUEUED IN THIS TRANSACTION (#410).
	//
	// One commit covers the audit row and the announcement, so there is no window
	// in which an override exists and the estate will never hear of it, and none
	// in which a FACT is published for an override that rolled back. A publish
	// placed after the commit would have both.
	//
	// A failure here FAILS THE OVERRIDE. That is the right direction: the caller
	// is told nothing was recorded and can retry, whereas committing the override
	// and dropping the FACT would leave a decision that is durable here and
	// absent from the platform's audit trail — the exact silence #410 exists to
	// end, reintroduced one layer down.
	ev, err := overrideEvent(pricing.Exception{
		ID: id, InstrumentID: instrumentID, Status: pricing.StatusOverridden,
	}, o)
	if err != nil {
		return fmt.Errorf("build override FACT %s: %w", id, err)
	}
	// EXPLICIT, because there is no inbound delivery to inherit one from.
	ev.TenantID = p.tenant
	rec, err := outbox.From(ctx, ev)
	if err != nil {
		return fmt.Errorf("build override outbox record %s: %w", id, err)
	}
	if err := outbox.Enqueue(ctx, tx, rec); err != nil {
		return fmt.Errorf("enqueue override FACT %s: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit override %s: %w", id, err)
	}
	return nil
}

func (p *PostgresExceptions) Get(ctx context.Context, id string) (pricing.Exception, bool, error) {
	var e pricing.Exception
	var kind, status string
	err := p.pool.QueryRow(ctx, `
		SELECT exception_id, kind, instrument_id, detail, status, detected_at
		FROM exceptions WHERE exception_id = $1
	`, id).Scan(&e.ID, &kind, &e.InstrumentID, &e.Detail, &status, &e.DetectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pricing.Exception{}, false, nil
	}
	if err != nil {
		return pricing.Exception{}, false, fmt.Errorf("get exception %s: %w", id, err)
	}
	e.Kind = pricing.ExceptionKind(kind)
	e.Status = pricing.Status(status)
	if e.Overrides, err = p.loadOverrides(ctx, id); err != nil {
		return pricing.Exception{}, false, err
	}
	return e, true, nil
}

func (p *PostgresExceptions) Open(ctx context.Context) ([]pricing.Exception, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT exception_id, kind, instrument_id, detail, status, detected_at
		FROM exceptions WHERE status = $1 ORDER BY exception_id
	`, string(pricing.StatusOpen))
	if err != nil {
		return nil, fmt.Errorf("query open exceptions: %w", err)
	}
	defer rows.Close()
	var out []pricing.Exception
	for rows.Next() {
		var e pricing.Exception
		var kind, status string
		if err := rows.Scan(&e.ID, &kind, &e.InstrumentID, &e.Detail, &status, &e.DetectedAt); err != nil {
			return nil, fmt.Errorf("scan exception: %w", err)
		}
		e.Kind = pricing.ExceptionKind(kind)
		e.Status = pricing.Status(status)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PostgresExceptions) loadOverrides(ctx context.Context, id string) ([]pricing.Override, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT actor, approver, reason, chosen_price, overridden_at
		FROM exception_overrides WHERE exception_id = $1 ORDER BY seq
	`, id)
	if err != nil {
		return nil, fmt.Errorf("query overrides %s: %w", id, err)
	}
	defer rows.Close()
	var out []pricing.Override
	for rows.Next() {
		var o pricing.Override
		var price string
		if err := rows.Scan(&o.Actor, &o.Approver, &o.Reason, &price, &o.At); err != nil {
			return nil, fmt.Errorf("scan override %s: %w", id, err)
		}
		// A stored price that will not parse is a corrupt audit record. Refuse the
		// read rather than serving a reviewer a zero, or a number nobody chose.
		rat, ok := new(big.Rat).SetString(price)
		if !ok {
			return nil, fmt.Errorf("override %s: chosen price %q is not an exact decimal", id, price)
		}
		o.ChosenPrice = rat
		out = append(out, o)
	}
	return out, rows.Err()
}

var (
	_ GoldenStore    = (*PostgresGolden)(nil)
	_ ExceptionStore = (*PostgresExceptions)(nil)
)
