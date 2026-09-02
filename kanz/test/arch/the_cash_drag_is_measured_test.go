package arch

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// UNINVESTED CASH MUST STAY MEASURABLE, AND ITS REFUSALS MUST STAY VISIBLE
// (#963).
//
// Cash drag is unlike every other condition this estate watches: it breaches no
// mandate, trips no risk limit and produces no reconciliation break, so this
// measurement is the ONLY thing that would ever report it. A control with no
// second opinion behind it has to be guarded harder than one whose failure some
// other alarm would eventually catch.
//
// Four ways it rots, each with a precedent:
//
//  1. A REFUSAL BECOMES A MEASURED ZERO. treasury.Drag.Percent returns 0 on an
//     UNKNOWN drag, so a caller that observed it unconditionally would put every
//     unmeasurable book in the lowest histogram bucket — rendering an estate that
//     can see nothing as an estate that is fully invested. Wrong, and in the
//     flattering direction, which is why nothing downstream would question it.
//     This is #757's shape (a withheld value read as a computed zero) on the
//     other side of the platform.
//
//  2. THE REFUSAL REASONS GET ENUMERATED BY HAND at the consumer. treasury.Reasons
//     is the one list; a copy in the metrics file means a new reason ships with
//     no series and its refusals are invisible. #806 (marks) and #803 (refusal
//     flags) were each exactly that.
//
//  3. AN IDLE-CASH THRESHOLD GETS WRITTEN INTO AN ALERT. A threshold is a cash
//     policy — a target buffer, a maximum idle balance — and #963 places that
//     with mandates because it is a mandate-shaped constraint. A number in a
//     rules file is a second copy of a constraint living outside the component
//     that owns constraints, which is the one thing "Changing the platform" ranks
//     canonical domain models above.
//
//  4. THE MEASUREMENT MOVES ONTO THE ORDER PATH. Idle cash sits in portfolios
//     that are NOT trading, so sampling it at the pre-trade gate measures exactly
//     the set least likely to have any. It has to ride the post-trade monitor's
//     sweep, which is the only loop in the estate that already walks every book
//     whether it traded or not.

const (
	treasuryPkgDir     = "../../internal/treasury"
	treasuryMetricsRel = "../../services/compliance/cmd/compliance/treasurymetrics.go"
	treasuryMonitorRel = "../../services/compliance/internal/monitor/monitor.go"
	treasuryRulesRel   = "../../infra/observability/alerts/operational.rules.yaml"
	treasuryTestsRel   = "../../infra/observability/alerts/operational_test.yaml"
)

// TestARefusedDragIsNeverObservedIntoTheHistogram holds rule one.
//
// STRUCTURAL, because the defect is invisible at runtime: a histogram fed a 0.0
// for every unmeasurable book produces a perfectly healthy-looking distribution.
// The observer must record the share ONLY under a Measured() guard, and the
// refusal branch must reach the counter instead.
func TestARefusedDragIsNeverObservedIntoTheHistogram(t *testing.T) {
	src := readStripped(t, treasuryMetricsRel)
	body := funcBody(t, src, "func cashDragObserver(")

	obsIdx := strings.Index(body, ".Observe(")
	if obsIdx < 0 {
		t.Fatal("the observer never records an idle-cash share. kanz_treasury_idle_cash_share would " +
			"export a permanently empty histogram, and TreasuryCashDragNotObserved — an == 0 rule " +
			"over its _count — would then fire forever on a working estate (#963).")
	}
	guardIdx := strings.Index(body, "Measured()")
	if guardIdx < 0 || guardIdx > obsIdx {
		t.Fatal("the idle-cash share is observed without a Measured() guard in front of it. " +
			"treasury.Drag.Percent returns 0 on a REFUSAL, so every book whose cash nobody can " +
			"vouch for (#588 — which today is most of them) would land in the lowest bucket and " +
			"the estate would render as fully invested. That is #757's failure — a withheld " +
			"value read as a computed zero — with a treasury decision on the end of it (#963).")
	}
	if !strings.Contains(body, "dragUnmeasurable.WithLabelValues") {
		t.Fatal("the observer does not count refusals by reason. The refusals are the more useful " +
			"half of this measurement today: with #588 open, 'we cannot measure our own cash " +
			"drag, and here is which feed is why' is what sizes whether the sweep engine is " +
			"worth building at all.")
	}
}

