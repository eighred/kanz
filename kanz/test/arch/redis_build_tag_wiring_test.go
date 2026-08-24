package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SERVICE'S BUILD TAG AND ITS ESTATE WIRING MUST AGREE (#111).
//
// There are two shared-state seams behind `-tags redis`, and each service that
// has one ships a PAIR: `//go:build !redis` (per-instance, in-process) and
// `//go:build redis` (cross-pod, Redis-backed). Which one is in the binary is
// decided by the Dockerfile; whether it is USED is decided by a `*_REDIS_URL` in
// the estate. Those are three separate places, and nothing connected them.
//
// So they drifted, silently, in the direction that looks fine:
//
//   - webhook-ingest is coherent — Dockerfile passes `-tags redis`, and
//     webhook-ingest-deploy.yaml sets WEBHOOK_INGEST_REDIS_URL_FILE.
//   - risk-engine is NOT. It runs `replicas: 3` plus a KEDA ScaledObject on
//     PER-INSTANCE dedup, because its Dockerfile carries no tag and nothing sets
//     RISK_ENGINE_REDIS_URL. The cross-pod deduper it needs is written, compiled
//     by CI, and linked into no shipped binary.
//
// AND THE DRIFT WAS ACTIVELY MISLEADING, which is why this is a guard rather
// than a comment. pkg/bus/dedup_redis.go asserted "The one production wiring of
// this deduper is risk-engine (redis build tag, RISK_ENGINE_REDIS_URL set)" and
// used that claim to BOUND a documented claim-inversion — a safety argument
// resting on a wiring that did not exist. A reader sizing that risk would have
// taken the reassurance at face value. Prose cannot keep that honest; this can.
//
// THE FAILURE MODES ARE ASYMMETRIC, so the two checks below are too:
//
//   - Tagged file, untagged Dockerfile: the capability is dead code that ships.
//     Bad, but it degrades to the in-process path, which is a real (if weaker)
//     implementation. Exemptible, with the issue that retires it.
//   - REDIS_URL set on an untagged binary: the operator asked for cross-pod
//     shared state and got per-instance, with only a log line saying so. There
//     is no reading under which that is intended, so it is NOT exemptible.

// redisTagExempt lists services that ship a `redis`-tagged file WITHOUT building
// with the tag, each with the issue that retires the entry. Default-deny: a
// service not listed here must build with the tag.
//
// A DEAD ENTRY IS A FAILURE, not tidy-up: an exemption that outlives its repair
// is how the next person learns the rule is optional.
var redisTagExempt = map[string]string{
	"risk-engine": "#111 — the cross-pod dedup cutover. The tagged file exists and CI compiles it, " +
		"but the Dockerfile does not pass -tags redis and nothing sets RISK_ENGINE_REDIS_URL, so " +
		"`replicas: 3` runs on per-instance dedup. Remove this entry when the cutover lands.",
}

var (
	reRedisBuildTag = regexp.MustCompile(`(?m)^//go:build\s+redis\s*$`)
	reTagsRedis     = regexp.MustCompile(`-tags\s+(redis|"[^"]*\bredis\b[^"]*"|'[^']*\bredis\b[^']*')`)
	reRedisURLEnv   = regexp.MustCompile(`([A-Z0-9_]*_REDIS_URL(?:_FILE)?)`)
)

