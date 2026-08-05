package arch

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY WORKLOAD DECLARES WHAT A VOLUNTARY DISRUPTION MAY DO TO IT.
//
// Before #234 the estate answered that question for four workloads out of twenty
// and left the other sixteen to the default, which is "anything". Five of the
// sixteen pin `replicas: 1` as a CORRECTNESS bound — three of them because two
// venue-adapter pods would spend double the REST weight against one exchange API
// key and get the account rate-limited or banned. Nothing bounded their eviction.
//
// The concrete failure: `kubectl drain` during a routine node upgrade evicts the
// single venue-binance pod with no budget in the way. Binance's user-data
// WebSocket carries NO replay, so the fills that arrive while the pod is
// rescheduling are never delivered to the OMS. Order state diverges from exchange
// state, no error is raised anywhere, and no restart repairs it — the divergence
// is discovered at reconciliation, by which time the fund has been trading against
// a book it believes and the exchange does not.
//
// The same grep found the second half: NOTHING under infra/deploy set
// terminationGracePeriodSeconds, though nats.yaml and kafka.yaml both do. Every
// service therefore had Kubernetes' default of 30 seconds against a shutdown path
// that already cost more than that — and a SIGKILL that lands mid-drain abandons
// exactly the in-flight events the drain exists to finish.
//
// TestServiceDrainBudgetMatchesTheCode below re-derives the floor from the source
// on every run and logs it, which is why no number is written here: a comment
// stating a timeout is a comment that outlives the timeout. The three that were
// written down anyway — in this header, in availability.yaml and in twenty-two
// manifests — all still said 27 when the real figure had grown to 37 (#264).
func TestEveryWorkloadDeclaresItsDisruptionPosture(t *testing.T) {
	root := moduleRoot(t)
	est := k8sEstate(t, root)

	// NON-VACUITY: both arms iterate the workload set. An empty or truncated scan
	// would report a fully-budgeted estate.
	if len(est.workloads) < 20 {
		t.Fatalf("scanned infra/deploy and found only %d workloads — this estate has more than twenty. "+
			"A disruption-coverage check over almost no workloads passes trivially.", len(est.workloads))
	}
	if len(est.pdbs) < 5 {
		t.Fatalf("found only %d PodDisruptionBudget documents under infra/deploy. Finding almost none "+
			"means the PDB parse is broken, not that the estate stopped budgeting disruption.", len(est.pdbs))
	}

	// DEAD-ENTRY CHECK. An exemption naming a workload that no longer exists is a
	// waiver protecting nothing while reading as a reviewed decision.
	liveWorkloads := map[string]bool{}
	for _, w := range est.workloads {
		liveWorkloads[w.namespace+"/"+w.name] = true
	}
	var dead []string
	for key := range noDisruptionBudget {
		if !liveWorkloads[key] {
			dead = append(dead, key)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("noDisruptionBudget names %d workload(s) that no longer exist: %s\n\n"+
			"Update the key if it was renamed, delete the entry if it was removed. A stale exemption "+
			"cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}

	drainBudget := serviceDrainBudgetSeconds(t, root)
	var problems []string

	covered := map[int]bool{} // index into est.pdbs
	for _, w := range est.workloads {
		key := w.namespace + "/" + w.name

		// --- ARM 1: a disruption budget, or a written waiver ---------------
		reason, exempt := noDisruptionBudget[key]
		var match *k8sPDB
		for i := range est.pdbs {
			if pdbSelects(&est.pdbs[i], &w) {
				covered[i] = true
				if match == nil {
					match = &est.pdbs[i]
				}
			}
		}
		switch {
		case exempt && strings.TrimSpace(reason) == "":
			problems = append(problems, fmt.Sprintf("%s: %q is in noDisruptionBudget with no reason written", w.file, key))
		case exempt:
			// waived, on the record
		case match == nil:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q (namespace %q, replicas %s) has NO PodDisruptionBudget selecting its pods. "+
					"An unbudgeted workload is evicted by `kubectl drain` on the node's schedule, not "+
					"yours — every replica at once if they happen to share a node.\n      %s",
				w.file, w.kind, w.name, w.namespace, k8sIntText(w.replicas), disruptionPostureRule))

		// --- ARM 2: the budget must have the right SHAPE -------------------
		//
		// A PDB that exists but bounds nothing is worse than none: it reads as a
		// decision. `minAvailable: 1` on a KEDA-scaled Deployment permits evicting
		// every pod but one — fifteen of sixteen, on market-data, during exactly
		// the backlog that caused the scale-up.
		case match.minAvailable == "" && match.maxUnavailable == "":
			problems = append(problems, fmt.Sprintf(
				"%s: PodDisruptionBudget %q selects %s %q but sets neither minAvailable nor maxUnavailable. "+
					"That is not a budget; it is an object that looks like one.",
				match.file, match.name, w.kind, w.name))
		case match.maxUnavailable == "0":
			problems = append(problems, fmt.Sprintf(
				"%s: PodDisruptionBudget %q sets maxUnavailable: 0 for %s %q. That blocks eviction "+
					"permanently AND at every replica count — including during a rollout, which cannot "+
					"then complete. If the intent is an eviction wall on a singleton, say so the way the "+
					"rest of the estate does: minAvailable: 1 on a replicas: 1 workload.",
				match.file, match.name, w.kind, w.name))
		}

		// --- ARM 3: a chosen, sufficient grace period ----------------------
		switch {
		case w.grace == nil:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets no terminationGracePeriodSeconds, so it inherits Kubernetes' default of 30. "+
					"This estate's shutdown path needs %ds (see TestServiceDrainBudgetMatchesTheCode), which "+
					"leaves a margin of %ds — a SIGKILL at the 30-second mark lands mid-drain and abandons "+
					"the in-flight events the drain exists to finish. Declare the number.",
				w.file, w.kind, w.name, drainBudget, 30-drainBudget))
		case *w.grace < drainBudget:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q allows %ds to terminate, but this estate's shutdown path takes up to %ds "+
					"(bus drain + HTTP shutdown + OTLP flush + any budget a main reaches through a package "+
					"it calls, run sequentially — re-derived from the source by "+
					"TestServiceDrainBudgetMatchesTheCode, which logs the breakdown). The kubelet SIGKILLs "+
					"the process partway through its own drain.",
				w.file, w.kind, w.name, *w.grace, drainBudget))
		case *w.grace > maxDrainGraceSeconds:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q allows %ds to terminate. A grace period is a promise `kubectl drain` WAITS on, "+
					"and a node's drain takes as long as its slowest pod — past %ds this turns a routine "+
					"upgrade into a stuck node. If a workload genuinely needs longer, it needs a preStop "+
					"hook and a reason, not a larger number.",
				w.file, w.kind, w.name, *w.grace, maxDrainGraceSeconds))
		}
	}

	// --- ARM 4: no PDB may select nothing ----------------------------------
	//
	// The other direction, and the one a coverage check alone always misses. A
	// PDB whose selector has drifted off its workload — a renamed app label, a
	// tenant stamp added to the pods but not the budget — still exists, still
	// appears in `kubectl get pdb`, and protects zero pods. It reads as coverage
	// from every angle except the only one that matters.
	for i := range est.pdbs {
		if covered[i] {
			continue
		}
		p := est.pdbs[i]
		problems = append(problems, fmt.Sprintf(
			"%s: PodDisruptionBudget %q (namespace %q, selector %v) selects NO workload declared under "+
				"infra/deploy. It will be listed by `kubectl get pdb` and protect nothing — either its "+
				"selector drifted off the pod labels it was written for, or the workload it guarded is gone.",
			p.file, p.name, p.namespace, k8sSortedLabels(p.selector)))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the estate's voluntary-disruption posture is undeclared or unenforceable:\n\n  %s\n\n%s",
			strings.Join(problems, "\n\n  "), disruptionPostureRule)
	}
}