// TestTheRefusalReasonsAreDerivedFromTheDomain holds rule two.
func TestTheRefusalReasonsAreDerivedFromTheDomain(t *testing.T) {
	src := readStripped(t, treasuryMetricsRel)
	if !strings.Contains(src, "treasury.Reasons()") {
		t.Fatal("the metrics file no longer seeds its labels from treasury.Reasons(). A list written " +
			"here is a second copy of the refusal vocabulary, and the copy is what goes stale: a " +
			"new reason ships with no series, so the books it refuses become invisible on every " +
			"dashboard and in the TreasuryCashDragUnmeasurable breakdown. #806 and #803 were both " +
			"one hand-maintained enumeration missing a member (#963).")
	}

	// And the domain list must actually be the full set the domain can return.
	// A Reasons() that had drifted from the constants would seed a subset and the
	// guard above would still pass.
	dom := readStripped(t, treasuryPkgDir+"/treasury.go")
	declared := regexp.MustCompile(`Reason([A-Za-z]+)\s+Reason\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(dom, -1)
	if len(declared) == 0 {
		t.Fatal("derived NO Reason constants from internal/treasury — the guard would pass vacuously")
	}
	seeded := funcBody(t, dom, "func Reasons() []Reason")
	var missing []string
	for _, m := range declared {
		if m[2] == "" { // ReasonNone is the absence of a reason, not a refusal
			continue
		}
		if !strings.Contains(seeded, "Reason"+m[1]) {
			missing = append(missing, m[2])
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("refusal reasons Measure can return that Reasons() does not name (%d):\n\n  %s\n\n"+
			"These seed no metric series, so a book refused for one of them is counted into a "+
			"label nothing reads and the refusal is invisible.", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestNoAlertPutsAThresholdOnTheIdleCashLevel holds rule three.
//
// THE HISTOGRAM MAY BE READ AND MUST NOT BE COMPARED. Every Treasury rule may
// query the refusal counter and the histogram's _count — that is the coverage
// question — but a comparison against a BUCKET or a quantile of the share itself
// is a judgement about how much idle cash is too much, and that judgement is a
// cash policy #963 places with mandates.
func TestNoAlertPutsAThresholdOnTheIdleCashLevel(t *testing.T) {
	for _, r := range treasuryRules(t) {
		expr := r.Expr
		if !strings.Contains(expr, "kanz_treasury_idle_cash_share") {
			continue
		}
		// _count and _sum are cardinality, not level. _bucket and any quantile
		// over it are a statement about the distribution's position.
		if strings.Contains(expr, "_bucket") || strings.Contains(expr, "histogram_quantile") {
			t.Fatalf("alert %s compares the idle-cash DISTRIBUTION:\n\n%s\n\n"+
				"That is a threshold on how much idle cash is acceptable, which is a cash policy — a "+
				"target operating buffer and a maximum idle balance. #963 places that with mandates "+
				"because it is a mandate-shaped constraint, and compliance is the one service that "+
				"must not hold a second copy of a constraint. Export the histogram for reading; when "+
				"the policy lands the rule becomes 'this book is outside the buffer its mandate "+
				"declares', against a different series.", r.Alert, expr)
		}
	}
}

// TestTheCashDragRidesThePostTradeSweep holds rule four.
func TestTheCashDragRidesThePostTradeSweep(t *testing.T) {
	mon := readStripped(t, treasuryMonitorRel)
	if !strings.Contains(mon, "observeCashDrag(") {
		t.Fatal("the compliance post-trade monitor no longer observes cash drag. That monitor is the " +
			"only loop in this estate that re-runs EVERY book it holds on an interval, whether the " +
			"portfolio traded or not — and idle cash sits precisely in the portfolios that are not " +
			"trading. Measuring it anywhere order-driven samples the set least likely to have any (#963).")
	}
	body := funcBody(t, mon, "func (m *Monitor) evaluate(")
	call := strings.Index(body, "observeCashDrag(")
	mandate := strings.Index(body, "m.mandates.Mandate(")
	switch {
	case call < 0:
		t.Fatal("evaluate no longer measures cash drag. Both paths into the monitor — the FACT " +
			"handler and the interval sweep — funnel through this function; a measurement " +
			"anywhere else covers one of them.")
	case mandate >= 0 && call > mandate:
		t.Fatal("cash drag is measured AFTER the mandate lookup in Monitor.evaluate. The branches " +
			"under that lookup return early on an unresolvable tenant and an unappliable mandate, " +
			"so the measurement would silently stop sampling exactly the portfolios in the worst " +
			"operational state — and idle cash is a fact about a book's composition, not about " +
			"whether a mandate governs it (#963).")
	}

	// It must NOT also be on the order path, where it would sample the wrong set
	// and put work in front of every order.
	gate := readStripped(t, "../../internal/compliance/gate.go")
	if strings.Contains(gate, "treasury.") {
		t.Fatal("internal/compliance's pre-trade gate references the treasury package. Cash drag " +
			"must not be measured on the order-admission path: it samples only portfolios that " +
			"ARE trading, which is the set least likely to be sitting on idle cash, and it puts " +
			"a measurement in front of every order for a number nothing on that path reads.")
	}
}

// TestEveryTreasuryAlertHasAHarnessCaseIncludingSilence holds the proof standard.
//
// THE `== 0` RULE IS THE ONE THAT NEEDS THE SILENT CASE. An == 0 arm over a
// series no pod exports evaluates to nothing and is quiet in exactly the state it
// exists to detect, and a harness INVENTS its input series so it cannot see that
// on its own. The named group below supplies every series seeded and flat, which
// is what registerTreasuryMetrics actually produces at startup.
func TestEveryTreasuryAlertHasAHarnessCaseIncludingSilence(t *testing.T) {
	b, err := os.ReadFile(treasuryTestsRel)
	if err != nil {
		t.Fatalf("read alert tests: %v", err)
	}
	harness := string(b)

	const silent = "nothing has been measured or refused at all"
	if !strings.Contains(harness, "name: "+silent+"\n") {
		t.Fatalf("the alert harness no longer contains the %q case. TreasuryCashDragNotObserved is "+
			"an == 0 rule; without a group that supplies its series present and FLAT, nothing "+
			"proves it fires when the monitor stops evaluating rather than staying silent over "+
			"an empty vector.", silent)
	}
	rules := treasuryRules(t)
	if len(rules) == 0 {
		t.Fatal("found NO Treasury alert rules — the guard would pass vacuously")
	}
	for _, r := range rules {
		if !strings.Contains(harness, "alertname: "+r.Alert) {
			t.Fatalf("alert %s has no case in operational_test.yaml. A rule that has never been "+
				"executed is a claim, not a control.", r.Alert)
		}
	}
}

// TestTheTreasurySeriesAreRegisteredWithoutABroker holds the wiring rule.
//
// THE SAME DEFECT TWICE IN TWO PRs. #973 registered the copilot's answer metrics
// inside the branch that builds its NATS producer, so a deployment with no broker
// kept no record of anything AND exported no series saying so. This work then
// reproduced it exactly: registerTreasuryMetrics sat in runConsumers, which only
// runs with COMPLIANCE_NATS_URL set — and a compliance with no broker consumes
// nothing, evaluates no book, and measures no portfolio's idle cash, which is
// PRECISELY the state TreasuryCashDragNotObserved was written for. An == 0 rule
// over an absent series evaluates to nothing, so the alert would have been silent
// in the one state it exists for.
//
// Caught by running the built binary, which no unit test reaches. The guard is
// structural because the runtime symptom is an ABSENCE — a scrape with no
// kanz_treasury_ lines looks like a service that has not started yet.
func TestTheTreasurySeriesAreRegisteredWithoutABroker(t *testing.T) {
	src := readStripped(t, "../../services/compliance/cmd/compliance/main.go")
	call := strings.Index(src, "registerTreasuryMetrics(")
	if call < 0 {
		t.Fatal("compliance never registers the cash-drag series — every Treasury rule queries an " +
			"empty vector and never fires (#963).")
	}
	consumers := strings.Index(src, "func runConsumers(")
	if consumers >= 0 && call > consumers {
		t.Fatal("registerTreasuryMetrics is called inside runConsumers, which only runs when " +
			"COMPLIANCE_NATS_URL is set. A broker-less compliance then exports NO cash-drag " +
			"series at all — and that deployment consumes nothing, evaluates no book and measures " +
			"no idle cash, which is exactly what TreasuryCashDragNotObserved pages on. An == 0 " +
			"rule over an absent series is silent, so the alert would miss the state it was " +
			"written for. Register in run(), before the consumer path. This is the same wiring " +
			"defect #973 found in the copilot's answer metrics (#963).")
	}
}

// treasuryRules parses the Treasury-prefixed alerting rules.
func treasuryRules(t *testing.T) []copilotRule {
	t.Helper()
	b, err := os.ReadFile(treasuryRulesRel)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	// The same shape copilotRules decodes, so a rule-file schema change fails in
	// both guards rather than silently emptying one of them.
	var doc struct {
		Groups []struct {
			Rules []copilotRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse rules: %v", err)
	}
	var out []copilotRule
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if strings.HasPrefix(r.Alert, "Treasury") {
				out = append(out, r)
			}
		}
	}
	return out
}
