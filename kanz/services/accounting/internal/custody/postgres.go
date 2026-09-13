package custody

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// Postgres is the durable Store over the 0007_custody_recon.sql schema.
//
// IT IS THE ONLY PRODUCTION HOME FOR THE BREAK LIFECYCLE. Statements and runs
// could be rebuilt by replaying the bus; an operator's assignment and explanation
// could not, and neither could first_seen_at — the instant the whole age of a
// break is measured from. See the migration's header for why that matters more
// than it sounds.
//
// NOTE THAT IT HONOURS ctx WHERE MemoryStore IGNORES IT. Any behaviour that
// depends on a deadline or a cancellation is unobservable through the in-memory
// seam and is only actually proven here.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns the
// pool, which must be tenant-scoped (internal/pg.NewTenantPool) — every table
// here is RLS-protected and an unscoped session raises rather than reading empty.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Ping reports whether the store is reachable.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// SaveStatement implements Store, idempotent on statement_id.
//
// A REDELIVERY REPLACES RATHER THAN ACCUMULATES, because the statement is a
// LEVEL. Two rows for one statement_id would make "the newest statement" depend
// on which copy arrived last, and a custodian resending an unchanged statement
// would look like new information.
func (p *Postgres) SaveStatement(ctx context.Context, s Statement) error {
	if err := s.Validate(); err != nil {
		return err
	}
	positions, err := ratsToJSON(s.Positions)
	if err != nil {
		return fmt.Errorf("custody: statement %s positions: %w", s.StatementID, err)
	}
	cash, err := ratsToJSON(s.Cash)
	if err != nil {
		return fmt.Errorf("custody: statement %s cash: %w", s.StatementID, err)
	}
	// THE GRAIN IS PERSISTED WITH THE LINES, never re-derived from whether the
	// list is empty (#1049). A statement read back with an empty list and no
	// grain would be indistinguishable from a balances-only feed, and the
	// transaction pass would then run against nothing and report every execution
	// in the window as one the custodian never saw.
	transactions, err := transactionsToJSON(s.Transactions)
	if err != nil {
		return fmt.Errorf("custody: statement %s transactions: %w", s.StatementID, err)
	}
	const q = `
INSERT INTO custody_statements
    (statement_id, custodian_id, portfolio_id, business_date, positions, cash, received_at, transactions, grain, values_verified)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true)
ON CONFLICT (tenant_id, statement_id) DO UPDATE SET
    custodian_id  = EXCLUDED.custodian_id,
    portfolio_id  = EXCLUDED.portfolio_id,
    business_date = EXCLUDED.business_date,
    positions     = EXCLUDED.positions,
    cash          = EXCLUDED.cash,
    received_at   = EXCLUDED.received_at,
    transactions  = EXCLUDED.transactions,
    grain         = EXCLUDED.grain, values_verified = true`
	tx, err := p.exactTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, q,
		s.StatementID, s.CustodianID, s.PortfolioID, BusinessDay(s.BusinessDate),
		positions, cash, s.ReceivedAt.UTC(), transactions, s.Grain.String())
	if err != nil {
		return fmt.Errorf("custody: save statement %s: %w", s.StatementID, err)
	}
	return tx.Commit(ctx)
}

