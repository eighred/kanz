package arch

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

	// --- the DSN must be the one convention, not a third copy of it ------------
	dsn := regexp.MustCompile(`postgres://[^\s"']+`).FindString(pgCfg)
	if dsn == "" {
		t.Fatal("postgres-dev.yaml no longer carries a postgres:// DSN. That manifest is the " +
			"source of truth for the rig's Postgres convention (itself pinned to CI); if it " +
			"changed shape, teach this guard the new one rather than deleting it.")
	}
	if !strings.Contains(devCfg, dsn) {
		t.Errorf("rig-dev-secrets.yaml does not carry the DSN %q from postgres-dev.yaml.\n"+
			"A rig whose Secret addresses a different database than the one its Postgres "+
			"manifest creates fails at runtime with an authentication error that looks like a "+
			"password problem and is not — which is exactly the failure that cost a session "+
			"on 2026-07-20.", dsn)
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
