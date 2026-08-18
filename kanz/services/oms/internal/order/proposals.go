package order

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
)

// OrderProposal is an order held for a second signature (#410): the
// dual-control record plus the exact command the approval covers.
//
// THE COMMAND TRAVELS WITH THE PROPOSAL rather than being resent at approval
// time. The digest would catch an approver's client that sent something else,
// but only after making "the approver supplies the payload" the normal path —
// and a control whose safety depends on a check that fires on the happy path is
// one refactor away from being ceremonial. Same rule
// services/datamaster/internal/store/proposals.go states for the override path.
//
// Proposal.ID and Proposal.Subject are BOTH the order id, and that is not a
// redundancy to tidy away: see migrations/0009_order_proposals.sql. An order is
// one decision, so a second proposal for one order_id is always a redelivery
// rather than a second decision, and making the order id the key lets the engine
// refuse it exactly as the orders primary key refuses a duplicate admission.
type OrderProposal struct {
	dualcontrol.Proposal
	// Command is the order that will be submitted if this is approved.
	Command *orderpb.SubmitOrder
	// PortfolioID is denormalized off the command so "whose pending orders are
	// these" is answerable without decoding a protobuf.
	PortfolioID string
	// Approver is the SECOND signature, empty while pending. A decided proposal
	// KEEPS ITS ROW — this is the OMS's only durable record that two people
	// signed, because an admitted order carries the order and not who approved
	// it.
	Approver string
	// DecidedAt is when the second signature was given; zero while pending.
	DecidedAt time.Time
	// ExpiryAnnouncedAt is when this proposal's expiry was announced; zero means
	// it has not been. NOT the same question as "has it expired" — expiry is a
	// property of the clock passing expires_at, and this records that the estate
	// was TOLD. Separating them is what makes the announcement exactly-once
	// across two replicas instead of once per sweeper tick, forever.
	ExpiryAnnouncedAt time.Time
}

// ErrProposalExists is returned by ProposalStore.Put when this order already has
// a proposal. It is the signal that THIS delivery lost the race — another
// delivery of the same SubmitOrder already held the order — and it means exactly
// what ErrExists means on the admission path: ack, announce nothing, stop.
var ErrProposalExists = errors.New("oms: order already has a pending proposal")

