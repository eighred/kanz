package custody

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// Publisher is the bus publish surface — satisfied by *bus.Producer. It is the
// same one-method interface consume.Publisher is, for the same reason: this
// package must be testable without a broker, and must not grow a dependency on
// the producer's construction.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// BookLoader materializes a portfolio's current book. It is a function rather
// than the ledger.Store interface so the reconciler depends on the one operation
// it needs instead of on the whole journal surface.
type BookLoader func(ctx context.Context, portfolioID string) (*ledger.Book, error)

// LedgerBookLoader is the production BookLoader over a ledger store.
func LedgerBookLoader(store ledger.Store) BookLoader {
	return func(ctx context.Context, portfolioID string) (*ledger.Book, error) {
		book, _, err := ledger.MaterializeCurrent(ctx, store, portfolioID)
		return book, err
	}
}

// Reconciler performs one reconciliation and records what it concluded.
//
// EVERY PATH THROUGH Run ENDS IN A RECORDED RUN, including the ones that failed
// and the one where the custodian said nothing. That is the whole design: the
// pre-#962 control produced output only on success-with-findings, so silence
// covered "clean", "never ran", "the custodian stopped sending" and "the book
// would not materialize" indistinguishably. Each of those is now a different
// outcome on a FACT with a timestamp.
type Reconciler struct {
	store     Store
	book      BookLoader
	publisher Publisher
	tolerance *big.Rat
	metrics   *Metrics
	logger    *slog.Logger
	now       func() time.Time
}