const disruptionPostureRule = "RULE: every workload under infra/deploy carries a PodDisruptionBudget beside it " +
	"and an explicit terminationGracePeriodSeconds, or a written waiver in noDisruptionBudget. " +
	"replicas >= 2 takes `maxUnavailable: 1` (it bounds the drain at ANY replica count, which " +
	"minAvailable: 1 does not once KEDA is scaling); a correctness-pinned singleton takes " +
	"`minAvailable: 1`, which is zero allowed disruptions ON PURPOSE — a drain that must wait for an " +
	"operator is loud and recoverable, a fill lost while the only exchange session reschedules is " +
	"neither. The full reasoning, including what that costs at node-upgrade time, is in " +
	"infra/deploy/availability.yaml."

// maxDrainGraceSeconds bounds the grace period from above. Only checking the
// floor is a half-check: `terminationGracePeriodSeconds: 3600` satisfies it and
// makes every node drain on the estate take an hour, which is how "we are
// draining that node" becomes "that node is stuck". 90s is comfortably above the
// derived need and below the point at which a rolling node upgrade stops being
// routine. (infra/kafka/kafka.yaml's 120 is deliberately outside this tree: a
// broker flushing page cache and closing partitions genuinely has more to do
// than any of these services, and it is a StatefulSet drained one pod at a time.)
const maxDrainGraceSeconds = 90