// ProposalStore is the durable home of orders awaiting a second signature.
//
// It is deliberately NOT part of the orders table. An unapproved order must not
// be an order: the routing gate, the startup sweep, the position projector,
// tv-sync and kanz-web all read `orders` and fold status, and a ninth
// OrderStatus would have made a held order visible to every one of them as
// something to work. See migrations/0009 for the full argument.
type ProposalStore interface {
	// Put holds an order AND the FACT announcing that it is held, in ONE
	// transaction. It returns ErrProposalExists — and writes nothing at all,
	// including no outbox record — if the order already has a proposal.
	//
	// THE announce PARAMETER IS THE SAME DECISION Store.Create's IS (#292). A
	// held order whose announcement was a second, independent write could be
	// held durably and announced to nobody: the trader would see neither an
	// acceptance nor a rejection nor a pending notice, which is the silent drop
	// this whole control exists to end. nil announces nothing; it is a slice
	// rather than a variadic so that omitting it does not compile.
	Put(ctx context.Context, p OrderProposal, announce []outbox.Record) error

	// Get reads a proposal without deciding it, for the checks that must be able
	// to REFUSE without consuming it — a self-approval must leave the order
	// pending for somebody who may actually approve it.
	Get(ctx context.Context, orderID string) (OrderProposal, bool, error)

	// Claim records approver as the second signature and reports whether THIS
	// caller is the one that got it.
	//
	// IT IS THE SERIALISATION POINT, and that is why it exists rather than a
	// Save. Two approvers acting on one pending order at the same moment both
	// pass every check — they are different people, the digest matches for both,
	// neither has expired — and would both admit it, which on this path means
	// two deliveries reaching a live venue for one decision. Whoever claims it
	// applies it; the other is told it is already decided. A mutex cannot do
	// this: the shipped OMS runs replicas: 2, and a lock in one process says
	// nothing about the other pod.
	//
	// It refuses a self-approval rather than recording one, and the database
	// refuses it again (0009's CHECK) — this is not a duplicate of
	// dualcontrol.Approve's rule, it is the last line before the row that an
	// auditor reads.
	Claim(ctx context.Context, orderID, approver string, at time.Time) (bool, error)

	// ExpiredUnannounced returns proposals past their deadline that nobody decided
	// and whose expiry has NOT been announced, oldest first, capped by limit.
	//
	// IT IS THE OTHER QUEUE, AND ITS ABSENCE IS WHY EXPIRY WAS SILENT (#539).
	// Pending deliberately filters an expired proposal out — it is not work anybody
	// can still do — and until this existed nothing looked at those rows again. The
	// row stayed approver = '' forever, invisible rather than terminal, and the
	// trader who submitted it saw ORDER_PENDING_APPROVAL and then nothing.
	ExpiredUnannounced(ctx context.Context, now time.Time, limit int) ([]OrderProposal, error)

	// AnnounceExpiry marks one expired proposal announced and enqueues announce in
	// the SAME transaction, reporting whether THIS call won.
	//
	// IT IS THE SERIALISATION POINT FOR EXPIRY, exactly as Claim is for approval,
	// and for the same reason: the shipped OMS runs replicas: 2, so both sweepers
	// see the same expired row on the same tick. The verdict is the engine's —
	// a conditional UPDATE whose rows-affected decides — because a mutex in one
	// process says nothing about the other pod. A second winner would publish a
	// second ORDER_REJECTED for one order.
	//
	// THE DEADLINE IS RE-CHECKED HERE, not trusted from the caller. A sweeper whose
	// clock drifted must not be able to kill an order somebody still has time to
	// sign, so the predicate belongs where the row is.
	AnnounceExpiry(ctx context.Context, orderID string, at time.Time, announce []outbox.Record) (bool, error)

	// Pending lists proposals still awaiting a signature at now, oldest first,
	// so a held order is VISIBLE rather than silently dropped.
	Pending(ctx context.Context, now time.Time) ([]OrderProposal, error)
}

// validate refuses a proposal the store cannot honestly hold. It runs in BOTH
// implementations so the memory seam cannot accept what Postgres refuses —
// fakeBus already taught this repository what a permissive double costs.
func (p OrderProposal) validate() error {
	switch {
	case p.ID == "" || p.Subject == "":
		return errors.New("oms: proposal has no order id")
	case p.ID != p.Subject:
		// The two are one value by construction (see the type doc). A caller
		// that set them apart is holding a proposal whose key and whose subject
		// name different orders, and the approval would cover the wrong one.
		return fmt.Errorf("oms: proposal id %q and subject %q must both be the order id", p.ID, p.Subject)
	case p.Command == nil:
		return errors.New("oms: proposal carries no command, so approving it could admit nothing")
	case p.Command.GetOrderId() != p.ID:
		return fmt.Errorf("oms: proposal %q holds a command for order %q", p.ID, p.Command.GetOrderId())
	case p.Act == "":
		return errors.New("oms: proposal names no act, so an approval for any act would cover it")
	case p.Proposer == "":
		return errors.New("oms: proposal has no proposer, so no approver could ever differ from it")
	case p.Digest == "":
		return errors.New("oms: proposal has no digest, so an approval would cover nothing")
	case p.CreatedAt.IsZero() || p.ExpiresAt.IsZero():
		return errors.New("oms: proposal has no creation time or no expiry — unknown is not forever")
	case !p.ExpiresAt.After(p.CreatedAt):
		return errors.New("oms: proposal is born expired and could never be approved")
	}
	return nil
}

