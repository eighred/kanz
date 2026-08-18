package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// OverrideProposal is a pricing override waiting for its second signature
// (#410): the dual-control record plus the payload the approval covers.
//
// THE PAYLOAD TRAVELS WITH THE PROPOSAL rather than being resent at approval
// time. The digest would catch a client that sent something else, but only after
// making "the approver's client supplies the value" the normal path — and a
// control whose safety depends on a check that fires on the happy path is one
// refactor away from being ceremonial.
type OverrideProposal struct {
	dualcontrol.Proposal
	Reason      string
	ChosenPrice *big.Rat
}

// PayloadDigest is the one definition of what an override approval covers.
//
// IT IS A FUNCTION, NOT A CONVENTION. The proposer and the approver hash the
// same fields in the same order in the same process, but the two calls are in
// different code paths, and a digest computed two ways is a digest that
// eventually differs — at which point every approval fails and the obvious fix
// is to stop checking it.
func PayloadDigest(exceptionID, reason string, chosenPrice *big.Rat) string {
	price := ""
	if chosenPrice != nil {
		price = chosenPrice.RatString()
	}
	return dualcontrol.Digest(exceptionID, reason, price)
}

// ErrNoProposal is returned when a proposal id is unknown to the store — either
// it never existed, it was already applied, or another tenant owns it. The three
// are deliberately indistinguishable, the same stance as notFoundBody.
var ErrNoProposal = errors.New("store: no such pending override proposal")

// ProposalStore persists pending dual-control proposals.
//
// There is no "applied" flag. The durable record of what happened is the
// append-only exception_overrides row carrying both names; a second copy of that
// fact here would be a second answer that can disagree with it. A proposal is
// therefore CLAIMED — removed — when it is applied or rejected.
type ProposalStore interface {
	Put(ctx context.Context, p OverrideProposal) error
	// Get reads a proposal without taking it, for the checks that must be able
	// to refuse without consuming the proposal (a self-approval must leave it
	// pending for someone who may actually approve it).
	Get(ctx context.Context, proposalID string) (OverrideProposal, bool, error)
	// Claim atomically removes a proposal and reports whether THIS caller is the
	// one that got it.
	//
	// IT IS THE SERIALISATION POINT, and the reason it exists rather than a
	// Delete. Two approvers acting on the same pending override at the same
	// moment both pass every check — they are different people, the digest
	// matches for both, neither has expired — and would both apply, appending
	// two override records for one decision and marking the exception OVERRIDDEN
	// twice. Whoever claims it applies it; the other is told it is already
	// decided.
	Claim(ctx context.Context, proposalID string) (bool, error)
	// Pending lists proposals not yet expired at now, oldest first, so an
	// unapproved act is VISIBLE rather than silently dropped.
	Pending(ctx context.Context, now time.Time) ([]OverrideProposal, error)
	// Lapsed lists proposals that expired without a signature and have not yet
	// been purged, so "nobody signed it" is a state a proposer can SEE rather
	// than infer from an absence (#563).
	//
	// It takes no window. A lapsed proposal is visible for exactly as long as its
	// row exists, and PurgeLapsed is the only thing that ends that — so a
	// proposal missing from both lists was purged or never existed, and there is
	// no third state where the row is present and hidden.
	Lapsed(ctx context.Context, now time.Time) ([]OverrideProposal, error)
	// PurgeLapsed removes proposals that expired before cutoff and reports how
	// many went. It is what bounds the table: a proposal nobody signs is never
	// claimed, so without this every override ever proposed and left unsigned
	// stays forever.
	PurgeLapsed(ctx context.Context, cutoff time.Time) (int64, error)
}

// MemoryProposals is the in-process ProposalStore.
//
// FOR TESTS AND THE EPHEMERAL-MASTER POSTURE ONLY. With more than one replica a
// proposal put here is approvable only on the pod that took it, so the second
// signature succeeds or fails depending on which pod the approver's request
// reached. The composition root picks this only where it already accepts an
// ephemeral master, and says so.
type MemoryProposals struct {
	mu sync.RWMutex
	by map[string]OverrideProposal
}

func NewMemoryProposals() *MemoryProposals {
	return &MemoryProposals{by: map[string]OverrideProposal{}}
}

