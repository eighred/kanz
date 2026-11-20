package custody

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
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
var errRunReceiptLimit = errors.New("custody: comparison exceeds the retained receipt limit")

const historicalBreakLimit = 4096

var ErrRunBusy = errors.New("custody: reconciliation pair is being committed")
var ErrRunCompletionMissing = errors.New("custody: existing run has no immutable completion FACT")
var ErrRunIdentityConflict = errors.New("custody: run ID belongs to another subject")
var ErrHistoricalLifecycle = errors.New("custody: a later business date already controls outstanding breaks")
var ErrStatementChanged = errors.New("custody: the authoritative statement changed during comparison")

const custodyPairLockKeySQL = `hashtextextended(app_current_tenant() || ':custody-run:' || $1,0)`

func lockCustodyPair(ctx context.Context, tx pgx.Tx, subject Subject) (string, error) {
	var tenant string
	var locked bool
	err := tx.QueryRow(ctx, `SELECT app_current_tenant(), pg_try_advisory_xact_lock(`+custodyPairLockKeySQL+`)`, pairKey(subject.PortfolioID, subject.CustodianID)).Scan(&tenant, &locked)
	if err == nil && !locked {
		err = ErrRunBusy
	}
	return tenant, err
}

func verifying(run Run) bool { return run.Outcome == OutcomeClean || run.Outcome == OutcomeBreaks }

func (run Run) detection() Detection {
	if run.Outcome != OutcomeBreaks || run.StatementID == "" {
		return Detection{}
	}
	d := Detection{RunID: run.RunID, StatementID: run.StatementID, BusinessDate: BusinessDay(run.Subject.BusinessDate).Format(time.DateOnly)}
	if run.Basis != nil {
		d.ScopeMode = run.Basis.Mode
		d.JournalPosition = strconv.FormatInt(run.Basis.JournalPosition, 10)
	}
	return d
}

func (run *Run) confirmStatement(current Statement, err error) error {
	if err != nil {
		return err
	}
	prior := run.compared
	run.compared = nil
	if !sameComparison(current, *prior) {
		return ErrStatementChanged
	}
	return nil
}

func sameComparison(current, prior Statement) bool {
	return current.StatementID == prior.StatementID && current.PortfolioID == prior.PortfolioID &&
		current.CustodianID == prior.CustodianID && current.BusinessDate.Equal(prior.BusinessDate) &&
		current.ReceivedAt.Equal(prior.ReceivedAt) && current.Grain == prior.Grain &&
		maps.EqualFunc(current.Positions, prior.Positions, equalFigure) &&
		maps.EqualFunc(current.Cash, prior.Cash, equalFigure) &&
		slices.EqualFunc(current.Transactions, prior.Transactions, equalTransaction)
}

func equalFigure(a, b *big.Rat) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(b) == 0
}

func equalTransaction(a, b recon.Transaction) bool {
	return a.ExternalRef == b.ExternalRef && a.InstrumentID == b.InstrumentID && a.QuantityInInstrumentUnits == b.QuantityInInstrumentUnits &&
		a.EconomicsConvention == b.EconomicsConvention && equalFigure(a.Commission, b.Commission) && a.CurrencyCode == b.CurrencyCode && a.TradeDate.Equal(b.TradeDate) &&
		a.SettlementDate.Equal(b.SettlementDate) && equalFigure(a.Quantity, b.Quantity) &&
		equalFigure(a.Price, b.Price) && equalFigure(a.Cash, b.Cash)
}

// The customer can reopen a signed full comparison by its run ID even when
// the signed source attests balances only. Preserve the bounds on the other
// retained execution evidence and the existing unsigned comparison contract.
func (run Run) requiresBoundedReceipt() bool {
	return run.Outcome.IsHistoricalComparison() || (verifying(run) && isBatchStatement(run.StatementID)) ||
		run.ExecutionQuantities != nil || run.ExecutionEconomics != nil || len(run.HistoricalQuantityResolutions) > 0
}

