// Package store is the data master's durable home for the resolved golden
// records and the price-exception oversight queue (PARITY-02b). It defines the
// seams the server composes over — an in-memory default (the test seam,
// preserving the exact MASTER-01 contract) and a Postgres backend (postgres.go)
// that survives a restart with no change to resolution or arbitration.
package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// GoldenStore persists resolved golden records — replace-on-write on the
// canonical instrument id (a resolution is the whole truth for an instrument).
type GoldenStore interface {
	Put(ctx context.Context, rec master.SecurityMaster) error
	Get(ctx context.Context, instrumentID string) (master.SecurityMaster, bool, error)
}

// Claim names the dual-control proposal an override answers, and is consumed by
// the same write that applies the override (#807).
//
// IT IS DATA RATHER THAN A SEPARATE CALL, and that is the whole of it. The claim
// and the override used to be two transactions: the approve handler called
// ProposalStore.Claim, which committed, and then ExceptionStore.Override, which
// committed again. A process death in that window SPENT THE SECOND SIGNATURE and
// applied nothing — the proposal was gone, the exception was still OPEN, and
// nothing anywhere recorded that an approval had been given. Recovering meant a
// human noticing the contradiction and a second approver signing again, while a
// price the system itself flagged went on valuing the book.
//
// The same decision order.Store.Save makes with its announce records (#292): the
// thing that must not be separable from a state change rides the state change.
//
// The zero value is the single-signed path — no proposal, nothing to consume.
type Claim struct {
	// ProposalID is the proposal this override consumes. Empty means the
	// override answers no proposal.
	ProposalID string
}

// Held reports whether this override answers a dual-control proposal.
func (c Claim) Held() bool { return c.ProposalID != "" }

// ExceptionStore persists the oversight exception queue with the exact
// in-memory contract: Add is idempotent on the deterministic exception id (a
// re-detected break does not wipe its review state), and Override appends an
// immutable audit record, moving the entry to OVERRIDDEN.
type ExceptionStore interface {
	Add(ctx context.Context, e pricing.Exception) error
	AddAll(ctx context.Context, exs []pricing.Exception) error
	// Override appends the audit record, flips the status, announces the FACT
	// and CONSUMES claim — all or none of them.
	//
	// claim IS A PARAMETER RATHER THAN A PRIOR CALL so that omitting it does not
	// compile, the same reason order.ProposalStore.Put takes a slice rather than
	// a variadic. A caller that claimed the proposal itself and then called this
	// is the two-transaction shape #807 removed, and no signature stops that if
	// the argument is optional.
	//
	// It returns ErrProposalAlreadyDecided, HAVING WRITTEN NOTHING, when the
	// claim finds no proposal to take: somebody else decided it first, and
	// applying a second override for one decision is the outcome Claim has
	// always existed to prevent.
	//
	// It returns pricing.ErrAlreadyOverridden, ALSO HAVING WRITTEN NOTHING, when
	// the exception has already been decided (#816). That is the same outcome
	// arriving by the other door: a proposal is single-use, but nothing stopped a
	// second single-signed override — or a second proposal — from appending a
	// second authorisation for one act. AN OVERRIDE IS NOT REPEATABLE, and both
	// backends refuse rather than silently no-op — see ErrAlreadyOverridden for
	// why the two are not interchangeable here.
	Override(ctx context.Context, id string, o pricing.Override, claim Claim) error
	Get(ctx context.Context, id string) (pricing.Exception, bool, error)
	Open(ctx context.Context) ([]pricing.Exception, error)
}

// MemoryGoldenStore is the in-process GoldenStore. Goroutine-safe.
type MemoryGoldenStore struct {
	mu      sync.RWMutex
	records map[string]master.SecurityMaster
}

// NewMemoryGoldenStore returns an empty in-memory GoldenStore.
func NewMemoryGoldenStore() *MemoryGoldenStore {
	return &MemoryGoldenStore{records: make(map[string]master.SecurityMaster)}
}

func (m *MemoryGoldenStore) Put(_ context.Context, rec master.SecurityMaster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[rec.InstrumentID] = rec
	return nil
}

func (m *MemoryGoldenStore) Get(_ context.Context, instrumentID string) (master.SecurityMaster, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.records[instrumentID]
	return rec, ok, nil
}

// QueueStore adapts the in-memory pricing.Queue to the ExceptionStore seam, so
// the existing idempotent-add / append-only-override logic is reused verbatim
// as the test default.
type QueueStore struct {
	q *pricing.Queue
	// proposals is where a claimed override takes its proposal from. Nil is the
	// legitimate "this queue answers no proposals" case — every caller that never
	// arms dual control — and a claim arriving on a nil one is REFUSED rather
	// than applied unclaimed, because "there was nothing to consume" and "two
	// people signed" must not be the same outcome.
	proposals ProposalStore
}