// NewReconciler wires a Reconciler.
//
// A NIL PUBLISHER IS ANNOUNCED, NOT TOLERATED SILENTLY. Without one the runs
// happen and the estate never hears them, which is a control that looks alive
// from the inside and is invisible from the outside — the exact condition #418
// established must never be indistinguishable from a healthy seam.
func NewReconciler(store Store, book BookLoader, publisher Publisher, tolerance *big.Rat, metrics *Metrics, logger *slog.Logger, now func() time.Time) (*Reconciler, error) {
	if store == nil {
		return nil, errors.New("custody: nil store")
	}
	if book == nil {
		return nil, errors.New("custody: nil book loader")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if tolerance == nil {
		tolerance = new(big.Rat)
	}
	if publisher == nil {
		logger.Error("accounting: custody reconciliation is running WITHOUT a publisher — runs will be " +
			"recorded locally and no ReconciliationRun FACT will reach the estate, so every downstream " +
			"reader sees an estate that has never reconciled (#962)")
	}
	return &Reconciler{
		store: store, book: book, publisher: publisher,
		tolerance: tolerance, metrics: metrics, logger: logger, now: now,
	}, nil
}

// Reconcile performs the reconciliation for one subject and records the run.
//
// IT RETURNS THE RUN AND AN ERROR INDEPENDENTLY, and callers must read both. A
// FAILED run is still a run that happened and is still published; the error is
// what the scheduler logs and counts. Returning only the error would lose the
// evidence, and returning only the run would let a caller treat a failure as a
// verdict about the book.
func (r *Reconciler) Reconcile(ctx context.Context, subject Subject) (Run, error) {
	if err := subject.Validate(); err != nil {
		return Run{}, err
	}
	subject.BusinessDate = BusinessDay(subject.BusinessDate)
	now := r.now().UTC()

	stmt, err := r.store.LatestStatement(ctx, subject)
	switch {
	case errors.Is(err, ErrNoStatement):
		// THE CUSTODIAN SAID NOTHING. This is the case that used to be pure
		// silence, and it is emitted as a first-class outcome rather than
		// skipped: an empty statement would reconcile as "the custodian holds
		// nothing" and manufacture a break for every position the book holds.
		return r.record(ctx, Run{
			RunID:       runID(subject, now),
			Subject:     subject,
			Outcome:     OutcomeNoStatement,
			Tolerance:   r.tolerance,
			CompletedAt: now,
		})
	case err != nil:
		return r.recordFailure(ctx, subject, now, fmt.Sprintf("load statement: %v", err))
	}

	book, err := r.book(ctx, subject.PortfolioID)
	if err != nil {
		return r.recordFailure(ctx, subject, now, fmt.Sprintf("materialize book: %v", err))
	}

	detected := recon.Reconcile(book, recon.Statement{
		PortfolioID: subject.PortfolioID,
		Positions:   stmt.Positions,
		Cash:        stmt.Cash,
	}, r.tolerance)

	stored, err := r.store.UpsertBreaks(ctx, subject, FromRecon(subject, detected, now), now)
	if err != nil {
		return r.recordFailure(ctx, subject, now, fmt.Sprintf("upsert breaks: %v", err))
	}

	outcome := OutcomeClean
	if len(detected) > 0 {
		outcome = OutcomeBreaks
	}
	// THE RUN CARRIES THE STORED BREAKS, NOT THE FRESHLY DETECTED ONES. The
	// stored copies hold the lifecycle — the assignee, the explanation and the
	// FirstSeenAt an operator has been working against — and publishing the
	// freshly built ones would announce every break as OPEN and one run old, so
	// any consumer of the FACT would see an aging problem reset to zero daily.
	return r.record(ctx, Run{
		RunID:       runID(subject, now),
		Subject:     subject,
		Outcome:     outcome,
		StatementID: stmt.StatementID,
		Breaks:      breaksForSubject(stored, subject),
		Tolerance:   r.tolerance,
		CompletedAt: now,
	})
}

// recordFailure records and publishes a FAILED run, and returns the underlying
// error alongside it.
//
// A FAILURE IS RECORDED RATHER THAN SWALLOWED because the alternative is that a
// reconciliation which could not run looks exactly like one that never came due.
// The estate must be able to tell "the book would not materialize for three days"
// from "nothing was scheduled".
func (r *Reconciler) recordFailure(ctx context.Context, subject Subject, now time.Time, reason string) (Run, error) {
	run, err := r.record(ctx, Run{
		RunID:         runID(subject, now),
		Subject:       subject,
		Outcome:       OutcomeFailed,
		Tolerance:     r.tolerance,
		CompletedAt:   now,
		FailureReason: reason,
	})
	if err != nil {
		return run, err
	}
	return run, errors.New("custody: " + reason)
}

// record persists a run, publishes it, and moves the metrics.
//
// THE ORDER IS DELIBERATE: persist, then publish, then observe. The store is the
// local record and must not depend on the broker being up. The metric moves LAST
// and only on a successful publish, so the staleness gauge and the FACT cannot
// disagree — a run the estate never heard of must not read as recent evidence
// that the control is alive, which is precisely the false assurance this issue
// exists to remove.
func (r *Reconciler) record(ctx context.Context, run Run) (Run, error) {
	if err := r.store.SaveRun(ctx, run); err != nil {
		return run, fmt.Errorf("custody: save run: %w", err)
	}
	if err := r.publish(ctx, run); err != nil {
		r.metrics.runFailed(run.Subject.CustodianID)
		return run, err
	}
	r.metrics.observeRun(run)
	if outstanding, err := r.store.OutstandingBreaks(ctx); err == nil {
		r.metrics.observeBreaks(outstanding, run.CompletedAt)
	}
	return run, nil
}

// publish emits the run as a FACT. A nil publisher publishes nothing and says so
// once at construction rather than on every run.
func (r *Reconciler) publish(ctx context.Context, run Run) error {
	if r.publisher == nil {
		return nil
	}
	msg, err := runToProto(run)
	if err != nil {
		return err
	}
	// THE PARTITION KEY IS THE (portfolio, custodian) PAIR, which is the
	// granularity a consumer folds at and the granularity the staleness gauge is
	// keyed on. Keying on the run id would order nothing that matters and would
	// make every run its own key; keying on the portfolio alone would interleave
	// two custodians' runs for one book and let the older one land last.
	return r.publisher.Publish(ctx, bus.Event{
		Subject:          SubjectRun,
		EventType:        SubjectRun,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "accounting",
		EventTime:        run.CompletedAt,
		PartitionKey:     pairKey(run.Subject.PortfolioID, run.Subject.CustodianID),
		PayloadSchemaRef: schemaRefRun,
		Payload:          msg,
	})
}

// runToProto renders a run onto the wire.
//
// A FIGURE THAT WILL NOT FIT REFUSES THE PUBLISH rather than going out scaled or
// wrapped (#94). A break whose difference wrapped would be announced as a
// smaller disagreement than it is, and a reconciliation that understates a
// difference is worse than one that never ran — it is a control actively
// reporting the wrong answer.
func runToProto(run Run) (*accountingpb.ReconciliationRun, error) {
	tolerance, ok := dec.ToProtoScaled(orZeroRat(run.Tolerance))
	if !ok {
		return nil, fmt.Errorf("custody: run %s: tolerance is not representable as a Decimal", run.RunID)
	}
	out := &accountingpb.ReconciliationRun{
		RunId:         run.RunID,
		PortfolioId:   run.Subject.PortfolioID,
		CustodianId:   run.Subject.CustodianID,
		BusinessDate:  timestamppb.New(BusinessDay(run.Subject.BusinessDate)),
		Outcome:       wireOutcome(run.Outcome),
		StatementId:   run.StatementID,
		Tolerance:     tolerance,
		CompletedAt:   timestamppb.New(run.CompletedAt),
		FailureReason: run.FailureReason,
	}
	for _, b := range run.Breaks {
		kind, ok := wireKind(b.Kind)
		if !ok {
			return nil, fmt.Errorf("custody: run %s: break kind %q has no wire representation", run.RunID, b.Kind)
		}
		ibor, iok := dec.ToProtoScaled(orZeroRat(b.IBOR))
		cust, cok := dec.ToProtoScaled(orZeroRat(b.Custodian))
		diff, dok := dec.ToProtoScaled(orZeroRat(b.Diff))
		if !iok || !cok || !dok {
			return nil, fmt.Errorf("custody: run %s: break %s figures are not representable as Decimals", run.RunID, b.BreakID)
		}
		out.Breaks = append(out.Breaks, &accountingpb.ReconciliationBreak{
			BreakId:         b.BreakID,
			Kind:            kind,
			Key:             b.Key,
			Ibor:            ibor,
			Custodian:       cust,
			Difference:      diff,
			Status:          wireStatus(b.Status),
			Assignee:        b.Assignee,
			Explanation:     b.Explanation,
			FirstSeenAt:     timestamppb.New(b.FirstSeenAt),
			LastSeenAt:      timestamppb.New(b.LastSeenAt),
			StatusChangedAt: timestamppb.New(b.StatusChangedAt),
		})
	}
	return out, nil
}

func wireStatus(s BreakStatus) accountingpb.BreakStatus {
	switch s {
	case BreakOpen:
		return accountingpb.BreakStatus_BREAK_STATUS_OPEN
	case BreakAssigned:
		return accountingpb.BreakStatus_BREAK_STATUS_ASSIGNED
	case BreakExplained:
		return accountingpb.BreakStatus_BREAK_STATUS_EXPLAINED
	case BreakResolved:
		return accountingpb.BreakStatus_BREAK_STATUS_RESOLVED
	default:
		return accountingpb.BreakStatus_BREAK_STATUS_UNSPECIFIED
	}
}

// breaksForSubject filters stored breaks to the ones belonging to subject's pair.
func breaksForSubject(all []Break, subject Subject) []Break {
	var out []Break
	for _, b := range all {
		if sameSubject(b, subject) {
			out = append(out, b)
		}
	}
	return out
}

// runID derives a run's identity. It includes the completion instant so a
// restated statement for an already-reconciled date produces a NEW run rather
// than colliding with the old one — the history must show both, and which came
// first.
func runID(subject Subject, now time.Time) string {
	return subject.Key() + "|" + now.UTC().Format(time.RFC3339Nano)
}

func orZeroRat(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}