// noDisruptionBudget is the default-deny allow-list of workloads permitted to run
// with no PodDisruptionBudget. Keyed "<namespace>/<name>"; a dead entry fails the
// guard above, so a waiver cannot outlive the workload it covered.
var noDisruptionBudget = map[string]string{
	"kanz-services/postgres": "postgres-dev.yaml carries kanz.eighred.com/posture: dev-only and " +
		"test/arch/rig_dev_posture_test.go holds it to that: it is the RIG's database, backed by an " +
		"emptyDir, and it exists so a developer can run the estate without CNPG. A minAvailable: 1 PDB " +
		"on its single replica would make allowed-disruptions zero — blocking node drains on the dev rig " +
		"to protect a database whose entire contents are discarded when the pod restarts anyway. There is " +
		"no durable state here to lose and therefore no disruption to budget. If this workload ever stops " +
		"being dev-only it stops qualifying, and the posture guard is what would have to change first. " +
		"Tracked by #234 alongside the rest of the drain posture.",
}

// pdbSelects reports whether a PodDisruptionBudget's selector matches a
// workload's pod template — the same subset rule the API server applies.
//
// Subset, not equality, and that is load-bearing in both directions here. The
// base `oms` PDB selects on `app: oms` alone, so it legitimately covers the
// per-tenant oms-acme pods too (their labels are a superset). An empty selector
// is NOT a match: in a PDB it means "every pod in the namespace", which no
// manifest on this estate declares and which would silently mark every workload
// covered.
func pdbSelects(p *k8sPDB, w *k8sWorkload) bool {
	if p.namespace != w.namespace || len(p.selector) == 0 || len(w.podLabels) == 0 {
		return false
	}
	for k, v := range p.selector {
		if w.podLabels[k] != v {
			return false
		}
	}
	return true
}

