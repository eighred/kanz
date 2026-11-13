package custody

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"

	"github.com/eighred/kanz/internal/outbox"
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

// BookLoader materializes the book ONE custodian's statement is compared against.
// It is a function rather than the ledger.Store interface so the reconciler
// depends on the one operation it needs instead of on the whole journal surface.
//
// IT TAKES THE WHOLE SUBJECT, NOT THE PORTFOLIO ID (#1006). It used to take
// portfolioID alone, and there was therefore no parameter through which the
// custodian on the subject COULD reach the book side — the comparison loaded the
// whole portfolio and handed it to one custodian's statement. Threading the
// subject rather than a second string is what makes the omission impossible to
// reintroduce: a loader that ignores the custodian now has to ignore a field it
// was handed.
//
// IT RETURNS THE EXECUTIONS AS WELL AS THE FOLD (#1049). The comparison has two
// grains — netted positions and cash, and the EXECUTIONS behind them — and both
// come out of ONE call over one slice of the journal. A second loader would be a
// second answer to "which entries is this custodian compared against", which is
// the shape #1073 was filed on; returning them together is what makes the
// transaction pass inherit the custodian scoping the guard over this hop already
// enforces, rather than needing a guard of its own to be remembered.
type BookLoader func(ctx context.Context, subject Subject) (*ledger.Book, []ledger.Execution, error)

// EffectiveCutoff explicitly selects an effective instant for the read-only
// ad-hoc comparison, whose request has no statement business date. Zero refuses
// the read; omitting the option retains the dated statement-close contract.
type EffectiveCutoff struct{ AsOf time.Time }

