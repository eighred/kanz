package arch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eighred/kanz/internal/tenantgen"
)

// MT-02 Task 3/4 — the tenant compute guard.
//
// tenancy.yaml lists tenant NATS accounts (the storage/streams isolation
// boundary, MT-01c); infra/deploy/tenants/ lists tenant compute (the
// per-tenant OMS manifest, MT-02). Nothing compares them. A tenant
// provisioned with an account and no compute manifest is exactly the
// production defect this whole epic closes, and it would silently return the
// day someone adds a tenancy.yaml account by hand without also adding the
// manifest — or adds a manifest without ever admitting its ServiceAccount to
// the broker (SEC-M3 all over again).
//
// The corrected MT-02 design commits a RENDERED, plain-YAML manifest per
// tenant rather than a kustomize overlay referencing the base out-of-tree —
// see kanz/infra/deploy/tenants/README.md and internal/tenantgen's package
// doc for why. A committed copy is only safe because nothing hand-maintains
// it: this guard re-renders EVERY committed tenant manifest from the LIVE
// base (infra/deploy/oms-deploy.yaml) via the exact same
// internal/tenantgen.Render used by cmd/kanz-tenantgen, and diffs the WHOLE
// document byte-for-byte. A base gaining a new env var, volume, or field
// becomes a CI failure here, not a silently divergent tenant — and because
// the diff is over the entire rendered output, not a blacklist of fields
// this test's author thought to check, no future field can slip past it
// unnoticed.
//
// This guard is PURE GO (ground truth #7 — CI has no kubectl/kustomize, so a
// guard that shells out would skip in CI exactly the way DATA-M6 did).
//
// It parses, rather than greps, tenancy.yaml: `data["tenants.conf"]` is
// reached via a real YAML unmarshal of the outer ConfigMap, not a keyword
// search over the whole raw file — tenancy.yaml's header discusses tenants
// in prose ("Reserved platform/legacy tenant", "per-tenant streams", ...),
// and a grep across the entire byte content would over-report on that prose
// exactly the way the retired internal/integrity package once did
// (KANZ_BRAIN.md). The extracted `tenants.conf` value is NATS's own
// config-language, not YAML, so it gets its own brace-depth-aware scanner
// below (natsAccounts) rather than a second YAML parse — that scanner is
// structural (it tracks block boundaries), not a flat regex over the blob,
// for the same reason.

// tenantConfigMap is the minimal shape of infra/nats/tenancy.yaml's outer
// Kubernetes object — just enough to reach the ConfigMap's data.
type tenantConfigMap struct {
	Data map[string]string `yaml:"data"`
}

// blockOpenLine matches a NATS-config-language line that OPENS a new
// top-level block, e.g. `acme {` or `__system__ {` — a bare identifier
// (letters, digits, underscore, or `$`) followed by `{` and nothing else.
// It deliberately does not match a self-contained line like
// `jetstream { max_memory: 64MB, ... }` (that has a trailing `}` too, so the
// regex's `$` anchor after `\{` fails), which is what keeps a one-line inner
// block from ever being mistaken for a new tenant account.
var blockOpenLine = regexp.MustCompile(`^([A-Za-z0-9_$]+)\s*\{\s*$`)

