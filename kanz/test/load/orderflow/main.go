// Command orderflow drives the ORDER ADMISSION path under sustained concurrency
// and reports the rate at which the first control degrades (#865).
//
// # What was measured before this existed, and what was not
//
// test/load's k6 scripts (smoke/baseline/soak over config.js readMix) drive
// exposure, measures and scenario — the READ path. test/load/ingest drives
// PositionState ticks into the risk-engine's recompute — the write path OF RISK
// STATE. Neither submits an order. So every latency and error-budget number this
// platform can state describes a query, and the controls that decide whether
// capital MOVES — the gateway's trade gate, the OMS's per-order claim, the
// Postgres admission gate, the pre-trade compliance/mandate/margin gate, the
// outbox relay — had no rate at which they are known to degrade. An admission
// control whose degradation point is unknown is one you discover during an
// incident.
//
// # What is saturated, and why that is a legitimate measurement
//
// The REAL front door: HTTP POST /v1/orders on the api-gateway, authenticated,
// with an Idempotency-Key, exactly as a client issues one. Nothing is skipped and
// nothing is weakened — the order crosses the gateway's auth, role and halt
// checks, is published as a COMMAND onto the tenant-routed subject, is delivered
// by a real broker to the OMS's durable consumer under the real delivery
// contract, takes the per-order claim, passes the pre-trade gate, commits through
// the admission gate with its ORDER_ACCEPTED FACT in the same transaction, and is
// routed to a venue.
//
// THE ONLY THING SIMULATED IS THE EXCHANGE, and the harness REFUSES TO RUN unless
// that is true (see preflight.go). Every control on CLAUDE.md's order path runs;
// what does not run is the venue adapter's exchange REST/WS hop, because a load
// test may not place real orders. That is the honest boundary of the number this
// produces: it is an ADMISSION capacity, not an execution capacity, and the
// report says so in as many words.
//
// # What is measured
//
// Latency alone would miss every failure mode #865 names, because none of them is
// an error the client sees — a gateway 202 means "the command was published",
// not "the order was admitted". So each stage reports:
//
//   - the OFFERED rate (open model: a fixed submissions/sec, independent of how
//     fast admission runs) and the rate actually achieved;
//   - ADMISSION LATENCY: submit → the order's own terminal FACT, read off the
//     bus. This is the number that degrades while HTTP stays green;
//   - the standing COMMAND BACKLOG, kanz_bus_pending_messages{group,subject=
//     order.order.submit} — the same series capacity-model.md already sizes KEDA
//     against, so a write-path capacity number and the autoscaling policy speak
//     about one quantity;
//   - EXACTLY-ONCE across submissions and FACTs: every accepted submission
//     produced exactly one terminal FACT, and no order produced two;
//   - CONTROL DELTAS: the counters in scrape.go that mean a control degraded or
//     that the run measured something other than what it claims.
//
// # The budget is derived, not invented
//
// The delivery contract this whole path is sized by is stated in pkg/bus/tuning.go
// and has never been measured:
//
//	MaxAckPending × worst-case per-message handling  <  AckWait
//
// The order-command durable runs on the work class: AckWait 60s, MaxAckPending 32,
// so worst-case handling must stay under 60s/32 = 1.875s. Past it the broker
// redelivers a command whose first copy is still running — a second concurrent
// dispatch of one order. The measured submit→FACT time is an UPPER BOUND on that
// handling (it also contains the gateway hop, two broker hops and this process's
// own delivery), so a p99 inside the budget proves the contract holds with room
// to spare, and a p99 outside it is a signal to look, not yet a violation.
//
// # Where it may run
//
//	go run ./test/load/orderflow
//
// Against a stack you stood up yourself. It is NOT in the per-PR CI gate and
// deliberately so — see test/load/README.md, "Why the write path is not a CI
// smoke". The one thing it does unconditionally is refuse: preflight.go establishes
// from the system under test's OWN metrics that no order can reach an exchange,
// and submits a single canary order to prove the path admits before any load
// starts.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/secret"
)