func (m *MemoryProposals) Put(_ context.Context, p OverrideProposal) error {
	if p.ID == "" {
		return fmt.Errorf("store: proposal has no id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copied: the caller keeps a pointer to the price and a stored *big.Rat it
	// can still mutate is not a record of what was proposed.
	p.ChosenPrice = new(big.Rat).Set(p.ChosenPrice)
	m.by[p.ID] = p
	return nil
}

func (m *MemoryProposals) Get(_ context.Context, id string) (OverrideProposal, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.by[id]
	if !ok {
		return OverrideProposal{}, false, nil
	}
	p.ChosenPrice = new(big.Rat).Set(p.ChosenPrice)
	return p, true, nil
}

func (m *MemoryProposals) Claim(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.by[id]; !ok {
		return false, nil
	}
	delete(m.by, id)
	return true, nil
}

func (m *MemoryProposals) Pending(_ context.Context, now time.Time) ([]OverrideProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OverrideProposal, 0, len(m.by))
	for _, p := range m.by {
		if !p.Pending(now) {
			continue
		}
		p.ChosenPrice = new(big.Rat).Set(p.ChosenPrice)
		out = append(out, p)
	}
	sortByCreatedAt(out)
	return out, nil
}

func (m *MemoryProposals) Lapsed(_ context.Context, now time.Time) ([]OverrideProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OverrideProposal, 0, len(m.by))
	for _, p := range m.by {
		// NOT !p.Pending(now): Pending is false for a CLAIMED proposal too, and a
		// claimed one is not in this map to begin with. Expiry is the only way a
		// row survives without being pending, and spelling it as the expiry
		// comparison keeps that true if Pending ever grows another clause.
		if !p.ExpiresAt.After(now) {
			p.ChosenPrice = new(big.Rat).Set(p.ChosenPrice)
			out = append(out, p)
		}
	}
	sortByCreatedAt(out)
	return out, nil
}

func (m *MemoryProposals) PurgeLapsed(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, p := range m.by {
		if p.ExpiresAt.Before(cutoff) {
			delete(m.by, id)
			n++
		}
	}
	return n, nil
}

// PostgresProposals is the durable, tenant-scoped ProposalStore. It shares the
// tenant-bound pool every other store here uses, so RLS scopes it.
type PostgresProposals struct{ pool *pgxpool.Pool }

func NewPostgresProposals(pool *pgxpool.Pool) *PostgresProposals {
	return &PostgresProposals{pool: pool}
}

func (p *PostgresProposals) Put(ctx context.Context, prop OverrideProposal) error {
	if prop.ID == "" || prop.Proposer == "" || prop.ChosenPrice == nil {
		return fmt.Errorf("store: proposal requires an id, a proposer and a chosen price")
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO exception_override_proposals
			(tenant_id, proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at)
		VALUES (app_current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, prop.ID, prop.Subject, string(prop.Act), prop.Proposer, prop.Digest,
		prop.Reason, prop.ChosenPrice.RatString(), prop.CreatedAt, prop.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: put proposal %s: %w", prop.ID, err)
	}
	return nil
}

func (p *PostgresProposals) Get(ctx context.Context, id string) (OverrideProposal, bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at
		FROM exception_override_proposals WHERE proposal_id = $1
	`, id)
	prop, err := scanProposal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return OverrideProposal{}, false, nil
	}
	if err != nil {
		return OverrideProposal{}, false, fmt.Errorf("store: get proposal %s: %w", id, err)
	}
	return prop, true, nil
}

func (p *PostgresProposals) Claim(ctx context.Context, id string) (bool, error) {
	// ONE STATEMENT. A SELECT followed by a DELETE would leave the window this
	// method exists to close: both approvers see the row, both delete it, both
	// apply. The rows-affected count of a single DELETE is the atomic answer to
	// "did I get it", and Postgres serialises the two writers for free.
	tag, err := p.pool.Exec(ctx, `DELETE FROM exception_override_proposals WHERE proposal_id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("store: claim proposal %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (p *PostgresProposals) Pending(ctx context.Context, now time.Time) ([]OverrideProposal, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at
		FROM exception_override_proposals
		WHERE expires_at > $1
		ORDER BY created_at, proposal_id
	`, now)
	if err != nil {
		return nil, fmt.Errorf("store: list pending proposals: %w", err)
	}
	defer rows.Close()
	var out []OverrideProposal
	for rows.Next() {
		prop, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan pending proposal: %w", err)
		}
		out = append(out, prop)
	}
	return out, rows.Err()
}

// Lapsed lists proposals that expired unsigned and survive only because nothing
// claimed them.
//
// THE SAME COLUMN LIST AND THE SAME INDEX AS Pending, with the comparison
// inverted: exception_override_proposals_pending_idx is (tenant_id, expires_at),
// so both halves of the split are one index scan and neither read degrades as
// lapsed proposals accumulate between purges.
func (p *PostgresProposals) Lapsed(ctx context.Context, now time.Time) ([]OverrideProposal, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at
		FROM exception_override_proposals
		WHERE expires_at <= $1
		ORDER BY created_at, proposal_id
	`, now)
	if err != nil {
		return nil, fmt.Errorf("store: list lapsed proposals: %w", err)
	}
	defer rows.Close()
	var out []OverrideProposal
	for rows.Next() {
		prop, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan lapsed proposal: %w", err)
		}
		out = append(out, prop)
	}
	return out, rows.Err()
}

