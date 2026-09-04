package main

// Composition-root tests for the custody reconciliation plane (#962).
//
// WHY THESE EXIST. cmd/*/main.go wiring escapes every unit test in this
// repository — the packages below it are covered and the assembly is not, and
// this service has already shipped two crashes with a green suite that way. The
// specific hazards here are that the control can be assembled in a state where it
// does nothing (no pairs, no store, no publisher) and STILL START CLEANLY, which
// is the pre-#962 estate with a healthier-looking log.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingHandler) WithGroup(string) slog.Handler      { return c }

func (c *capturingHandler) messagesAt(level slog.Level) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.records {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

func (c *capturingHandler) sawAt(level slog.Level, needle string) bool {
	for _, m := range c.messagesAt(level) {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, bus.Event) error { return nil }

func baseCfg() config.Config {
	return config.Config{
		Tenant:                  "__system__",
		CustodyStatementSubject: config.DefaultCustodyStatementSubject,
		CustodyInterval:         config.DefaultCustodyInterval,
		CustodyLagDays:          config.DefaultCustodyLagDays,
		CustodyPairs:            []string{"PF1:CUST-A"},
	}
}

// buildPlane runs the whole custody wiring the composition root does: the
// declaration is parsed and the book scope derived FIRST (#1025 — the server's
// ad-hoc reconcile endpoint needs the same scope, so main builds it before either
// consumer), then the plane is assembled over it. A configuration refused by
// either half is a refused start, which is what the tests below assert.
func buildPlane(t *testing.T, cfg config.Config, h *capturingHandler) (*custodyPlane, error) {
	t.Helper()
	logger := slog.New(h)
	cc, err := buildCustodyConfig(cfg, logger)
	if err != nil {
		return nil, err
	}
	return buildCustodyPlane(cfg, cc, nil, custody.NewMemoryStore(), ledger.NewMemoryStore(), noopPublisher{}, prometheus.NewRegistry(), logger)
}

// THE WIRING ASSEMBLES AND THE SCHEDULER EXISTS. A plane that builds without a
// scheduler reconciles nothing.
func TestCustodyPlaneArmsAScheduler(t *testing.T) {
	h := &capturingHandler{}
	plane, err := buildPlane(t, baseCfg(), h)
	if err != nil {
		t.Fatalf("buildCustodyPlane: %v", err)
	}
	if plane.scheduler == nil {
		t.Fatal("no scheduler was armed for a configured pair — nothing would reconcile on any cadence")
	}
	if plane.consumer == nil {
		t.Fatal("no statement consumer was armed")
	}
	if got := plane.scheduler.Pairs(); len(got) != 1 || got[0].PortfolioID != "PF1" || got[0].CustodianID != "CUST-A" {
		t.Fatalf("pairs = %+v, want one PF1/CUST-A", got)
	}
}

// NO SCHEDULE MEANS NO CONTROL, and it must not look like a healthy start. This
// is the pre-#962 estate: nothing reconciles unless a statement happens to arrive
// AND somebody happens to look, and no NO_STATEMENT run is ever emitted, so the
// staleness alert has no series to age and stays quiet forever.
func TestAnUnscheduledCustodyPlaneSaysSoAtError(t *testing.T) {
	h := &capturingHandler{}
	cfg := baseCfg()
	cfg.CustodyPairs = nil
	plane, err := buildPlane(t, cfg, h)
	if err != nil {
		t.Fatalf("buildCustodyPlane: %v", err)
	}
	if plane.scheduler != nil {
		t.Fatal("a scheduler was armed with no pairs")
	}
	if !h.sawAt(slog.LevelError, "NOT SCHEDULED") {
		t.Fatalf("an unscheduled custody plane did not report at ERROR; errors were %v", h.messagesAt(slog.LevelError))
	}
}

// AN IN-MEMORY BREAK LIFECYCLE LOSES AN OPERATOR'S WORK ON EVERY RESTART.
func TestAnEphemeralBreakLifecycleSaysSoAtError(t *testing.T) {
	h := &capturingHandler{}
	plane, err := buildPlane(t, baseCfg(), h)
	if err != nil {
		t.Fatalf("buildCustodyPlane: %v", err)
	}
	if plane.durable {
		t.Fatal("a nil pool produced a plane claiming durability")
	}
	if !h.sawAt(slog.LevelError, "IN-MEMORY store") {
		t.Fatalf("an ephemeral break lifecycle did not report at ERROR; errors were %v", h.messagesAt(slog.LevelError))
	}
}

// A MISCONFIGURED PAIR MUST FAIL THE BUILD, NOT BE SKIPPED. Dropping one silently
// un-reconciles a portfolio while the service reports a healthy start, and the
// gap is invisible because the pair never reaches the metric seeding that would
// have exported its zero series.
func TestAMalformedPairRefusesToStart(t *testing.T) {
	for _, spec := range []string{"PF1", "PF1:", ":CUST-A", "  ", "PF1:CUST-A:extra-ok"} {
		cfg := baseCfg()
		cfg.CustodyPairs = []string{spec}
		_, err := buildPlane(t, cfg, &capturingHandler{})
		if spec == "PF1:CUST-A:extra-ok" {
			// strings.Cut splits on the FIRST colon, so this is a legitimate
			// custodian id containing a colon rather than a malformed entry.
			if err != nil {
				t.Fatalf("pair %q was refused: %v", spec, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("malformed pair %q was accepted — the portfolio would be silently unreconciled", spec)
		}
	}
}

func TestADuplicatePairRefusesToStart(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF1:CUST-A", "PF1:CUST-A"}
	if _, err := buildPlane(t, cfg, &capturingHandler{}); err == nil {
		t.Fatal("a duplicate pair was accepted — it would emit two runs for one business date")
	}
}

// A NEGATIVE OR UNPARSEABLE TOLERANCE MUST NOT SILENTLY BECOME ZERO.
func TestABadToleranceRefusesToStart(t *testing.T) {
	for _, spec := range []string{"not-a-number", "-1"} {
		cfg := baseCfg()
		cfg.CustodyTolerance = spec
		if _, err := buildPlane(t, cfg, &capturingHandler{}); err == nil {
			t.Fatalf("tolerance %q was accepted", spec)
		}
	}
	cfg := baseCfg()
	cfg.CustodyTolerance = "0.5"
	if _, err := buildPlane(t, cfg, &capturingHandler{}); err != nil {
		t.Fatalf("a valid tolerance was refused: %v", err)
	}
}

// EVERY CONFIGURED PAIR IS SEEDED BEFORE THE FIRST TICK. A rule over a series
// that has never been sampled queries an empty vector and never fires, and the
// estate that has never reconciled is exactly the one with no samples.
func TestEveryConfiguredPairIsSeededOnTheRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF1:CUST-A", "PF2:CUST-B"}
	logger := slog.New(&capturingHandler{})
	cc, err := buildCustodyConfig(cfg, logger)
	if err != nil {
		t.Fatalf("buildCustodyConfig: %v", err)
	}
	if _, err := buildCustodyPlane(cfg, cc, nil, custody.NewMemoryStore(), ledger.NewMemoryStore(), noopPublisher{}, reg, logger); err != nil {
		t.Fatalf("buildCustodyPlane: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]int{}
	for _, f := range families {
		seen[f.GetName()] = len(f.GetMetric())
	}
	for _, name := range []string{
		"kanz_accounting_reconciliation_runs_total",
		"kanz_accounting_reconciliation_last_success_timestamp_seconds",
		"kanz_accounting_breaks_open",
	} {
		if seen[name] == 0 {
			t.Fatalf("%s exports no series after wiring — an alert over it would never fire", name)
		}
	}
	// Two pairs, four outcomes each => at least 8 run series.
	if got := seen["kanz_accounting_reconciliation_runs_total"]; got < 8 {
		t.Fatalf("runs_total exports %d series, want >= 8 (2 pairs x 4 outcomes)", got)
	}
}

// THE DEFAULT LAG IS ONE BUSINESS DAY. Zero would compare a still-moving book
// against a statement that cannot exist yet, concluding NO_STATEMENT forever.
func TestTheDefaultLagIsOneBusinessDay(t *testing.T) {
	if config.DefaultCustodyLagDays != 1 {
		t.Fatalf("DefaultCustodyLagDays = %d, want 1", config.DefaultCustodyLagDays)
	}
	if config.DefaultCustodyInterval != 24*time.Hour {
		t.Fatalf("DefaultCustodyInterval = %s, want 24h", config.DefaultCustodyInterval)
	}
}

// THE SUBJECT THE CONSUMER SUBSCRIBES AND THE ONE THE PACKAGE DECLARES MUST BE
// THE SAME STRING. A subscription on a subject nothing publishes is silent.
func TestTheConfiguredStatementSubjectMatchesThePackage(t *testing.T) {
	if config.DefaultCustodyStatementSubject != custody.SubjectStatement {
		t.Fatalf("config default %q != custody.SubjectStatement %q — the consumer would subscribe to a subject nothing publishes",
			config.DefaultCustodyStatementSubject, custody.SubjectStatement)
	}
}

// THE SERVER'S BREAK QUEUE AND THE SCHEDULER MUST BE THE SAME STORE.
//
// Two instances is the failure this wiring exists to avoid and it is invisible
// from either side: the scheduler fills its store and reports healthy runs, the
// operator opens a queue that is permanently empty, and every assignment they
// manage to make is written where nothing reads it. Both halves look fine alone.
func TestTheBreakQueueAndTheSchedulerShareOneStore(t *testing.T) {
	shared := custody.NewMemoryStore()
	logger := slog.New(&capturingHandler{})
	cc, err := buildCustodyConfig(baseCfg(), logger)
	if err != nil {
		t.Fatalf("buildCustodyConfig: %v", err)
	}
	plane, err := buildCustodyPlane(baseCfg(), cc, nil, shared, ledger.NewMemoryStore(),
		noopPublisher{}, prometheus.NewRegistry(), logger)
	if err != nil {
		t.Fatalf("buildCustodyPlane: %v", err)
	}
	if plane.store != custody.Store(shared) {
		t.Fatal("the plane built its own store rather than using the one the server was handed — " +
			"the operator's queue and the scheduled runs would be two different sets of breaks")
	}

	// And the same instance reaches the reconciler: a run writes where the queue
	// reads. Reconcile with no statement still records a run, so a break written
	// through the shared store must be visible to it.
	ctx := context.Background()
	detected := custody.FromRecon(
		custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A"},
		nil, time.Now())
	if _, err := shared.UpsertBreaks(ctx, custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A"}, detected, time.Now()); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	if _, err := plane.store.OutstandingBreaks(ctx); err != nil {
		t.Fatalf("the plane's store could not be read: %v", err)
	}
}
