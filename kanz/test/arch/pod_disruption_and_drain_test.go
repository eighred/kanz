package arch

import (
	"fmt"
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
// that takes up to 27 — three seconds of margin, and a SIGKILL that lands
// mid-drain abandons exactly the in-flight events the drain exists to finish.
//
// TestServiceDrainBudgetMatchesTheCode below re-derives that 27 from the source
// on every run rather than trusting this paragraph, because a comment stating a
// timeout is a comment that outlives the timeout.
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
					"leaves %ds of margin — a SIGKILL at the %ds mark lands mid-drain and abandons the "+
					"in-flight events the drain exists to finish. Declare the number.",
				w.file, w.kind, w.name, drainBudget, 30-drainBudget, drainBudget))
		case *w.grace < drainBudget:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q allows %ds to terminate, but this estate's shutdown path takes up to %ds "+
					"(bus drain + HTTP shutdown + OTLP flush, run sequentially — re-derived from the source "+
					"by TestServiceDrainBudgetMatchesTheCode). The kubelet SIGKILLs the process partway "+
					"through its own drain.",
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
// 45 seconds is only defensible while the shutdown path still costs 27. If
// someone raises bus.defaultDrainGrace to 30s, or a service's http.Server
// shutdown budget to 60s, every manifest's grace period becomes too short at
// once and NOTHING would say so — the number would sit in twenty-three files
// looking deliberate. This test recomputes the floor from the source so that
// change fails the build in TestEveryWorkloadDeclaresItsDisruptionPosture above
// rather than at the next node upgrade.
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

// worstServiceShutdownSeconds sums the shutdown budgets in each service's main
// and returns the largest total, with the service it came from.
//
// Every `context.WithTimeout(context.Background(), N*time.Second)` in a
// services/*/cmd/*/main.go is a shutdown budget — the http.Server drain and the
// deferred observability flush — and they run SEQUENTIALLY on the way out, so
// within one file they add. context.Background() is what distinguishes them:
// anything on the running path derives from the signal context and is already
// cancelled by the time these start.
func worstServiceShutdownSeconds(t *testing.T, root string) (int, string) {
	t.Helper()
	mains, err := filepath.Glob(filepath.Join(root, "services", "*", "cmd", "*", "main.go"))
	if err != nil {
		t.Fatalf("glob service mains: %v", err)
	}
	if len(mains) < 15 {
		t.Fatalf("found only %d services/*/cmd/*/main.go files — this module has far more, so the "+
			"worst-case shutdown budget below would be derived from an unrepresentative sample", len(mains))
	}
	best, who := 0, ""
	for _, p := range mains {
		total := 0
		for _, m := range goShutdownTimeout.FindAllStringSubmatch(readFile(t, p), -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s: shutdown timeout %q is not a whole number of seconds", p, m[1])
			}
			total += n
		}
		if total > best {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				rel = p
			}
			best, who = total, filepath.ToSlash(rel)
		}
	}
	return best, who
}