// LedgerBookLoader is the production BookLoader over a ledger store.
//
// EVERY PATH IS SCOPED, AND THAT IS THE POINT (#1073). A portfolio custodied in
// two or more places folds only the entries that settled against THIS custodian's
// declared exchange accounts, and REFUSES when the journal holds value on an
// account no custodian claims. A portfolio custodied in ONE place folds the
// entries that settled against the accounts its journal actually touched — which
// needs no declaration, because a single custodian holds all of them.
//
// WHAT NO PATH DOES ANY MORE IS COMPARE THE WHOLE BOOK. The single-custodian
// branch used to, and the two branches therefore disagreed about the same entries:
// ledger.MaterializeForAccounts excludes an entry that settled against no exchange
// account, and the whole-book branch included it. Running the second against a
// custodian statement makes recon.Reconcile union the currencies and report the
// entire un-attributed balance as a cash break — an investor subscription into the
// fund's own bank, reported as a difference with a custodian that cannot see it,
// on the DEFAULT configuration. A break queue with routine false positives in it
// is one an operations team stops reading, and the real break then arrives in a
// queue nobody trusts.
//
// THE RESIDUE IS SAID OUT LOUD RATHER THAN DROPPED. Cash held away from every
// custodian is real money in the book of record that no custodian statement can
// confirm, so it is an UNRECONCILED BUCKET rather than a break or a zero, and the
// loader names it and its amount on every run that has one. Book.CashBalance
// remains the portfolio total for NAV and every other reader.
func LedgerBookLoader(store ledger.Store, scope *BookScope, logger *slog.Logger, cutoff ...EffectiveCutoff) BookLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, subject Subject) (*ledger.Book, []ledger.Execution, error) {
		var close time.Time
		switch len(cutoff) {
		case 0:
			if err := subject.Validate(); err != nil {
				return nil, nil, err
			}
			// BusinessDay names a UTC calendar date. Its close includes that
			// entire date and excludes the following midnight; no venue-local
			// close is inferred.
			close = BusinessDay(subject.BusinessDate).AddDate(0, 0, 1).Add(-time.Nanosecond)
		case 1:
			close = cutoff[0].AsOf.UTC()
			if close.IsZero() {
				return nil, nil, errors.New("custody: explicit effective cutoff is zero")
			}
			// Validate the same subject identity against the explicit read
			// instant. This does not invent a stored statement business date.
			identity := subject
			identity.BusinessDate = close
			if err := identity.Validate(); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, errors.New("custody: multiple effective cutoffs")
		}
		// Both modes are restated effective views of the recorded journal,
		// with no historical knowledge-time bound.
		if !scope.Declared(subject.PortfolioID) {
			// ONE CUSTODIAN: the scope is derived from the journal rather than
			// declared. See ledger.MaterializeAttributed for why that is scoped by
			// construction and why no declaration could add to it.
			basis, err := ledger.MaterializeAttributed(ctx, store, subject.PortfolioID, close)
			if err != nil {
				return nil, nil, err
			}
			accounts, residue := basis.Accounts, basis.Residue
			if len(accounts) == 0 && !residue.Empty() {
				// THE MIRROR-IMAGE DEFECT, REFUSED RATHER THAN SERVED. Every entry
				// in this journal declares that it settled against NO exchange
				// account, so the derived basis folds to NOTHING and every holding
				// the custodian reports comes back as MISSING_IN_IBOR — the whole
				// book as breaks, in the other direction from #1073's cash break.
				// NewBookScope already refuses an operator who declares an empty
				// account set, in these words and for this reason; a basis derived
				// to the same emptiness must not be quieter than one declared.
				return nil, nil, fmt.Errorf("custody: portfolio %s holds value (%s) and NO journal entry names "+
					"an exchange account, so the book slice compared against %s's statement would fold to "+
					"nothing and every position the custodian holds would break as MISSING_IN_IBOR. The "+
					"producers stamp venue_account_id and never default it: set it on the fills and cash "+
					"movements that settled at this custodian",
					subject.PortfolioID, residue.Describe(), subject.CustodianID)
			}
			stateResidue(logger, subject, BasisDerived, residue)
			stateUnreferenced(logger, subject, BasisDerived, basis)
			return basis.Book, basis.Executions, nil
		}
		accounts, claimed := scope.For(subject.PortfolioID, subject.CustodianID)
		if len(accounts) == 0 {
			// NewBookScope refuses this configuration, so reaching it means the
			// scope was built by some other path. Fail rather than fall back to
			// the whole book, which is the exact wrong answer #1006 is about.
			return nil, nil, fmt.Errorf("custody: no exchange accounts declared for %s:%s, so there is no "+
				"book slice to compare its statement against", subject.PortfolioID, subject.CustodianID)
		}
		basis, err := ledger.MaterializeForAccounts(ctx, store, subject.PortfolioID, accounts, claimed, close)
		if err != nil {
			return nil, nil, err
		}
		if len(basis.Unmapped) > 0 {
			// A PARTIAL BOOK MUST NOT RECONCILE. accounting.proto says it on the
			// statement side of this same comparison: "a partial level read as
			// complete manufactures a break for every position it omitted, which
			// is worse than no reconciliation at all because it buries the real
			// breaks in noise." An account nobody claimed is that, on the book
			// side — so the run FAILS and names the account.
			return nil, nil, fmt.Errorf("custody: portfolio %s holds value in exchange account(s) %s, which no "+
				"custodian in ACCOUNTING_CUSTODY_ACCOUNTS claims. Reconciling %s without them would compare "+
				"a book missing those holdings and break every one of them; add each account to the custodian "+
				"that holds it", subject.PortfolioID, strings.Join(basis.Unmapped, ", "), subject.CustodianID)
		}
		stateResidue(logger, subject, BasisDeclared, basis.Residue)
		stateUnreferenced(logger, subject, BasisDeclared, basis)
		return basis.Book, basis.Executions, nil
	}
}

// stateUnreferenced names the trades in the comparison basis that carry NO
// external reference, and what that costs (#1049).
//
// A TRANSACTION PASS THAT SHRINKS REPORTS FEWER BREAKS, which reads exactly like
// a book coming into agreement. An entry with no SourceRef cannot be matched
// against a custodian trade line, so it is in the netted pass and in no
// transaction pass — and a producer that stopped stamping the reference would
// silently move executions out of the finer control into the coarser one with
// every signal staying green. WARN for stateResidue's reason: it is not an error,
// the netted pass still covers those entries, and what is wrong is not saying so.
func stateUnreferenced(logger *slog.Logger, subject Subject, basisKind string, basis ledger.CustodyBasis) {
	if basis.Unreferenced == 0 {
		return
	}
	logger.Warn("accounting: custody reconciliation holds trades with NO EXTERNAL REFERENCE — they are in "+
		"the netted position and cash comparison and in NO transaction comparison, so no custodian trade "+
		"line can ever be matched against them (#1049)",
		"portfolio", subject.PortfolioID, "custodian", subject.CustodianID, "basis", basisKind,
		"unreferenced_trades", basis.Unreferenced, "referenced_trades", len(basis.Executions),
		"consequence", "for these entries the control is back at the netted grain: a wrongly-booked "+
			"execution and a missing one on the same instrument are indistinguishable. ledger.FromFill "+
			"stamps SourceRef from the venue fill id; a trade without one did not come from a fill")
}