func runRecord(ctx context.Context, tenant string, run Run, checks ...func(context.Context, bus.Event) error) (outbox.Record, error) {
	msg, err := runToProto(run)
	if err != nil {
		return outbox.Record{}, err
	}
	// The immutable lookup bounds both the full payload and the working breaks.
	// Refuse before the queue transaction commits an unreadable completion.
	if run.requiresBoundedReceipt() && (len(msg.Breaks) > historicalBreakLimit || len(run.HistoricalQuantityResolutions) > historicalBreakLimit || proto.Size(msg) > BatchLimit) {
		return outbox.Record{}, errRunReceiptLimit
	}
	record, err := outbox.From(ctx, bus.Event{Subject: SubjectRun, EventType: SubjectRun,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
		TenantID: tenant, EventTime: run.CompletedAt, CorrelationID: run.RunID, CausationID: run.RequestID,
		PartitionKey:     pairKey(run.Subject.PortfolioID, run.Subject.CustodianID),
		PayloadSchemaRef: schemaRefRun, Payload: msg})
	if err != nil {
		return outbox.Record{}, err
	}
	for _, check := range checks {
		if err := record.PreflightPublication(ctx, check); err != nil {
			return outbox.Record{}, err
		}
	}
	return record, nil
}

func cloneRun(run Run) Run {
	run.Tolerance = cloneRat(run.Tolerance)
	run.Basis = run.Basis.clone()
	run.ExecutionQuantities = proto.CloneOf(run.ExecutionQuantities)
	run.ExecutionEconomics = proto.CloneOf(run.ExecutionEconomics)
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

func (m *MemoryStore) CommitRun(ctx context.Context, run Run, checks ...func(context.Context, bus.Event) error) (Run, error) {
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
	if run.Outcome.IsHistoricalComparison() || run.Outcome == OutcomeHistoricalQuantityCorrection || len(run.HistoricalQuantityResolutions) > 0 {
		return Run{}, ErrDurableActionStore
	}
	run = cloneRun(run)
	if run.Basis != nil && run.Basis.Assignments != nil {
		return Run{}, ErrUnprovenAssignment
	}
	if run.ExecutionQuantities != nil || run.ExecutionEconomics != nil {
		return Run{}, errBatchReceipt
	}
	var olderBookBasis bool
	if verifying(run) {
		date := BusinessDay(run.Subject.BusinessDate)
		history := m.runs[pairKey(run.Subject.PortfolioID, run.Subject.CustodianID)]
		for _, prior := range slices.Backward(history) {
			if !verifying(prior) || prior.Subject.PortfolioID != run.Subject.PortfolioID || prior.Subject.CustodianID != run.Subject.CustodianID {
				continue
			}
			priorDate := BusinessDay(prior.Subject.BusinessDate)
			if priorDate.After(date) {
				return Run{}, ErrHistoricalLifecycle
			}
			if priorDate.Equal(date) && run.Basis != nil && prior.Basis != nil && prior.Basis.JournalPosition > run.Basis.JournalPosition {
				olderBookBasis = true
			}
		}
	}
	if run.observedMissing {
		_, err := m.latestStatementLocked(run.Subject)
		if !errors.Is(err, ErrNoStatement) {
			return Run{}, ErrStatementChanged
		}
		run.observedMissing = false
	}
	if olderBookBasis {
		return Run{}, ErrBookBasisChanged
	}
	if run.compared != nil {
		current, err := m.latestStatementLocked(run.Subject)
		if err := run.confirmStatement(current, err); err != nil {
			return Run{}, err
		}
	}
	// Stage into a detached map. Encoding/tenant/outbox refusal must leave the
	// original lifecycle intact, just as a PostgreSQL rollback does.
	staged := NewMemoryStore()
	for id, b := range m.breaks {
		staged.breaks[id] = b
	}
	if verifying(run) {
		for i := range run.Breaks {
			run.Breaks[i].comparison = run.Basis
			run.Breaks[i].Detection = run.detection()
		}
		stored, err := staged.UpsertBreaks(ctx, run.Subject, run.Breaks, run.CompletedAt)
		if err != nil {
			return Run{}, err
		}
		run.Breaks = breaksForSubject(stored, run.Subject)
	}
	record, err := runRecord(ctx, bus.TenantIDFromContext(ctx), run, checks...)
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

func (p *Postgres) CommitRun(ctx context.Context, run Run, checks ...func(context.Context, bus.Event) error) (Run, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Re-read the admitted set after the tenant assignment lock is acquired.
	// A deployment-level repeatable-read default would retain an older snapshot.
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenant, err := lockCustodyPair(ctx, tx, run.Subject)
	if err != nil {
		return Run{}, err
	}
	// Run IDs are tenant-wide even if an erroneous caller changes the pair.
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(app_current_tenant() || ':custody-run-id:' || $1,0))`, run.RunID).Scan(&locked); err != nil {
		return Run{}, err
	}
	if !locked {
		return Run{}, ErrRunBusy
	}
	if run.RequestID != "" && bus.TenantIDFromContext(ctx) != tenant {
		return Run{}, ErrInvalidManualRequest
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
		if run.RequestID != "" {
			prior, err := storedManualReceipt(ctx, tx, run)
			if err != nil {
				return Run{}, err
			}
			return prior, tx.Commit(ctx)
		}
		var payload []byte
		// Older scheduled runs kept their only completion in the relay. Never
		// reconstruct their figures from the current, mutable break queue.
		var basis *RunBasis
		err := tx.QueryRow(ctx, `SELECT COALESCE(manual_receipt, (
			SELECT payload FROM outbox WHERE producer_service=$1 AND event_type=$2 AND correlation_id=$3 ORDER BY id LIMIT 1
		)), book_basis FROM custody_runs WHERE tenant_id=app_current_tenant() AND run_id=$3`,
			outbox.ServiceAccounting, SubjectRun, run.RunID).Scan(&payload, &basis)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && payload == nil) {
			return Run{}, ErrRunCompletionMissing
		}
		if err != nil {
			return Run{}, err
		}
		prior, err := runFromRecord(payload)
		if err != nil {
			return Run{}, err
		}
		if prior.RunID != run.RunID || prior.Subject.Key() != run.Subject.Key() || prior.RequestID != "" {
			return Run{}, ErrRunIdentityConflict
		}
		prior.Basis = basis
		return prior, tx.Commit(ctx)
	}
	if run.ExecutionEconomics != nil || run.Outcome.IsHistoricalComparison() || run.Outcome == OutcomeHistoricalQuantityCorrection || len(run.HistoricalQuantityResolutions) > 0 || (run.Basis != nil && run.Basis.HistoricalDetections != nil) {
		return Run{}, ErrUnverifiedPrecision
	}
	// Existing store operations open SAVEPOINTs on this handle. Their local
	// commits cannot survive failure of the enclosing run/outbox transaction.
	staged := &Postgres{pool: p.pool, tx: tx}
	var signedDigest []byte
	switch {
	case run.observedMissing, run.RequestID != "" && verifying(run),
		verifying(run) && run.compared != nil && isBatchStatement(run.StatementID) && run.Basis != nil && run.Basis.Mode == BasisDeclared:
		// A retained signed revision can arrive before its delivery to the
		// statement consumer. Keep that intake frontier stable through commit
		// so neither an absence nor an older comparison is recorded meanwhile.
		signedDigest, err = confirmSignedIntake(ctx, tx, tenant, run)
	}
	if err != nil {
		return Run{}, err
	}
	if run.ExecutionQuantities != nil && len(signedDigest) == 0 {
		return Run{}, errBatchReceipt
	}
	if run.observedMissing {
		_, err := staged.LatestStatement(ctx, run.Subject)
		if err == nil {
			return Run{}, ErrStatementChanged
		}
		if !errors.Is(err, ErrNoStatement) {
			return Run{}, err
		}
		run.observedMissing = false
	}
	if verifying(run) {
		var verifiedAssignment bool
		if run.Basis != nil && run.Basis.Assignments != nil {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtextextended('custody-assignment-tenant:' || app_current_tenant(),0))"); err != nil {
				return Run{}, err
			}
			verifiedAssignment, err = confirmRunAssignments(ctx, tx, run.Subject, run.Basis, false)
			if err != nil {
				return Run{}, err
			}
		}
		// The transaction already owns the pair lock. A later clean run is as
		// authoritative as a later break and must prevent historical reopening.
		var later bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM custody_runs WHERE tenant_id=app_current_tenant()
			AND portfolio_id=$1 AND custodian_id=$2 AND outcome IN ('clean','breaks')
			AND business_date > $3::date)`, run.Subject.PortfolioID, run.Subject.CustodianID,
			BusinessDay(run.Subject.BusinessDate)).Scan(&later)
		if err != nil {
			return Run{}, err
		}
		if run.Basis != nil {
			var newer bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM custody_runs WHERE tenant_id=app_current_tenant()
				AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3::date AND outcome IN ('clean','breaks')
				AND (book_basis->>'journal_position')::bigint > $4)`, run.Subject.PortfolioID, run.Subject.CustodianID, BusinessDay(run.Subject.BusinessDate), run.Basis.JournalPosition).Scan(&newer); err != nil {
				return Run{}, err
			}
			if newer {
				return Run{}, ErrBookBasisChanged
			}
		}
		manualSnapshot := later && run.RequestID != ""
		if manualSnapshot && (!verifiedAssignment || run.Actor == "" || run.RunID != "manual:"+run.RequestID ||
			len(signedDigest) == 0 || run.compared == nil || run.Basis.Transactions == nil ||
			!run.Basis.Transactions.matches(*run.compared, run.Outcome, run.Basis.JournalPosition)) {
			return Run{}, ErrHistoricalLifecycle
		}
		if !later && len(signedDigest) > 0 {
			// The working queue and its completion must describe the effective
			// book at commit, even if no newer comparison has run yet.
			if err := lockJournalForComparison(ctx, tx, run.Subject.PortfolioID); err != nil {
				return Run{}, err
			}
			present, changed, err := journalPrefixAtClose(ctx, tx, run.Subject, run.Basis.JournalPosition)
			if err != nil {
				return Run{}, err
			}
			if !present || changed {
				return Run{}, ErrBookBasisChanged
			}
		}
		if manualSnapshot {
			if err := confirmCurrentJournalPosition(ctx, tx, run); err != nil {
				return Run{}, err
			}
		}
		if run.compared != nil {
			current, err := staged.LatestStatement(ctx, run.Subject)
			if err := run.confirmStatement(current, err); err != nil {
				return Run{}, err
			}
			if (!later || manualSnapshot) && len(signedDigest) > 0 {
				run.ExecutionEconomics, err = compareExecutionEconomics(ctx, tx, run, current)
				if err != nil {
					return Run{}, err
				}
			}
		}
		switch {
		case manualSnapshot:
			// The detected rows describe this comparison. Only the later close
			// owns the working queue, including its execution observations.
			run.Outcome = OutcomeHistoricalClean
			if len(run.Breaks) != 0 {
				run.Outcome = OutcomeHistoricalBreaks
			}
			for i := range run.Breaks {
				run.Breaks[i].ValuesVerified = true
			}
			run.measured = nil
		case later:
			// Never sweep another date's working set. Only a newer independently signed
			// equality for a still-open item can yield this narrower completion.

			resolutions, err := quantityObservations(ctx, tx, run, signedDigest, true)
			if err != nil {
				return Run{}, err
			}
			if len(resolutions) == 0 {
				// Scheduled redelivery has no operator request identity. A fully
				// retained correction for this exact source and prefix is its original
				// result; repeating it must not emit a second completion or false failure.
				if len(run.measured) > 0 && run.Basis != nil && len(run.Basis.Accounts[run.Subject.CustodianID]) == 1 {
					var id string
					err := tx.QueryRow(ctx, `SELECT run_id FROM custody_runs
                        WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3
                          AND outcome='historical_quantity_correction' AND statement_id=$4 AND signed_statement_sha256=$5
                          AND (book_basis->>'journal_position')::bigint=$6
                        ORDER BY completed_at DESC,run_id LIMIT 1`, run.Subject.PortfolioID, run.Subject.CustodianID, run.Subject.BusinessDate, run.StatementID, signedDigest, run.Basis.JournalPosition).Scan(&id)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return Run{}, err
					}
					if err == nil {
						prior, err := historicalReceiptSnapshot(ctx, tx, tenant, run.Subject, run.Basis.Accounts[run.Subject.CustodianID][0], id)
						if err != nil {
							return Run{}, err
						}
						if prior.State == "recorded" && run.Basis.sameScope(prior.Run.Basis, run.Subject.CustodianID) {
							return prior.Run, tx.Commit(ctx)
						}
					}
				}
				return Run{}, ErrHistoricalLifecycle
			}
			if err := confirmCurrentJournalPosition(ctx, tx, run); err != nil {
				return Run{}, err
			}
			run.Outcome = OutcomeHistoricalQuantityCorrection
			run.HistoricalQuantityResolutions = resolutions
			run.Basis = run.Basis.clone()
			run.Basis.HistoricalDetections = make(map[string]string, len(resolutions))
			for _, link := range resolutions {
				run.Basis.HistoricalDetections[link.BreakId] = link.DetectionRunId
			}
			run.ExecutionQuantities = nil
			run.Breaks = nil
		default:
			// This many detected rows cannot fit the saved view even before
			// operator annotations are added. Check the final bytes in runRecord.
			if len(run.Breaks) > historicalBreakLimit && run.requiresBoundedReceipt() {
				return Run{}, errRunReceiptLimit
			}
			if d := run.detection(); d.RunID != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.custody_detection_run',$1,true)`, d.RunID); err != nil {
					return Run{}, err
				}
			}
			if run.Basis != nil {
				var assignmentJSON string
				if run.Basis.Assignments != nil {
					value, err := json.Marshal(assignmentsForCustodian(run.Basis.Assignments, run.Subject.CustodianID))
					if err != nil {
						return Run{}, err
					}
					assignmentJSON = string(value)
				}
				if _, err := tx.Exec(ctx, `SELECT set_config('app.custody_break_scope',
                jsonb_strip_nulls(jsonb_build_object('mode',$1::text,'accounts',$2::text[],'assignments',NULLIF($3,'')::jsonb))::text,true)`,
					run.Basis.Mode, run.Basis.Accounts[run.Subject.CustodianID], assignmentJSON); err != nil {
					return Run{}, err
				}
			}
			stored, err := staged.UpsertBreaks(ctx, run.Subject, run.Breaks, run.CompletedAt)
			if err != nil {
				return Run{}, err
			}
			run.Breaks = breaksForSubject(stored, run.Subject)
		}
	}
	record, err := runRecord(ctx, tenant, run, checks...)
	if err != nil {
		return Run{}, err
	}
	if err := staged.saveRun(ctx, run, signedDigest, record.Payload); err != nil {
		return Run{}, err
	}
	if len(run.measured) > 0 {
		if _, err := quantityObservations(ctx, tx, run, signedDigest, false); err != nil {
			return Run{}, err
		}
		run.measured = nil
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, record); err != nil {
		return Run{}, err
	}
	return run, tx.Commit(ctx)
}