// natsAccounts parses the NATS server config-language `tenants.conf` value
// into account name -> set of user SPIFFE IDs. It tracks brace DEPTH rather
// than grepping the blob: a top-level account can only open at depth 1 (i.e.
// directly inside the outer `accounts { ... }` block), and only a `{ user:
// "..." }` line found while INSIDE that account's own block (depth 2) is
// attributed to it. userLine is nats_identity_test.go's existing regex,
// reused rather than redefined.
func natsAccounts(conf string) map[string]map[string]bool {
	accounts := map[string]map[string]bool{}
	depth := 0
	current := ""
	for _, raw := range strings.Split(strings.ReplaceAll(conf, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")

		if depth == 2 {
			if m := userLine.FindStringSubmatch(line); m != nil {
				accounts[current][m[1]] = true
			}
		}
		if depth == 1 && opens > closes {
			if m := blockOpenLine.FindStringSubmatch(line); m != nil && m[1] != "accounts" {
				current = m[1]
				if accounts[current] == nil {
					accounts[current] = map[string]bool{}
				}
			}
		}
		depth += opens - closes
	}
	return accounts
}

// isPlatformNATSAccount excludes the two reserved, non-tenant accounts: the
// platform's own __system__ (archiver's manifest: "__system__ carries the
// platform's cross-cutting FACTs") and NATS's reserved system account — which
// tenancy.yaml declares as `SYS` (see its `system_account: SYS` line; this is
// the "$SYS" the plan refers to, spelled without the leading `$` in this
// file's own account block).
func isPlatformNATSAccount(name string) bool {
	return name == "__system__" || name == "SYS"
}

// TestTenantComputeGuard asserts, both directions, that every tenant NATS
// account has a matching compute manifest and vice versa, that each
// manifest's rendered ServiceAccount is the exact SPIFFE ID its account
// admits, and that each committed manifest is byte-identical to what
// internal/tenantgen.Render produces from the LIVE base today.
// natsAccountUsers is the ONE structural read of tenancy.yaml's account -> users
// map: a real YAML unmarshal of the outer ConfigMap to reach
// data["tenants.conf"], then the brace-depth scanner above over the NATS
// config-language value inside it. Both this guard and the FACT-return guard in
// tenant_bridge_parity_test.go ask the same question — "does account X admit
// SVID Y?" — and a second, looser answer (a strings.Contains over the raw file)
// would match this file's own prose and the OTHER account's copy of the same
// service name.
func natsAccountUsers(t *testing.T) map[string]map[string]bool {
	t.Helper()
	tenancyPath := filepath.Join(moduleRoot(t), "infra", "nats", "tenancy.yaml")
	raw, err := os.ReadFile(tenancyPath)
	if err != nil {
		t.Fatalf("read %s: %v", tenancyPath, err)
	}
	var cm tenantConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse %s as YAML: %v", tenancyPath, err)
	}
	conf, ok := cm.Data["tenants.conf"]
	if !ok {
		t.Fatalf(`%s has no data["tenants.conf"] key — has the ConfigMap shape changed?`, tenancyPath)
	}
	return natsAccounts(conf)
}

