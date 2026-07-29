package arch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A SERVICE PINNED TO ONE REPLICA MUST NOT BE SCALED PAST IT BY A DR PROCEDURE.
//
// infra/dr/failover.sh step 4 ran `k -n "$SVC_NS" scale deploy --all --replicas=2`
// (#149). `--all` is unqualified, and eight Deployments in that namespace pin
// `replicas: 1` as a CORRECTNESS bound, not a cost one — so the documented region
// cutover brought the estate up in a configuration those manifests forbid, and it
// did so only during a real failover, when attention is scarcest. The failure modes
// are not degradations and they do NOT heal when the pod count comes back down:
// venue-binance/venue-okx share one exchange API key and can get the account
// BANNED; archiver can invert per-key order in the durable log of record;
// market-ingest and tv-sync each fold two competing books.
//
// The repair is that the manifest carries the fact and the script selects on it:
// `kanz.io/singleton: "true"` on the Deployment's own metadata.labels, and
// `scale deploy -l 'kanz.io/singleton' --replicas=1` instead of `--all`. A
// hardcoded list of singletons inside failover.sh would have been a SECOND copy of
// the same fact, and the second copy is the one that drifts — which is the defect
// this guard exists to make impossible, not merely unlikely.
//
// So this guard checks the label in BOTH directions. One direction alone is a
// half-check: "every singleton is labelled" lets the label rot onto a Deployment
// that has since been scaled to 3 (and DR would then pin a horizontally-scaled
// service to one pod, an outage), and "every label is honest" is satisfied by an
// estate with no labels at all.
func TestSingletonDeploymentsCarryTheDRSingletonLabel(t *testing.T) {
	root := moduleRoot(t)
	deployments := drDeploymentManifests(t, root)

	// Non-vacuity. A scan that finds nothing must FAIL, not pass: a moved
	// directory, a renamed field or a parser that silently skipped every document
	// would otherwise report a clean estate, which is exactly the shape of false
	// green this repository has been bitten by before.
	if len(deployments) < 10 {
		t.Fatalf("scanned infra/deploy and found only %d Deployment manifests — this estate has "+
			"far more than that. Finding none (or nearly none) is a BROKEN SCAN reported as a clean "+
			"result. Did infra/deploy move, or did the parse stop early?", len(deployments))
	}
	singletons := 0
	for _, d := range deployments {
		if d.replicas != nil && *d.replicas == 1 {
			singletons++
		}
	}
	if singletons < 5 {
		t.Fatalf("found only %d Deployments pinning replicas: 1 across %d manifests. At the time this "+
			"guard was written there were nine (archiver, compliance, lake-sink, market-ingest, operator, "+
			"postgres, tv-sync, venue-binance, venue-okx). Finding almost none means the replica field is "+
			"not being read, not that the estate stopped having singletons.", singletons, len(deployments))
	}

	var problems []string
	for _, d := range deployments {
		_, labelled := d.labels[drSingletonLabel]
		pinned := d.replicas != nil && *d.replicas == 1

		switch {
		case pinned && !labelled:
			problems = append(problems, fmt.Sprintf(
				"%s: Deployment %q pins replicas: 1 but carries no %s label on its metadata.labels — "+
					"infra/dr/failover.sh would scale it to 2 during a region cutover",
				d.file, d.name, drSingletonLabel))

		case labelled && !pinned:
			problems = append(problems, fmt.Sprintf(
				"%s: Deployment %q carries %s but pins replicas: %s — the label is now a LIE, and DR "+
					"would scale this service DOWN to one pod",
				d.file, d.name, drSingletonLabel, drReplicaText(d.replicas)))

		case labelled && d.labels[drSingletonLabel] != "true":
			problems = append(problems, fmt.Sprintf(
				"%s: Deployment %q sets %s=%q; the only accepted value is \"true\"",
				d.file, d.name, drSingletonLabel, d.labels[drSingletonLabel]))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the singleton label and the replica pin disagree:\n\n  %s\n\n"+
			"RULE, both directions: a Deployment that pins `replicas: 1` carries `%s: \"true\"` on its own "+
			"metadata.labels (NOT on spec.template.metadata.labels — `kubectl scale deploy -l` selects the "+
			"CONTROLLER, so a pod-template label is invisible to it), and nothing else carries that label.\n\n"+
			"The label is how infra/dr/failover.sh tells a correctness pin from a capacity choice without "+
			"keeping its own list of service names. Note the selector matches on KEY EXISTENCE, so setting "+
			"the value to \"false\" does not opt out — it reads as an opt-out while still being scaled to 1. "+
			"If a service genuinely became horizontally safe, raise its replicas AND delete the label in the "+
			"same commit; if it became a singleton, add both.",
			strings.Join(problems, "\n  "), drSingletonLabel)
	}
}

