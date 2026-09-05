package main

import (
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// custodyPlane is everything #962 adds, assembled once so the composition root
// wires one thing rather than five.
//
// IT IS BUILT EVEN WHEN IT IS NOT ARMED, and the difference is reported out loud.
// "no custody reconciliation configured" and "custody reconciliation running
// cleanly" must never look the same from outside the process — that is the whole
// premise of the issue, applied to the wiring that decides whether the control
// exists at all.
type custodyPlane struct {
	store      custody.Store
	reconciler *custody.Reconciler
	scheduler  *custody.Scheduler
	consumer   *custody.StatementConsumer
	metrics    *custody.Metrics
	// durable is false when the break lifecycle lives in memory, which loses an
	// operator's assignments and every break's age on restart.
	durable bool
}

// parseCustodyPairs parses comma-separated "portfolio:custodian" pairs.
//
// A MALFORMED PAIR IS AN ERROR AND NOT A SKIP. Silently dropping one would
// un-reconcile a portfolio while the service reported a healthy start, and the
// gap would be invisible precisely because the pair never reaches the metric
// seeding that would have exported its zero series.
//
// WHITESPACE INSIDE AN ENTRY IS REFUSED, AND THAT IS THE SAME RULE, NOT A NEW ONE
// (#1029). strings.Cut splits on the FIRST colon, so "PF1:CUST-A PF2:CUST-B" —
// what an operator writes after declaring ACCOUNTING_CUSTODY_ACCOUNTS, whose
// entries ARE whitespace-separated — was accepted as ONE pair whose custodian id
// is "CUST-A PF2:CUST-B". The pod started clean, PF2 was never reconciled at all,
// and PF1's pair reconciled forever as NO_STATEMENT against a custodian no
// statement will ever name. That is a malformed pair swallowed rather than
// refused: the shape this function's own doc says must be an error.
func parseCustodyPairs(specs []string) ([]custody.Subject, error) {
	out := make([]custody.Subject, 0, len(specs))
	seen := map[string]struct{}{}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if strings.ContainsFunc(spec, unicode.IsSpace) {
			return nil, fmt.Errorf("accounting: ACCOUNTING_CUSTODY_PAIRS entry %q contains whitespace, "+
				"which is not a separator here — PAIRS ARE SEPARATED BY COMMAS. Accepted, it would be "+
				"ONE pair whose custodian id is the rest of the line, reconciling forever as "+
				"NO_STATEMENT while every portfolio after the space went unreconciled with nothing "+
				"said. Write \"PF1:CUST-A,PF1:CUST-B\". (ACCOUNTING_CUSTODY_ACCOUNTS separates ITS "+
				"entries by whitespace instead, because a comma there separates one custodian's "+
				"accounts.)", spec)
		}
		portfolio, custodian, ok := strings.Cut(spec, ":")
		if !ok || portfolio == "" || custodian == "" {
			return nil, fmt.Errorf("accounting: ACCOUNTING_CUSTODY_PAIRS entry %q is not \"portfolio:custodian\"", spec)
		}
		key := portfolio + "|" + custodian
		if _, dup := seen[key]; dup {
			// A duplicate would reconcile the pair twice per tick and emit two
			// runs for one business date — two rows of evidence about one
			// comparison, which is a history nobody can read.
			return nil, fmt.Errorf("accounting: ACCOUNTING_CUSTODY_PAIRS names %s twice", key)
		}
		seen[key] = struct{}{}
		out = append(out, custody.Subject{PortfolioID: portfolio, CustodianID: custodian})
	}
	return out, nil
}

// custodyTolerance parses the configured tolerance. Empty means an exact match is
// required, which is the safe default: a tolerance nobody set must not silently
// become one somebody would have argued about.
func custodyTolerance(spec string) (*big.Rat, error) {
	if strings.TrimSpace(spec) == "" {
		return new(big.Rat), nil
	}
	r, err := dec.ParseRat(spec)
	if err != nil {
		return nil, fmt.Errorf("accounting: ACCOUNTING_CUSTODY_TOLERANCE %q: %w", spec, err)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("accounting: ACCOUNTING_CUSTODY_TOLERANCE %q is negative", spec)
	}
	return r, nil
}

// custodyConfig is the (portfolio, custodian) work list and the book-side scope
// derived from it — the two values that must be THE SAME for the scheduled runs
// and for the ad-hoc reconcile endpoint on the server (#1025).
//
// IT IS BUILT ONCE, BEFORE EITHER CONSUMER EXISTS, and for the reason
// newCustodyStore is shared: two scopes built from the same environment agree
// today and diverge the first time one of them is rebuilt from something else,
// and a disagreement about which accounts a custodian holds is invisible until a
// real break is buried in the fabricated ones. Building it early also means a bad
// declaration refuses the START rather than the first tick — or, worse for the
// endpoint, the first investigation.
type custodyConfig struct {
	pairs []custody.Subject
	scope *custody.BookScope
}

