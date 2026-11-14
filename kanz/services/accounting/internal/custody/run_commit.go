package custody

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrPublicationPending = errors.New("custody: run committed; completion FACT publication pending")
var ErrRunBusy = errors.New("custody: reconciliation pair is being committed")
var ErrRunCompletionMissing = errors.New("custody: existing run has no immutable completion FACT")
var ErrRunIdentityConflict = errors.New("custody: run ID belongs to another subject")

func verifying(run Run) bool { return run.Outcome == OutcomeClean || run.Outcome == OutcomeBreaks }

func runRecord(ctx context.Context, tenant string, run Run) (outbox.Record, error) {
	msg, err := runToProto(run)
	if err != nil {
		return outbox.Record{}, err
	}
	return outbox.From(ctx, bus.Event{Subject: SubjectRun, EventType: SubjectRun,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
		TenantID: tenant, EventTime: run.CompletedAt, CorrelationID: run.RunID,
		PartitionKey:     pairKey(run.Subject.PortfolioID, run.Subject.CustodianID),
		PayloadSchemaRef: schemaRefRun, Payload: msg})
}

func cloneRun(run Run) Run {
	run.Tolerance = cloneRat(run.Tolerance)
	run.Breaks = append([]Break(nil), run.Breaks...)
	for i := range run.Breaks {
		b := &run.Breaks[i]
		b.PortfolioID, b.CustodianID = run.Subject.PortfolioID, run.Subject.CustodianID
		b.IBOR, b.Custodian, b.Diff = cloneRat(b.IBOR), cloneRat(b.Custodian), cloneRat(b.Diff)
	}
	return run
}

func (m *MemoryStore) Outbox() outbox.Queue { return m.outbox }
func (p *Postgres) Outbox() outbox.Queue    { return outbox.NewPostgres(p.pool, outbox.ServiceAccounting) }

// NewMemoryStoreWithOutbox shares the accounting relay's volatile queue. Losing
// the process still loses both the volatile book and its completion records.
func NewMemoryStoreWithOutbox(queue *outbox.Memory) (*MemoryStore, error) {
	if queue == nil {
		return nil, errors.New("custody: memory store needs an accounting outbox")
	}
	m := NewMemoryStore()
	m.outbox = queue
	return m, nil
}

func (m *MemoryStore) CommitRun(ctx context.Context, run Run) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref, ok := m.runIDs[run.RunID]; ok {
		if !ref.completionCaptured {
			return Run{}, ErrRunCompletionMissing
		}
		prior := m.runs[ref.pair][ref.index]
		if prior.Subject.Key() != run.Subject.Key() {
			return Run{}, ErrRunIdentityConflict
		}
		return cloneRun(prior), nil
	}
	run = cloneRun(run)
	// Stage into a detached map. Encoding/tenant/outbox refusal must leave the
	// original lifecycle intact, just as a PostgreSQL rollback does.
	staged := NewMemoryStore()
	for id, b := range m.breaks {
		staged.breaks[id] = b
	}
	if verifying(run) {
		stored, err := staged.UpsertBreaks(ctx, run.Subject, run.Breaks, run.CompletedAt)
		if err != nil {
			return Run{}, err
		}
		run.Breaks = breaksForSubject(stored, run.Subject)
	}
	record, err := runRecord(ctx, bus.TenantIDFromContext(ctx), run)
	if err != nil {
		return Run{}, err
	}
	if err := m.outbox.Append(record); err != nil {
		return Run{}, err
	}
	m.breaks = staged.breaks
	m.appendRunLocked(run, true)
	return cloneRun(run), nil
}

