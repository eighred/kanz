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

	"github.com/kanz-eng/kanz/internal/tenantgen"
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
func TestTenantComputeGuard(t *testing.T) {
	root := moduleRoot(t)

	tenancyPath := filepath.Join(root, "infra", "nats", "tenancy.yaml")
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

	tenantAccounts := map[string]map[string]bool{}
	for name, users := range natsAccounts(conf) {
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

	basePath := filepath.Join(root, "infra", "deploy", "oms-deploy.yaml")
	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read %s: %v", basePath, err)
	}

	tenantsDir := filepath.Join(root, "infra", "deploy", "tenants")
	entries, err := os.ReadDir(tenantsDir)
	if err != nil {
		t.Fatalf("read %s: %v", tenantsDir, err)
	}

	// tenant -> the SPIFFE ID its committed manifest's rendered ServiceAccount
	// actually carries (spiffe://kanz.internal/ns/kanz-services/sa/<name>,
	// per ground truth #5), read back from the manifest itself rather than
	// assumed from the directory name.
	overlays := map[string]string{}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tenant := e.Name()
		manifestPath := filepath.Join(tenantsDir, tenant, "oms-"+tenant+".yaml")
		committed, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("infra/deploy/tenants/%s has no oms-%s.yaml (%v) — every tenant directory must hold its rendered compute manifest", tenant, tenant, err)
		}
		// Counted HERE — the manifest exists, which is all the non-vacuity check
		// below asks. `overlays` cannot answer that: the drift branch `continue`s
		// past it, so a drifted tenant would leave overlays empty and the check
		// would report "no tenant has compute provisioned yet" at someone whose
		// manifest is sitting right there, drifted. A guard that fails for the
		// right reason and then names the wrong one costs the next reader an hour
		// (see DATA-M6: drain() reported bad=29759308 for a reader that had hung).
		found++

		rendered, err := tenantgen.Render(baseBytes, tenant)
		if err != nil {
			t.Fatalf("infra/deploy/tenants/%s: re-render from the live base failed: %v", tenant, err)
		}
		if !bytes.Equal(rendered, committed) {
			t.Errorf("infra/deploy/tenants/%s/oms-%s.yaml has DRIFTED from infra/deploy/oms-deploy.yaml — "+
				"the committed manifest no longer matches what internal/tenantgen.Render produces from the live "+
				"base. Re-run: go run ./cmd/kanz-tenantgen -tenant %s, then commit the result.",
				tenant, tenant, tenant)
			continue
		}

		sa, err := tenantgen.ServiceAccountName(committed)
		if err != nil {
			t.Fatalf("infra/deploy/tenants/%s/oms-%s.yaml: %v", tenant, tenant, err)
		}
		overlays[tenant] = "spiffe://kanz.internal/ns/kanz-services/sa/" + sa
	}
	if found == 0 {
		t.Fatal("zero tenant compute manifests found under infra/deploy/tenants/ — non-vacuous by design" +
			"(MT-02 Task 3): finding none is a FAILURE, not a pass. Either no tenant has compute provisioned yet " +
			"(run provision-tenant.sh's compute step) or infra/deploy/tenants/ moved.")
	}

	var problems []string
	for tenant := range tenantAccounts {
		if _, ok := overlays[tenant]; !ok {
			problems = append(problems, fmt.Sprintf(
				"tenant %q has a NATS account in infra/nats/tenancy.yaml but no infra/deploy/tenants/%s/ manifest — "+
					"it is provisioned with streams and no compute; run provision-tenant.sh's compute step "+
					"(see kanz/infra/deploy/tenants/README.md) then commit the result", tenant, tenant))
		}
	}
	for tenant, expectedSPIFFE := range overlays {
		users, ok := tenantAccounts[tenant]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"infra/deploy/tenants/%s/ has a compute manifest but no NATS account in infra/nats/tenancy.yaml — "+
					"its pod would authenticate and reach no account (SEC-M3); add %q to the tenant's account "+
					"the same way provision-tenant.sh's compute step instructs (tenantctl.sh's static-mode path)",
				tenant, expectedSPIFFE))
			continue
		}
		if !users[expectedSPIFFE] {
			var got []string
			for u := range users {
				got = append(got, u)
			}
			sort.Strings(got)
			problems = append(problems, fmt.Sprintf(
				"tenant %q: manifest renders ServiceAccount SPIFFE ID %s but tenancy.yaml's %q account admits %v — "+
					"the pod would authenticate and reach no account (SEC-M3)", tenant, expectedSPIFFE, tenant, got))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("tenant compute guard (MT-02 Task 3/4):\n  %s", strings.Join(problems, "\n  "))
	}
	t.Logf("%d tenant(s) checked, each with a matching NATS account and non-drifted compute manifest", len(overlays))
}
