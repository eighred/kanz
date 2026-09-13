package custody

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// ErrNoStatement is returned by LatestStatement when the custodian has said
// nothing for a subject.
//
// IT IS A DISTINCT ERROR AND NOT A NIL STATEMENT, because the caller must not be
// able to treat "no statement" as "an empty statement". An empty statement
// reconciles as "the custodian holds nothing", which manufactures a
// MISSING_AT_CUSTODIAN break for every position the book holds — a screen full of
// fabricated breaks that buries the real ones. The correct response to silence is
// OutcomeNoStatement, and this error is what routes to it.
var ErrNoStatement = errors.New("custody: no statement for subject")

// Store is the durable home of statements, runs and the break lifecycle.
//
// THE BREAK STATE IS THE PART THAT MUST BE DURABLE. Statements and runs could in
// principle be replayed off the bus; an operator's assignment and explanation
// exist nowhere else, and losing them resets every break to OPEN — which
// regenerates the exact "list nobody reads" this control was built to prevent.
type Store interface {
	// SaveStatement records a custodian statement, idempotent on StatementID. A
	// restatement for a subject already reconciled REPLACES the latest statement;
	// it does not mutate the runs that reconciled the earlier one, because the
	// history must reconstruct what was known and when.
	SaveStatement(ctx context.Context, s Statement) error

	// LatestStatement returns the newest statement for a subject by ReceivedAt,
	// or ErrNoStatement.
	LatestStatement(ctx context.Context, subject Subject) (Statement, error)

	// SaveRun records a completed run. Idempotent on RunID.
	SaveRun(ctx context.Context, r Run) error

	// LatestRun returns the newest run for a (portfolio, custodian) pair across
	// ALL business dates, or ErrNoRun. It is what the staleness gauge ages, so it
	// deliberately ignores the business date: a run for last Friday performed
	// this morning is recent evidence about the control, whatever date it
	// reconciled.
	LatestRun(ctx context.Context, portfolioID, custodianID string) (Run, error)

	// UpsertBreaks reconciles a run's detected breaks against the stored ones for
	// the same (portfolio, custodian), and returns the stored state afterwards.
	//
	// IT CARRIES THE LIFECYCLE FORWARD, which is the reason it is one store
	// operation rather than a read and a write in the caller. A break already
	// known keeps its status, assignee, explanation and FirstSeenAt, and only its
	// figures and LastSeenAt advance. A stored outstanding break the run NO
	// LONGER detects is resolved, because the book and the custodian now agree —
	// that is the only automatic path to RESOLVED, and it is what stops the queue
	// growing forever.
	UpsertBreaks(ctx context.Context, subject Subject, detected []Break, now time.Time) ([]Break, error)

	// OutstandingBreaks returns every break that is still work, for the gauge and
	// for the operator surface. A nil portfolioID/custodianID means "all".
	OutstandingBreaks(ctx context.Context) ([]Break, error)

	// LoadBreak returns one break by id, or ErrNoBreak.
	LoadBreak(ctx context.Context, breakID string) (Break, error)

	// ApplyAction atomically binds a decision to its actor, revision, retry key
	// and durable evidence. Only reconciliation may create or resolve a break.
	ApplyAction(ctx context.Context, a Action) (ActionEvidence, error)
	ActionsEnabled() bool

	// Subjects returns the (portfolio, custodian) pairs the store has ever seen a
	// statement for. It is the scheduler's fallback work list when the deployment
	// does not enumerate pairs explicitly.
	Subjects(ctx context.Context) ([]Subject, error)
}

// ErrNoRun is returned by LatestRun when nothing has ever reconciled a pair.
//
// A PAIR WITH NO RUN IS THE ALERTING CASE, not an empty one. It is the estate
// that has never reconciled at all, which is strictly worse than one whose last
// run is old, so the caller must render it as an infinite age rather than skip
// it — see Reconciler.observeStaleness.
var ErrNoRun = errors.New("custody: no run for pair")

// ErrNoBreak is returned by LoadBreak for an unknown id.
var ErrNoBreak = errors.New("custody: no such break")