// LatestStatement implements Store.
func (p *Postgres) LatestStatement(ctx context.Context, subject Subject) (Statement, error) {
	const q = `
SELECT statement_id, custodian_id, portfolio_id, business_date, positions, cash, received_at, transactions, grain, values_verified
FROM custody_statements
WHERE portfolio_id = $1 AND custodian_id = $2 AND business_date = $3
ORDER BY received_at DESC
LIMIT 1`
	var (
		s                  Statement
		positions, cash    []byte
		transactions       []byte
		grain              string
		verified           bool
		businessDate, recv time.Time
	)
	err := p.pool.QueryRow(ctx, q, subject.PortfolioID, subject.CustodianID, BusinessDay(subject.BusinessDate)).
		Scan(&s.StatementID, &s.CustodianID, &s.PortfolioID, &businessDate, &positions, &cash, &recv,
			&transactions, &grain, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Statement{}, ErrNoStatement
	}
	if err != nil {
		return Statement{}, fmt.Errorf("custody: latest statement: %w", err)
	}
	if !verified {
		return Statement{}, ErrUnverifiedPrecision
	}
	s.BusinessDate = BusinessDay(businessDate)
	s.ReceivedAt = recv.UTC()
	if s.Positions, err = jsonToRats(positions); err != nil {
		return Statement{}, fmt.Errorf("custody: statement %s positions: %w", s.StatementID, err)
	}
	if s.Cash, err = jsonToRats(cash); err != nil {
		return Statement{}, fmt.Errorf("custody: statement %s cash: %w", s.StatementID, err)
	}
	if s.Transactions, err = jsonToTransactions(transactions); err != nil {
		return Statement{}, fmt.Errorf("custody: statement %s transactions: %w", s.StatementID, err)
	}
	s.Grain = parseGrain(grain)
	return s, nil
}

// SaveRun implements Store, idempotent on run_id. The run's breaks are NOT
// written here — they live in custody_breaks under their own cross-run identity,
// and UpsertBreaks owns them.
func (p *Postgres) SaveRun(ctx context.Context, r Run) error {
	tolerance, err := exactStored(orZeroRat(r.Tolerance))
	if err != nil {
		return err
	}
	const q = `
INSERT INTO custody_runs
    (run_id, portfolio_id, custodian_id, business_date, outcome, statement_id, tolerance, completed_at, failure_reason, values_verified)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true)
ON CONFLICT (tenant_id, run_id) DO NOTHING`
	tx, err := p.exactTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, q,
		r.RunID, r.Subject.PortfolioID, r.Subject.CustodianID, BusinessDay(r.Subject.BusinessDate),
		r.Outcome.String(), r.StatementID, tolerance, r.CompletedAt.UTC(), r.FailureReason)
	if err != nil {
		return fmt.Errorf("custody: save run %s: %w", r.RunID, err)
	}
	return tx.Commit(ctx)
}