// NewQueueStore wraps a pricing.Queue (or a fresh one if nil) together with the
// proposal store whose approvals its overrides consume.
//
// proposals IS EXPLICIT, INCLUDING WHEN IT IS NIL. The ephemeral posture builds
// both halves in one process and has to hand this store the same MemoryProposals
// the server approves against, or a dual-signed override would consume nothing
// and the proposal would stay approvable after it had already been applied.
func NewQueueStore(q *pricing.Queue, proposals ProposalStore) *QueueStore {
	if q == nil {
		q = pricing.NewQueue()
	}
	return &QueueStore{q: q, proposals: proposals}
}

func (s *QueueStore) Add(_ context.Context, e pricing.Exception) error { s.q.Add(e); return nil }

func (s *QueueStore) AddAll(_ context.Context, exs []pricing.Exception) error {
	s.q.AddAll(exs)
	return nil
}

// Override applies o and consumes claim.
//
// THIS PAIR IS NOT ONE TRANSACTION, AND DOES NOT NEED TO BE. The window #807
// closes is a process death between the claim and the override; here both halves
// are maps in one process's memory, so the death that could separate them
// destroys the exception queue as well — the ephemeral posture already says out
// loud that every override it holds is discarded on the next restart and that it
// MUST run exactly one replica.
//
// WHAT IS STILL ORDERED DELIBERATELY: everything that can refuse runs BEFORE the
// proposal is consumed. pricing.Queue.Override fails in exactly three ways — an
// unknown exception, an override that is not storable audit evidence, and an
// exception a human has already decided (#816) — and all three are decidable up
// front, so no refusal reachable from here can spend a signature and apply
// nothing.
//
// THE THIRD ONE IS THE NEWEST AND THE EASIEST TO PUT IN THE WRONG PLACE. Left
// where pricing.Queue.Override raises it, it would fire after the claim below
// and spend a second signature on an override that does not happen — which is
// #807's cost, arriving through #816's repair.
func (s *QueueStore) Override(ctx context.Context, id string, o pricing.Override, claim Claim) error {
	if !claim.Held() {
		return s.q.Override(id, o)
	}
	if s.proposals == nil {
		return fmt.Errorf("store: override for exception %s carries proposal %s but this queue was "+
			"built with no proposal store, so approving it would consume nothing", id, claim.ProposalID)
	}
	e, ok := s.q.Get(id)
	if !ok {
		return fmt.Errorf("pricing: unknown exception %q", id)
	}
	if err := o.Validate(); err != nil {
		return err
	}
	// THE PROPOSAL IS PEEKED BEFORE THE EXCEPTION IS JUDGED, AND THE ORDER IS THE
	// POINT (#816).
	//
	// Both refusals can be true at once — a spent approval re-presented for the
	// exception that same approval decided — and the two backends must pick the
	// same one, or a caller branching on the sentinel gets a different answer on
	// the ephemeral posture than on the durable one. PostgresExceptions.Override
	// claims FIRST and checks the status after, so there "the proposal is gone"
	// wins; this reproduces that precedence without consuming anything.
	//
	// Peeking rather than claiming is what keeps the promise above: everything
	// that can refuse runs before the signature is spent. It is not a
	// serialisation point and does not need to be — Claim below is still what
	// decides, and a proposal taken between these two lines comes back from there
	// as ErrProposalAlreadyDecided.
	_, held, err := s.proposals.Get(ctx, claim.ProposalID)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("%w: proposal %s", ErrProposalAlreadyDecided, claim.ProposalID)
	}
	// THE THIRD REFUSAL, PRE-CHECKED FOR THE SAME REASON AS THE TWO ABOVE.
	//
	// pricing.Queue.Override refuses an already-decided exception itself, but it
	// runs AFTER the claim below — and a refusal that runs after the claim spends
	// the second signature on an act that does not happen. This is not a second
	// rule; it is the same predicate reached early enough to keep that promise.
	if e.Status.AlreadyOverridden() {
		return fmt.Errorf("%w: %q", pricing.ErrAlreadyOverridden, id)
	}
	claimed, err := s.proposals.Claim(ctx, claim.ProposalID)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: proposal %s", ErrProposalAlreadyDecided, claim.ProposalID)
	}
	return s.q.Override(id, o)
}

func (s *QueueStore) Get(_ context.Context, id string) (pricing.Exception, bool, error) {
	e, ok := s.q.Get(id)
	return e, ok, nil
}

func (s *QueueStore) Open(_ context.Context) ([]pricing.Exception, error) { return s.q.Open(), nil }

var (
	_ GoldenStore    = (*MemoryGoldenStore)(nil)
	_ ExceptionStore = (*QueueStore)(nil)
)