// MemoryStore is the in-process Store — the test seam and the single-replica
// default, in the same relationship to Postgres that ledger.MemoryStore is.
//
// IT IS NOT A PRODUCTION HOME FOR THIS DATA. Break lifecycle state is an
// operator's work and survives nothing here; a restart silently resets every
// assignment and explanation to OPEN. The composition root says so out loud at
// startup rather than letting the estate discover it, exactly as the ledger's own
// durability posture does.
//
// NOTE ALSO THAT IT IGNORES ctx, WHERE POSTGRES HONOURS IT. A context deadline is
// therefore unobservable through this store, so a timeout or cancellation
// behaviour proven only against MemoryStore is not proven at all — the
// Postgres-gated tests are the ones that exercise it.
type MemoryStore struct {
	mu         sync.Mutex
	statements map[string][]Statement // subject key -> statements, append order
	runs       map[string][]Run       // portfolio|custodian -> runs, append order
	runIDs     map[string]struct{}
	breaks     map[string]Break // break id -> break
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		statements: map[string][]Statement{},
		runs:       map[string][]Run{},
		runIDs:     map[string]struct{}{},
		breaks:     map[string]Break{},
	}
}

func pairKey(portfolioID, custodianID string) string { return portfolioID + "|" + custodianID }

// SaveStatement implements Store.
func (m *MemoryStore) SaveStatement(_ context.Context, s Statement) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := s.Subject().Key()
	for i, existing := range m.statements[key] {
		if existing.StatementID == s.StatementID {
			m.statements[key][i] = cloneStatement(s)
			return nil
		}
	}
	m.statements[key] = append(m.statements[key], cloneStatement(s))
	return nil
}

// LatestStatement implements Store.
func (m *MemoryStore) LatestStatement(_ context.Context, subject Subject) (Statement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.statements[subject.Key()]
	if len(list) == 0 {
		return Statement{}, ErrNoStatement
	}
	best := list[0]
	for _, s := range list[1:] {
		if s.ReceivedAt.After(best.ReceivedAt) {
			best = s
		}
	}
	return cloneStatement(best), nil
}

// SaveRun implements Store.
func (m *MemoryStore) SaveRun(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.runIDs[r.RunID]; dup {
		return nil
	}
	m.runIDs[r.RunID] = struct{}{}
	key := pairKey(r.Subject.PortfolioID, r.Subject.CustodianID)
	m.runs[key] = append(m.runs[key], r)
	return nil
}

// LatestRun implements Store.
func (m *MemoryStore) LatestRun(_ context.Context, portfolioID, custodianID string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.runs[pairKey(portfolioID, custodianID)]
	if len(list) == 0 {
		return Run{}, ErrNoRun
	}
	best := list[0]
	for _, r := range list[1:] {
		if r.CompletedAt.After(best.CompletedAt) {
			best = r
		}
	}
	return best, nil
}

// UpsertBreaks implements Store.
func (m *MemoryStore) UpsertBreaks(_ context.Context, subject Subject, detected []Break, now time.Time) ([]Break, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]struct{}, len(detected))
	for _, d := range detected {
		seen[d.BreakID] = struct{}{}
		if prior, ok := m.breaks[d.BreakID]; ok && prior.Status != BreakResolved {
			// KNOWN AND STILL OUTSTANDING: the figures and LastSeenAt advance;
			// the operator's work does not. Overwriting status here is the bug
			// that resets an assignment on every daily run.
			prior.IBOR, prior.Custodian, prior.Diff = d.IBOR, d.Custodian, d.Diff
			prior.ValuesVerified = true
			prior.LastSeenAt = now
			prior.Revision++
			m.breaks[d.BreakID] = prior
			continue
		}
		// NEW, or a break that had been resolved and has come back. A returning
		// break is genuinely new work — it is first seen now, and it does not
		// inherit the explanation that was written for the previous occurrence,
		// which by definition did not hold.
		fresh := d
		fresh.ValuesVerified = true
		fresh.FirstSeenAt, fresh.LastSeenAt, fresh.StatusChangedAt = now, now, now
		fresh.Status = BreakOpen
		fresh.Revision = 1
		if prior, ok := m.breaks[d.BreakID]; ok {
			fresh.Revision = prior.Revision + 1
		}
		fresh.Assignee, fresh.Explanation = "", ""
		m.breaks[d.BreakID] = fresh
	}

	// A stored outstanding break this run no longer detects: the book and the
	// custodian now agree, so it is resolved. This is the only automatic route to
	// RESOLVED and it is what stops the queue growing without bound.
	for id, b := range m.breaks {
		if _, still := seen[id]; still {
			continue
		}
		// SCOPED TO THIS SUBJECT'S PAIR. A run reconciles one (portfolio,
		// custodian); resolving on absence across the whole store would clear
		// every other pair's breaks on every run, because this run never had
		// anything to say about them.
		if !sameSubject(b, subject) || !b.Status.Outstanding() {
			continue
		}
		b.Status = BreakResolved
		b.Revision++
		b.StatusChangedAt = now
		m.breaks[id] = b
	}
	return m.outstandingLocked(), nil
}