// PurgeLapsed deletes proposals that expired before cutoff.
//
// IDEMPOTENT AND SAFE TO RUN CONCURRENTLY, which is why it is a DELETE and not a
// mark-then-sweep. The OMS needed announce-exactly-once because its expiry
// publishes a FACT and a duplicate FACT is a second answer; nothing is published
// here (#563 records why: datamaster emits one subject and nothing consumes it),
// so two purgers racing on the same rows produce one outcome and a smaller
// count on the loser.
//
// IT CANNOT TAKE AN APPROVED PROPOSAL. Claim deletes on approval, so a row that
// still exists was never approved — the property the OMS needs a predicate for
// is structural here.
func (p *PostgresProposals) PurgeLapsed(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM exception_override_proposals
		WHERE expires_at < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: purge lapsed proposals: %w", err)
	}
	return tag.RowsAffected(), nil
}

// scanner is what pgx.Row and pgx.Rows have in common.
type scanner interface{ Scan(dest ...any) error }

func scanProposal(s scanner) (OverrideProposal, error) {
	var (
		prop  OverrideProposal
		act   string
		price string
	)
	if err := s.Scan(&prop.ID, &prop.Subject, &act, &prop.Proposer, &prop.Digest,
		&prop.Reason, &price, &prop.CreatedAt, &prop.ExpiresAt); err != nil {
		return OverrideProposal{}, err
	}
	prop.Act = dualcontrol.Act(act)
	rat, ok := new(big.Rat).SetString(price)
	if !ok {
		// A stored price that will not parse is a corrupt record. Refuse the read
		// rather than approving an override for a price nobody proposed — the
		// same stance loadOverrides takes on the audit trail.
		return OverrideProposal{}, fmt.Errorf("proposal %s: chosen price %q is not an exact decimal", prop.ID, price)
	}
	prop.ChosenPrice = rat
	return prop, nil
}

func sortByCreatedAt(ps []OverrideProposal) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && proposalLess(ps[j], ps[j-1]); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func proposalLess(a, b OverrideProposal) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	// Ties broken by id so the order is total. Two proposals created in the same
	// nanosecond would otherwise list in whichever order the map yielded, and a
	// pending list that reorders between reads reads as activity.
	return a.ID < b.ID
}

// ApplyOverride is the one place a proposal becomes an audit record.
//
// It exists so the two identities cannot be separated on their way into the
// trail: an approver check that happens somewhere else, and a write that takes
// actor and approver as two adjacent strings, is exactly the arrangement in
// which they end up transposed or one of them dropped.
func (prop OverrideProposal) ApplyOverride(approver string, at time.Time) pricing.Override {
	return pricing.Override{
		Actor:       prop.Proposer,
		Approver:    approver,
		Reason:      prop.Reason,
		ChosenPrice: prop.ChosenPrice,
		At:          at,
	}
}

var (
	_ ProposalStore = (*MemoryProposals)(nil)
	_ ProposalStore = (*PostgresProposals)(nil)
)
