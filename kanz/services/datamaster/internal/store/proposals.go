package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore"
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

// Record, WithRecord, Decided, Copy and Validate make an override proposal
// storable by internal/dualcontrol/proposalstore, which is where the mechanics
// this act shares with every other one now live (#562).
func (prop OverrideProposal) Record() dualcontrol.Proposal { return prop.Proposal }

// WithRecord returns a copy carrying r. It is how the shared contract builds its
// malformed cases without knowing what a price is.
func (prop OverrideProposal) WithRecord(r dualcontrol.Proposal) OverrideProposal {
	prop.Proposal = r
	return prop
}

// Decided is ALWAYS FALSE HERE, and that is this act's answer rather than a
// stub. Claim removes the row: an override that was applied is not in this store
// to be asked about, and its durable record is the append-only
// exception_overrides row carrying both names. A stored proposal is by
// construction one nobody has signed.
func (prop OverrideProposal) Decided() bool { return false }

// Copy is a DEEP copy. The caller keeps a pointer to the price, and a stored
// *big.Rat it can still mutate is not a record of what was proposed — the digest
// would then cover a value the store no longer holds.
func (prop OverrideProposal) Copy() OverrideProposal {
	if prop.ChosenPrice != nil {
		prop.ChosenPrice = new(big.Rat).Set(prop.ChosenPrice)
	}
	return prop
}

// Validate refuses a proposal this store cannot honestly hold.
//
// IT RUNS IN BOTH BACKENDS, which is the repair #562 was filed on. Before the
// shared store, MemoryProposals checked only that the id was non-empty: it
// accepted a proposal with no proposer (every approver differs from "", so the
// self-approval check passes vacuously), silently OVERWROTE an id already held,
// and panicked on a nil price where Postgres returns an error. The in-process
// store had no test of its own, so none of the three was visible.
func (prop OverrideProposal) Validate() error {
	if err := proposalstore.Wellformed(prop.Proposal); err != nil {
		return err
	}
	if prop.ChosenPrice == nil {
		return fmt.Errorf("store: proposal %s carries no chosen price, so approving it would "+
			"override the price to nothing", prop.ID)
	}
	return nil
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
	// Put holds a proposal, returning proposalstore.ErrExists if the id is
	// already held. BOTH backends refuse the duplicate: the in-process one used
	// to overwrite it, which let a redelivery replace the proposer an approver is
	// being asked to differ from while Postgres's primary key refused the same
	// write.
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
//
// THE MECHANICS ARE THE SHARED ONES (#562): one map, one lock, one copy-on-read
// discipline and one total order, in internal/dualcontrol/proposalstore, so this
// act's in-process backend cannot drift from the OMS's or from act two's. What
// stays here is this act's DISPOSITION — Claim removes the row — because the
// durable record of an applied override is the append-only exception_overrides
// row and a second copy here would be a second answer that can disagree with it.
type MemoryProposals struct {
	core *proposalstore.Memory[OverrideProposal]
}

func NewMemoryProposals() *MemoryProposals {
	return &MemoryProposals{core: proposalstore.NewMemory[OverrideProposal]()}
}

// Put holds a proposal. There is nothing to commit alongside it: this act
// publishes no FACT for a held override (#563 records why), so the announcement
// argument the OMS's equivalent carries has no counterpart here.
func (m *MemoryProposals) Put(ctx context.Context, p OverrideProposal) error {
	return m.core.Insert(ctx, p, nil)
}

func (m *MemoryProposals) Get(ctx context.Context, id string) (OverrideProposal, bool, error) {
	return m.core.Get(ctx, id)
}

// Claim REMOVES the proposal. See ProposalStore.Claim: the removal is the
// serialisation point, and this act keeps no approver on the row.
func (m *MemoryProposals) Claim(ctx context.Context, id string) (bool, error) {
	return m.core.Remove(ctx, id)
}

func (m *MemoryProposals) Pending(ctx context.Context, now time.Time) ([]OverrideProposal, error) {
	return m.core.Select(ctx, func(p OverrideProposal) bool { return p.Pending(now) })
}

func (m *MemoryProposals) Lapsed(ctx context.Context, now time.Time) ([]OverrideProposal, error) {
	// NOT !p.Pending(now): Pending is false for a CLAIMED proposal too, and a
	// claimed one is not in this store to begin with. Expiry is the only way a
	// row survives without being pending, and proposalstore.Expired is the same
	// comparison the OMS's expiry queue uses, so the two acts cannot drift on
	// what "past its deadline" means.
	return m.core.Select(ctx, func(p OverrideProposal) bool {
		return proposalstore.Expired(p.Proposal, now)
	})
}

func (m *MemoryProposals) PurgeLapsed(ctx context.Context, cutoff time.Time) (int64, error) {
	return m.core.Purge(ctx, func(p OverrideProposal) bool { return p.ExpiresAt.Before(cutoff) })
}

// PostgresProposals is the durable, tenant-scoped ProposalStore. It shares the
// tenant-bound pool every other store here uses, so RLS scopes it.
type PostgresProposals struct{ pool *pgxpool.Pool }

func NewPostgresProposals(pool *pgxpool.Pool) *PostgresProposals {
	return &PostgresProposals{pool: pool}
}

func (p *PostgresProposals) Put(ctx context.Context, prop OverrideProposal) error {
	// THE SAME VALIDATION THE IN-PROCESS BACKEND RUNS, spelled once in
	// OverrideProposal.Validate. It used to be a weaker ad-hoc subset here and a
	// bare id check there, which is exactly how the two backends came to accept
	// different things.
	if err := prop.Validate(); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO exception_override_proposals
			(tenant_id, proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at)
		VALUES (app_current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, prop.ID, prop.Subject, string(prop.Act), prop.Proposer, prop.Digest,
		prop.Reason, prop.ChosenPrice.RatString(), prop.CreatedAt, prop.ExpiresAt)
	if err != nil {
		// A PRIMARY-KEY VIOLATION IS NOT A WRITE FAILURE, it is "somebody already
		// holds this id", and the caller has to be able to tell them apart: one is
		// a quiet no-op on a redelivery, the other is a 500. Named as
		// proposalstore.ErrExists so the two backends answer a duplicate the same
		// way; every other SQLSTATE — the foreign key that refuses a proposal for
		// an exception that does not exist, the CHECKs — stays an error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: proposal %s", proposalstore.ErrExists, prop.ID)
		}
		return fmt.Errorf("store: put proposal %s: %w", prop.ID, err)
	}
	return nil
}

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

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