// LatestRun implements Store.
func (p *Postgres) LatestRun(ctx context.Context, portfolioID, custodianID string) (Run, error) {
	const q = `
SELECT run_id, portfolio_id, custodian_id, business_date, outcome, statement_id, tolerance, completed_at, failure_reason, values_verified
FROM custody_runs
WHERE portfolio_id = $1 AND custodian_id = $2
ORDER BY completed_at DESC
LIMIT 1`
	var (
		r                         Run
		outcome, tolerance        string
		businessDate, completedAt time.Time
		verified                  bool
	)
	err := p.pool.QueryRow(ctx, q, portfolioID, custodianID).Scan(
		&r.RunID, &r.Subject.PortfolioID, &r.Subject.CustodianID, &businessDate,
		&outcome, &r.StatementID, &tolerance, &completedAt, &r.FailureReason, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("custody: latest run: %w", err)
	}
	r.Subject.BusinessDate = BusinessDay(businessDate)
	r.CompletedAt = completedAt.UTC()
	r.Outcome = parseOutcome(outcome)
	if !verified {
		return Run{}, ErrUnverifiedPrecision
	}
	r.Tolerance, err = parseStored(tolerance)
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// UpsertBreaks implements Store.
//
// IT IS ONE TRANSACTION, and that is not incidental. The redetection update and
// the resolve-on-absence sweep are two halves of one statement about what this
// run found; splitting them lets a crash between the two leave a break both
// "still detected" and "resolved because absent", which is a lifecycle state no
// operator can reason about and no later run repairs.
func (p *Postgres) UpsertBreaks(ctx context.Context, subject Subject, detected []Break, now time.Time) ([]Break, error) {
	tx, err := p.exactTransaction(ctx)
	if err != nil {
		return nil, fmt.Errorf("custody: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids := make([]string, 0, len(detected))
	for _, b := range detected {
		ibor, err := optionalStored(b.IBOR)
		if err != nil {
			return nil, err
		}
		custodian, err := optionalStored(b.Custodian)
		if err != nil {
			return nil, err
		}
		difference, err := optionalStored(b.Diff)
		if err != nil {
			return nil, err
		}
		ids = append(ids, b.BreakID)
		// THE UPDATE LIST IS DELIBERATELY THE THREE FIGURES AND last_seen_at.
		// status, assignee, explanation and first_seen_at are the operator's
		// work; a run that overwrote them would reset every assignment every
		// morning, which is precisely what makes a break queue unreadable.
		//
		// A break that had been RESOLVED and is detected again is genuinely new
		// work: it is re-opened, first seen now, and it does NOT inherit the
		// explanation written for the previous occurrence — which, by the fact of
		// its return, did not hold.
		const q = `
INSERT INTO custody_breaks
    (break_id, portfolio_id, custodian_id, kind, break_key, ibor, custodian, difference,
     status, assignee, explanation, first_seen_at, last_seen_at, status_changed_at, values_verified)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', '', '', $9, $9, $9, true)
ON CONFLICT (tenant_id, break_id) DO UPDATE SET
    ibor              = EXCLUDED.ibor,
    custodian         = EXCLUDED.custodian,
    difference        = EXCLUDED.difference,
    last_seen_at      = EXCLUDED.last_seen_at, values_verified = true,
    status            = CASE WHEN custody_breaks.status = 'resolved' THEN 'open'  ELSE custody_breaks.status END,
    assignee          = CASE WHEN custody_breaks.status = 'resolved' THEN ''      ELSE custody_breaks.assignee END,
    explanation       = CASE WHEN custody_breaks.status = 'resolved' THEN ''      ELSE custody_breaks.explanation END,
    first_seen_at     = CASE WHEN custody_breaks.status = 'resolved' THEN EXCLUDED.first_seen_at ELSE custody_breaks.first_seen_at END,
    status_changed_at = CASE WHEN custody_breaks.status = 'resolved' THEN EXCLUDED.status_changed_at ELSE custody_breaks.status_changed_at END`
		if _, err := tx.Exec(ctx, q,
			b.BreakID, subject.PortfolioID, subject.CustodianID, b.Kind.String(), b.Key,
			ibor, custodian, difference,
			now.UTC()); err != nil {
			return nil, fmt.Errorf("custody: upsert break %s: %w", b.BreakID, err)
		}
	}

	// RESOLVE ON ABSENCE, scoped to this run's pair. The book and the custodian
	// now agree about anything this run did not report, which is the only
	// automatic route to RESOLVED and the thing that stops the queue growing
	// without bound. Scoping to the pair matters: a sweep across the whole tenant
	// would resolve every OTHER custodian's breaks on every run, because this run
	// never had anything to say about them.
	const sweep = `
UPDATE custody_breaks
SET status = 'resolved', status_changed_at = $3
WHERE portfolio_id = $1 AND custodian_id = $2
  AND status IN ('open', 'assigned', 'explained')
  AND NOT (break_id = ANY($4))`
	if _, err := tx.Exec(ctx, sweep, subject.PortfolioID, subject.CustodianID, now.UTC(), ids); err != nil {
		return nil, fmt.Errorf("custody: resolve absent breaks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("custody: commit: %w", err)
	}
	return p.OutstandingBreaks(ctx)
}

// OutstandingBreaks implements Store.
func (p *Postgres) OutstandingBreaks(ctx context.Context) ([]Break, error) {
	const q = `
SELECT break_id, kind, break_key, ibor, custodian, difference, status, assignee, explanation,
       first_seen_at, last_seen_at, status_changed_at, revision, values_verified
FROM custody_breaks
WHERE status IN ('open', 'assigned', 'explained')
ORDER BY break_id`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("custody: outstanding breaks: %w", err)
	}
	defer rows.Close()
	return scanBreaks(rows)
}

// LoadBreak implements Store.
func (p *Postgres) LoadBreak(ctx context.Context, breakID string) (Break, error) {
	const q = `
SELECT break_id, kind, break_key, ibor, custodian, difference, status, assignee, explanation,
       first_seen_at, last_seen_at, status_changed_at, revision, values_verified
FROM custody_breaks
WHERE break_id = $1`
	rows, err := p.pool.Query(ctx, q, breakID)
	if err != nil {
		return Break{}, fmt.Errorf("custody: load break: %w", err)
	}
	defer rows.Close()
	out, err := scanBreaks(rows)
	if err != nil {
		return Break{}, err
	}
	if len(out) == 0 {
		return Break{}, ErrNoBreak
	}
	return out[0], nil
}

// Subjects implements Store.
func (p *Postgres) Subjects(ctx context.Context) ([]Subject, error) {
	const q = `
SELECT DISTINCT portfolio_id, custodian_id
FROM custody_statements
ORDER BY portfolio_id, custodian_id`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("custody: subjects: %w", err)
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		var s Subject
		if err := rows.Scan(&s.PortfolioID, &s.CustodianID); err != nil {
			return nil, fmt.Errorf("custody: scan subject: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanBreaks(rows pgx.Rows) ([]Break, error) {
	var out []Break
	for rows.Next() {
		var (
			b                                  Break
			kind, status                       string
			ibor, custodian, difference        string
			firstSeen, lastSeen, statusChanged time.Time
		)
		if err := rows.Scan(&b.BreakID, &kind, &b.Key, &ibor, &custodian, &difference,
			&status, &b.Assignee, &b.Explanation, &firstSeen, &lastSeen, &statusChanged, &b.Revision, &b.ValuesVerified); err != nil {
			return nil, fmt.Errorf("custody: scan break: %w", err)
		}
		b.Kind = parseKind(kind)
		b.Status = parseStatus(status)
		if b.ValuesVerified {
			var err error
			b.IBOR, err = parseRatText(ibor)
			if err != nil {
				return nil, err
			}
			b.Custodian, err = parseRatText(custodian)
			if err != nil {
				return nil, err
			}
			b.Diff, err = parseRatText(difference)
			if err != nil {
				return nil, err
			}
		}
		b.FirstSeenAt, b.LastSeenAt, b.StatusChangedAt = firstSeen.UTC(), lastSeen.UTC(), statusChanged.UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// parseKind maps a stored kind name back to the engine's value.
//
// IT DERIVES THE MAPPING FROM recon.BreakKinds() rather than switching on string
// literals, so a kind added to the engine round-trips through storage without a
// second edit here — the same derive-don't-retype stance the metric labels take.
func parseKind(name string) recon.BreakKind {
	for _, k := range recon.BreakKinds() {
		if k.String() == name {
			return k
		}
	}
	return recon.BreakKind(-1)
}

// parseGrain maps a stored grain name back to the engine's value.
//
// AN UNRECOGNISED NAME BECOMES GrainUnknown, which keeps the transaction pass
// unrun rather than guessing at the nearest value — the same stance grainFromWire
// takes on an unrecognised wire enum, and the same one parseKind takes when it
// returns a kind no consumer will match. Deriving the mapping from recon.Grains()
// rather than switching on string literals means a grain added to the engine
// round-trips through storage with no second edit here.
func parseGrain(name string) recon.Grain {
	for _, g := range recon.Grains() {
		if g.String() == name {
			return g
		}
	}
	return recon.GrainUnknown
}

func parseStatus(name string) BreakStatus {
	for _, s := range BreakStatuses() {
		if s.String() == name {
			return s
		}
	}
	return BreakStatusUnspecified
}

func parseOutcome(name string) Outcome {
	for _, o := range Outcomes() {
		if o.String() == name {
			return o
		}
	}
	return OutcomeUnspecified
}

// ratsToJSON renders a rat map as {key: exact rational text}.
//
// TEXT, NEVER A JSON NUMBER. encoding/json renders a number as float64, so a
// quantity with more precision than a float carries would be silently rounded on
// the way into the book of record's own reconciliation — the exact class of error
// the platform's Decimal rule exists to prevent.
func ratsToJSON(in map[string]*big.Rat) ([]byte, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == nil {
			return nil, fmt.Errorf("nil value for %q", k)
		}
		text, err := exactStored(v)
		if err != nil {
			return nil, err
		}
		out[k] = text
	}
	return json.Marshal(out)
}

// storedTransaction is the JSONB shape of one custodian trade line.
//
// EVERY FIGURE IS TEXT, for ratsToJSON's reason: encoding/json renders a number
// as float64, so a quantity with more precision than a float carries would be
// silently rounded on the way into the record the book of record is reconciled
// against. Dates are RFC3339 for the same reason a text decimal is text — a
// stable, exact, human-readable round trip.
type storedTransaction struct {
	ExternalRef    string `json:"external_ref"`
	TradeDate      string `json:"trade_date,omitempty"`
	SettlementDate string `json:"settlement_date,omitempty"`
	InstrumentID   string `json:"instrument_id,omitempty"`
	Quantity       string `json:"quantity,omitempty"`
	Price          string `json:"price,omitempty"`
	Cash           string `json:"cash,omitempty"`
	CurrencyCode   string `json:"currency_code,omitempty"`
}

func transactionsToJSON(in []recon.Transaction) ([]byte, error) {
	out := make([]storedTransaction, 0, len(in))
	for _, tx := range in {
		for _, r := range []*big.Rat{tx.Quantity, tx.Price, tx.Cash} {
			if r != nil && !boundedRat(r) {
				return nil, ErrUnverifiedPrecision
			}
		}
		if tx.ExternalRef == "" {
			return nil, fmt.Errorf("transaction with no external_ref")
		}
		out = append(out, storedTransaction{
			ExternalRef:    tx.ExternalRef,
			TradeDate:      formatTime(tx.TradeDate),
			SettlementDate: formatTime(tx.SettlementDate),
			InstrumentID:   tx.InstrumentID,
			Quantity:       ratText(tx.Quantity),
			Price:          ratText(tx.Price),
			Cash:           ratText(tx.Cash),
			CurrencyCode:   tx.CurrencyCode,
		})
	}
	return json.Marshal(out)
}

func jsonToTransactions(raw []byte) ([]recon.Transaction, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var stored []storedTransaction
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, nil
	}
	out := make([]recon.Transaction, 0, len(stored))
	for _, st := range stored {
		tx := recon.Transaction{
			ExternalRef:  st.ExternalRef,
			InstrumentID: st.InstrumentID,
			CurrencyCode: st.CurrencyCode,
		}
		var err error
		// A FIGURE THAT WILL NOT PARSE REFUSES THE WHOLE STATEMENT rather than
		// being dropped from the line. A trade line read back without its
		// quantity still matches on its reference, so dropping the figure would
		// leave a break stating an amount of zero about a real execution.
		if tx.Quantity, err = parseRatText(st.Quantity); err != nil {
			return nil, fmt.Errorf("%q quantity: %w", st.ExternalRef, err)
		}
		if tx.Price, err = parseRatText(st.Price); err != nil {
			return nil, fmt.Errorf("%q price: %w", st.ExternalRef, err)
		}
		if tx.Cash, err = parseRatText(st.Cash); err != nil {
			return nil, fmt.Errorf("%q cash: %w", st.ExternalRef, err)
		}
		if tx.TradeDate, err = parseTimeText(st.TradeDate); err != nil {
			return nil, fmt.Errorf("%q trade_date: %w", st.ExternalRef, err)
		}
		if tx.SettlementDate, err = parseTimeText(st.SettlementDate); err != nil {
			return nil, fmt.Errorf("%q settlement_date: %w", st.ExternalRef, err)
		}
		out = append(out, tx)
	}
	return out, nil
}

func ratText(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.RatString()
}

func parseRatText(text string) (*big.Rat, error) {
	if text == "" {
		return nil, nil
	}
	return parseStored(text)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTimeText(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func jsonToRats(raw []byte) (map[string]*big.Rat, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text map[string]string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, err
	}
	if len(text) == 0 {
		return nil, nil
	}
	out := make(map[string]*big.Rat, len(text))
	for k, v := range text {
		r, err := parseStored(v)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", k, err)
		}
		out[k] = r
	}
	return out, nil
}