func (p *Postgres) CommitRun(ctx context.Context, run Run) (Run, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT app_current_tenant(), pg_try_advisory_xact_lock(hashtextextended(app_current_tenant() || ':custody-run:' || $1,0))`, pairKey(run.Subject.PortfolioID, run.Subject.CustodianID)).Scan(&tenant, &locked); err != nil {
		return Run{}, err
	}
	if !locked {
		return Run{}, ErrRunBusy
	}
	// Run IDs are tenant-wide even if an erroneous caller changes the pair.
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(app_current_tenant() || ':custody-run-id:' || $1,0))`, run.RunID).Scan(&locked); err != nil {
		return Run{}, err
	}
	if !locked {
		return Run{}, ErrRunBusy
	}
	if incoming := bus.TenantIDFromContext(ctx); incoming != "" {
		if err := bus.RequireTenantScope(incoming, tenant); err != nil {
			return Run{}, err
		}
		tenant = incoming
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM custody_runs WHERE run_id=$1)`, run.RunID).Scan(&exists); err != nil {
		return Run{}, err
	}
	if exists {
		var payload []byte
		err := tx.QueryRow(ctx, `SELECT payload FROM outbox WHERE producer_service=$1 AND event_type=$2 AND correlation_id=$3 ORDER BY id LIMIT 1`, outbox.ServiceAccounting, SubjectRun, run.RunID).Scan(&payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, ErrRunCompletionMissing
		}
		if err != nil {
			return Run{}, err
		}
		prior, err := runFromRecord(payload)
		if err != nil {
			return Run{}, err
		}
		if prior.RunID != run.RunID || prior.Subject.Key() != run.Subject.Key() {
			return Run{}, ErrRunIdentityConflict
		}
		return prior, tx.Commit(ctx)
	}
	// Existing store operations open SAVEPOINTs on this handle. Their local
	// commits cannot survive failure of the enclosing run/outbox transaction.
	staged := &Postgres{pool: p.pool, tx: tx}
	if verifying(run) {
		stored, err := staged.UpsertBreaks(ctx, run.Subject, run.Breaks, run.CompletedAt)
		if err != nil {
			return Run{}, err
		}
		run.Breaks = breaksForSubject(stored, run.Subject)
	}
	if err := staged.SaveRun(ctx, run); err != nil {
		return Run{}, err
	}
	record, err := runRecord(ctx, tenant, run)
	if err != nil {
		return Run{}, err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, record); err != nil {
		return Run{}, err
	}
	return run, tx.Commit(ctx)
}

// Duplicate completion returns the original immutable FACT, not today's break
// queue: operator actions or later runs may have changed that queue meanwhile.
func runFromRecord(payload []byte) (Run, error) {
	var msg accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &msg); err != nil {
		return Run{}, err
	}
	run := Run{RunID: msg.GetRunId(), Subject: Subject{PortfolioID: msg.GetPortfolioId(), CustodianID: msg.GetCustodianId(), BusinessDate: msg.GetBusinessDate().AsTime()},
		Outcome: parseOutcome(strings.ToLower(strings.TrimPrefix(msg.GetOutcome().String(), "RECONCILIATION_OUTCOME_"))), StatementID: msg.GetStatementId(), CompletedAt: msg.GetCompletedAt().AsTime(), FailureReason: msg.GetFailureReason()}
	var ok bool
	if run.Tolerance, ok = dec.FromProtoChecked(msg.GetTolerance()); !ok {
		return Run{}, ErrUnverifiedPrecision
	}
	for _, row := range msg.GetBreaks() {
		b := Break{BreakID: row.GetBreakId(), PortfolioID: run.Subject.PortfolioID, CustodianID: run.Subject.CustodianID, Key: row.GetKey(), Status: parseStatus(strings.ToLower(strings.TrimPrefix(row.GetStatus().String(), "BREAK_STATUS_"))), Assignee: row.GetAssignee(), Explanation: row.GetExplanation(),
			Kind: recon.BreakKind(-1), FirstSeenAt: row.GetFirstSeenAt().AsTime(), LastSeenAt: row.GetLastSeenAt().AsTime(), StatusChangedAt: row.GetStatusChangedAt().AsTime(), ValuesVerified: true}
		for _, kind := range recon.BreakKinds() {
			wire, _ := wireKind(kind)
			if wire == row.GetKind() {
				b.Kind = kind
			}
		}
		if b.Kind == recon.BreakKind(-1) || b.Status == BreakStatusUnspecified {
			return Run{}, fmt.Errorf("custody: invalid stored completion break")
		}
		values := []**big.Rat{&b.IBOR, &b.Custodian, &b.Diff}
		for i, decimal := range []*commonpb.Decimal{row.GetIbor(), row.GetCustodian(), row.GetDifference()} {
			// An omitted optional figure in the immutable FACT is unavailable,
			// including on a retry after the live break queue has changed.
			if decimal == nil {
				continue
			}
			value, ok := dec.FromProtoChecked(decimal)
			if !ok {
				return Run{}, ErrUnverifiedPrecision
			}
			*values[i] = value
		}
		run.Breaks = append(run.Breaks, b)
	}
	if run.RunID == "" || run.Outcome == OutcomeUnspecified {
		return Run{}, fmt.Errorf("custody: invalid stored completion")
	}
	return run, nil
}
