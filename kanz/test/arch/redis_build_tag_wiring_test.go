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
// Source capability, selected image and endpoint configuration are separate
// contracts. Existing webhook/gateway images always include their tagged
// adapter. Risk explicitly offers a tagged shared-dedup target while its final
// default target remains redis-free; the executable stage edges are checked
// below. This source guard does not prove a running image or backend.
//
// A configured endpoint on a binary lacking the adapter must refuse startup.
// Risk's runtime tests enforce that refusal; silently using a local window
// would hide an operator's explicit choice. Default per-instance operation is
// deliberate and remains supported without endpoint configuration.

// redisTagExempt lists services that ship a `redis`-tagged file WITHOUT building
// with the tag, each with the issue that retires the entry. Default-deny: a
// service not listed here must build with the tag.
//
// A DEAD ENTRY IS A FAILURE, not tidy-up: an exemption that outlives its repair
// is how the next person learns the rule is optional.
var redisTagExempt = map[string]string{}

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
		found[deployedServiceName(strings.Split(filepath.ToSlash(rel), "/")[0])] = true
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
		if deployedServiceName(e.Name()) == "risk-engine" {
			// Risk has two deliberate image targets; a tag in an unreachable stage
			// or in the default image is not a qualified optional capability.
			if !riskSharedDedupTargetsValid(b) {
				t.Error("risk-engine must retain a redis-free default target and a shared-dedup target linked to its tagged build")
			} else {
				out["risk-engine"] = true
			}
			continue
		}
		if reTagsRedis.Match(stripDockerfileComments(b)) {
			out[deployedServiceName(e.Name())] = true
		}
	}
	return out
}

// riskSharedDedupTargetsValid follows the source -> build -> runtime edges of
// the owning Dockerfile. Existing callers build the last target, so its identity
// is part of the compatibility contract, not merely a named-stage inventory.
func riskSharedDedupTargetsValid(b []byte) bool {
	type stage struct {
		from string
		body string
	}
	stages := map[string]stage{}
	current, last := "", ""
	for _, line := range strings.Split(string(stripDockerfileComments(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "FROM" && fields[2] == "AS" {
			current, last = fields[3], fields[3]
			if _, duplicate := stages[current]; duplicate {
				return false
			}
			stages[current] = stage{from: fields[1]}
			continue
		}
		value := stages[current]
		value.body += line + "\n"
		stages[current] = value
	}
	shared, defaults := stages["shared-dedup"], stages["default"]
	sharedBuild, defaultBuild := stages["build-shared"], stages["build"]
	return last == "default" && shared.from == "runtime" && defaults.from == "runtime" &&
		sharedBuild.from == "source" && defaultBuild.from == "source" &&
		reTagsRedis.MatchString(sharedBuild.body) && !reTagsRedis.MatchString(defaultBuild.body) &&
		strings.Contains(defaultBuild.body, "go build -trimpath") &&
		strings.TrimSpace(shared.body) == "COPY --from=build-shared /out/risk-engine /risk-engine" &&
		strings.TrimSpace(defaults.body) == "COPY --from=build /out/risk-engine /risk-engine"
}

func TestRiskSharedDedupImageTargetsHaveExecutableEdges(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "services", "risk_engine", "Dockerfile")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !riskSharedDedupTargetsValid(b) {
		t.Fatal("owning Dockerfile does not satisfy the optional image contract")
	}
	mutants := map[string]string{
		"missing-tag":                   strings.Replace(string(b), "go build -tags redis -trimpath", "go build -trimpath", 1),
		"default-links-shared":          strings.Replace(string(b), "COPY --from=build /out/risk-engine", "COPY --from=build-shared /out/risk-engine", 1),
		"shared-links-default":          strings.Replace(string(b), "COPY --from=build-shared /out/risk-engine", "COPY --from=build /out/risk-engine", 1),
		"default-compiles-shared":       strings.Replace(string(b), "RUN CGO_ENABLED=0 go build -trimpath", "RUN CGO_ENABLED=0 go build -tags redis -trimpath", 1),
		"shared-is-default":             string(b) + "\nFROM shared-dedup AS selected\n",
		"shared-build-has-wrong-source": strings.Replace(string(b), "FROM source AS build-shared", "FROM build AS build-shared", 1),
	}
	for name, mutant := range mutants {
		t.Run(name, func(t *testing.T) {
			if mutant == string(b) {
				t.Fatal("mutation did not change its intended edge")
			}
			if riskSharedDedupTargetsValid([]byte(mutant)) {
				t.Fatal("broken executable image edge was accepted")
			}
		})
	}
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