// MemoryProposals is the in-process ProposalStore.
//
// FOR TESTS AND THE SINGLE-REPLICA POSTURE ONLY, and it degrades with the order
// store and the outbox rather than separately (see openStores): with no
// OMS_DATABASE_URL a crash loses the orders, the FACTs and the pending
// proposals together, so the three cannot come back disagreeing. With more than
// one replica a proposal held here is approvable only on the pod that took it.
type MemoryProposals struct {
	mu     sync.RWMutex
	by     map[string]OrderProposal
	outbox *outbox.Memory
}

// NewMemoryProposals returns an empty in-process ProposalStore writing its FACTs
// into q. q is never nil in the composition root: it is the SAME queue the order
// store enqueues into, so a pending-approval FACT and an acceptance cannot end
// up in two different places.
func NewMemoryProposals(q *outbox.Memory) *MemoryProposals {
	return &MemoryProposals{by: map[string]OrderProposal{}, outbox: q}
}

// Put holds the proposal and enqueues its FACT in the same lock hold — this
// store's whole equivalent of the transaction PostgresProposals.Put opens.
func (m *MemoryProposals) Put(_ context.Context, p OrderProposal, announce []outbox.Record) error {
	if err := p.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.by[p.ID]; ok {
		return ErrProposalExists
	}
	// THE FALLIBLE WRITE FIRST. A failure must leave NOTHING behind, and the map
	// write cannot fail — the same ordering MemoryStore.Create uses.
	if len(announce) > 0 {
		if m.outbox == nil {
			return errors.New("oms: proposal store has no outbox, so a held order would be announced to nobody")
		}
		if err := m.outbox.Append(announce...); err != nil {
			return err
		}
	}
	p.Command = proto.Clone(p.Command).(*orderpb.SubmitOrder)
	m.by[p.ID] = p
	return nil
}

func (m *MemoryProposals) Get(_ context.Context, orderID string) (OrderProposal, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.by[orderID]
	if !ok {
		return OrderProposal{}, false, nil
	}
	p.Command = proto.Clone(p.Command).(*orderpb.SubmitOrder)
	return p, true, nil
}

func (m *MemoryProposals) Claim(_ context.Context, orderID, approver string, at time.Time) (bool, error) {
	if approver == "" {
		return false, errors.New("oms: an approval must come from an authenticated subject")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.by[orderID]
	if !ok || p.Approver != "" {
		return false, nil
	}
	if dualcontrol.SameSubject(approver, p.Proposer) {
		// Refused rather than recorded, and the proposal is LEFT PENDING for
		// somebody who may actually approve it. Postgres refuses this too, at
		// the CHECK constraint — that is the line an auditor's row depends on,
		// and this one keeps the two stores agreeing.
		return false, fmt.Errorf("%w: %q proposed this order and cannot approve it",
			dualcontrol.ErrSelfApproval, p.Proposer)
	}
	p.Approver = approver
	p.DecidedAt = at.UTC()
	m.by[orderID] = p
	return true, nil
}

func (m *MemoryProposals) Pending(_ context.Context, now time.Time) ([]OrderProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OrderProposal, 0, len(m.by))
	for _, p := range m.by {
		// THE SAME THREE CLAUSES THE POSTGRES QUERY CARRIES (#548): undecided, not
		// yet announced, not yet expired. The announced check is not implied by the
		// expiry one — a pod running fast can announce a proposal a slower reader
		// still considers live — and a seam that listed it would offer as work an
		// order the estate has already been told will not trade.
		if p.Approver != "" || !p.ExpiryAnnouncedAt.IsZero() || !p.Pending(now) {
			continue
		}
		p.Command = proto.Clone(p.Command).(*orderpb.SubmitOrder)
		out = append(out, p)
	}
	sortProposals(out)
	return out, nil
}