func k8sSortedLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// THE GRACE PERIOD IS DERIVED FROM THE CODE, AND STAYS DERIVED.
//
// Whatever the manifests say is only defensible while the shutdown path still
// costs less. If someone raises bus.defaultDrainGrace to 30s, or a service's
// http.Server shutdown budget to 60s, every manifest's grace period becomes too
// short at once and NOTHING would say so — the number would sit in twenty-four
// files looking deliberate. This test recomputes the floor from the source so
// that change fails the build in TestEveryWorkloadDeclaresItsDisruptionPosture
// above rather than at the next node upgrade.
//
// The floor is deliberately an UPPER BOUND on what the process can spend, not a
// prediction of what it will: the estate-wide bus term is added to every
// service, including the two shapes that already contain it (risk-engine's drain
// watchdog, accounting/wealth/alternatives' join budget). Over-stating the worst
// case costs headroom in a manifest; under-stating it costs a truncated drain,
// and only one of those is recoverable.
func TestServiceDrainBudgetMatchesTheCode(t *testing.T) {
	root := moduleRoot(t)
	bus := busDrainSeconds(t, root)
	svc, worst := worstServiceShutdownSeconds(t, root)

	if bus <= 0 || svc <= 0 {
		t.Fatalf("derived a bus drain of %ds and a worst-case service shutdown of %ds — a zero here "+
			"means the source scan stopped matching, and every grace-period check downstream would "+
			"then compare against a floor of nothing", bus, svc)
	}
	got := serviceDrainBudgetSeconds(t, root)
	if got != bus+svc {
		t.Fatalf("drain budget %ds != bus %ds + service %ds", got, bus, svc)
	}
	t.Logf("drain budget = %ds bus (pkg/bus/nats.go: DrainGrace + drainStopSlack) + %ds sequential "+
		"shutdown in %s = %ds; manifests must allow at least this and at most %ds",
		bus, svc, worst, got, maxDrainGraceSeconds)

	// The floor must stay under the ceiling, or the two arms of the manifest
	// check contradict each other and NO value can satisfy both.
	if got >= maxDrainGraceSeconds {
		t.Fatalf("the derived drain budget (%ds) has reached maxDrainGraceSeconds (%ds). The shutdown "+
			"path has grown past what a routine node drain can absorb: shorten it, or raise the ceiling "+
			"with a written reason — do not leave a rule no manifest can satisfy.", got, maxDrainGraceSeconds)
	}
}

func serviceDrainBudgetSeconds(t *testing.T, root string) int {
	t.Helper()
	svc, _ := worstServiceShutdownSeconds(t, root)
	return busDrainSeconds(t, root) + svc
}

