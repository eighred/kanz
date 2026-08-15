package arch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// EVERY SERVICE IS DEPLOYABLE, OR IT SAYS WHY NOT — IN WRITING.
//
// We reported "15 images referenced by a manifest, 15 images built, no gaps" and
// called deployability complete. That was TRUE AND MEANINGLESS: it is a closed loop
// over the manifests that HAPPEN TO EXIST, and it is silent about the services that
// must RUN. webhook-ingest — the HMAC webhook that receives every TradingView signal,
// the FIRST HOP of the delivered trading loop — had no manifest and no image, and the
// parity check was green the whole time (EXEC-M10, found by deploying to a real
// cluster).
//
// A missing file cannot fail a check that only looks at the files present. So this
// test starts from the SERVICES, not the manifests: every service with a cmd/
// entrypoint must have
//
//	a Dockerfile · an entry in the CI build matrix · a deploy manifest
//
// or an explicit, reasoned exemption below. "We forgot" is not a reason anyone can
// write down, which is the point.
func TestEveryServiceIsDeployableOrExempt(t *testing.T) {
	root := moduleRoot(t)

	build, err := os.ReadFile(filepath.Join(filepath.Dir(root), ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("read the image build matrix: %v", err)
	}
	// NORMALIZE THE LINE ENDINGS before matching. The check below is an exact match
	// ending in "\n", and git on Windows (core.autocrlf=true) hands this file over with
	// CRLF — so every service looked un-built, and this guard failed for ALL of them on
	// any Windows checkout. It could never have been run by a Windows developer; it
	// passed only because CI is Linux. A test that cannot run on a contributor's machine
	// is a test that contributor cannot trust.
	matrix := strings.ReplaceAll(string(build), "\r\n", "\n")

	var problems []string
	for _, svc := range servicesWithEntrypoints(t, root) {
		if reason, ok := notDeployed[svc]; ok {
			if strings.TrimSpace(reason) == "" {
				problems = append(problems, svc+": exempted with no reason")
			}
			continue
		}
		var missing []string
		if _, err := os.Stat(filepath.Join(root, "services", svc, "Dockerfile")); err != nil {
			missing = append(missing, "Dockerfile")
		}
		if !strings.Contains(matrix, "- service: "+svc+"\n") {
			missing = append(missing, "an entry in .github/workflows/build.yml")
		}
		if !manifestExists(root, svc) {
			missing = append(missing, "a deploy manifest (infra/deploy/"+svc+"-deploy.yaml)")
		}
		if len(missing) > 0 {
			problems = append(problems, svc+" is missing: "+strings.Join(missing, ", "))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these services cannot run in production:\n\n  %s\n\n"+
			"A service is DEPLOYABLE (Dockerfile + build-matrix entry + manifest) or it is EXEMPT with a written reason "+
			"in notDeployed. Nothing else. An image/manifest parity check cannot see a service that has neither.",
			strings.Join(problems, "\n  "))
	}
}

// notDeployed is the list of services that deliberately do not run in production,
// each with the reason. A service is here because somebody DECIDED it, not because
// somebody forgot it.
var notDeployed = map[string]string{
	// THE OLD REASON HERE WAS "PARKED — the client surface is the CLI". That
	// premise stopped being true on 2026-08-10: the web surface is now the
	// PRIMARY operator surface (#371) and the CLI is no longer the direction. It
	// is recorded because a stale justification is worse than none — it is why
	// nobody re-examined the entry.
	"web-bff": "NOT YET, AND THE REASON IS NOW SEQUENCING RATHER THAN INTENT. This is the primary " +
		"operator surface (#371) and it serves the compiled SPA on its own origin. What it still " +
		"lacks is a Dockerfile, a build-matrix entry and a manifest, plus the cloudflared sidecar " +
		"that is the ONLY way it is meant to be reachable (infra/edge). Deploying it with an " +
		"ordinary Ingress would open the public port the zero-ingress design exists to avoid, so " +
		"the manifest and the tunnel land together or not at all. Retired by #371.",

	// RETIRED 2026-08-15 by the mechanism that was supposed to retire it. This
	// entry read "NOT YET, AND FOR ONE CONCRETE REASON: THE SIGNING KEY … there
	// is no secret store in this estate yet, so the key's home is an open
	// question". The question is now answered the same way every other secret on
	// this platform is answered — a Vault SecretProviderClass mounted over the
	// SEC-01d CSI driver (infra/security/secrets), at kv/kanz/identity, on its
	// OWN mount separate from the DSN so the migrate initContainer never sees it.
	// The reasoning the entry gave was right and is preserved beside the mount:
	// IDENTITY_ALLOW_EPHEMERAL_KEY is deliberately absent from the manifest,
	// because two replicas generating their own keys serve disjoint JWKS.

	"optimization": "NOT YET, AND DELIBERATELY. It materializes an approved rebalance proposal into OMS " +
		"order COMMANDS — it is a capital path, not an analytic. It does not run until the execution loop " +
		"has run in production and there is a human approval surface in front of it. Deploying a service " +
		"that can emit orders, before anyone has watched the order path work, is the wrong order.",

	"autopilot": "NOT YET, AND DELIBERATELY. It is the closed-loop ops CONTROLLER — it consumes quality/ " +
		"drift/SLO signals and ACTS on the platform. An autonomous remediator must not be turned on before " +
		"the system it remediates has ever run in production and been watched by a human.",

	"performance": "ANALYTICS PLANE, PARKED. Stateless return/attribution analytics with no consumer on " +
		"the active execution direction. It ships behind the analytics surface, not with the trading loop.",

	"lineage": "ANALYTICS/GOVERNANCE PLANE, PARKED. The OpenLineage harvester and provenance catalog. Same " +
		"reason as performance: no consumer on the active direction.",
}

// servicesWithEntrypoints lists every services/<name> that builds a binary.
func servicesWithEntrypoints(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "services", e.Name(), "cmd")); err != nil {
			continue // a library-only service directory builds no image
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// manifestPath returns the path to the service's workload manifest, or "" if it
// has none. risk-engine ships an Argo Rollout rather than a Deployment; both are
// workloads, and a caller that checked only for -deploy.yaml would report the one
// service running a progressive rollout as undeployable.
//
// This is the single answer to "where does this service's workload live".
// TestEveryDeployableServiceIsScrapable reads the manifest body and must look in
// the same place this guard checks for existence — two copies of the naming rule
// would drift, and the drift would show up as a guard passing against a file the
// other guard never saw.
func manifestPath(root, svc string) string {
	for _, name := range []string{svc + "-deploy.yaml", svc + "-rollout.yaml"} {
		p := filepath.Join(root, "infra", "deploy", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// manifestExists reports whether the service has a workload manifest.
func manifestExists(root, svc string) bool { return manifestPath(root, svc) != "" }

// A PROBE MUST POINT AT A PATH THE SERVICE ACTUALLY SERVES.
//
// market-ingest shipped a manifest that probed /healthz while the service served
// /livez. The probe 404'd, so the readiness signal — the thing that pulls a broken
// pod out of its Service — was decorative. Nobody caught it, because the manifest
// and the mux were only ever read by different people.
//
// The compiler cannot see a YAML file, so this test does: for every workload
// manifest, take the path each probe is aimed at and require that the service
// actually registers it.
//
// Scope, honestly: it looks in the service's own tree, and then in internal/ (the
// venue adapters register their probes in the shared internal/venueadapter/server).
// A path registered nowhere fails. This is a string check, not a behavioural one —
// the venue adapters additionally DRIVE their real probe handler with the manifest's
// path (internal/venueadapter/server/manifest_test.go), which is stronger, and is
// the shape to copy if a probe here ever gets subtle.
func TestEveryProbePointsAtARouteTheServiceServes(t *testing.T) {
	root := moduleRoot(t)

	probeRe := regexp.MustCompile(`(livenessProbe|readinessProbe|startupProbe):\s*\n\s*httpGet:\s*\{\s*path:\s*(\S+?),`)

	manifests, err := filepath.Glob(filepath.Join(root, "infra", "deploy", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, m := range manifests {
		base := filepath.Base(m)
		svc := strings.TrimSuffix(strings.TrimSuffix(base, "-deploy.yaml"), "-rollout.yaml")
		if _, err := os.Stat(filepath.Join(root, "services", svc)); err != nil {
			continue // not a service workload (scaling policy, analysis template, …)
		}
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range probeRe.FindAllStringSubmatch(string(body), -1) {
			kind, path := hit[1], hit[2]
			if servesPath(t, root, svc, path) {
				continue
			}
			problems = append(problems, base+": the "+kind+" is aimed at "+path+
				", which "+svc+" does not register")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these probes point at paths that do not exist:\n\n  %s\n\n"+
			"A probe on a path the service does not serve 404s, so the signal it carries — readiness, which is what "+
			"pulls a broken pod out of its Service — is decorative. This is how market-ingest shipped a manifest "+
			"probing /healthz against a service serving /livez.", strings.Join(problems, "\n  "))
	}
}

// A DECLARED VOLUME MUST BE MOUNTED, OR IT IS DEAD CONFIGURATION THAT READS AS
// WORKING.
//
// webhook-ingest-deploy.yaml declared a `redis` volume backing the CROSS-POD nonce
// replay store and mounted only webhook-config and tmp — the volume's own comment
// said removing it "stops the pod from starting" (the binary refuses to run
// without a reachable nonce store), which is exactly what a declared-but-unmounted
// volume also does, just later and less legibly: WEBHOOK_INGEST_REDIS_URL_FILE
// pointed at a path (/run/secrets/redis/redis-url) nothing ever mounted, so the
// platform's own public entrance could not start from its own manifest.
//
// This is a class of defect, not an instance of one: any workload manifest can
// declare a volume in `volumes:` and forget it in every container's
// `volumeMounts:`, and nothing before this test read the two lists together. So
// this checks every pod spec (Deployment/StatefulSet/Rollout/etc — anything with a
// spec.template.spec) under infra/deploy/ and infra/messaging/, across both
// containers and initContainers (oms-deploy.yaml's kanz-migrate initContainer
// mounts its own DSN volume, which is a legitimate mount site, not a miss).
//
// Parsed structurally (gopkg.in/yaml.v3), not grepped: a regex over volume names
// cannot tell "declared, never mounted" from "declared, mounted three fields
// later" without effectively re-implementing a YAML parser badly.
func TestEveryDeclaredVolumeIsMounted(t *testing.T) {
	root := moduleRoot(t)

	var manifests []string
	for _, dir := range []string{"deploy", "messaging"} {
		matches, err := filepath.Glob(filepath.Join(root, "infra", dir, "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, matches...)
	}
	if len(manifests) == 0 {
		t.Fatal("found zero manifests under infra/deploy or infra/messaging — non-vacuous by design: " +
			"finding none is a FAILURE, not a pass. Did these directories move?")
	}
	sort.Strings(manifests)

	var problems []string
	for _, m := range manifests {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		body := readFile(t, m)

		dec := yaml.NewDecoder(strings.NewReader(body))
		for {
			var doc volumeCheckDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: parse as YAML: %v", rel, err)
			}
			spec := doc.Spec.Template.Spec
			if len(spec.Volumes) == 0 {
				continue // a Service/PDB/Ingress/ScaledObject/etc has no pod spec here — nothing to check
			}

			mounted := map[string]bool{}
			for _, c := range spec.Containers {
				for _, vm := range c.VolumeMounts {
					mounted[vm.Name] = true
				}
			}
			for _, c := range spec.InitContainers {
				for _, vm := range c.VolumeMounts {
					mounted[vm.Name] = true
				}
			}

			for _, v := range spec.Volumes {
				if mounted[v.Name] {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s: %s %q declares volume %q but no container or initContainer mounts it",
					rel, doc.Kind, doc.Metadata.Name, v.Name))
			}
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these manifests declare a volume that nothing mounts:\n\n  %s\n\n"+
			"A declared-and-unmounted volume is dead configuration that reads as working: whatever the volume "+
			"backs (a Vault CSI secret, a config file) is never actually available inside the container, and the "+
			"failure surfaces later as a startup error that looks like a code bug. Either mount it at the path the "+
			"consuming code expects, or remove the volume if it is genuinely unused.",
			strings.Join(problems, "\n  "))
	}
}

// volumeCheckDoc is the minimal shape of a Kubernetes workload manifest needed to
// check volumes against volumeMounts. Deployment/StatefulSet/DaemonSet/Job and the
// Argo Rollout all carry the pod spec at spec.template.spec (the same assumption
// tools/rig_dev_patch.py makes); anything else (Service, PodDisruptionBudget,
// Ingress, ScaledObject, AnalysisTemplate, ...) decodes with an empty
// spec.template.spec and is skipped above.
type volumeCheckDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Volumes []struct {
					Name string `yaml:"name"`
				} `yaml:"volumes"`
				Containers     []volumeCheckContainer `yaml:"containers"`
				InitContainers []volumeCheckContainer `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type volumeCheckContainer struct {
	VolumeMounts []struct {
		Name string `yaml:"name"`
	} `yaml:"volumeMounts"`
}

// servesPath reports whether the service registers the HTTP path.
func servesPath(t *testing.T, root, svc, path string) bool {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(root, "services", svc),
		filepath.Join(root, "internal", "venueadapter"), // the shared adapter probe surface
	} {
		found := false
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if strings.Contains(string(b), `"`+path+`"`) || strings.Contains(string(b), `"GET `+path+`"`) {
				found = true
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}
