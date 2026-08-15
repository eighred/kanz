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

	// THE DEAD-ENTRY ARM, which this guard did not have until #371.
	//
	// Every other exemption map in test/arch carries one, and this one's absence
	// had a cost the moment it mattered: web-bff acquired a Dockerfile, a matrix
	// entry and a manifest while its "NOT YET" exemption went on sitting there.
	// The loop above SKIPS an exempted service, so a stale entry is not untidy —
	// it silently switches the check off for something now fully deployable, and
	// would keep doing so if the manifest were later deleted.
	//
	// An exemption is a claim that something is deliberately NOT deployed. Once
	// all three artefacts exist the claim is false, and a false claim left in
	// place is how it becomes the reason nobody re-examined it.
	var stale []string
	for svc, reason := range notDeployed {
		if _, err := os.Stat(filepath.Join(root, "services", svc)); err != nil {
			stale = append(stale, svc+": no such service under services/")
			continue
		}
		_, dockerErr := os.Stat(filepath.Join(root, "services", svc, "Dockerfile"))
		if dockerErr == nil && strings.Contains(matrix, "- service: "+svc+"\n") && manifestExists(root, svc) {
			stale = append(stale, svc+": has a Dockerfile, a build-matrix entry AND a manifest, so the "+
				"exemption is a false claim ("+firstSentence(reason)+")")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("notDeployed has %d stale entr(y/ies):\n\n  %s\n\n"+
			"The loop above SKIPS an exempted service, so a stale entry switches this check off for "+
			"something that is now deployable. Delete the entry.", len(stale), strings.Join(stale, "\n  "))
	}
}

// firstSentence trims a reason to its opening claim, so a stale-entry report
// names what was asserted without reprinting a paragraph.
func firstSentence(reason string) string {
	if i := strings.IndexAny(reason, ".\n"); i > 0 {
		return strings.TrimSpace(reason[:i])
	}
	if len(reason) > 80 {
		return strings.TrimSpace(reason[:80])
	}
	return strings.TrimSpace(reason)
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
	// RETIRED 2026-08-15 — the condition this entry set was MET, not waived. It
	// read: "the manifest and the tunnel land together or not at all. Deploying
	// it with an ordinary Ingress would open the public port the zero-ingress
	// design exists to avoid." web-bff-deploy.yaml carries the cloudflared
	// SIDECAR and no Service and no Ingress, so nothing listens publicly — and
	// the sidecar is what makes WEB_BFF_TRUSTED_PROXIES=127.0.0.1 an exact peer
	// rather than a CIDR anyone in the pod range could present.

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

	// RETIRED 2026-08-15 — THE ENTRY WAS FACTUALLY FALSE, and the dead-entry arm
	// above is what surfaced it. It read "NOT YET, AND DELIBERATELY … it does not
	// run until the execution loop has run in production", while
	// infra/deploy/optimization-deploy.yaml has declared `kind: Deployment,
	// replicas: 2` the whole time. Whatever the intent was, the manifest deploys
	// it.
	//
	// THAT WAS NOT MERELY UNTIDY. Four other guards SKIP a service listed here —
	// nats_identity, nats_jetstream_machinery, nats_service_permissions and
	// prometheus_scrape — so a service that materializes rebalance proposals into
	// ORDER COMMANDS was exempt from the broker-permission check, which is the
	// check whose absence leaves a publisher UNRESTRICTED within its account.
	// The gap was invisible because the exemption looked like a decision.

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

	// A SIDECAR'S PROBES BELONG TO A DIFFERENT PROGRAM.
	//
	// This guard derives the service from the FILENAME and then asks whether that
	// service registers the path. web-bff-deploy.yaml is the estate's first pod
	// with a second container — cloudflared, whose /ready is a route cloudflared
	// serves and web-bff never will — so a file-wide scan reported a correct
	// manifest as broken.
	//
	// The premise was never "every probe in the file", it was "every probe aimed
	// at THIS service". Probes on containers running someone else's image are
	// skipped, and skipped by IMAGE rather than by container name, because a name
	// is a label anyone can choose and the image is what decides which program
	// answers the request.
	foreign := foreignContainerProbePaths(t, manifests)

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
			if foreign[base+" "+path] {
				continue // a sidecar's own route; see foreignContainerProbePaths
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

// foreignContainerProbePaths collects "<file> <path>" for every httpGet probe on
// a container whose image is NOT this repository's own build of the service the
// file is named for.
//
// KEYED BY FILE AND PATH, not by path alone: a sidecar's /ready must not excuse a
// SERVICE probe on /ready in some other manifest. That would be the quiet
// widening this whole guard exists to prevent, introduced by the fix for it.
func foreignContainerProbePaths(t *testing.T, manifests []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range manifests {
		base := filepath.Base(path)
		svc := strings.TrimSuffix(strings.TrimSuffix(base, "-deploy.yaml"), "-rollout.yaml")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(body)))
		for {
			var doc probeScanWorkload
			if err := dec.Decode(&doc); err != nil {
				break
			}
			for _, c := range doc.Spec.Template.Spec.Containers {
				// The service's OWN image. Anything else is a sidecar.
				if strings.Contains(c.Image, "ghcr.io/eighred/"+svc+":") ||
					strings.Contains(c.Image, "ghcr.io/eighred/"+svc+"@") {
					continue
				}
				for _, p := range []probeScanProbe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
					if p.HTTPGet.Path != "" {
						out[base+" "+p.HTTPGet.Path] = true
					}
				}
			}
		}
	}
	return out
}

type probeScanProbe struct {
	HTTPGet struct {
		Path string `yaml:"path"`
	} `yaml:"httpGet"`
}

type probeScanWorkload struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image          string         `yaml:"image"`
					LivenessProbe  probeScanProbe `yaml:"livenessProbe"`
					ReadinessProbe probeScanProbe `yaml:"readinessProbe"`
					StartupProbe   probeScanProbe `yaml:"startupProbe"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}