// The two ways a portfolio's comparison basis is scoped, named here because the
// residue log and the composition root's posture gauge must use the SAME words —
// an operator correlating a per-run WARN with kanz_accounting_custody_comparison_basis
// is reading one fact through two surfaces.
const (
	BasisDeclared = "declared"
	BasisDerived  = "derived"
)

// stateResidue names the entries the comparison basis excluded, and what that
// means, whenever there are any.
//
// IT IS A WARN AND NOT AN INFO. "Nothing reconciles this money" is the sentence an
// operator needs to have read before they treat a clean run as evidence the book
// of record agrees with the world: the run they are looking at compared a SUBSET
// of the portfolio, and Info is where that would be filtered out. It is not an
// error either — the exclusion is correct, and a fund bank account is a legitimate
// place for cash to be. What is wrong is not saying so.
func stateResidue(logger *slog.Logger, subject Subject, basis string, residue ledger.UnattributedResidue) {
	if residue.Empty() {
		return
	}
	logger.Warn("accounting: custody reconciliation EXCLUDED entries that settled against no exchange "+
		"account — no custodian statement can report them, so they are in no comparison basis and "+
		"NOTHING RECONCILES THEM (#1073)",
		"portfolio", subject.PortfolioID, "custodian", subject.CustodianID, "basis", basis,
		"entries", residue.Entries, "residue", residue.Describe(),
		"consequence", "the run's verdict is about the holdings attributable to an exchange account; "+
			"this balance is real in the book of record and is confirmed by nobody")
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
	relay     *outbox.Relay
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
	if store.Outbox() == nil {
		return nil, errors.New("custody: missing completion outbox")
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
	var relay *outbox.Relay
	if publisher != nil {
		var err error
		relay, err = outbox.NewRelay(store.Outbox(), publisher, logger)
		if err != nil {
			return nil, err
		}
	}
	return &Reconciler{
		store: store, book: book,
		relay: relay, tolerance: tolerance, metrics: metrics, logger: logger, now: now,
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
		return r.recordFailure(ctx, subject, now, fmt.Errorf("load statement: %w", err))
	}

	// BOTH GRAINS OUT OF ONE CALL, and the pair is bound here rather than
	// re-derived: the executions must be the ones behind THIS book, over the same
	// slice of the journal the custodian is scoped to.
	book, executions, err := r.book(ctx, subject)
	if err != nil {
		return r.recordFailure(ctx, subject, now, fmt.Errorf("materialize book: %w", err))
	}

	detected, leg, err := recon.Reconcile(book, executions, recon.Statement{
		PortfolioID:  subject.PortfolioID,
		Positions:    stmt.Positions,
		Cash:         stmt.Cash,
		BusinessDate: subject.BusinessDate,
		Transactions: stmt.Transactions,
		Grain:        stmt.Grain,
	}, r.tolerance)
	if err != nil {
		return r.recordFailure(ctx, subject, now, fmt.Errorf("compare executions: %w", err))
	}
	r.stateLeg(subject, stmt, leg, len(executions))

	outcome := OutcomeClean
	if len(detected) > 0 {
		outcome = OutcomeBreaks
	}
	// THE RUN CARRIES THE STORED BREAKS, NOT THE FRESHLY DETECTED ONES. The
	// stored copies hold the lifecycle — the assignee, the explanation and the
	// FirstSeenAt an operator has been working against — and publishing the
	// freshly built ones would announce every break as OPEN and one run old, so
	// any consumer of the FACT would see an aging problem reset to zero daily.
	run, err := r.record(ctx, Run{
		RunID:       runID(subject, now),
		Subject:     subject,
		Outcome:     outcome,
		StatementID: stmt.StatementID,
		Breaks:      FromRecon(subject, detected, now),
		Tolerance:   r.tolerance,
		CompletedAt: now,
	})
	if err != nil && !errors.Is(err, ErrPublicationPending) {
		return r.recordFailure(ctx, subject, now, fmt.Errorf("commit reconciliation: %w", err))
	}
	return run, err
}

// stateLeg says, once per run, whether the TRANSACTION pass actually happened
// (#1049).
//
// "NO EXECUTION BREAKS" AND "NO EXECUTION COMPARISON" ARE THE SAME CLEAN RUN
// otherwise, and that is the netted-grain blindness this issue is about arriving
// one level up: a run against a balances-only feed produces exactly the output of
// a run against a full feed that matched every fill. The gauge
// kanz_accounting_custody_statement_grain_produced states the BUILD's posture at
// startup; this states what the statement in front of this run actually carried.
func (r *Reconciler) stateLeg(subject Subject, stmt Statement, leg recon.LegStatus, executions int) {
	if leg.Ran() {
		r.logger.Info("accounting: custody reconciliation compared executions",
			"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
			"statement", stmt.StatementID, "statement_transactions", len(stmt.Transactions),
			"book_executions", executions)
		return
	}
	r.logger.Warn("accounting: custody reconciliation compared NETTED BALANCES ONLY — no execution was "+
		"matched against a custodian trade line, so a wrongly-booked execution and a missing one on the "+
		"same instrument are indistinguishable in this run (#1049)",
		"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
		"statement", stmt.StatementID, "reason", leg.String(), "grain", stmt.Grain.String(),
		"book_executions", executions,
		"consequence", "a CLEAN verdict here is a statement about totals. 'A fill never reached the "+
			"book' remains inferable from a position difference rather than observed on a reference")
}

// recordFailure records and publishes a FAILED run, and returns the underlying
// error alongside it.
//
// A FAILURE IS RECORDED RATHER THAN SWALLOWED because the alternative is that a
// reconciliation which could not run looks exactly like one that never came due.
// The estate must be able to tell "the book would not materialize for three days"
// from "nothing was scheduled".
func (r *Reconciler) recordFailure(ctx context.Context, subject Subject, now time.Time, reason error) (Run, error) {
	run, err := r.record(ctx, Run{
		RunID:         runID(subject, now),
		Subject:       subject,
		Outcome:       OutcomeFailed,
		Tolerance:     r.tolerance,
		CompletedAt:   now,
		FailureReason: reason.Error(),
	})
	if err != nil {
		return run, errors.Join(reason, err)
	}
	return run, fmt.Errorf("custody: %w", reason)
}

// record persists a run, publishes it, and moves the metrics.
//
// Run, lifecycle and completion record commit before publishing. A broker error
// leaves a committed business result and a queued FACT, never a partial rollback.
// Freshness advances only after a successful inline flush. Background relay
// recovery can leave this timestamp conservative until the next successful run;
// a queued completion must never imply that the estate already received it.
func (r *Reconciler) record(ctx context.Context, run Run) (Run, error) {
	stored, err := r.store.CommitRun(ctx, run)
	if err != nil {
		return run, fmt.Errorf("custody: commit run: %w", err)
	}
	run = stored
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

// publish drains after commit, including any earlier queued completions for the
// pair. The service-owned accounting relay provides recovery after a restart.
func (r *Reconciler) publish(ctx context.Context, run Run) error {
	if r.relay == nil {
		return ErrPublicationPending
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.relay.Flush(ctx, pairKey(run.Subject.PortfolioID, run.Subject.CustodianID)); err != nil {
		return fmt.Errorf("%w: %w", ErrPublicationPending, err)
	}
	return nil
}

// runToProto renders a run onto the wire.
//
// A FIGURE THAT WILL NOT FIT REFUSES THE PUBLISH rather than going out scaled or
// wrapped (#94). A break whose difference wrapped would be announced as a
// smaller disagreement than it is, and a reconciliation that understates a
// difference is worse than one that never ran — it is a control actively
// reporting the wrong answer.
func runToProto(run Run) (*accountingpb.ReconciliationRun, error) {
	tolerance, ok := exactProto(orZeroRat(run.Tolerance))
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
		ibor, iok := exactProto(b.IBOR)
		cust, cok := exactProto(b.Custodian)
		diff, dok := exactProto(b.Diff)
		// Reference-only discrepancies can lack the reporting side's quantity.
		// Omit that unavailable amount and difference, preserving the reference
		// and known opposite-side zero. Legacy or unrepresentable figures still
		// refuse publication rather than acquiring an exact-looking value.
		iborAbsent := b.ValuesVerified && b.Kind == recon.BreakExecutionMissingAtCustodian && b.IBOR == nil
		custAbsent := b.ValuesVerified && b.Kind == recon.BreakExecutionMissingInIBOR && b.Custodian == nil
		diffAbsent := (iborAbsent || custAbsent) && b.Diff == nil
		if (!iok && !iborAbsent) || (!cok && !custAbsent) || (!dok && !diffAbsent) {
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