// config is the whole run, read from the environment.
//
// EVERY MALFORMED VALUE STOPS THE RUN (#692, the same stance test/load/ingest
// takes). A load generator that silently falls back to a default rate because
// RATE=1k did not parse produces a number somebody will quote, measured against a
// rate nobody set.
type config struct {
	baseURL     string // the api-gateway — the ONLY way this harness submits an order
	token       string // bearer; must carry the trade role AND the portfolio entitlement
	natsURL     string // the spine, for reading the FACTs admission produces
	metricsURL  string // the OMS's own /metrics — the ground truth every refusal turns on
	tenant      string
	portfolio   string
	instrument  string
	venue       string // empty ⇒ let the router choose (exercises the SOR default)
	group       string // the OMS consumer group, for the backlog signal
	stages      []int  // offered submissions/sec, one plateau each
	stageFor    time.Duration
	drainFor    time.Duration
	budget      time.Duration // admission p99 budget; derived, see loadConfig
	settleAfter time.Duration // pause between stages so a backlog can be seen to clear
}

// admissionBudget is the per-message handling ceiling the delivery contract is
// sized by, read from pkg/bus rather than restated here.
//
// bus.WorkAckWait is exported; the work class's MaxAckPending is not, so the
// divisor is written here — the ONE number in this file copied from another
// package. It is asserted against pkg/bus's own tuning in main_test.go, so a
// change there fails a test rather than leaving this harness quoting a budget the
// estate no longer runs.
const workMaxAckPending = 32

func admissionBudget() time.Duration { return bus.WorkAckWait / workMaxAckPending }

func loadConfig() (config, error) {
	c := config{
		// Localhost defaults, and only localhost defaults. A write harness that
		// could default to anything reachable is a write harness that can be run
		// against production by forgetting a variable.
		baseURL:    env.Or("ORDERFLOW_BASE_URL", "http://localhost:8080"),
		natsURL:    env.Or("ORDERFLOW_NATS_URL", "nats://localhost:4222"),
		metricsURL: env.Or("ORDERFLOW_OMS_METRICS_URL", "http://localhost:8090/metrics"),
		// token is read LAST, below, through pkg/secret so ORDERFLOW_TOKEN_FILE
		// works: a bearer that can move capital does not belong in a shell's
		// environment or in a process listing, and the estate already has one way
		// to say that.
		tenant:     env.Or("ORDERFLOW_TENANT", "load-test"),
		portfolio:  env.Or("PORTFOLIO", "PF1"),
		instrument: env.Or("ORDERFLOW_INSTRUMENT", "AAPL"),
		venue:      os.Getenv("ORDERFLOW_VENUE"),
		group:      env.Or("ORDERFLOW_CONSUMER_GROUP", "oms"),
		budget:     admissionBudget(),
	}
	var err error
	if c.token, err = secret.Read("ORDERFLOW_TOKEN"); err != nil {
		return config{}, err
	}
	if c.stages, err = intList("STAGES", []int{5, 10, 20, 40, 80}); err != nil {
		return config{}, err
	}
	if c.stageFor, err = env.Duration("STAGE_DURATION", 20*time.Second); err != nil {
		return config{}, err
	}
	if c.drainFor, err = env.Duration("DRAIN_TIMEOUT", 30*time.Second); err != nil {
		return config{}, err
	}
	if c.settleAfter, err = env.Duration("SETTLE_INTERVAL", 3*time.Second); err != nil {
		return config{}, err
	}
	if raw, ok := env.Lookup("ADMISSION_BUDGET"); ok {
		if c.budget, err = env.Duration("ADMISSION_BUDGET", c.budget); err != nil {
			return config{}, err
		}
		log.Printf("orderflow: admission budget OVERRIDDEN to %s (derived value is %s = bus.WorkAckWait/%d) — "+
			"a number measured against an overridden budget is not comparable with one that was not: %q",
			c.budget, admissionBudget(), workMaxAckPending, raw)
	}
	if c.token == "" {
		return config{}, errors.New("ORDERFLOW_TOKEN (or ORDERFLOW_TOKEN_FILE) is unset. The gateway refuses an unauthenticated " +
			"caller, so an unset token measures the latency of 401s at whatever rate the runner can " +
			"produce them. Mint one with: go run ./cmd/kanz-devtoken --secret <API_GATEWAY_JWT_SECRET> " +
			"--tenant <t> --role <required-role>,<trade-role> --portfolio <PORTFOLIO>")
	}
	if c.stageFor <= 0 || c.drainFor <= 0 {
		return config{}, fmt.Errorf("STAGE_DURATION (%s) and DRAIN_TIMEOUT (%s) must both be positive",
			c.stageFor, c.drainFor)
	}
	for _, r := range c.stages {
		if r < 1 {
			return config{}, fmt.Errorf("STAGES contains %d; an offered rate below 1/s measures nothing", r)
		}
	}
	return c, nil
}