var goSecondsConst = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w+)\s*=\s*(\d+)\s*\*\s*time\.Second`)

// busDrainSeconds reads defaultDrainGrace + drainStopSlack out of pkg/bus/nats.go.
// Subscribe's shutdown waits `grace`, then gives up `drainStopSlack` later
// (nats.go's `case <-time.After(grace + drainStopSlack)`), so the two add.
func busDrainSeconds(t *testing.T, root string) int {
	t.Helper()
	src := readFile(t, filepath.Join(root, "pkg", "bus", "nats.go"))
	want := map[string]int{"defaultDrainGrace": 0, "drainStopSlack": 0}
	for _, m := range goSecondsConst.FindAllStringSubmatch(src, -1) {
		if _, ok := want[m[1]]; !ok {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("pkg/bus/nats.go: %s = %q is not a whole number of seconds", m[1], m[2])
		}
		want[m[1]] = n
	}
	for name, n := range want {
		if n == 0 {
			t.Fatalf("pkg/bus/nats.go no longer declares `%s = N * time.Second`. This guard derives every "+
				"manifest's terminationGracePeriodSeconds from those two constants; if they moved or changed "+
				"units, point it at the new spelling rather than hardcoding the old number here.", name)
		}
	}
	return want["defaultDrainGrace"] + want["drainStopSlack"]
}

var goShutdownTimeout = regexp.MustCompile(`context\.WithTimeout\(context\.Background\(\),\s*(\d+)\s*\*\s*time\.Second\)`)

// serviceMains returns every services/*/cmd/*/main.go, module-relative with
// forward slashes, sorted.
func serviceMains(t *testing.T, root string) []string {
	t.Helper()
	abs, err := filepath.Glob(filepath.Join(root, "services", "*", "cmd", "*", "main.go"))
	if err != nil {
		t.Fatalf("glob service mains: %v", err)
	}
	if len(abs) < 15 {
		t.Fatalf("found only %d services/*/cmd/*/main.go files — this module has far more, so the "+
			"worst-case shutdown budget below would be derived from an unrepresentative sample", len(abs))
	}
	out := make([]string, 0, len(abs))
	for _, p := range abs {
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			t.Fatalf("relativise %s: %v", p, rerr)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

// worstServiceShutdownSeconds sums the shutdown budgets each service's main
// SPENDS — the ones spelled in the main plus the ones it reaches through a
// package it calls — and returns the largest total, with the service it came
// from.
//
// Every `context.WithTimeout(context.Background(), N*time.Second)` in a
// services/*/cmd/*/main.go is a shutdown budget — the http.Server drain and the
// deferred observability flush — and they run SEQUENTIALLY on the way out, so
// within one file they add. context.Background() is what distinguishes them:
// anything on the running path derives from the signal context and is already
// cancelled by the time these start.
//
// # A budget reached THROUGH a main is still spent by the pod (#264)
//
// This used to read the mains and nothing else, and risk-engine does not spell
// its drain in one: cmd/risk-engine/main.go calls app.App.Run, and the whole
// ordered drain — await consumers, flush debounced recomputes, checkpoint
// durable state, close transports — runs under DefaultShutdownTimeout in
// services/risk-engine/internal/app/lifecycle.go. The guard derived 20s for
// risk-engine while the process could spend 40, and raising that const to 120s
// moved the derived budget by exactly zero seconds. A grace period derived from
// a number that cannot move is not derived from anything.
//
// # Why a declared site, and not a walk that adds up whatever it finds
//
// The two available shapes fail in opposite directions, and only one of those
// failures announces itself.
//
// A walk that SUMS is silent when it misses: a duration assembled from config, a
// budget behind an interface, a `time.After` instead of a context — each is
// simply absent from the total, and an under-count is indistinguishable from a
// correct count. That is the defect being fixed here; reintroducing it in a
// wider form is not a fix.
//
// A declaration is silent in the other direction: someone adds a lifecycle
// timeout and never lists it. So the declaration alone is not enough either.
//
// Both are therefore used, for different jobs. The ARITHMETIC comes from
// serviceShutdownExtras, whose values are read out of the source on every run,
// so the number cannot drift from the const. The WALK contributes no number at
// all — TestNoUndeclaredShutdownBudgetReachableFromAMain below uses it purely as
// a default-deny detector, and fails on any shutdown-shaped timeout in a main's
// import closure that is neither declared here nor waived with a reason. The
// walk may still miss a spelling. What it cannot do is let a budget it DID find
// go uncounted, and that is the failure mode that produced this issue.
func worstServiceShutdownSeconds(t *testing.T, root string) (int, string) {
	t.Helper()
	best, who := 0, ""
	for _, rel := range serviceMains(t, root) {
		total := 0
		for _, m := range goShutdownTimeout.FindAllStringSubmatch(readFile(t, filepath.Join(root, filepath.FromSlash(rel))), -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s: shutdown timeout %q is not a whole number of seconds", rel, m[1])
			}
			total += n
		}
		total += shutdownExtraSeconds(t, root, rel)
		if total > best {
			best, who = total, rel
		}
	}
	return best, who
}

// shutdownSite is one shutdown budget a service main reaches but does not
// spell: where it is written, what carries it, and why it is on the way out.
type shutdownSite struct {
	// file is module-relative with forward slashes.
	file string
	// name is the const/var identifier, or the context variable of a
	// `<name>, cancel := context.WithTimeout(context.Background(), N*time.Second)`.
	name string
	// absentInMain is a spelling that must NOT appear in the main. A declared
	// default is only the live value while the main leaves it alone; if the
	// composition root starts overriding it, this table is quoting a number the
	// process no longer uses. Empty ⇒ no such override exists to guard against.
	absentInMain string
	// why states what the budget bounds and why it ADDS to the main's own,
	// rather than overlapping with it.
	why string
}

// serviceShutdownExtras declares the shutdown budgets that are spent by a
// service main but written somewhere else. Keyed by the main's module-relative
// path. Default-deny: TestNoUndeclaredShutdownBudgetReachableFromAMain fails on
// any shutdown-shaped timeout in a main's import closure that is missing here,
// and on any entry here the walk can no longer find — so neither a new budget
// nor a deleted one can go unnoticed.
var serviceShutdownExtras = map[string][]shutdownSite{
	"services/risk-engine/cmd/risk-engine/main.go": {{
		file:         "services/risk-engine/internal/app/lifecycle.go",
		name:         "DefaultShutdownTimeout",
		absentInMain: "ShutdownTimeout:",
		why: "App.Run puts the ENTIRE ordered drain under this watchdog — await consumers, flush " +
			"debounced recomputes, checkpoint durable state, close transports — and it runs to " +
			"completion before main.go reaches httpSrv.Shutdown. It therefore ADDS to the 15s HTTP " +
			"drain and 5s OTLP flush spelled in the main; it does not overlap them. The bus drain " +
			"is the one thing inside it (step 2 waits for Subscribe to return), which is why the " +
			"estate-wide bus term added by busDrainSeconds makes this service's floor conservative " +
			"rather than short.",
	}},
}

// shutdownExtraSeconds totals the declared out-of-main budgets for one main,
// reading each value out of the source so the number here can never disagree
// with the const.
func shutdownExtraSeconds(t *testing.T, root, mainRel string) int {
	t.Helper()
	total := 0
	for _, s := range serviceShutdownExtras[mainRel] {
		if strings.TrimSpace(s.why) == "" {
			t.Fatalf("serviceShutdownExtras[%q] lists %s:%s with no reason written. An entry with no "+
				"stated reason is a number in the drain budget that no reviewer can check.",
				mainRel, s.file, s.name)
		}
		vals := shutdownSiteSeconds(t, root, s)
		switch {
		case len(vals) == 0:
			t.Fatalf("serviceShutdownExtras[%q] names %s:%s, but that file no longer declares `%s` as a "+
				"whole number of seconds. Point the entry at the new spelling or delete it — a dead "+
				"entry silently removes its seconds from the derived drain budget, which is exactly "+
				"how #264 happened.", mainRel, s.file, s.name, s.name)
		case len(vals) > 1:
			t.Fatalf("serviceShutdownExtras[%q] names %s:%s, but that file declares it %d times with "+
				"differing values %v. The guard cannot pick one; give the shutdown budget a single "+
				"declaration.", mainRel, s.file, s.name, len(vals), vals)
		}
		if s.absentInMain != "" && strings.Contains(readFile(t, filepath.Join(root, filepath.FromSlash(mainRel))), s.absentInMain) {
			t.Fatalf("%s now contains %q, so it overrides %s:%s and the %ds this guard just read from "+
				"there is not what the process spends. Declare the override's value instead.",
				mainRel, s.absentInMain, s.file, s.name, vals[0])
		}
		total += vals[0]
	}
	return total
}

// shutdownSiteSeconds returns the distinct second values a named site declares
// in its file — a const/var assignment, or a context.WithTimeout bound to that
// variable name.
func shutdownSiteSeconds(t *testing.T, root string, s shutdownSite) []int {
	t.Helper()
	q := regexp.QuoteMeta(s.name)
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+|var\s+)?` + q + `\s*:?=\s*(\d+)\s*\*\s*time\.Second\b` +
		`|\b` + q + `\s*,\s*\w+\s*:?=\s*context\.WithTimeout\(context\.Background\(\),\s*(\d+)\s*\*\s*time\.Second\)`)
	body := readFile(t, filepath.Join(root, filepath.FromSlash(s.file)))
	seen := map[int]bool{}
	var out []int
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s: %s = %q is not a whole number of seconds", s.file, s.name, raw)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// shutdownCtxSite matches the exact spelling worstServiceShutdownSeconds treats
// as a shutdown budget: a context.WithTimeout off context.Background() with a
// literal number of seconds. Background() is the tell — anything on the running
// path derives from the signal context and is already cancelled by then.
var shutdownCtxSite = regexp.MustCompile(`(\w+)\s*,\s*\w+\s*:?=\s*context\.WithTimeout\(context\.Background\(\),\s*(\d+)\s*\*\s*time\.Second\)`)

