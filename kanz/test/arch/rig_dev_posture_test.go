package arch

import (
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE RIG HAS EXACTLY ONE DECLARED DEVIATION FROM PRODUCTION.
//
// The rig's manifests were hand-stripped of their CSI volumes on 2026-07-12 and
// nothing recorded that, so the rig drifted eight days from the repo and could
// not be used as evidence about any change. rig-dev-secrets.yaml replaces the
// hand-stripping with one declared, guarded substitution.
//
// This guard pins two things: that the dev Secret carries the SAME Postgres
// convention as the rest of the repo, and that it cannot quietly become a
// production manifest.
func TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts(t *testing.T) {
	root := moduleRoot(t)
	dev := readFile(t, filepath.Join(root, "infra", "deploy", "rig-dev-secrets.yaml"))
	pg := readFile(t, filepath.Join(root, "infra", "deploy", "postgres-dev.yaml"))

	// Comments are prose, not configuration. Both of this repo's previous posture
	// guards were dead because their needles matched a comment (see the board's
	// "guards that could not fire" row); strip them before scanning.
	devCfg := stripYAMLComments(dev)
	pgCfg := stripYAMLComments(pg)

	// --- every mounted SecretProviderClass must have a same-named dev Secret ---
	//
	// This is the arm that matters. rig-apply.sh rewrites each Vault CSI volume into
	// a reference to a Secret named after its SecretProviderClass; if that Secret is
	// not declared anywhere, the workload mounts nothing and fails at boot. The rig
	// hit exactly this on 2026-07-20 — tv-sync-db did not exist in the cluster — and
	// nothing in the repo would have told us.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*name:\s*(\S+)`).FindAllStringSubmatch(devCfg+pgCfg, -1) {
		declared[m[1]] = true
	}

	for _, svc := range rigWorkloads {
		manifest := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "deploy", svc)))
		for _, m := range regexp.MustCompile(`secretProviderClass:\s*(\S+)`).FindAllStringSubmatch(manifest, -1) {
			spc := m[1]
			if !declared[spc] {
				t.Errorf("%s mounts SecretProviderClass %q but no dev Secret of that name is declared "+
					"in rig-dev-secrets.yaml or postgres-dev.yaml.\n"+
					"tools/rig-apply.sh rewrites the Vault CSI volume into secretName: %s, so the pod "+
					"will mount an empty volume and report a config error that looks like a code bug and "+
					"is not. Declare the Secret with the SecretProviderClass's objectName values as its "+
					"KEYS — those are the filenames the service reads.", svc, spc, spc)
			}
		}
	}

	// redis.yaml lives under infra/messaging, not infra/deploy, so it is not a
	// rigWorkloads entry — but tools/rig-apply.sh now routes it through the same
	// tools/rig_dev_patch.py as the workloads above, so its Vault CSI volume
	// (redis-auth) is bound by the exact same rule: no matching dev Secret means an
	// empty mount and Redis exits 1 rather than start unauthenticated.
	redisManifest := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "messaging", "redis.yaml")))
	for _, m := range regexp.MustCompile(`secretProviderClass:\s*(\S+)`).FindAllStringSubmatch(redisManifest, -1) {
		spc := m[1]
		if !declared[spc] {
			t.Errorf("infra/messaging/redis.yaml mounts SecretProviderClass %q but no dev Secret of that name is "+
				"declared in rig-dev-secrets.yaml or postgres-dev.yaml.\n"+
				"tools/rig-apply.sh routes this manifest through tools/rig_dev_patch.py, which rewrites the Vault "+
				"CSI volume into secretName: %s — an undeclared Secret mounts empty and Redis's own start script "+
				"refuses to run rather than come up unauthenticated.", spc, spc)
		}
	}

	// --- the redis-url HOST and the redis-auth NAMESPACE must match redis.yaml's own Service ---
	//
	// The original Critical-1 bug was exactly a wrong host: redis-url pointed at
	// redis.kanz-services.svc while Redis's Service is declared in kanz-messaging, so
	// the DNS name could never resolve. The password-match arm below would not have
	// caught that — it only compares the two passwords, never the host — so a future
	// edit that reverted the host (or moved redis-auth back to kanz-services) would
	// pass every existing arm and silently reintroduce the exact bug.
	//
	// Derived, not hardcoded: parsed structurally out of infra/messaging/redis.yaml's
	// own Service document (gopkg.in/yaml.v3, the same parser and pattern
	// TestEveryDeclaredVolumeIsMounted uses), so a manifest edit that moves Redis to a
	// different namespace changes what this guard expects rather than leaving it
	// checking a copied-and-pasted literal.
	var redisSvcName, redisSvcNamespace string
	dec := yaml.NewDecoder(strings.NewReader(redisManifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("infra/messaging/redis.yaml: parse as YAML: %v", err)
		}
		if doc.Kind == "Service" {
			redisSvcName = doc.Metadata.Name
			redisSvcNamespace = doc.Metadata.Namespace
			break
		}
	}
	if redisSvcName == "" || redisSvcNamespace == "" {
		t.Fatal("infra/messaging/redis.yaml: found no Service to derive the redis-url host from. " +
			"This guard needs a Service document (kind: Service) to know the DNS name Redis actually answers on.")
	}
	wantHost := redisSvcName + "." + redisSvcNamespace + ".svc"

	urlHost := regexp.MustCompile(`redis-url:\s*redis://:[^@\s]+@([^\s:]+):`).FindStringSubmatch(devCfg)
	if urlHost == nil {
		t.Fatal("rig-dev-secrets.yaml's webhook-ingest-redis Secret no longer carries a redis://:<password>@<host>: DSN " +
			"this guard can parse a host out of.")
	}
	if urlHost[1] != wantHost {
		t.Errorf("rig-dev-secrets.yaml: webhook-ingest-redis's redis-url points at host %q, but "+
			"infra/messaging/redis.yaml's Service is %q in namespace %q, which only resolves as %q. "+
			"A wrong host here is exactly the Critical-1 bug this rig shipped with: the DSN's host cannot "+
			"resolve, and webhook-ingest never reaches its nonce store.", urlHost[1], redisSvcName, redisSvcNamespace, wantHost)
	}

	authNamespace := regexp.MustCompile(`(?s)name:\s*redis-auth\b.*?namespace:\s*(\S+)`).FindStringSubmatch(devCfg)
	if authNamespace == nil {
		t.Fatal("rig-dev-secrets.yaml declares no redis-auth Secret with a namespace this guard can parse.")
	}
	if authNamespace[1] != redisSvcNamespace {
		t.Errorf("rig-dev-secrets.yaml: the redis-auth Secret declares namespace %q, but "+
			"infra/security/secrets/secretproviderclass.yaml's redis-auth SecretProviderClass (and the Redis pod "+
			"that mounts it) live in namespace %q, matching infra/messaging/redis.yaml's own Service. A same-named "+
			"Secret in the wrong namespace is invisible to the pod that needs it — the mount resolves to nothing "+
			"and Redis refuses to start unauthenticated.", authNamespace[1], redisSvcNamespace)
	}

	// --- the redis-auth password and the password embedded in redis-url MUST MATCH ---
	//
	// Redis authenticates with exactly one password (redis.yaml's --requirepass); a
	// webhook-ingest DSN carrying a different one connects to nothing and the
	// cross-pod nonce store never comes up. Two literal copies of a secret in one
	// file is exactly the kind of drift a text diff misses, so parse both out of the
	// SAME devCfg blob and compare them structurally rather than trusting they were
	// typed identically.
	urlPassword := regexp.MustCompile(`redis-url:\s*redis://:([^@\s]+)@`).FindStringSubmatch(devCfg)
	authPassword := regexp.MustCompile(`(?s)name:\s*redis-auth\b.*?password:\s*(\S+)`).FindStringSubmatch(devCfg)
	if urlPassword == nil {
		t.Fatal("rig-dev-secrets.yaml's webhook-ingest-redis Secret no longer carries a redis://:<password>@ DSN. " +
			"Redis refuses to start unauthenticated (infra/messaging/redis.yaml), so an unauthenticated DSN cannot work.")
	}
	if authPassword == nil {
		t.Fatal("rig-dev-secrets.yaml declares no redis-auth Secret with a password key. infra/messaging/redis.yaml " +
			"mounts secretProviderClass: redis-auth and refuses to start without one.")
	}
	if urlPassword[1] != authPassword[1] {
		t.Errorf("rig-dev-secrets.yaml: the password embedded in webhook-ingest-redis's redis-url (%q) does not "+
			"match redis-auth's password (%q). Redis authenticates with exactly one password; a webhook-ingest DSN "+
			"carrying a different one connects to nothing and the cross-pod nonce store never comes up.",
			urlPassword[1], authPassword[1])
	}

	// --- every database a dev Secret addresses must be one Postgres actually creates ---
	//
	// A DSN naming a database postgres-dev.yaml never CREATEs fails at runtime with an
	// authentication error that looks like a wrong password and is really a missing
	// database — the failure that cost a session on 2026-07-20. This checks BOTH files'
	// DSNs (oms-db in postgres-dev, tv-sync-db + redis in rig-dev-secrets) against the
	// set of databases the initdb script creates. It is why tv-sync gets its OWN
	// database (tvsync): kanz-migrate keys schema_migrations by version, so sharing the
	// OMS's database makes their two 0001 migrations collide, and the guard now proves
	// the separate database it depends on is actually provisioned.
	created := map[string]bool{}
	for _, m := range regexp.MustCompile(`CREATE DATABASE\s+(\w+)`).FindAllStringSubmatch(pgCfg, -1) {
		created[m[1]] = true
	}
	if len(created) == 0 {
		t.Fatal("postgres-dev.yaml's initdb no longer CREATEs any database. That manifest is the source " +
			"of truth for which databases the rig's Secrets may address (itself pinned to CI); if it " +
			"changed shape, teach this guard the new one rather than deleting it.")
	}
	pgDSNs := regexp.MustCompile(`postgres://[^/\s"']+/([A-Za-z0-9_]+)`).FindAllStringSubmatch(devCfg+pgCfg, -1)
	if pgDSNs == nil {
		t.Fatal("no postgres:// DSN with a database name found in rig-dev-secrets.yaml or postgres-dev.yaml. " +
			"The rig's workloads reach Postgres through these DSNs; if their shape changed, teach this guard.")
	}
	for _, m := range pgDSNs {
		if db := m[1]; !created[db] {
			t.Errorf("a dev Secret's DSN addresses database %q, but postgres-dev.yaml's initdb never "+
				"CREATE DATABASE %s. The pod fails at runtime with an authentication error that looks like a "+
				"wrong password and is really a missing database — the failure that cost a session on "+
				"2026-07-20. Point the DSN at a created database, or add the database to the initdb script.", db, db)
		}
	}

	// --- posture ---------------------------------------------------------------
	// Scans the RAW file: the header is a comment, and stripping comments first
	// would make this arm permanently true. That inversion is deliberate.
	if !strings.Contains(dev, "DEV-ONLY") {
		t.Error("rig-dev-secrets.yaml must state DEV-ONLY in its header. It commits real-looking " +
			"credentials in plaintext; the only thing making that acceptable is that it is " +
			"unmistakably a dev artifact.")
	}
	// EVERY Secret must carry the label, not merely one of them. A plain
	// strings.Contains here would pass as long as ANY Secret in the file kept the
	// correct label, so relabeling a single Secret (leaving the rest alone) would
	// slip straight through — this repo has already shipped that exact class of
	// dead guard twice (see the board's "guards that could not fire" row, and
	// commits d34bb7d / 0d48f3c which had to fix it). Count instances instead of
	// merely detecting presence.
	wantSecrets := strings.Count(devCfg, "kind: Secret")
	gotLabels := strings.Count(devCfg, "kanz.eighred.com/posture: dev-only")
	if gotLabels < wantSecrets {
		t.Errorf("rig-dev-secrets.yaml declares %d Secret(s) but only %d carry the label "+
			"kanz.eighred.com/posture: dev-only, matching postgres-dev.yaml. A second spelling of "+
			"the posture label on even one Secret is a second convention, and the point of this "+
			"file is that the rig has exactly one.", wantSecrets, gotLabels)
	}

	// --- it must never grow into the production secret surface ------------------
	for _, forbidden := range []string{"binance", "okx", "api_key", "apikey", "private_key"} {
		if strings.Contains(strings.ToLower(devCfg), forbidden) {
			t.Errorf("rig-dev-secrets.yaml contains %q. Exchange credentials do not belong in a "+
				"committed file under any posture. If the rig needs a venue adapter, point it at "+
				"the simulator — never at a real key.", forbidden)
		}
	}
}