// intList parses a comma-separated integer list, refusing a malformed entry
// rather than dropping it — a dropped stage is a ramp that skipped the rate
// somebody wanted measured.
func intList(key string, def []int) ([]int, error) {
	raw, ok := env.Lookup(key)
	if !ok {
		return def, nil
	}
	var out []int
	for _, part := range env.SplitList(raw) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", key, part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is set to %q and parsed to no stages at all", key, raw)
	}
	return out, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("orderflow: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	submitter, err := newSubmitter(cfg)
	if err != nil {
		return err
	}

	// The FACT reader is armed BEFORE anything is submitted, including the canary.
	// A run that started submitting first would have no way to tell an order whose
	// FACT it missed from an order that was never admitted, and "we did not see the
	// announcement" would be reported as "the platform lost an order".
	ledger, closeLedger, err := watchFacts(ctx, cfg, submitter.runTag)
	if err != nil {
		return err
	}
	defer closeLedger()

	before, err := preflight(ctx, cfg, submitter, ledger)
	if err != nil {
		return fmt.Errorf("REFUSING TO RUN: %w", err)
	}

	results := make([]stageResult, 0, len(cfg.stages))
	for _, rate := range cfg.stages {
		if ctx.Err() != nil {
			break
		}
		res := runStage(ctx, cfg, submitter, ledger, rate)
		results = append(results, res)
		log.Printf("orderflow: stage %d/s — offered=%d accepted_http=%d admitted=%d "+
			"p50=%s p99=%s peak_outstanding=%d verdict=%s",
			rate, res.offered, res.http202, res.admitted,
			res.p50.Round(time.Millisecond), res.p99.Round(time.Millisecond),
			res.peakOutstanding, res.verdict)
		if res.degraded() {
			log.Printf("orderflow: stopping the ramp — %s", res.verdict)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(cfg.settleAfter):
		}
	}

	after, serr := fetchScrape(ctx, cfg.metricsURL)
	if serr != nil {
		// Not fatal: the load already ran. But the control deltas are half the
		// point of this harness, so a run that cannot read them says so rather
		// than printing a report that looks complete.
		log.Printf("orderflow: WARNING — the closing metrics scrape failed (%v). The control deltas "+
			"below are UNKNOWN, not zero", serr)
	}
	report(cfg, results, before, after)
	return nil
}