// ExpiredUnannounced implements ProposalStore.
func (m *MemoryProposals) ExpiredUnannounced(_ context.Context, now time.Time, limit int) ([]OrderProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OrderProposal, 0, len(m.by))
	for _, p := range m.by {
		if p.Approver != "" || !p.ExpiryAnnouncedAt.IsZero() || p.Pending(now) {
			continue
		}
		p.Command = proto.Clone(p.Command).(*orderpb.SubmitOrder)
		out = append(out, p)
	}
	sortProposals(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AnnounceExpiry implements ProposalStore.
func (m *MemoryProposals) AnnounceExpiry(_ context.Context, orderID string, at time.Time, announce []outbox.Record) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.by[orderID]
	// The same predicate the Postgres UPDATE carries, spelled once per store. A
	// decided proposal never expires, an announced one never re-announces, and a
	// live one cannot be killed early.
	if !ok || p.Approver != "" || !p.ExpiryAnnouncedAt.IsZero() || p.Pending(at) {
		return false, nil
	}
	// ENQUEUED BEFORE THE MARKER IS SET, so a rejected record leaves the proposal
	// exactly as it was — the memory seam's stand-in for the Postgres rollback.
	if err := m.outbox.Append(announce...); err != nil {
		return false, err
	}
	p.ExpiryAnnouncedAt = at.UTC()
	m.by[orderID] = p
	return true, nil
}

// PostgresProposals is the durable, tenant-scoped ProposalStore. It shares the
// pool the order store and the outbox use, so RLS scopes it and the proposal
// row, the FACT and the order it will become live in one failure domain.
type PostgresProposals struct {
	pool  *pgxpool.Pool
	queue *outbox.Postgres
}

// NewPostgresProposals returns the durable store over an existing pool.
func NewPostgresProposals(pool *pgxpool.Pool, queue *outbox.Postgres) *PostgresProposals {
	return &PostgresProposals{pool: pool, queue: queue}
}