func TestRedisBuildTagMatchesTheEstateWiring(t *testing.T) {
	root := moduleRoot(t)
	servicesDir := filepath.Join(root, "services")

	tagged := servicesWithRedisTaggedSource(t, servicesDir)

	// NON-VACUITY (1/2). The seams exist; if this finds none, the scan is looking
	// in the wrong place and every assertion below is about nothing.
	if len(tagged) == 0 {
		t.Fatal("found no service with a `//go:build redis` file under services/*/cmd/. The shared-state " +
			"seams have moved or been renamed — MOVE THIS GUARD WITH THEM rather than leaving it " +
			"passing over a tree it no longer understands")
	}

	builds := servicesBuildingWithRedisTag(t, servicesDir)

	// NON-VACUITY (2/2). At least one service is known to build with the tag
	// (webhook-ingest). Zero means the Dockerfile scan is broken, which would
	// make every service look exempt-worthy.
	if len(builds) == 0 {
		t.Fatal("no service Dockerfile builds with -tags redis. webhook-ingest does, so this scan is " +
			"broken rather than the tree being empty")
	}

	// CHECK 1: a tagged source file without a tagged Dockerfile is dead code that ships.
	var undeclared []string
	for _, svc := range tagged {
		if builds[svc] {
			if reason, ok := redisTagExempt[svc]; ok {
				t.Errorf("%s is listed in redisTagExempt but now BUILDS with -tags redis. Delete the "+
					"entry — an exemption that outlives its repair teaches the next reader that the "+
					"rule is optional.\n  stale reason: %s", svc, reason)
			}
			continue
		}
		if _, ok := redisTagExempt[svc]; ok {
			continue
		}
		undeclared = append(undeclared, svc)
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("service(s) %v ship a `//go:build redis` file but their Dockerfile does not pass "+
			"-tags redis.\n\n"+
			"The shipped binary compiles the `!redis` half, so the cross-pod implementation is dead "+
			"code that deploys. Either add the tag, or add an entry to redisTagExempt naming the issue "+
			"that retires it.", undeclared)
	}

	// CHECK 2: the estate asking for Redis on a binary that cannot use it.
	// NOT exemptible — there is no reading under which this is intended.
	for svc, envs := range servicesWiredToRedis(t, filepath.Join(root, "infra")) {
		if !builds[svc] {
			t.Errorf("the estate sets %v for %q, but its Dockerfile does not build with -tags redis.\n\n"+
				"The operator asked for cross-pod shared state and gets PER-INSTANCE state, with at "+
				"most a log line saying so. This is not exemptible: a variable that is read by no code "+
				"in the shipped binary is a configuration that silently does nothing.", envs, svc)
		}
	}
}

// servicesWithRedisTaggedSource returns services having a `//go:build redis` Go
// file anywhere under services/<svc>/ (the seam may live in cmd/ or internal/).
func servicesWithRedisTaggedSource(t *testing.T, servicesDir string) []string {
	t.Helper()
	found := map[string]bool{}
	err := filepath.Walk(servicesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		// Test files do not ship, so a redis-tagged TEST is not evidence that the
		// binary needs the tag.
		if strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !reRedisBuildTag.Match(b) {
			return nil
		}
		rel, _ := filepath.Rel(servicesDir, path)
		found[strings.Split(filepath.ToSlash(rel), "/")[0]] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk services/: %v", err)
	}
	out := make([]string, 0, len(found))
	for s := range found {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// servicesBuildingWithRedisTag returns services whose Dockerfile passes the tag.
func servicesBuildingWithRedisTag(t *testing.T, servicesDir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(servicesDir, e.Name(), "Dockerfile"))
		if rerr != nil {
			continue // not every service ships an image
		}
		if reTagsRedis.Match(stripDockerfileComments(b)) {
			out[e.Name()] = true
		}
	}
	return out
}

// stripDockerfileComments removes comment lines before the -tags scan.
//
// THE GUARD USED TO MATCH ITS OWN DOCUMENTATION. Both Dockerfiles that carry the
// tag also EXPLAIN it in a comment directly above the RUN line, and those
// comments contain the literal string "-tags redis". Matching raw bytes, the
// guard therefore reported a service as building with the tag when the RUN line
// had lost it and only the prose remained — the precise half-wiring CHECK 2
// calls "not exemptible", passing green.
//
// Found by mutation: deleting `-tags redis` from api-gateway's RUN line left
// this guard passing. A guard that greps source must never be able to be
// satisfied by a sentence describing what the code should do.
func stripDockerfileComments(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		kept = append(kept, ln)
	}
	return []byte(strings.Join(kept, "\n"))
}

// servicesWiredToRedis maps service name -> the *_REDIS_URL variables the estate
// sets for it, derived from the manifest FILENAME (svc-deploy.yaml /
// svc-rollout.yaml), which is how this tree names them.
func servicesWiredToRedis(t *testing.T, infraDir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := filepath.Walk(infraDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !(strings.HasSuffix(info.Name(), ".yaml") || strings.HasSuffix(info.Name(), ".yml")) {
			return nil
		}
		base := info.Name()
		svc := ""
		for _, suffix := range []string{"-deploy.yaml", "-rollout.yaml", "-deployment.yaml"} {
			if trimmed, ok := strings.CutSuffix(base, suffix); ok {
				svc = trimmed
			}
		}
		if svc == "" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		seen := map[string]bool{}
		for line := range strings.SplitSeq(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			// Only lines that SET the variable, not prose about it. Comments are
			// where these variables are most often discussed.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, m := range reRedisURLEnv.FindAllString(trimmed, -1) {
				if !seen[m] {
					seen[m] = true
					out[svc] = append(out[svc], m)
				}
			}
		}
		sort.Strings(out[svc])
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/: %v", err)
	}
	return out
}