// report prints the run. It is deliberately a plain text block rather than JSON:
// the audience is a person deciding whether a number may be quoted, and the
// caveats are as load-bearing as the numbers.
func report(cfg config, results []stageResult, before, after scrape) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== orderflow: write-path admission capacity ===\n")
	fmt.Fprintf(&b, "gateway=%s tenant=%s portfolio=%s instrument=%s venue=%q group=%s\n",
		cfg.baseURL, cfg.tenant, cfg.portfolio, cfg.instrument, cfg.venue, cfg.group)
	fmt.Fprintf(&b, "admission p99 budget=%s (bus.WorkAckWait %s / MaxAckPending %d — the per-message\n"+
		"  handling ceiling the delivery contract is sized by). MEASURED submit->announce is broker\n"+
		"  QUEUE WAIT + HANDLING, and the contract bounds handling alone, so a p99 past the budget\n"+
		"  marks SATURATION rather than proving a contract breach.\n"+
		"  outstd = peak submitted-but-unannounced, sampled here every 100ms (exact).\n"+
		"  pending = kanz_bus_pending_messages, refreshed by the OMS every 15s — it CORROBORATES\n"+
		"  outstd and cannot resolve a plateau shorter than about 45s.\n",
		cfg.budget, bus.WorkAckWait, workMaxAckPending)
	fmt.Fprintf(&b, "\n%-7s %8s %8s %9s %9s %9s %9s %8s %8s  %s\n",
		"rate/s", "offered", "http202", "admitted", "p50", "p95", "p99", "outstd", "pending", "verdict")
	for _, r := range results {
		fmt.Fprintf(&b, "%-7d %8d %8d %9d %9s %9s %9s %8d %8s  %s\n",
			r.rate, r.offered, r.http202, r.admitted,
			r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.p99.Round(time.Millisecond),
			r.peakOutstanding, gaugeText(r.maxBacklog), r.verdict)
		for _, line := range r.notes {
			fmt.Fprintf(&b, "%-8s %s\n", "", line)
		}
	}

	fmt.Fprintf(&b, "\n--- exactly-once ---\n")
	ex := results
	var subs, facts, dupes, missing int
	for _, r := range ex {
		subs += r.http202
		facts += r.admitted + r.refused
		dupes += r.duplicated
		missing += r.missing
	}
	fmt.Fprintf(&b, "submissions accepted by the gateway: %d\n", subs)
	fmt.Fprintf(&b, "terminal order FACTs for those ids:   %d (%d admitted, %d refused)\n",
		facts, subs-missing-countRefused(ex)*0, countRefused(ex))
	switch {
	case dupes > 0:
		fmt.Fprintf(&b, "DUPLICATE ANNOUNCEMENTS: %d order id(s) produced MORE THAN ONE terminal FACT.\n"+
			"  One intent became two announcements — the shape a redelivery overtaking a running\n"+
			"  handler produces. This is a finding, not noise.\n", dupes)
	case missing > 0:
		fmt.Fprintf(&b, "UNANNOUNCED: %d order id(s) the gateway accepted produced NO terminal FACT\n"+
			"  within the drain window. Either admission is still behind, or the estate does not\n"+
			"  know about an order the store may have admitted.\n", missing)
	default:
		fmt.Fprintf(&b, "one submission, one announcement, for every order in this run.\n")
	}

	fmt.Fprintf(&b, "\n--- control deltas across the run ---\n")
	if after == nil {
		fmt.Fprintf(&b, "UNKNOWN — the closing scrape failed. No control claim can be made about this run.\n")
	} else {
		invalid := false
		for _, d := range controlDeltas(before, after) {
			switch {
			case d.unknown:
				fmt.Fprintf(&b, "  %-52s UNKNOWN (not exported by this deployment)\n", d.metric)
			case d.delta == 0:
				fmt.Fprintf(&b, "  %-52s 0\n", d.metric)
			default:
				tag := "DEGRADED"
				if d.invalidates {
					tag = "INVALIDATES THIS RUN"
					invalid = true
				}
				fmt.Fprintf(&b, "  %-52s +%.0f  %s\n     %s\n", d.metric, d.delta, tag, d.meaning)
			}
		}
		if invalid {
			fmt.Fprintf(&b, "\nTHE NUMBERS ABOVE MUST NOT BE QUOTED. At least one counter marked\n"+
				"INVALIDATES THIS RUN moved, which means the flow did not traverse the control this\n"+
				"harness claims to have measured.\n")
		}
	}

	fmt.Fprintf(&b, "\n--- what this number is not ---\n")
	fmt.Fprintf(&b, "It is an ADMISSION capacity measured against a SIMULATED venue: the exchange\n"+
		"REST/WS hop inside the venue adapter did not run, and a real adapter adds a network\n"+
		"round-trip per order inside the same delivery budget. It is also a figure for the ONE\n"+
		"stack it ran against; a capacity guarantee needs a multi-node deployment. See\n"+
		"test/load/capacity-model.md.\n")
	fmt.Print(b.String())
}

