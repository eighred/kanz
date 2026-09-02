package custody

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"
)

// Scheduler runs reconciliations on a timer, per (portfolio, custodian) pair.
//
// THE SCHEDULE IS WHAT MAKES ABSENCE DETECTABLE, and it is the half the pre-#962
// design was missing entirely. Reconciliation triggered only by an arriving
// statement can never report that a statement did NOT arrive — the code path
// simply is not entered — so a custodian that quietly stopped sending produced an
// estate indistinguishable from one reconciling cleanly every day. A scheduled
// run comes due whether or not anything arrived, finds no statement, and emits
// OutcomeNoStatement. That is the entire reason this type exists.
type Scheduler struct {
	reconciler *Reconciler
	pairs      []Subject
	interval   time.Duration
	// businessDate maps the run instant to the business date to reconcile.
	businessDate func(time.Time) time.Time
	logger       *slog.Logger
	now          func() time.Time
}

// SchedulerConfig is the deployment's statement of what reconciles and how often.
type SchedulerConfig struct {
	// Pairs are the (portfolio, custodian) pairs to reconcile. BusinessDate on
	// each is ignored — the scheduler derives it per tick.
	Pairs []Subject
	// Interval is how often each pair is reconciled.
	Interval time.Duration
	// LagDays is how many days back from the run instant the reconciled business
	// date sits.
	//
	// IT DEFAULTS TO 1, NOT 0, AND THAT IS A DOMAIN FACT RATHER THAN A
	// CONVENIENCE. A custodian states holdings as of the CLOSE of a business day
	// and transmits them afterwards, so reconciling "today" at any point during
	// today compares a book that is still moving against a statement that cannot
	// exist yet. Every such run would conclude NO_STATEMENT, the alert would fire
	// permanently, and an alert nobody can ever clear trains its readers to
	// ignore the file it lives in.
	LagDays int
}

// NewScheduler wires a Scheduler.
//
// AN EMPTY PAIR LIST IS REFUSED. A scheduler with nothing to do starts happily,
// logs nothing of consequence and reconciles no book — which is the pre-#962
// state reproduced exactly, with the added cost of looking configured. The
// composition root must either name the pairs or not construct one.
func NewScheduler(reconciler *Reconciler, cfg SchedulerConfig, logger *slog.Logger, now func() time.Time) (*Scheduler, error) {
	if reconciler == nil {
		return nil, errors.New("custody: nil reconciler")
	}
	if len(cfg.Pairs) == 0 {
		return nil, errors.New("custody: scheduler configured with no (portfolio, custodian) pairs — it would " +
			"reconcile nothing while appearing configured (#962)")
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("custody: scheduler interval must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	lag := cfg.LagDays
	if lag == 0 {
		lag = 1
	}
	pairs := make([]Subject, 0, len(cfg.Pairs))
	for _, p := range cfg.Pairs {
		if p.PortfolioID == "" || p.CustodianID == "" {
			return nil, errors.New("custody: scheduler pair needs both a portfolio and a custodian")
		}
		pairs = append(pairs, Subject{PortfolioID: p.PortfolioID, CustodianID: p.CustodianID})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].PortfolioID != pairs[j].PortfolioID {
			return pairs[i].PortfolioID < pairs[j].PortfolioID
		}
		return pairs[i].CustodianID < pairs[j].CustodianID
	})
	return &Scheduler{
		reconciler:   reconciler,
		pairs:        pairs,
		interval:     cfg.Interval,
		businessDate: func(t time.Time) time.Time { return BusinessDay(t.AddDate(0, 0, -lag)) },
		logger:       logger,
		now:          now,
	}, nil
}

// Pairs returns the configured pairs. The composition root uses it to seed the
// metric series before the first tick.
func (s *Scheduler) Pairs() []Subject { return append([]Subject(nil), s.pairs...) }

// Run reconciles every pair once, immediately, and then on each interval until
// ctx is done.
//
// IT RUNS ONCE BEFORE THE FIRST TICK because otherwise a service restarting more
// often than the interval never reconciles at all, and the gap is invisible: each
// process looks healthy for its whole short life. This is the same reason the
// snapshotter primes before its ticker.
func (s *Scheduler) Run(ctx context.Context) error {
	s.Tick(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick reconciles every configured pair once.
//
// ONE PAIR'S FAILURE MUST NOT SKIP THE REST. A book that will not materialize is
// a per-portfolio problem, and letting it abort the sweep would silently stop
// reconciling every OTHER portfolio in the estate — turning one portfolio's
// defect into an estate-wide loss of the control, which is exactly the
// blast-radius mistake a fail-closed design is supposed to avoid.
func (s *Scheduler) Tick(ctx context.Context) {
	date := s.businessDate(s.now())
	for _, pair := range s.pairs {
		if ctx.Err() != nil {
			return
		}
		subject := Subject{PortfolioID: pair.PortfolioID, CustodianID: pair.CustodianID, BusinessDate: date}
		run, err := s.reconciler.Reconcile(ctx, subject)
		if err != nil {
			s.logger.Error("accounting: custody reconciliation failed",
				"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
				"business_date", date.Format("2006-01-02"), "error", err)
			continue
		}
		switch run.Outcome {
		case OutcomeNoStatement:
			// WARN AND NOT ERROR, because it is not this platform's fault and it
			// is already carried by a FACT, a counter and an alert. It is logged
			// at all so an operator reading the service's own output can see the
			// gap without going to Prometheus first.
			s.logger.Warn("accounting: custody reconciliation found no statement — the custodian has sent "+
				"nothing for this business date, so the book is unverified against it",
				"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
				"business_date", date.Format("2006-01-02"))
		case OutcomeBreaks:
			s.logger.Warn("accounting: custody reconciliation found breaks",
				"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
				"business_date", date.Format("2006-01-02"), "breaks", len(run.Breaks))
		default:
			s.logger.Info("accounting: custody reconciliation complete",
				"portfolio", subject.PortfolioID, "custodian", subject.CustodianID,
				"business_date", date.Format("2006-01-02"), "outcome", run.Outcome.String())
		}
	}
}