// namedDurationSite matches a const or var whose NAME says it bounds a
// shutdown. It catches the shape a context.WithTimeout does not: a budget
// declared once and passed around, which is what lifecycle.go does.
var namedDurationSite = regexp.MustCompile(`(?mi)^\s*(?:const\s+|var\s+)?(\w*(?:shutdown|drain|terminat|graceful)\w*)\s*=\s*(\d+)\s*\*\s*time\.Second`)

// notAShutdownBudget waives sites the walk finds that do not lengthen a pod's
// termination. Keyed "<module-relative file>:<identifier>". Default-deny with a
// dead-entry check: a waiver whose site the walk can no longer see fails the
// guard, so a waiver cannot outlive the code it excused — and if the walk itself
// stops matching, every entry here goes dead at once and says so.
var notAShutdownBudget = map[string]string{
	"pkg/bus/nats.go:defaultDrainGrace": "counted ONCE for the whole estate by busDrainSeconds, not " +
		"per service: every service reaches pkg/bus, and adding it per main would multiply one " +
		"7-second drain by twenty-six.",
	"pkg/bus/nats.go:drainStopSlack": "the other half of the same estate-wide bus term; see " +
		"defaultDrainGrace above.",
	"services/operator/internal/nodeops/nodeops.go:drainRetryInterval": "the interval between retries " +
		"of a KUBERNETES node drain the operator performs on OTHER pods. It bounds nothing about the " +
		"operator's own termination — the operator pod's shutdown is the 5s OTLP flush in its main.",
	"services/market-data/internal/feed/conform.go:ctx": "a running-path fetch of the vendor's " +
		"instrument conformance data. It is off context.Background() because it must outlive the " +
		"request that triggered it, not because it runs after SIGTERM.",
	"services/datamaster/internal/store/lock.go:ctx": "a running-path advisory-lock release, bounded " +
		"off Background so the release still runs when the caller's context is already cancelled. It " +
		"happens per operation, not on the way out.",
	"services/oms/internal/outbox/postgres.go:ctx": "a running-path outbox row update, bounded off " +
		"Background for the same reason: the row must be marked whether or not the caller's context " +
		"survived. Not part of the drain.",
}