func countRefused(rs []stageResult) int {
	n := 0
	for _, r := range rs {
		n += r.refused
	}
	return n
}

// stageResult is one plateau's measurement.
type stageResult struct {
	rate       int
	offered    int
	http202    int
	httpOther  map[int]int
	admitted   int
	refused    int
	duplicated int
	missing    int
	p50        time.Duration
	p95        time.Duration
	p99        time.Duration
	// peakOutstanding is the deepest submitted-but-unannounced queue this stage
	// reached, sampled by the harness at 100ms resolution.
	peakOutstanding int64
	// maxBacklog is kanz_bus_pending_messages for the order-command durable. -1
	// means the series could not be read at all, and is never printed as zero.
	maxBacklog float64
	verdict    string
	notes      []string
}

func (r stageResult) degraded() bool { return !strings.HasPrefix(r.verdict, "ok") }

// runStage offers `rate` submissions/sec for cfg.stageFor, then waits up to
// cfg.drainFor for every accepted submission to produce its terminal FACT.
//
// OPEN MODEL. The pacing ticker fires regardless of how fast anything downstream
// is, so the measured quantity is "the rate the platform sustains", not "how fast
// N in-flight clients happen to go". Where the submitters cannot keep up with the
// ticker the shortfall is REPORTED (offered vs rate × duration) rather than
// silently turning this into a closed model — a load generator that quietly
// throttles itself reports the capacity of its own client.
func runStage(ctx context.Context, cfg config, s *submitter, l *ledger, rate int) stageResult {
	res := stageResult{rate: rate, httpOther: map[int]int{}}
	stageCtx, cancel := context.WithTimeout(ctx, cfg.stageFor)
	defer cancel()

	// TWO BACKLOG SIGNALS, AND THE PRIMARY ONE IS THE HARNESS'S OWN.
	//
	// kanz_bus_pending_messages is the estate's KEDA signal and the right thing to
	// cross-check against, but it is refreshed by each subscription's own poller
	// every backlogPollInterval (15s, pkg/bus/backlog.go) — so on a 20s plateau one
	// or two polls land inside the window, and a reading of 0 means "the polls that
	// landed read 0", NOT "no backlog existed". A harness reporting that number
	// alone prints a reassuring zero for a stage whose admission latency has
	// already blown out, which is what an earlier revision of this file did.
	//
	// The primary signal is therefore computed here: 202s issued minus terminal
	// FACTs seen, sampled every 100ms. It is exact, sub-second, and needs no broker
	// introspection.
	outstanding := watchOutstanding(stageCtx, s, l, &res)
	gauge := make(chan float64, 1)
	go func() { gauge <- sampleBacklog(stageCtx, cfg) }()

	ids := s.offer(stageCtx, cfg, rate, &res)
	outstanding()
	res.maxBacklog = <-gauge

	deadline := time.Now().Add(cfg.drainFor)
	// Keep sampling through the drain: the deepest queue is usually reached AFTER
	// the last submission, while admission works off what the plateau left behind.
	drainCtx, cancelDrain := context.WithCancel(ctx)
	drained := watchOutstanding(drainCtx, s, l, &res)
	stats := l.await(ctx, ids, deadline)
	cancelDrain()
	drained()
	res.admitted, res.refused = stats.admitted, stats.refused
	res.duplicated, res.missing = stats.duplicated, stats.missing
	res.p50, res.p95, res.p99 = stats.p50, stats.p95, stats.p99
	res.verdict = verdictFor(cfg, res)
	return res
}