// A DR PROCEDURE MUST NOT SCALE DEPLOYMENTS IT HAS NOT NAMED OR SELECTED.
//
// This is the exact defect from #149, pinned so it cannot come back: `scale deploy
// --all --replicas=N` applies one number to every workload in the namespace, which
// silently overrides every manifest's own replica decision. It is also not
// idempotent with respect to intent — re-running a partial failover re-inflates
// anything an operator had manually corrected back down.
//
// Comments are stripped before the check, so failover.sh can (and does) explain the
// old defect in prose without tripping the guard on its own explanation.
func TestFailoverDoesNotScaleEveryDeploymentBlindly(t *testing.T) {
	root := moduleRoot(t)
	rel := filepath.Join("infra", "dr", "failover.sh")
	script := readFile(t, filepath.Join(root, rel))

	commands := drShellCommandLines(script)
	// Non-vacuity: if comment-stripping ate the whole file, every check below
	// passes against nothing.
	if len(commands) < 10 {
		t.Fatalf("%s yielded only %d non-comment command lines — the script is a runbook of five steps and "+
			"has many more than that. A check over an empty command set passes trivially.",
			filepath.ToSlash(rel), len(commands))
	}

	var problems []string
	for _, cmd := range commands {
		if !strings.Contains(cmd.text, " scale ") {
			continue
		}
		if !drScalesDeployments(cmd.text) {
			continue
		}
		where := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), cmd.line)
		switch {
		case strings.Contains(cmd.text, "--all"):
			problems = append(problems, fmt.Sprintf("%s: %s", where, strings.TrimSpace(cmd.text)))
		case !strings.Contains(cmd.text, " -l ") && !strings.Contains(cmd.text, "--selector") &&
			!strings.Contains(cmd.text, "deploy/") && !strings.Contains(cmd.text, "deployment/"):
			problems = append(problems, fmt.Sprintf("%s (no target selector at all): %s",
				where, strings.TrimSpace(cmd.text)))
		}
	}
	if len(problems) > 0 {
		t.Fatalf("%s scales deployments without qualifying which ones:\n\n  %s\n\n"+
			"`scale deploy --all --replicas=N` forces one replica count onto every workload in the namespace "+
			"and overrides each manifest's own pin. Nine Deployments pin `replicas: 1` as a CORRECTNESS bound: "+
			"the venue adapters share ONE exchange API key (a second pod doubles request weight and can get the "+
			"account banned), archiver is the single writer to the durable log of record, market-ingest/tv-sync/"+
			"compliance each fold a partitioned stream into a book. None of that is repaired by scaling back down.\n\n"+
			"Scale by the label the manifests carry instead:\n"+
			"    k -n \"$SVC_NS\" scale deploy -l '!%s' --replicas=2\n"+
			"    k -n \"$SVC_NS\" scale deploy -l '%s'  --replicas=1\n"+
			"which brings each Deployment up at the count its own manifest asks for, and covers a NEW singleton "+
			"the day it is added rather than the day someone remembers this list.",
			filepath.ToSlash(rel), strings.Join(problems, "\n  "), drSingletonLabel, drSingletonLabel)
	}
}