// sameSubject reports whether a stored break belongs to subject. The break id
// carries the pair, which is why it can be answered without a second field.
func sameSubject(b Break, subject Subject) bool {
	prefix := subject.PortfolioID + "|" + subject.CustodianID + "|"
	return len(b.BreakID) >= len(prefix) && b.BreakID[:len(prefix)] == prefix
}

// OutstandingBreaks implements Store.
func (m *MemoryStore) OutstandingBreaks(_ context.Context) ([]Break, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.outstandingLocked(), nil
}

func (m *MemoryStore) outstandingLocked() []Break {
	var out []Break
	for _, b := range m.breaks {
		if b.Status.Outstanding() {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BreakID < out[j].BreakID })
	return out
}

// LoadBreak implements Store.
func (m *MemoryStore) LoadBreak(_ context.Context, breakID string) (Break, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.breaks[breakID]
	if !ok {
		return Break{}, ErrNoBreak
	}
	return b, nil
}

// Subjects implements Store.
func (m *MemoryStore) Subjects(_ context.Context) ([]Subject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]Subject{}
	for _, list := range m.statements {
		for _, s := range list {
			seen[pairKey(s.PortfolioID, s.CustodianID)] = Subject{PortfolioID: s.PortfolioID, CustodianID: s.CustodianID}
		}
	}
	out := make([]Subject, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PortfolioID != out[j].PortfolioID {
			return out[i].PortfolioID < out[j].PortfolioID
		}
		return out[i].CustodianID < out[j].CustodianID
	})
	return out, nil
}

// cloneStatement deep-copies the rat maps so a caller mutating its own statement
// cannot reach into the store's copy.
func cloneStatement(s Statement) Statement {
	out := s
	out.BusinessDate = BusinessDay(s.BusinessDate)
	out.Positions = cloneRats(s.Positions)
	out.Cash = cloneRats(s.Cash)
	out.Transactions = cloneTransactions(s.Transactions)
	return out
}

// cloneTransactions deep-copies the trade lines, INCLUDING their rats, for
// cloneRats's reason: a caller mutating a figure on its own statement must not
// reach into the store's copy and change what a later run reconciles against.
func cloneTransactions(in []recon.Transaction) []recon.Transaction {
	if in == nil {
		return nil
	}
	out := make([]recon.Transaction, len(in))
	for i, tx := range in {
		tx.Quantity = cloneRat(tx.Quantity)
		tx.Price = cloneRat(tx.Price)
		tx.Cash = cloneRat(tx.Cash)
		out[i] = tx
	}
	return out
}

func cloneRat(r *big.Rat) *big.Rat {
	if r == nil {
		return nil
	}
	return new(big.Rat).Set(r)
}

func cloneRats(in map[string]*big.Rat) map[string]*big.Rat {
	if in == nil {
		return nil
	}
	out := make(map[string]*big.Rat, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = nil
			continue
		}
		out[k] = new(big.Rat).Set(v)
	}
	return out
}