// EVERY SHUTDOWN BUDGET A MAIN CAN REACH IS EITHER COUNTED OR WAIVED.
//
// The half of #264 that keeps it fixed. worstServiceShutdownSeconds adds the
// budgets declared in serviceShutdownExtras; this walks each service main's
// same-module import closure and fails on a shutdown-shaped timeout that is in
// neither table. Without it, the next lifecycle timeout — #233 left
// awaitConsumers copied into accounting, wealth and alternatives, all three
// naming lifecycle.go as its proper home, so promoting it is the intended next
// change — would land in a package no main spells and drop straight back out of
// the derived grace period.
//
// The dead-entry arms are also this test's non-vacuity proof: every declared
// site and every waiver must be FOUND by the walk. If the closure resolution or
// either pattern stops matching, the entries go dead and the guard fails, rather
// than quietly approving an estate it never looked at.
func TestNoUndeclaredShutdownBudgetReachableFromAMain(t *testing.T) {
	root := moduleRoot(t)

	byPath := map[string]pkgInfo{}
	for _, p := range loadPackages(t) {
		byPath[p.ImportPath] = p
	}

	declared := map[string]map[string]bool{} // main -> "<file>:<name>"
	for mainRel, sites := range serviceShutdownExtras {
		declared[mainRel] = map[string]bool{}
		for _, s := range sites {
			declared[mainRel][s.file+":"+s.name] = true
		}
	}

	sitesFound := map[string][]string{} // "<file>:<name>" -> mains that reach it
	scanned := map[string][]string{}    // import path -> site keys, cached
	pkgsWalked := map[string]bool{}
	var problems []string

	for _, mainRel := range serviceMains(t, root) {
		mainPkg := modulePath + "/" + path.Dir(mainRel)
		if _, ok := byPath[mainPkg]; !ok {
			t.Fatalf("`go list ./...` did not report %s. The import closure for %s would then be empty "+
				"and every budget it reaches would go uncounted — fix the package resolution, do not "+
				"skip the main.", mainPkg, mainRel)
		}
		for _, dep := range inModuleClosure(byPath, mainPkg) {
			// The main itself is read directly by worstServiceShutdownSeconds.
			if dep == mainPkg {
				continue
			}
			pkgsWalked[dep] = true
			keys, done := scanned[dep]
			if !done {
				keys = shutdownSitesInPackage(t, root, dep)
				scanned[dep] = keys
			}
			for _, key := range keys {
				sitesFound[key] = append(sitesFound[key], mainRel)
				if declared[mainRel][key] {
					continue
				}
				if reason, waived := notAShutdownBudget[key]; waived {
					if strings.TrimSpace(reason) == "" {
						problems = append(problems, fmt.Sprintf("%s is in notAShutdownBudget with no reason written", key))
					}
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s is reachable from %s and is NOT counted in its drain budget.\n      "+
						"A timeout the pod spends on the way out but the guard cannot see is how "+
						"terminationGracePeriodSeconds silently stops covering the drain (#264). Either add it "+
						"to serviceShutdownExtras[%q] with what it bounds, or waive it in notAShutdownBudget "+
						"with why it is not on the shutdown path.", key, mainRel, mainRel))
			}
		}
	}

	// NON-VACUITY / DEAD ENTRIES: everything either table names must still be
	// visible to the walk.
	for mainRel, sites := range serviceShutdownExtras {
		for _, s := range sites {
			key := s.file + ":" + s.name
			if !contains(sitesFound[key], mainRel) {
				problems = append(problems, fmt.Sprintf(
					"serviceShutdownExtras[%q] declares %s, but the walk of that main's import closure "+
						"does not reach it. Either the main stopped calling that package — in which case "+
						"its seconds are still being added to a budget nothing spends — or the closure "+
						"resolution is broken and this guard is checking nothing.", mainRel, key))
			}
		}
	}
	for key := range notAShutdownBudget {
		if len(sitesFound[key]) == 0 {
			problems = append(problems, fmt.Sprintf(
				"notAShutdownBudget waives %s, which the walk no longer finds. Delete the waiver if the "+
					"code is gone; if the code is still there, the walk stopped matching it and every "+
					"OTHER site it should have caught is now invisible too.", key))
		}
	}
	if len(pkgsWalked) < 100 {
		t.Fatalf("walked only %d in-module packages across every service main — this module has far "+
			"more, so a clean result below would mean the closure was truncated, not that the estate "+
			"declares its budgets.", len(pkgsWalked))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("shutdown budgets reachable from a service main are unaccounted for:\n\n  %s\n\n"+
			"RULE: the drain budget every manifest's terminationGracePeriodSeconds is derived from "+
			"must include every timeout the process can spend after SIGTERM, including the ones it "+
			"reaches through a package it calls.", strings.Join(problems, "\n\n  "))
	}
	t.Logf("walked %d in-module packages from %d service mains; %d shutdown-shaped site(s) accounted for",
		len(pkgsWalked), len(serviceMains(t, root)), len(sitesFound))
}

