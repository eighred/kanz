package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY SUBJECT kanz-py PUBLISHES MUST BE GRANTED TO IT (#112).
//
// # The gap this closes, and why it kept reopening
//
// TestServicePublishesOnlySubjectsItsTenancyPermissionsAllow does exactly this
// for Go, and its failure message says what is at stake: a service with the
// subject missing from its allow-list "will authenticate fine and then be DENIED
// at publish time (a NATS permissions violation), which looks like a broken
// feature, not a missing grant".
//
// It keys on servicesWithEntrypoints — Go packages under kanz/services. The
// inference service is PYTHON. TestThePythonServiceHasABrokerAccount already
// records that consequence in its own header:
//
//	Every guard that checks a service has a broker account keys on
//	servicesWithEntrypoints … The inference service is Python, lives in
//	kanz-py/, and was therefore invisible to all of them.
//
// It closed the "no account at all" half. This closes the other half: an account
// that exists and does not permit what the code actually publishes. #112 hit it
// immediately — kanz-py gained a second publisher (the model registry's append
// log) whose subject was in no allow-list, and every Go guard stayed green.
//
// # What it checks, and the distinction that cost a first attempt
//
// Subjects kanz-py hands to a producer — matched as `subject=NAME` inside an
// `Event(...)` construction — must appear in the inference service's
// tenancy.yaml publish allow-list.
//
// NOT "every SUBJECT_* constant". The first version of this guard did that and
// immediately reported two false positives: data.feature.drift_detected and
// inference.feature.computed are both SUBSCRIPTION subjects, named by constants
// exactly like the published ones. Demanding publish grants for them would have
// widened the allow-list to cover things the service only consumes — an
// over-grant introduced by the guard meant to prevent under-granting.
//
// Declaring a subject and publishing it are different facts, and only one of
// them needs a publish permission.
//
// It does NOT check the reverse direction (a granted subject nothing publishes).
// An over-grant is worth knowing about too, but it is a different failure with a
// different cost, and folding them together would make one message answer two
// questions.
func TestPythonPublishedSubjectsAreGranted(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)
	pyRoot := filepath.Join(repoRoot, "kanz-py")

	if _, err := os.Stat(pyRoot); err != nil {
		t.Skipf("kanz-py is not present: %v", err)
	}

	published := pythonPublishedSubjects(t, pyRoot)
	// NON-VACUITY: kanz-py has published inference.prediction.scored since
	// PRED-05, so a scan that finds nothing is broken rather than accurate.
	if len(published) == 0 {
		t.Fatal("found no PUBLISHED subjects in kanz-py — the scanner is broken, not the service " +
			"(inference.prediction.scored has been published since PRED-05)")
	}

	allowed := inferencePublishAllowList(t, root)
	if len(allowed) == 0 {
		t.Fatal("the inference service has no publish allow-list in infra/nats/tenancy.yaml — " +
			"under NATS semantics that is not 'no permissions', it is UNRESTRICTED within the account")
	}

	var problems []string
	for _, subj := range published {
		if allowed[subj] {
			continue
		}
		problems = append(problems, subj)
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("kanz-py publishes %d subject(s) its broker account does not permit:\n  %s\n\n"+
			"The service will authenticate fine and be DENIED at publish time, which looks like a "+
			"broken feature rather than a missing grant — and no Go guard can see it, because they "+
			"all key on kanz/services packages. Add the subject to the inference user's "+
			"permissions.publish.allow in infra/nats/tenancy.yaml.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// pythonPublishedSubjects returns the subjects kanz-py hands to a producer.
//
// TWO STEPS, because the two facts are separate in the source: collect every
// SUBJECT_* definition, then find the ones passed as `subject=` inside an
// `Event(...)` construction — which is the only way a subject reaches the
// producer in this codebase.
//
// A CONSTANT ALONE PROVES NOTHING. data.feature.drift_detected and
// inference.feature.computed are declared exactly like the published subjects
// and are consumed, not published.
func pythonPublishedSubjects(t *testing.T, pyRoot string) []string {
	t.Helper()
	defRe := regexp.MustCompile(`(?m)^(SUBJECT_[A-Z0-9_]*)\s*=\s*"([^"]+)"`)
	// `Event(` followed, within the construction, by `subject=NAME`. Bounded to
	// a few hundred characters so it cannot reach past the call it is reading.
	pubRe := regexp.MustCompile(`(?s)Event\(.{0,400}?subject=(SUBJECT_[A-Z0-9_]*)`)

	defs := map[string]string{}
	used := map[string]bool{}
	err := filepath.Walk(pyRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".py") {
			return nil
		}
		// Tests declare their own fixtures; the grant is about production code.
		if strings.Contains(filepath.ToSlash(path), "/tests/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			defs[m[1]] = m[2]
		}
		for _, m := range pubRe.FindAllStringSubmatch(src, -1) {
			used[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk kanz-py: %v", err)
	}
	// NON-VACUITY: the definition scan must have found something, or the
	// publish scan below resolves every name to nothing and the guard passes.
	if len(defs) == 0 {
		t.Fatal("found no SUBJECT_* definitions in kanz-py — the scanner is broken")
	}
	seen := map[string]bool{}
	var out []string
	for name := range used {
		subj, ok := defs[name]
		if !ok {
			t.Errorf("kanz-py publishes on %s, which is defined nowhere this guard can see — "+
				"it cannot check a grant for a subject it cannot resolve", name)
			continue
		}
		if !seen[subj] {
			seen[subj] = true
			out = append(out, subj)
		}
	}
	sort.Strings(out)
	return out
}

// inferencePublishAllowList reads the subjects the inference SPIFFE user may
// publish, from the production tenancy manifest ops actually applies.
func inferencePublishAllowList(t *testing.T, root string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	// The accounts block is NATS conf embedded in a ConfigMap, not YAML the
	// decoder can walk into — so the user's block is located textually and its
	// publish allow-list read from it. Bounded to that block, so another
	// service's grant cannot satisfy this one.
	const user = `sa/inference"`
	i := strings.Index(string(b), user)
	if i < 0 {
		t.Fatal("no inference user in infra/nats/tenancy.yaml — the Python service cannot " +
			"authenticate to the broker at all (SEC-M3)")
	}
	rest := string(b)[i:]
	// Stop at the next user block so the scan cannot wander into a neighbour's
	// permissions and report them as this service's.
	if j := strings.Index(rest[1:], `{ user: "spiffe://`); j >= 0 {
		rest = rest[:j+1]
	}
	pubAt := strings.Index(rest, "publish:")
	subAt := strings.Index(rest, "subscribe:")
	if pubAt < 0 {
		return nil
	}
	if subAt > pubAt {
		rest = rest[pubAt:subAt]
	} else {
		rest = rest[pubAt:]
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(rest, -1) {
		out[m[1]] = true
	}
	return out
}
