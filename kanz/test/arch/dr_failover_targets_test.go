package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE DR CUTOVER SCRIPT NAMES THINGS THAT EXIST, AND VERIFIES THEM AS WHAT THEY ARE (#629).
//
// # What went wrong without it
//
// infra/dr/failover.sh is the executable region cutover — "runbook-as-code ... A
// human runs it during a declared incident". Its wait list contained
// `risk-engine`, verified with `kubectl rollout status deploy/risk-engine`. But
// risk-engine is a kind: Rollout (infra/deploy/risk-engine-rollout.yaml) and
// there is no Deployment by that name anywhere in the estate.
//
// So that command returned `deployments.apps "risk-engine" not found` on every
// run, forever, and the trailing `|| true` erased it. The service that computes
// the fund's risk was the one entry in that list whose check COULD NEVER PASS,
// and the script was written so that read identically to a pass. The same step
// scaled by `deploy`, which resolves to deployments.apps and matches no Rollout,
// so it was never scaled up either.
//
// Nothing else covered it: dr_singleton_replicas_test.go scans Deployment
// manifests and this is not one, so the Rollout sat outside both the guard and
// the procedure. And per #106 there is no DR region to run the script against —
// its first execution is by definition during a region loss.
//
// # Why the guard reads the script rather than trusting a list here
//
// Restating the workload names in Go would be a second list that drifts from the
// shell one, which is the defect class this repository keeps finding. The names
// are read out of failover.sh, and the KINDS are read out of infra/deploy, so
// the assertion is between the two artifacts that actually have to agree.
//
// Comment lines are stripped first: failover.sh now explains the risk-engine
// mismatch in prose, and a scan that matched its own explanation would pass
// while the code said anything at all.
func TestDRFailoverWaitsOnWorkloadsAsTheKindTheyAre(t *testing.T) {
	root := moduleRoot(t)
	script := stripShellComments(readFile(t, filepath.Join(root, "infra", "dr", "failover.sh")))

	deployNames := shellForList(t, script, "d")
	rolloutNames := shellForList(t, script, "r")

	// NON-VACUITY. The wait list is a dozen services. A parse that found none
	// would pass however wrong the script had become — which is precisely the
	// state it was in.
	if len(deployNames) < 8 {
		t.Fatalf("parsed %d Deployment name(s) from failover.sh's wait list, want at least 8 — "+
			"the parse is broken and this guard proves nothing: %v", len(deployNames), deployNames)
	}
	if len(rolloutNames) == 0 {
		t.Fatal("parsed no Rollout names from failover.sh. risk-engine is a kind: Rollout and must be " +
			"waited on as one; a wait list with no Rollouts is the state #629 fixed.")
	}

	kinds := workloadKinds(t, filepath.Join(root, "infra", "deploy"))
	var problems []string
	check := func(names []string, want, verb string) {
		for _, n := range names {
			got, ok := kinds[n]
			if !ok {
				problems = append(problems, n+": failover.sh waits on it, but no workload manifest in "+
					"infra/deploy declares a "+want+" (or anything) by that name. During a cutover the "+
					"wait cannot pass, and the script's `|| true` makes that indistinguishable from a pass.")
				continue
			}
			if got != want {
				problems = append(problems, n+": failover.sh verifies it with "+verb+", which requires a "+
					want+" — but infra/deploy declares it as a "+got+". That check can NEVER succeed, on "+
					"any cluster, and it is silenced by `|| true`.")
			}
		}
	}
	check(deployNames, "Deployment", "`kubectl rollout status deploy/<name>`")
	check(rolloutNames, "Rollout", "wait_rollout_ready (the Rollout status subresource)")

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d workload(s) the DR cutover cannot verify:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// THE SPINE MANIFESTS THE CUTOVER APPLIES ALL EXIST, AND THE DEV ONE IS NOT AMONG THEM (#629).
//
// Step 2 used to be `k apply -f nats/ ... || k apply -f ../nats/`. infra/dr/nats/
// holds only README.md and rebuild-job.yaml, so the first arm applied the REBUILD
// job and exited 0 — the `||` could never fire, and the real spine
// (namespace.yaml, tenancy.yaml, nats.yaml, bootstrap-job.yaml) was never
// applied at all. Step 2 then waited 180s on a Job that had never been created,
// discarded the result, and reported success with no spine standing.
//
// AND THE FALLBACK WOULD HAVE BEEN WORSE THAN THE BUG. ../nats/ also holds
// bootstrap-job-dev-plaintext.yaml, whose first line reads "DEV-ONLY plaintext
// bootstrap. NEVER apply this to a cluster that has SPIRE." Applying the
// directory during a declared incident would have applied that too.
//
// So the script names files, and this checks the two things naming files can get
// wrong: a name that does not exist, and the one name that must never appear.
func TestDRFailoverAppliesRealSpineManifests(t *testing.T) {
	root := moduleRoot(t)
	script := stripShellComments(readFile(t, filepath.Join(root, "infra", "dr", "failover.sh")))

	re := regexp.MustCompile(`(?m)^\s*for m in ([^;]+); do`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatal("failover.sh no longer applies its spine manifests from a `for m in ...` list — if the " +
			"shape changed, move this guard with it. Applying `-f ../nats/` as a DIRECTORY is what this " +
			"exists to prevent: it would apply bootstrap-job-dev-plaintext.yaml to the DR region.")
	}
	names := strings.Fields(m[1])
	if len(names) < 3 {
		t.Fatalf("failover.sh applies only %d spine manifest(s) (%v) — the broker needs at least its "+
			"namespace, its config and the bootstrap Job that creates every JetStream stream", len(names), names)
	}

	var problems []string
	for _, n := range names {
		if n == "bootstrap-job-dev-plaintext.yaml" {
			problems = append(problems, "failover.sh applies bootstrap-job-dev-plaintext.yaml. Its own "+
				"first line reads \"DEV-ONLY plaintext bootstrap. NEVER apply this to a cluster that has "+
				"SPIRE\" — and the DR region has SPIRE.")
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "infra", "nats", n)); err != nil {
			problems = append(problems, n+": failover.sh applies infra/nats/"+n+", which does not exist. "+
				"Under `set -eu` that aborts the cutover at step 2 of 5 — Postgres promoted, no spine, "+
				"no services, no traffic.")
		}
	}

	// tenancy.yaml IS THE EASY ONE TO MISS, so it is named rather than left to
	// the existence check. nats.conf does `include "tenants.conf"`, which arrives
	// from the nats-tenants ConfigMap that file defines — without it the broker
	// does not start. It is excluded from the ApplicationSet on purpose ("applied
	// by their own tooling"), which is exactly why a named-file list has to carry
	// it explicitly and why omitting it would look deliberate.
	if !contains(names, "tenancy.yaml") {
		problems = append(problems, "failover.sh does not apply tenancy.yaml. nats.conf includes "+
			"tenants.conf from the nats-tenants ConfigMap that file defines, so the broker will not "+
			"start without it — and because the ApplicationSet excludes tenancy.yaml deliberately, "+
			"nothing else applies it either.")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d problem(s) in the DR cutover's spine apply:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// shellForList returns the words of `for <v> in <words>; do` in script.
func shellForList(t *testing.T, script, v string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*for ` + regexp.QuoteMeta(v) + ` in ([^;]+); do`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

// stripShellComments removes whole-line # comments so a scan cannot be satisfied
// by the script's own explanation of the bug it fixed.
func stripShellComments(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// workloadKinds maps a workload's metadata.name to its kind across infra/deploy.
// Only the kinds a cutover scales and waits on are collected; a Service or
// ConfigMap of the same name must not answer for a Deployment.
func workloadKinds(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	kindRe := regexp.MustCompile(`(?m)^kind:\s*(\S+)`)
	nameRe := regexp.MustCompile(`(?m)^\s{2}name:\s*(\S+)`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body := readFile(t, filepath.Join(dir, e.Name()))
		for _, doc := range strings.Split(body, "\n---") {
			km := kindRe.FindStringSubmatch(doc)
			if km == nil {
				continue
			}
			kind := km[1]
			if kind != "Deployment" && kind != "Rollout" && kind != "StatefulSet" {
				continue
			}
			if nm := nameRe.FindStringSubmatch(doc); nm != nil {
				out[nm[1]] = kind
			}
		}
	}
	return out
}