// inModuleClosure returns root plus every package inside this module it imports,
// transitively. Third-party and stdlib imports are out of scope: their shutdown
// budgets are not ours to move, and pkg/bus's is added estate-wide instead.
//
// Membership of byPath — what `go list ./...` reported — is the test for "inside
// this module", not the import-path prefix. The generated protobuf SDK is
// github.com/eighred/kanz/kanz-schemas-go/..., which shares the prefix but is a
// separate module living outside this tree.
func inModuleClosure(byPath map[string]pkgInfo, from string) []string {
	seen := map[string]bool{from: true}
	queue := []string{from}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		out = append(out, cur)
		for _, imp := range byPath[cur].Imports {
			if seen[imp] {
				continue
			}
			if _, ok := byPath[imp]; !ok {
				continue
			}
			seen[imp] = true
			queue = append(queue, imp)
		}
	}
	sort.Strings(out)
	return out
}

// shutdownSitesInPackage returns the "<file>:<identifier>" key of every
// shutdown-shaped timeout declared in a package's non-test Go files.
func shutdownSitesInPackage(t *testing.T, root, importPath string) []string {
	t.Helper()
	relDir := strings.TrimPrefix(importPath, modulePath+"/")
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(relDir)))
	if err != nil {
		t.Fatalf("read package dir for %s: %v", importPath, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel := relDir + "/" + name
		body := readFile(t, filepath.Join(root, filepath.FromSlash(rel)))
		for _, re := range []*regexp.Regexp{shutdownCtxSite, namedDurationSite} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				key := rel + ":" + m[1]
				if !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