// verdictFor names the FIRST thing that degraded, in the order that matters:
// correctness, then completion, then latency, then backlog. A stage that trips
// several reports the most serious one, because that is the one the ramp stopped
// for.
func verdictFor(cfg config, r stageResult) string {
	switch {
	case r.duplicated > 0:
		return fmt.Sprintf("EXACTLY-ONCE BROKEN: %d order(s) announced twice", r.duplicated)
	case r.missing > 0:
		return fmt.Sprintf("ADMISSION INCOMPLETE: %d accepted submission(s) produced no terminal FACT within %s",
			r.missing, cfg.drainFor)
	case r.http202 == 0 && r.offered > 0:
		return "GATEWAY REFUSED EVERY SUBMISSION"
	case r.refused > 0 && r.admitted == 0:
		return fmt.Sprintf("EVERY ORDER WAS REFUSED (%d) — this stage measured the refusal path", r.refused)
	case r.p99 > cfg.budget:
		// PRECISELY WHAT THIS DOES AND DOES NOT SAY. submit→announce is broker
		// QUEUE WAIT plus per-message HANDLING, and the delivery contract bounds
		// only the second term — so a p99 over the budget is not by itself proof
		// the contract was violated. It is the saturation point: past it the queue
		// is not draining at the offered rate, which is what the peak-outstanding
		// column beside it shows. The budget is used as the threshold because it
		// is a number the estate already committed to rather than one this harness
		// invented, and because the two terms become indistinguishable exactly
		// where it matters.
		return fmt.Sprintf("SATURATED: admission p99 %s is past the %s delivery-contract budget "+
			"(queue wait + handling; the contract bounds handling alone)",
			r.p99.Round(time.Millisecond), cfg.budget)
	case r.offered < r.rate*int(cfg.stageFor/time.Second)*9/10:
		return fmt.Sprintf("ok, BUT THE GENERATOR FELL BEHIND: offered %d of ~%d — this stage measured "+
			"the harness, not the platform", r.offered, r.rate*int(cfg.stageFor/time.Second))
	default:
		return "ok"
	}
}

// watchOutstanding samples the submitted-but-unannounced depth until ctx ends,
// keeping the peak on res. The returned func waits for the sampler to stop, so a
// caller reads res only after the goroutine writing it has finished.
func watchOutstanding(ctx context.Context, s *submitter, l *ledger, res *stageResult) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			if n := s.accepted.Load() - l.announced(); n > res.peakOutstanding {
				res.peakOutstanding = n
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() { <-done }
}

// sampleBacklog polls kanz_bus_pending_messages until ctx ends and returns the
// highest value seen. Returns -1 when the series could not be read AT ALL, so an
// unreadable signal is never reported as a backlog of zero.
func sampleBacklog(ctx context.Context, cfg config) float64 {
	max := -1.0
	for ctx.Err() == nil {
		if s, err := fetchScrape(ctx, cfg.metricsURL); err == nil {
			if v, ok := pendingCommands(s, cfg.group, "order.order.submit"); ok && v > max {
				max = v
			}
		}
		select {
		case <-ctx.Done():
			return max
		case <-time.After(time.Second):
		}
	}
	return max
}

// gaugeText renders the kanz_bus_pending_messages reading, keeping "could not be
// read" distinct from "zero".
func gaugeText(v float64) string {
	if v < 0 {
		return "unread"
	}
	return fmt.Sprintf("%.0f", v)
}

// percentiles over a sorted slice. Nearest-rank, which is what k6 reports and
// what every budget in test/load is stated against.
func percentiles(d []time.Duration) (p50, p95, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0, 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(p float64) time.Duration {
		i := int(float64(len(d))*p) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(d) {
			i = len(d) - 1
		}
		return d[i]
	}
	return at(0.50), at(0.95), at(0.99)
}