// Put writes the proposal and its FACT in ONE transaction.
//
// ON CONFLICT DO NOTHING, and the RowsAffected is the verdict — the same
// one-statement admission gate Store.Create uses, for the same reason. A SELECT
// followed by an INSERT reopens the check-then-act window, and here that window
// means two pending proposals for one order and two announcements of it.
//
// THE LOSER ENQUEUES NOTHING: the deferred Rollback discards the FACT with the
// refused INSERT, so a redelivered SubmitOrder cannot announce the same held
// order twice.
func (p *PostgresProposals) Put(ctx context.Context, prop OrderProposal, announce []outbox.Record) error {
	if err := prop.validate(); err != nil {
		return err
	}
	blob, err := proto.Marshal(prop.Command)
	if err != nil {
		return fmt.Errorf("oms: marshal proposed order %s: %w", prop.ID, err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("oms: hold order %s: begin: %w", prop.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	tag, err := tx.Exec(ctx, `
		INSERT INTO order_proposals
			(tenant_id, order_id, portfolio_id, act, proposer, digest, command, created_at, expires_at)
		VALUES (app_current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, order_id) DO NOTHING
	`, prop.ID, prop.PortfolioID, string(prop.Act), prop.Proposer, prop.Digest, blob,
		prop.CreatedAt, prop.ExpiresAt)
	if err != nil {
		return fmt.Errorf("oms: hold order %s: %w", prop.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProposalExists
	}
	if err := outbox.Enqueue(ctx, tx, announce...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("oms: hold order %s: commit: %w", prop.ID, err)
	}
	return nil
}

func (p *PostgresProposals) Get(ctx context.Context, orderID string) (OrderProposal, bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT order_id, portfolio_id, act, proposer, digest, command, approver, decided_at, created_at, expires_at
		FROM order_proposals WHERE order_id = $1
	`, orderID)
	prop, err := scanOrderProposal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderProposal{}, false, nil
	}
	if err != nil {
		return OrderProposal{}, false, fmt.Errorf("oms: read proposal %s: %w", orderID, err)
	}
	return prop, true, nil
}

// Claim is the serialisation point. See ProposalStore.Claim.
//
// ONE STATEMENT, and `approver = ”` in the WHERE is what makes it atomic: a
// SELECT to check it is still pending followed by an UPDATE would leave exactly
// the window this method exists to close. The rows-affected count of the single
// UPDATE is the engine's answer to "did I get it", and Postgres serialises the
// two writers for free.
//
// THE SELF-APPROVAL CHECK IS ALSO IN THE PREDICATE rather than only in Go. If it
// were only in Go the database would still refuse the row (0009's CHECK), but as
// a constraint violation — an error indistinguishable from a broken migration,
// on a control whose refusal an operator has to be able to read. In the
// predicate it is a clean "you did not get it", and the CHECK stays as the
// backstop for any writer that does not come through here.
func (p *PostgresProposals) Claim(ctx context.Context, orderID, approver string, at time.Time) (bool, error) {
	if approver == "" {
		return false, errors.New("oms: an approval must come from an authenticated subject")
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE order_proposals
		   SET approver = $2, decided_at = $3
		 WHERE order_id = $1
		   AND approver = ''
		   AND lower(btrim(proposer)) <> lower(btrim($2))
	`, orderID, approver, at.UTC())
	if err != nil {
		return false, fmt.Errorf("oms: claim proposal %s: %w", orderID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ExpiredUnannounced lists what died waiting. See ProposalStore.
//
// THE MIRROR OF Pending's PREDICATE, and both halves matter. `approver = ”`
// keeps a signed proposal out — it was decided, and the deadline stopped
// mattering the moment somebody signed. `expiry_announced_at IS NULL` keeps an
// already-announced one out, which is what stops the sweeper republishing the
// same ORDER_REJECTED on every tick for the rest of the deployment's life.
func (p *PostgresProposals) ExpiredUnannounced(ctx context.Context, now time.Time, limit int) ([]OrderProposal, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT order_id, portfolio_id, act, proposer, digest, command, approver, decided_at, created_at, expires_at
		FROM order_proposals
		WHERE approver = '' AND expiry_announced_at IS NULL AND expires_at <= $1
		ORDER BY expires_at, order_id
		LIMIT $2
	`, now.UTC(), expiryBatch(limit))
	if err != nil {
		return nil, fmt.Errorf("oms: list expired proposals: %w", err)
	}
	defer rows.Close()
	var out []OrderProposal
	for rows.Next() {
		prop, err := scanOrderProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, prop)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oms: list expired proposals: %w", err)
	}
	return out, nil
}

// expiryBatch bounds one sweep. A deployment that armed the control and then
// left the queue unattended for a week has a backlog, and announcing all of it
// in one transaction would hold a lock for as long as it takes.
func expiryBatch(limit int) int {
	if limit <= 0 {
		return defaultExpiryBatch
	}
	return limit
}

const defaultExpiryBatch = 100

// AnnounceExpiry is the serialisation point for expiry. See ProposalStore.
//
// THE SAME FIVE BEATS AS Put, AND FOR THE SAME REASON. The marker and the FACT
// it describes commit together, so the record cannot be enqueued for an expiry
// that was not marked, and the mark cannot claim an announcement that was never
// queued. Splitting them is the #292 defect: a crash between the two leaves the
// store and the estate disagreeing, recovered only by another marker and another
// compensator.
func (p *PostgresProposals) AnnounceExpiry(ctx context.Context, orderID string, at time.Time, announce []outbox.Record) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("oms: announce expiry %s: begin: %w", orderID, err)
	}
	// No-op after a successful Commit.
	defer func() { _ = tx.Rollback(ctx) }()

	// ONE STATEMENT, AND ITS ROWS-AFFECTED IS THE VERDICT. A SELECT-then-UPDATE
	// reopens check-then-act between two pods; every condition that decides
	// whether this expiry is announceable lives in the WHERE so the engine
	// settles it. expires_at <= $2 is re-checked HERE rather than trusted from
	// the sweeper: a pod whose clock drifted forward must not be able to kill an
	// order somebody still has time to sign.
	tag, err := tx.Exec(ctx, `
		UPDATE order_proposals
		   SET expiry_announced_at = $2
		 WHERE order_id = $1
		   AND approver = ''
		   AND expiry_announced_at IS NULL
		   AND expires_at <= $2
	`, orderID, at.UTC())
	if err != nil {
		return false, fmt.Errorf("oms: announce expiry %s: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		// Somebody else announced it, it was signed, or it has not expired. The
		// loser returns BEFORE the enqueue, so the deferred Rollback discards a
		// FACT that must not go out.
		return false, nil
	}
	if err := outbox.Enqueue(ctx, tx, announce...); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("oms: announce expiry %s: commit: %w", orderID, err)
	}
	return true, nil
}

// Pending lists what is still awaiting a signature.
//
// BOTH CLAUSES ARE LOAD-BEARING, AND NEITHER IS REDUNDANT (#548). Together they
// are exactly the predicate of order_proposals_open_idx (0011), and Postgres
// uses a partial index only where it can prove the predicate holds. Removing
// either as implied by the rest of the query turns this into a sequential scan
// of every order ever held.
//
// `expiry_announced_at IS NULL` IS NOT IMPLIED BY `expires_at > $1`. The CHECK
// added by 0010 makes them equivalent in practice — an announcement cannot
// predate the expiry it announces — but the planner does not reason across a
// CHECK constraint, so the clause has to be here for the index to be usable.
//
// IT IS ALSO MORE CORRECT, not just faster. A pod running fast can announce a
// proposal a slower reader still considers live. Without this clause that row
// comes back on the approver's queue as work AFTER the estate has been told the
// order was rejected and will not trade.
func (p *PostgresProposals) Pending(ctx context.Context, now time.Time) ([]OrderProposal, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT order_id, portfolio_id, act, proposer, digest, command, approver, decided_at, created_at, expires_at
		FROM order_proposals
		WHERE approver = '' AND expiry_announced_at IS NULL AND expires_at > $1
		ORDER BY created_at, order_id
	`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("oms: list pending proposals: %w", err)
	}
	defer rows.Close()
	var out []OrderProposal
	for rows.Next() {
		prop, err := scanOrderProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("oms: scan pending proposal: %w", err)
		}
		out = append(out, prop)
	}
	return out, rows.Err()
}

// proposalScanner is what pgx.Row and pgx.Rows have in common.
type proposalScanner interface{ Scan(dest ...any) error }

func scanOrderProposal(s proposalScanner) (OrderProposal, error) {
	var (
		prop      OrderProposal
		act       string
		blob      []byte
		decidedAt *time.Time
	)
	if err := s.Scan(&prop.ID, &prop.PortfolioID, &act, &prop.Proposer, &prop.Digest,
		&blob, &prop.Approver, &decidedAt, &prop.CreatedAt, &prop.ExpiresAt); err != nil {
		return OrderProposal{}, err
	}
	prop.Subject = prop.ID
	prop.Act = dualcontrol.Act(act)
	cmd := &orderpb.SubmitOrder{}
	if err := proto.Unmarshal(blob, cmd); err != nil {
		// A stored command that will not decode is a corrupt record. REFUSE the
		// read rather than returning a proposal with no order in it — an
		// approver would otherwise be shown an empty order to sign for.
		return OrderProposal{}, fmt.Errorf("proposal %s: stored command does not decode: %w", prop.ID, err)
	}
	prop.Command = cmd
	if decidedAt != nil {
		prop.DecidedAt = decidedAt.UTC()
	}
	return prop, nil
}

func sortProposals(ps []OrderProposal) {
	sort.Slice(ps, func(i, j int) bool {
		if !ps[i].CreatedAt.Equal(ps[j].CreatedAt) {
			return ps[i].CreatedAt.Before(ps[j].CreatedAt)
		}
		// Ties broken by id so the order is total. A pending list that reorders
		// between reads reads as activity.
		return ps[i].ID < ps[j].ID
	})
}

var (
	_ ProposalStore = (*MemoryProposals)(nil)
	_ ProposalStore = (*PostgresProposals)(nil)
)