func TestTenantComputeGuard(t *testing.T) {
	root := moduleRoot(t)

	tenantAccounts := map[string]map[string]bool{}
	for name, users := range natsAccountUsers(t) {
		if isPlatformNATSAccount(name) {
			continue
		}
		tenantAccounts[name] = users
	}
	if len(tenantAccounts) == 0 {
		t.Fatal("zero tenant accounts parsed from tenancy.yaml (excluding __system__/SYS) — non-vacuous by design " +
			"(MT-02 Task 3, matching the SEC-M5/ONBOARD-M1 lesson on false-green guards): finding none is a FAILURE, " +
			"not a pass. If tenancy.yaml's account block format genuinely changed, update natsAccounts — don't relax this check.")
	}

	// Bases, read once. EVERY declared service, not just the OMS: a tenant that
	// gets an order path and no consumers publishes its FACTs into an account
	// nothing else is a member of (#637), and a guard that only knew about the
	// OMS reported that arrangement as fully provisioned.
	baseBytes := map[string][]byte{}
	for _, svc := range tenantgen.Services {
		// Base AND Extra, through the package's own reader: a service whose
		// definition is split across files (risk-engine's Rollout and its KEDA
		// ScaledObject) renders from all of them, and a guard reading only Base
		// would report permanent drift against a manifest that is correct.
		b, err := tenantgen.ReadBases(root, svc)
		if err != nil {
			t.Fatalf("bases for declared per-tenant service %q: %v", svc.Name, err)
		}
		baseBytes[svc.Name] = b
	}
	if len(tenantgen.Services) == 0 {
		t.Fatal("internal/tenantgen.Services is empty — this guard checks nothing at all")
	}

	tenantsDir := filepath.Join(root, "infra", "deploy", "tenants")
	entries, err := os.ReadDir(tenantsDir)
	if err != nil {
		t.Fatalf("read %s: %v", tenantsDir, err)
	}

	// tenant -> service -> the SPIFFE ID that manifest's rendered ServiceAccount
	// actually carries (spiffe://kanz.internal/ns/kanz-services/sa/<name>,
	// per ground truth #5), read back from the manifest itself rather than
	// assumed from the directory name.
	overlays := map[string]map[string]string{}
	found := 0
	var problems []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tenant := e.Name()
		overlays[tenant] = map[string]string{}

		// Nothing hand-written in a tenant directory. Every file here is
		// generated, so a name the generator would never produce is either a
		// stale manifest for a service that was withdrawn from Services (still
		// syncing to the cluster — infra/deploy/ is raw-synced with prune:true,
		// so it keeps running) or a hand-edit that the drift diff below can
		// never see because nothing looks for it.
		declared := map[string]bool{}
		for _, svc := range tenantgen.Services {
			declared[filepath.Base(svc.ManifestPath(tenant))] = true
		}
		files, err := os.ReadDir(filepath.Join(tenantsDir, tenant))
		if err != nil {
			t.Fatalf("read infra/deploy/tenants/%s: %v", tenant, err)
		}
		for _, f := range files {
			if f.IsDir() || declared[f.Name()] {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"infra/deploy/tenants/%s/%s is not a manifest internal/tenantgen renders — it is either a "+
					"hand-edit (which the byte-for-byte drift diff cannot see, because it only re-renders "+
					"DECLARED services) or the leftover of a service withdrawn from tenantgen.Services. "+
					"infra/deploy/ is raw-synced with prune:true, so a leftover keeps DEPLOYING. "+
					"Delete it, or declare the service.", tenant, f.Name()))
		}

		for _, svc := range tenantgen.Services {
			manifestPath := filepath.Join(root, filepath.FromSlash(svc.ManifestPath(tenant)))
			committed, err := os.ReadFile(manifestPath)
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"infra/deploy/tenants/%s has no %s-%s.yaml (%v).\n      %s\n      Render it: "+
						"go run ./cmd/kanz-tenantgen -tenant %s, then commit the result.",
					tenant, svc.Name, tenant, err, svc.Why, tenant))
				continue
			}
			// Counted HERE — the manifest exists, which is all the non-vacuity check
			// below asks. `overlays` cannot answer that: the drift branch `continue`s
			// past it, so a drifted tenant would leave overlays empty and the check
			// would report "no tenant has compute provisioned yet" at someone whose
			// manifest is sitting right there, drifted. A guard that fails for the
			// right reason and then names the wrong one costs the next reader an hour
			// (see DATA-M6: drain() reported bad=29759308 for a reader that had hung).
			found++

			rendered, err := tenantgen.Render(baseBytes[svc.Name], svc, tenant)
			if err != nil {
				t.Fatalf("infra/deploy/tenants/%s: re-render %s from the live base failed: %v", tenant, svc.Name, err)
			}
			if !bytes.Equal(rendered, committed) {
				problems = append(problems, fmt.Sprintf(
					"infra/deploy/tenants/%s/%s-%s.yaml has DRIFTED from %s — "+
						"the committed manifest no longer matches what internal/tenantgen.Render produces from the live "+
						"base. Re-run: go run ./cmd/kanz-tenantgen -tenant %s, then commit the result.",
					tenant, svc.Name, tenant, svc.Base, tenant))
				continue
			}

			sa, err := tenantgen.ServiceAccountName(committed)
			if err != nil {
				t.Fatalf("infra/deploy/tenants/%s/%s-%s.yaml: %v", tenant, svc.Name, tenant, err)
			}
			overlays[tenant][svc.Name] = "spiffe://kanz.internal/ns/kanz-services/sa/" + sa
		}
	}
	if found == 0 {
		t.Fatal("zero tenant compute manifests found under infra/deploy/tenants/ — non-vacuous by design" +
			"(MT-02 Task 3): finding none is a FAILURE, not a pass. Either no tenant has compute provisioned yet " +
			"(run provision-tenant.sh's compute step) or infra/deploy/tenants/ moved.")
	}

	for tenant := range tenantAccounts {
		if _, ok := overlays[tenant]; !ok {
			problems = append(problems, fmt.Sprintf(
				"tenant %q has a NATS account in infra/nats/tenancy.yaml but no infra/deploy/tenants/%s/ manifest — "+
					"it is provisioned with streams and no compute; run provision-tenant.sh's compute step "+
					"(see kanz/infra/deploy/tenants/README.md) then commit the result", tenant, tenant))
		}
	}
	for tenant, byService := range overlays {
		users, ok := tenantAccounts[tenant]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"infra/deploy/tenants/%s/ has compute manifests but no NATS account in infra/nats/tenancy.yaml — "+
					"its pods would authenticate and reach no account (SEC-M3); add their SPIFFE IDs to the "+
					"tenant's account the same way provision-tenant.sh's compute step instructs "+
					"(tenantctl.sh's static-mode path)", tenant))
			continue
		}
		for _, svc := range tenantgen.Services {
			expectedSPIFFE, rendered := byService[svc.Name]
			if !rendered {
				continue // already reported above as missing or drifted
			}
			if !users[expectedSPIFFE] {
				var got []string
				for u := range users {
					got = append(got, u)
				}
				sort.Strings(got)
				problems = append(problems, fmt.Sprintf(
					"tenant %q: the %s manifest renders ServiceAccount SPIFFE ID %s but tenancy.yaml's %q account "+
						"admits %v — the pod would authenticate and reach no account (SEC-M3)",
					tenant, svc.Name, expectedSPIFFE, tenant, got))
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("tenant compute guard (MT-02 Task 3/4):\n  %s", strings.Join(problems, "\n  "))
	}
	t.Logf("%d tenant(s) x %d declared service(s) checked, each with a matching NATS account and non-drifted compute manifest",
		len(overlays), len(tenantgen.Services))
}