// drSingletonLabel marks a Deployment whose `replicas: 1` is a correctness bound.
// It lives on the Deployment's OWN metadata.labels because that is what
// `kubectl scale deploy -l` selects on.
const drSingletonLabel = "kanz.io/singleton"

// drDeployment is one Deployment document, reduced to the two facts this guard
// compares: the replica pin and the controller's labels.
type drDeployment struct {
	file     string // repo-relative, slash-separated — named in every failure message
	name     string
	replicas *int // nil ⇒ the field is absent (k8s defaults to 1, but that is not a PIN)
	labels   map[string]string
}

type drSingletonDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
	} `yaml:"spec"`
}

// drDeploymentManifests parses every YAML document under infra/deploy/ (recursively
// — MT-02 renders per-tenant workloads into infra/deploy/tenants/<tenant>/) and
// returns the Deployments.
//
// Parsed structurally with gopkg.in/yaml.v3, like deployability_test.go, not
// grepped: `replicas: 1` appears in prose comments in several of these files, and a
// regex cannot tell a Deployment's own metadata.labels from its pod template's.
//
// Deployment only, deliberately. `kubectl scale deploy` does not touch a
// StatefulSet, a DaemonSet or an Argo Rollout, so a singleton of those kinds is not
// reachable by the defect this guard covers. If a DR step ever scales another kind,
// widen this — do not assume the widening happened.
func drDeploymentManifests(t *testing.T, root string) []drDeployment {
	t.Helper()
	base := filepath.Join(root, "infra", "deploy")

	var out []drDeployment
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		dec := yaml.NewDecoder(strings.NewReader(readFile(t, p)))
		for {
			var doc drSingletonDoc
			derr := dec.Decode(&doc)
			if errors.Is(derr, io.EOF) {
				break
			}
			if derr != nil {
				t.Fatalf("%s: parse as YAML: %v", rel, derr)
			}
			if doc.Kind != "Deployment" {
				continue
			}
			out = append(out, drDeployment{
				file:     rel,
				name:     doc.Metadata.Name,
				replicas: doc.Spec.Replicas,
				labels:   doc.Metadata.Labels,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/deploy: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].name < out[j].name
	})
	return out
}

// drScriptLine is one command line of the script, carrying its 1-based line number
// in the FILE — so a failure message points an operator at the line they must edit,
// not at an index into a filtered slice.
type drScriptLine struct {
	line int
	text string
}

// drShellCommandLines returns the script's lines with `#` comments removed, so a
// comment DESCRIBING the old `--all` defect does not read as the defect. The rule
// is sh's own: a `#` starts a comment when it begins a word.
func drShellCommandLines(script string) []drScriptLine {
	var out []drScriptLine
	for n, line := range strings.Split(strings.ReplaceAll(script, "\r\n", "\n"), "\n") {
		if i := drCommentStart(line); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, drScriptLine{line: n + 1, text: line})
	}
	return out
}

// drCommentStart returns the index of the `#` that starts a comment, or -1.
func drCommentStart(line string) int {
	for i, r := range line {
		if r != '#' {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// drScalesDeployments reports whether a `kubectl scale` line targets Deployments.
// It accepts every spelling kubectl does, so `scale deployments --all` is not a way
// around the guard.
func drScalesDeployments(line string) bool {
	for _, kind := range []string{"deploy", "deployment", "deployments", "deploy.apps", "deployments.apps"} {
		if strings.Contains(line, " "+kind+" ") || strings.Contains(line, " "+kind+"/") {
			return true
		}
	}
	return false
}

// drReplicaText renders a replica pin for a failure message, distinguishing an
// explicit count from an absent field.
func drReplicaText(r *int) string {
	if r == nil {
		return "<unset>"
	}
	return fmt.Sprint(*r)
}
