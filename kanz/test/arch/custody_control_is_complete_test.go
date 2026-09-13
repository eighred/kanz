package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE CUSTODY RECONCILIATION CONTROL MUST STAY WHOLE (#962).
//
// # What this guards, and why prose could not
//
// The control is four things that only work together: a canonical statement FACT,
// a scheduled run that emits evidence for EVERY outcome, a break lifecycle, and
// metrics an alert can read. Any one of them silently absent returns the estate
// to the state #962 fixed — a reconciliation ENGINE with no reconciliation
// CONTROL — and the failure is quiet by construction, because a control that does
// not run produces no output to be missing.
//
// Three specific ways it could rot, each of which has a precedent in this
// repository:
//
//  1. A BREAK KIND IS ADDED TO THE ENGINE AND NOT TO THE WIRE. recon.BreakKind
//     and accounting.v1.ReconciliationBreakKind are two enumerations of one set,
//     and #806 (marks) and #803 (refusal flags) are both defects where somebody
//     enumerated a set by hand and missed a member. So this derives one from the
//     other rather than listing either: a list written here would be a further
//     copy of exactly the thing that breaks.
//
//  2. AN ALERT OUTLIVES THE SERIES IT READS. #62 deleted ten data-quality rules
//     because each queried a series no running pod exported, and a rule over an
//     empty vector never fires — the estate reads as alerted and is not. Every
//     custody metric named in operational.rules.yaml must therefore be registered
//     by custody/metrics.go.
//
//  3. A NEW OUTCOME STOPS BEING SEEDED. A CounterVec exports nothing for a label
//     it has never incremented, so the seeding in Metrics.SeedCustodian is what
//     makes "no failed runs" a zero rather than an absent series. It seeds from
//     Outcomes(), and Outcomes() must therefore name every non-zero Outcome the
//     package declares.
//
// # What it does NOT claim
//
// It does not prove the control runs in any deployment — that is
// TestCustodyPlaneArmsAScheduler's job at the composition root, and the alert's
// job at runtime. It proves the pieces still refer to each other.

const (
	custodyPkgDir   = "../../services/accounting/internal/custody"
	reconPkgDir     = "../../services/accounting/internal/recon"
	custodyProtoRel = "../../../kanz-schemas/proto/accounting/v1/accounting.proto"
	custodyRulesRel = "../../infra/observability/alerts/operational.rules.yaml"
)

// readStripped returns a file's source with // comments and block comments
// removed.
//
// COMMENTS ARE STRIPPED BECAUSE A GUARD THAT GREPS RAW SOURCE MATCHES ITS OWN
// PROSE. Three guards in this tree passed with the checked thing deleted, because
// the identifier they searched for also appeared in a comment beside it. Every
// match below is against code.
func readStripped(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func readAllStripped(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob %s: %v (found %d)", dir, err, len(files))
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b.WriteString(readStripped(t, f))
		b.WriteString("\n")
	}
	return b.String()
}

// engineBreakKinds derives the engine's kind set from recon.BreakKinds()'s body —
// the single place the engine names its own members.
func engineBreakKinds(t *testing.T) []string {
	t.Helper()
	src := readStripped(t, filepath.Join(reconPkgDir, "recon.go"))
	m := regexp.MustCompile(`func BreakKinds\(\) \[\]BreakKind \{\s*return \[\]BreakKind\{([^}]*)\}`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("recon.BreakKinds() not found, or its body is no longer a single literal slice — " +
			"this guard derives the engine's kind set from it, so a refactor here must update the guard " +
			"rather than the guard silently deriving an empty set")
	}
	var kinds []string
	for _, part := range strings.Split(m[1], ",") {
		if p := strings.TrimSpace(part); p != "" {
			kinds = append(kinds, p)
		}
	}
	if len(kinds) == 0 {
		t.Fatal("derived an EMPTY engine kind set — the guard would then pass vacuously")
	}
	sort.Strings(kinds)
	return kinds
}

// A KIND THE ENGINE CAN PRODUCE MUST HAVE A WIRE REPRESENTATION AND A STRING.
//
// The engine's set is the source of truth; the proto enum, the String() method
// and custody's wireKind switch are three consumers of it. A kind added to the
// engine and missed by any one of them reaches an operator classified as nothing
// in particular, or refuses the publish of the whole run.
func TestEveryEngineBreakKindReachesTheWire(t *testing.T) {
	kinds := engineBreakKinds(t)

	reconSrc := readStripped(t, filepath.Join(reconPkgDir, "recon.go"))
	custodySrc := readAllStripped(t, custodyPkgDir)
	protoSrc := readStripped(t, custodyProtoRel)

	var missing []string
	for _, kind := range kinds {
		// String(): the engine must render it as something other than "unknown",
		// because the stored break's kind column and every metric label are that
		// string.
		if !regexp.MustCompile(`case ` + regexp.QuoteMeta(kind) + `:`).MatchString(reconSrc) {
			missing = append(missing, kind+": recon.BreakKind.String() has no case for it")
		}
		// custody.wireKind: the single join between the engine and the wire.
		if !regexp.MustCompile(`case recon\.` + regexp.QuoteMeta(kind) + `:`).MatchString(custodySrc) {
			missing = append(missing, kind+": custody.wireKind has no case for it — the run publish would be refused")
		}
	}

	// The proto enum must have exactly as many non-UNSPECIFIED values as the
	// engine has kinds. Counting rather than name-matching, because the naming
	// conventions differ (BreakMissingInIBOR vs
	// RECONCILIATION_BREAK_KIND_MISSING_IN_IBOR) and a mapping table here would be
	// the third hand-written copy of the set.
	protoValues := regexp.MustCompile(`RECONCILIATION_BREAK_KIND_[A-Z_]+ = \d+;`).FindAllString(protoSrc, -1)
	nonUnspecified := 0
	for _, v := range protoValues {
		if !strings.Contains(v, "UNSPECIFIED") {
			nonUnspecified++
		}
	}
	if nonUnspecified != len(kinds) {
		missing = append(missing, "accounting.v1.ReconciliationBreakKind declares "+
			strconv.Itoa(nonUnspecified)+" kinds and the engine produces "+strconv.Itoa(len(kinds)))
	}

	if len(missing) > 0 {
		t.Fatalf("the engine's break kinds and their wire representations have drifted (%d):\n\n  %s\n\n"+
			"recon.BreakKinds() is the source of truth and every consumer derives from it. A kind the "+
			"engine can produce and the wire cannot carry either reaches an operator classified as "+
			"nothing in particular, or refuses the publish of the entire run — which loses the evidence "+
			"for the breaks that DID map. Add the case, do not add an exemption here.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// EVERY CUSTODY SERIES AN ALERT READS MUST BE REGISTERED BY THE PRODUCER.
//
// A rule over a series nothing exports never fires, so the estate reads as
// alerted and is not (#62). This derives the alert side from the rules file and
// the producer side from metrics.go, and names any rule whose series has no
// producer.
func TestEveryCustodyAlertReadsASeriesTheProducerRegisters(t *testing.T) {
	rules, err := os.ReadFile(custodyRulesRel)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(rules, &doc); err != nil {
		t.Fatalf("parse rules: %v", err)
	}

	metricsSrc := readStripped(t, filepath.Join(custodyPkgDir, "metrics.go"))
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name: "(kanz_accounting_[a-z_]+)"`).FindAllStringSubmatch(metricsSrc, -1) {
		registered[m[1]] = true
	}
	if len(registered) == 0 {
		t.Fatal("derived NO registered metric names from custody/metrics.go — the guard would pass vacuously")
	}

	seriesRe := regexp.MustCompile(`kanz_accounting_[a-z_]+`)
	var problems []string
	custodyAlerts := 0
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if !strings.HasPrefix(r.Alert, "Custody") {
				continue
			}
			custodyAlerts++
			for _, series := range seriesRe.FindAllString(r.Expr, -1) {
				if !registered[series] {
					problems = append(problems, r.Alert+" reads "+series+", which custody/metrics.go does not register")
				}
			}
		}
	}
	if custodyAlerts == 0 {
		t.Fatal("no Custody* alert rules found in operational.rules.yaml — #962's alerting has been " +
			"removed, and the control is once again unobservable: 'reconciled clean' and 'nobody ran " +
			"it' produce identical telemetry")
	}
	if len(problems) > 0 {
		t.Fatalf("custody alerts read series with no producer (%d):\n\n  %s\n\n"+
			"A rule over a series nothing exports queries an empty vector and NEVER FIRES, so the "+
			"estate reads as alerted while the condition is undetectable — the failure #62 deleted ten "+
			"rules for. Instrument first, then write the rule.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// custodyDiagnosticOnlySeries names a registered custody series that deliberately
// has NO alert of its own, and why. Anything not listed here must be read by some
// Custody rule's expression.
//
// THE ENTRY IS A CLAIM THAT THE SERIES IS FOR READING, NOT FOR PAGING. "Nothing
// alerts on it yet" is not a reason; "the condition it describes is already paged
// through a different series, and this one is what an operator opens next" is.
var custodyDiagnosticOnlySeries = map[string]string{
	"kanz_accounting_breaks_open": "the COUNT of outstanding breaks is not itself a page — a book that is " +
		"actively reconciling legitimately carries breaks, and paging on their existence would fire most " +
		"days and be silenced within a week. What IS pageable is a break that is not being CLOSED, which " +
		"is kanz_accounting_break_oldest_age_seconds and CustodyBreakAgeing. This series is the breakdown " +
		"by kind an operator opens next, and both alerts' descriptions send them to it.",
}

// EVERY REGISTERED CUSTODY SERIES MUST BE READ BY SOME ALERT.
//
// This is the inverse of the test above, and it exists because that one is
// satisfied by ANY surviving Custody rule: renaming the staleness alert away left
// it green, which was found by mutating it rather than by reading it. Deriving the
// required alert coverage from the PRODUCER's registrations closes that — a rule
// deleted or renamed leaves its series unread, and the series list is the source
// of truth rather than a list of alert names retyped here.
func TestEveryRegisteredCustodySeriesIsReadByAnAlert(t *testing.T) {
	metricsSrc := readStripped(t, filepath.Join(custodyPkgDir, "metrics.go"))
	registered := regexp.MustCompile(`Name: "(kanz_accounting_[a-z_]+)"`).FindAllStringSubmatch(metricsSrc, -1)
	if len(registered) == 0 {
		t.Fatal("derived NO registered metric names from custody/metrics.go — the guard would pass vacuously")
	}

	rules, err := os.ReadFile(custodyRulesRel)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(rules, &doc); err != nil {
		t.Fatalf("parse rules: %v", err)
	}

	// EXPRESSIONS ONLY. An annotation naming a series is prose, and a guard that
	// accepted it would pass on a rule that merely MENTIONS the metric it no
	// longer reads — the "a guard that matches prose checks nothing" failure that
	// has already produced three green guards over deleted code in this tree.
	var exprs strings.Builder
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if strings.HasPrefix(r.Alert, "Custody") {
				exprs.WriteString(r.Expr)
				exprs.WriteString("\n")
			}
		}
	}
	alerting := exprs.String()

	var unread []string
	used := map[string]bool{}
	for _, m := range registered {
		series := m[1]
		if strings.Contains(alerting, series) {
			continue
		}
		if _, exempt := custodyDiagnosticOnlySeries[series]; exempt {
			used[series] = true
			continue
		}
		unread = append(unread, series)
	}
	if len(unread) > 0 {
		sort.Strings(unread)
		t.Fatalf("custody series with no alert reading them (%d):\n\n  %s\n\n"+
			"The producer registers these and no Custody rule's EXPRESSION queries them, so the condition "+
			"each one describes is invisible: the control keeps running and nothing pages when it stops. "+
			"Either write the rule, or add the series to custodyDiagnosticOnlySeries with an argument that "+
			"it is for reading rather than paging.", len(unread), strings.Join(unread, "\n  "))
	}

	// DEAD-ENTRY CHECK: an exemption must not outlive the thing it excuses.
	for series := range custodyDiagnosticOnlySeries {
		if !used[series] {
			t.Fatalf("custodyDiagnosticOnlySeries names %q, which custody/metrics.go no longer registers, "+
				"or which an alert now reads. A stale exemption is a hole nobody is watching — remove it.", series)
		}
	}
}

// THE SEEDING MUST COVER EVERY OUTCOME THE PACKAGE DECLARES.
//
// Metrics.SeedCustodian seeds from Outcomes(), so an Outcome added to the type
// and omitted from Outcomes() gets no zero series — and a rule over it is silent
// in exactly the state it detects.
func TestOutcomesNamesEveryDeclaredOutcome(t *testing.T) {
	src := readStripped(t, filepath.Join(custodyPkgDir, "custody.go"))

	// The const block's members, minus the zero value (which is never a
	// legitimate conclusion and is deliberately not seeded).
	block := regexp.MustCompile(`(?s)const \(\s*\n\s*OutcomeUnspecified Outcome = iota(.*?)\n\)`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("the Outcome const block is no longer in the shape this guard derives from — update the " +
			"guard rather than letting it derive an empty set")
	}
	declared := regexp.MustCompile(`(?m)^\s*(Outcome[A-Za-z]+)\s*$`).FindAllStringSubmatch(block[1], -1)
	if len(declared) == 0 {
		t.Fatal("derived NO declared outcomes — the guard would pass vacuously")
	}

	listed := regexp.MustCompile(`func Outcomes\(\) \[\]Outcome \{\s*return \[\]Outcome\{([^}]*)\}`).FindStringSubmatch(src)
	if listed == nil {
		t.Fatal("custody.Outcomes() not found, or its body is no longer a single literal slice")
	}

	var missing []string
	for _, d := range declared {
		name := d[1]
		if !strings.Contains(listed[1], name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Outcomes() omits %v.\n\nMetrics.SeedCustodian seeds the runs counter from Outcomes(), so an "+
			"omitted outcome exports NO series until it first occurs — and a rule over an absent series "+
			"never fires, which means the alert is silent in precisely the state it exists to detect. "+
			"Outcomes() is the source of truth for the label set; add the member there.", missing)
	}
}

// THE STATEMENT SUBJECT MUST BE GRANTED, AND THE RUN SUBJECT PUBLISHABLE.
//
// A NATS subject missing from a service's subscribe allow-list is DENIED
// SILENTLY: the subscription succeeds and delivers nothing, forever (#787/#788).
// For this control that failure is indistinguishable from a custodian that has
// stopped transmitting — every scheduled run would conclude no_statement and the
// cause would be a permission rather than a counterparty.
func TestCustodySubjectsAreGrantedInTenancy(t *testing.T) {
	raw, err := os.ReadFile("../../infra/nats/tenancy.yaml")
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	tenancy := string(raw)

	custodySrc := readAllStripped(t, custodyPkgDir)
	subjects := map[string]string{}
	for _, m := range regexp.MustCompile(`Subject(Statement|Run|ActionRecorded)\s*=\s*"([a-z0-9._]+)"`).FindAllStringSubmatch(custodySrc, -1) {
		subjects[m[1]] = m[2]
	}
	if len(subjects) != 3 {
		t.Fatalf("derived %d custody subjects from the package, want 3 (SubjectStatement, SubjectRun and SubjectActionRecorded)", len(subjects))
	}

	// Both accounting users — the platform one and the per-tenant one — must
	// carry the grant. tenancy.yaml states the tenant lists are DERIVED from the
	// platform's, and TestTenantNATSPermissionsMirrorThePlatform enforces the
	// equality; this asserts the subject reaches both at all.
	for kind, subject := range subjects {
		if strings.Count(tenancy, `"`+subject+`"`) < 2 {
			t.Fatalf("custody %s subject %q appears in fewer than 2 allow-list entries in "+
				"infra/nats/tenancy.yaml.\n\nBoth the platform accounting user and accounting-acme need "+
				"it. A missing SUBSCRIBE is denied silently — the subscription delivers nothing and every "+
				"run concludes no_statement, blaming the custodian for a permission. A missing PUBLISH "+
				"means every run is recorded locally and the estate never hears it, so the staleness "+
				"alert has no series to age.", kind, subject)
		}
	}
}