// tenantctlComputeKafkaSAs pulls the default of COMPUTE_KAFKA_SAS out of
// infra/tenancy/tenantctl.sh.
var tenantctlComputeKafkaSAs = regexp.MustCompile(`(?m)^: "\$\{COMPUTE_KAFKA_SAS:=([^}]*)\}"`)

// A PER-TENANT KAFKA PRODUCER MUST BE GRANTED THE TENANT'S PREFIXED ACL (#637).
//
// The two halves live in different languages and neither can derive the other:
// internal/tenantgen declares WHICH services are rendered per tenant and which
// of them produce to Kafka; infra/tenancy/tenantctl.sh is what actually runs
// kafka-acls.sh against a cluster. Nothing but this test compares them.
//
// The failure is silent in the worst possible place. archiver-<tenant> maps
// every event to "<tenant>.{domain}.{entity}" (internal/topic.Qualify) and Kafka
// auto-create is disabled, so an ungranted principal cannot create or write
// those topics: the pod comes up Ready, subscribes, and NACKs every event
// forever with nothing reaching the durable log. That is a tenant trading
// against a store that is not backed up — the one ordering error AGENTS.md says
// outlives any issue.
//
// It also fails the other way: a name in the shell list that tenantgen does not
// render is a live prefixed WRITE grant on a tenant's durable log held by an
// identity no pod presents — and the next reader reads it as a decision.
func TestTenantctlGrantsKafkaToEveryPerTenantProducer(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "tenancy", "tenantctl.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := tenantctlComputeKafkaSAs.FindSubmatch(b)
	if m == nil {
		t.Fatalf("infra/tenancy/tenantctl.sh declares no COMPUTE_KAFKA_SAS default — MT-02 compute is in "+
			"kanz-services, not tenant-<t>, so TENANT_SAS does not cover it and no per-tenant Kafka producer "+
			"would be granted its own prefix at all (#637). Expected a line of the form: %s",
			`: "${COMPUTE_KAFKA_SAS:=archiver}"`)
	}
	var granted []string
	granted = append(granted, strings.Fields(string(m[1]))...)
	sort.Strings(granted)

	want := tenantgen.KafkaProducerNames()
	// NON-VACUITY: with no declared producer this comparison is two empty lists.
	if len(want) == 0 {
		t.Fatal("no internal/tenantgen.Service sets KafkaProducer — either per-tenant archiving was withdrawn " +
			"(then COMPUTE_KAFKA_SAS and this guard go with it) or the flag was dropped and every tenant's " +
			"durable log is now ungranted")
	}
	if strings.Join(granted, " ") != strings.Join(want, " ") {
		t.Fatalf("infra/tenancy/tenantctl.sh grants the tenant's prefixed Kafka ACL to %v, but "+
			"internal/tenantgen declares these per-tenant Kafka producers: %v.\n\n"+
			"A producer missing from the shell list comes up Ready and NACKs every event forever — Kafka "+
			"auto-create is disabled, so it cannot create the %s-prefixed topics it maps to, and that "+
			"tenant's FACTs never reach the durable log while every health check stays green. A name in the "+
			"shell list that tenantgen does not render is the inverse: a live prefixed WRITE grant on a "+
			"tenant's durable log held by an identity no pod presents.",
			granted, want, "{tenant}.")
	}
}