// buildCustodyConfig parses the custody declaration and derives the book scope.
//
// THE REFUSALS ARE NewBookScope's, AND THEY HAPPEN HERE SO THEY HAPPEN AT BOOT
// (#1006). A portfolio custodied in two places with nothing saying which accounts
// sit where would compare the WHOLE book against each custodian in turn, and
// report every position held at the other as MISSING_AT_CUSTODIAN. A control that
// will assert a wrong answer must not reach a running pod.
func buildCustodyConfig(cfg config.Config, logger *slog.Logger) (custodyConfig, error) {
	pairs, err := parseCustodyPairs(cfg.CustodyPairs)
	if err != nil {
		return custodyConfig{}, err
	}
	accountDecl, err := custody.ParseCustodyAccounts(cfg.CustodyAccounts)
	if err != nil {
		return custodyConfig{}, err
	}
	scope, err := custody.NewBookScope(pairs, accountDecl)
	if err != nil {
		return custodyConfig{}, err
	}
	for _, p := range pairs {
		if scope.Scoped(p.PortfolioID) {
			continue
		}
		// Said out loud because "one custodian, whole book" is CORRECT and
		// "several custodians, whole book" is the defect — and from outside the
		// process the two look identical. NewBookScope has already refused the
		// second, so this line is the positive record of the first.
		logger.Info("accounting: custody reconciliation compares the whole portfolio book",
			"portfolio", p.PortfolioID, "custodian", p.CustodianID,
			"reason", "one custodian configured for this portfolio")
	}
	return custodyConfig{pairs: pairs, scope: scope}, nil
}

// buildCustodyPlane assembles the custody reconciliation control.
//
// store is built by newCustodyStore so the SAME store instance backs both the
// scheduler and the operator break queue on the server — two stores would give an
// operator a queue that the runs never write to, which is worse than no queue.
// cc is built by buildCustodyConfig for the same reason, one layer over: the
// server's ad-hoc reconcile endpoint scopes its book by that same declaration.
// pool is nil for an in-memory deployment. publisher is nil when there is no
// broker; both cases are reported rather than inferred later from an absence of
// runs.
func buildCustodyPlane(
	cfg config.Config,
	cc custodyConfig,
	pool *pgxpool.Pool,
	store custody.Store,
	ledgerStore ledger.Store,
	publisher custody.Publisher,
	reg prometheus.Registerer,
	logger *slog.Logger,
) (*custodyPlane, error) {
	pairs := cc.pairs
	tolerance, err := custodyTolerance(cfg.CustodyTolerance)
	if err != nil {
		return nil, err
	}

	plane := &custodyPlane{metrics: custody.NewMetrics(reg), store: store, durable: pool != nil}
	if pool == nil {
		// THE BREAK LIFECYCLE IS AN OPERATOR'S WORK AND SURVIVES NOTHING HERE. A
		// restart resets every assignment and explanation to OPEN and every
		// break's age to zero, which regenerates the undifferentiated daily list
		// this control exists to replace. Said at ERROR because a deployment that
		// reaches production on it is a defect, not a configuration choice.
		logger.Error("accounting: custody reconciliation is using an IN-MEMORY store — every break's " +
			"assignee, explanation and age is lost on restart, so the queue resets to an " +
			"undifferentiated list of OPEN breaks each time the pod moves (#962). Set " +
			"ACCOUNTING_DATABASE_URL for a durable lifecycle")
	}

	plane.reconciler, err = custody.NewReconciler(
		plane.store, custody.LedgerBookLoader(ledgerStore, cc.scope), publisher, tolerance, plane.metrics, logger, nil)
	if err != nil {
		return nil, err
	}

	plane.consumer, err = custody.NewStatementConsumer(plane.store, cfg.Tenant, logger)
	if err != nil {
		return nil, err
	}

	if len(pairs) == 0 {
		// NO SCHEDULE MEANS NO CONTROL, and this is the sentence that separates it
		// from a healthy start. Without a schedule nothing reconciles unless a
		// statement happens to arrive AND somebody happens to look — which is the
		// pre-#962 estate exactly, and it emits no NO_STATEMENT run, so the
		// staleness alert has no series to age and stays quiet forever.
		logger.Error("accounting: custody reconciliation is NOT SCHEDULED — ACCOUNTING_CUSTODY_PAIRS is " +
			"empty, so no (portfolio, custodian) is reconciled on any cadence and no " +
			"ReconciliationRun FACT will ever be emitted. The book of record is unverified " +
			"against any custodian and nothing will say so (#962)")
		return plane, nil
	}

	plane.scheduler, err = custody.NewScheduler(plane.reconciler, custody.SchedulerConfig{
		Pairs:    pairs,
		Interval: cfg.CustodyInterval,
		LagDays:  cfg.CustodyLagDays,
	}, logger, nil)
	if err != nil {
		return nil, err
	}

	// SEEDED BEFORE THE FIRST TICK. A counter exports nothing for a label it has
	// never incremented, so every alert over these series would query an empty
	// vector — and a rule over an empty vector never fires (#62 deleted ten rules
	// for exactly this). The estate that has never reconciled at all is the one
	// the alerting most needs to see, and it is precisely the one with no samples.
	for _, p := range pairs {
		plane.metrics.SeedPair(p.PortfolioID, p.CustodianID)
	}
	logger.Info("accounting: custody reconciliation scheduled",
		"pairs", len(pairs), "interval", cfg.CustodyInterval.String(),
		"lag_days", cfg.CustodyLagDays, "tolerance", dec.Str(tolerance),
		"statement_subject", cfg.CustodyStatementSubject, "run_subject", custody.SubjectRun,
		"durable", plane.durable)
	return plane, nil
}

// newCustodyStore builds the durable custody store when a pool is available, and
// the in-memory one otherwise.
//
// IT IS SEPARATE FROM buildCustodyPlane BECAUSE THE SERVER NEEDS IT TOO. The
// operator break queue and the scheduled runs must be the same store: a server
// handed its own instance would show an operator an empty queue while the
// scheduler filled another one, and every transition they made would be written
// where nothing reads it.
func newCustodyStore(pool *pgxpool.Pool) custody.Store {
	if pool != nil {
		return custody.NewPostgres(pool)
	}
	return custody.NewMemoryStore()
}