// rigWorkloads are the manifests the dev rig applies. It is a deliberate, named
// list rather than a glob over infra/deploy: the rig runs a subset, and a glob
// would fail this guard on venue-binance/venue-okx, whose credentials must NEVER
// have a committed dev stand-in. Adding a workload to the rig means adding it here
// and declaring its Secret — which is the review conversation we want to force.
var rigWorkloads = []string{
	"oms-deploy.yaml",
	"tv-sync-deploy.yaml",
	"api-gateway-deploy.yaml",
	"webhook-ingest-deploy.yaml",
	"compliance-deploy.yaml",
}

// tools/rig-apply.sh --deploy must apply EXACTLY rigWorkloads, no more and no
// fewer. The two once disagreed: the guard above modeled the rig as this curated
// subset while apply_deploy globbed every *-deploy.yaml, so the rig came up as the
// whole 23-service estate — venue-binance/venue-okx crash-looping on absent
// exchange keys, an Argo Rollout that never ran, heavy ML services off the loop.
// A curated list in one file and a glob in the other is precisely the kind of
// silent drift this repo exists to make unrepresentable, so this arm pins them to
// each other: change one and the build fails until the other matches.
func TestRigApplyDeploysExactlyRigWorkloads(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	script := readFile(t, filepath.Join(repoRoot, "tools", "rig-apply.sh"))

	block := regexp.MustCompile(`(?s)RIG_WORKLOADS=\((.*?)\)`).FindStringSubmatch(script)
	if block == nil {
		t.Fatal("tools/rig-apply.sh no longer contains a RIG_WORKLOADS=( ... ) array. apply_deploy " +
			"must enumerate the rig's workloads explicitly (not glob infra/deploy), and this guard " +
			"reads that array to prove it matches rigWorkloads. If the array was renamed, teach this " +
			"guard the new name rather than deleting the check.")
	}

	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`\S+-(?:deploy|rollout)\.yaml`).FindAllString(block[1], -1) {
		got[m] = true
	}
	want := map[string]bool{}
	for _, w := range rigWorkloads {
		want[w] = true
	}

	for w := range want {
		if !got[w] {
			t.Errorf("rigWorkloads lists %q but tools/rig-apply.sh does not apply it. The rig would be "+
				"missing a workload its loop needs; add it to RIG_WORKLOADS in apply_deploy.", w)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("tools/rig-apply.sh applies %q but rigWorkloads does not list it. Either the rig is "+
				"deploying something the posture guard never vetted for a dev Secret, or the two drifted; "+
				"reconcile them. (A workload that mounts Vault CSI also needs a declared dev Secret here.)", g)
		}
	}
}