// ledger_prepare_append takes the exclusive form of this portfolio lock.
// Read the prefix in a fresh READ COMMITTED statement after acquiring it.
func lockJournalForComparison(ctx context.Context, tx pgx.Tx, portfolio string) error {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtext(app_current_tenant()),hashtext($1))`, portfolio).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrRunBusy
	}
	return nil
}

func confirmCurrentJournalPosition(ctx context.Context, tx pgx.Tx, run Run) error {
	if err := lockJournalForComparison(ctx, tx, run.Subject.PortfolioID); err != nil {
		return err
	}
	var stable bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(
		(SELECT position FROM ledger_heads WHERE tenant_id=app_current_tenant() AND portfolio_id=$1),
		CASE WHEN $3 THEN (SELECT count(*) FROM ledger_entries WHERE tenant_id=app_current_tenant() AND portfolio_id=$1) END
	) IS NOT DISTINCT FROM $2::bigint`, run.Subject.PortfolioID, run.Basis.JournalPosition, run.RequestID != "").Scan(&stable); err != nil {
		return err
	}
	if !stable {
		return ErrBookBasisChanged
	}
	return nil
}

// Duplicate completion returns the original immutable FACT, not today's break
// queue: operator actions or later runs may have changed that queue meanwhile.
func runFromRecord(payload []byte) (Run, error) {
	var msg accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &msg); err != nil {
		return Run{}, err
	}
	run := Run{ExecutionEconomics: msg.GetExecutionEconomics(), HistoricalQuantityResolutions: msg.GetHistoricalQuantityResolutions(), ExecutionQuantities: msg.GetExecutionQuantities(), RunID: msg.GetRunId(), RequestID: msg.GetRequestId(), Actor: msg.GetActor(), Subject: Subject{PortfolioID: msg.GetPortfolioId(), CustodianID: msg.GetCustodianId(), BusinessDate: msg.GetBusinessDate().AsTime()},
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
