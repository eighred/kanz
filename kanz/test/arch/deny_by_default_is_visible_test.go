package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A DENY-BY-DEFAULT CONTROL THAT SHIPS OFF MUST SAY SO IN THE MANIFEST.
//
// This estate has a family of controls that all work the same way: a *_REQUIRE_*
// flag, defaulting to FALSE, because arming it would refuse every order for every
// portfolio nobody has corrected yet — "a trading outage dressed as a control".
// That default is deliberate and correct, and it is written down at length beside
// each one in the deploy manifest.
//
// The manifest is the only place an operator can read what is switched on. A
// control that is off AND absent from it is not a posture anybody chose; it is a
// control nobody knows exists.
//
// THAT IS EXACTLY WHAT HAPPENED. OMS_REQUIRE_ORDER_TYPE_SUPPORT (#405) is the
// same class as OMS_REQUIRE_MANDATE, OMS_REQUIRE_VENUE_ACCOUNT and
// OMS_REQUIRE_VERIFIED_ACCOUNT — all three of which carry a paragraph in
// oms-deploy.yaml explaining why they ship false. The fourth was never added. It
// gates the case where an adapter never SAID which order types it supports, so a
// stop order is admitted, stored, announced as working, and refused only by the
// exchange — and an operator auditing the deployment would not have found the
// flag to consider arming.
//
// WHAT THIS CHECKS: every *_REQUIRE_* config key a deployed service reads is
// named in a deploy manifest.
//
// WHAT IT CANNOT CHECK: that the VALUE is right. Whether a control should be
// armed is a judgement about the estate; whether an operator can SEE it is not.

const denyDefaultDeployDir = "infra/deploy"

var (
	// requireKeyRe finds a deny-by-default control read from the environment.
	// Anchored on _REQUIRE_ rather than on "REQUIRE" so a field like
	// RequiredRole — which is not a boolean posture — is not swept in.
	requireKeyRe = regexp.MustCompile(`"([A-Z][A-Z0-9]*_REQUIRE_[A-Z0-9_]+)"`)
	// manifestNameRe finds an env key set in a manifest.
	manifestNameRe = regexp.MustCompile(`name:\s*([A-Z][A-Z0-9_]+)`)
)

// denyDefaultExempt maps a control to the reason its absence from every manifest
// is correct, and what retires the entry.
//
// THE ONLY ARGUMENT THIS LIST ACCEPTS IS THAT THE SERVICE IS NOT DEPLOYED. A
// control belonging to a service with no manifest cannot appear in one; a control
// belonging to a service that ships must.
var denyDefaultExempt = map[string]string{}

func TestEveryDenyByDefaultControlIsVisibleInAManifest(t *testing.T) {
	root := moduleRoot(t)

	// Which services actually ship. A control in a service with no manifest has
	// nowhere to be written down, and demanding it would be noise.
	manifests, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(denyDefaultDeployDir), "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", denyDefaultDeployDir, err)
	}
	if len(manifests) == 0 {
		t.Fatalf("found zero manifests under %s — the scanner is broken, not the estate",
			denyDefaultDeployDir)
	}
	named := map[string]bool{}
	prefixesDeployed := map[string]bool{}
	for _, m := range manifests {
		body, rerr := os.ReadFile(m)
		if rerr != nil {
			t.Fatalf("read %s: %v", m, rerr)
		}
		text := strings.ReplaceAll(string(body), "\r\n", "\n")
		for _, hit := range manifestNameRe.FindAllStringSubmatch(text, -1) {
			named[hit[1]] = true
			if i := strings.Index(hit[1], "_"); i > 0 {
				prefixesDeployed[hit[1][:i]] = true
			}
		}
	}

	var missing []string
	seenExempt := map[string]bool{}
	found := 0

	for _, gf := range goFilesUnder(t, filepath.Join(root, "services")) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		for _, hit := range requireKeyRe.FindAllStringSubmatch(gf.body, -1) {
			key := hit[1]
			found++
			if named[key] {
				continue
			}
			if reason, ok := denyDefaultExempt[key]; ok {
				seenExempt[key] = true
				t.Logf("%s: exempt — %s", key, reason)
				continue
			}
			// A service with no manifest at all has nowhere to write this down.
			prefix := key[:strings.Index(key, "_")]
			if !prefixesDeployed[prefix] {
				t.Logf("%s: %s is not deployed — no manifest to name it in", key, prefix)
				continue
			}
			missing = append(missing, key)
		}
	}

	// NON-VACUITY: this estate definitely has deny-by-default controls. Finding
	// none means the key spelling changed and this guard asserts nothing.
	if found < 3 {
		t.Fatalf("found %d *_REQUIRE_* control(s) across services/ — expected at least 3 (the OMS "+
			"mandate, venue-account and verified-account gates). The scan is broken", found)
	}

	if len(missing) > 0 {
		// SORT BEFORE uniq: the shared helper dedupes ADJACENT entries only, so
		// unsorted input would let a duplicate through and report it twice.
		sort.Strings(missing)
		missing = uniq(missing)
		t.Errorf("%d deny-by-default control(s) are read by a DEPLOYED service and named in no "+
			"manifest: %v.\n"+
			"Each of these ships OFF for a good reason — arming it would refuse every order for "+
			"everything nobody has corrected yet. But the manifest is the only place an operator "+
			"can read what is switched on, so a control that is off AND absent is not a posture "+
			"anybody chose; it is a control nobody knows exists. OMS_REQUIRE_ORDER_TYPE_SUPPORT "+
			"sat in exactly that state while its three siblings each carried a paragraph. Add it "+
			"to the manifest with its default and the reason, or add an argued entry to "+
			"denyDefaultExempt.", len(missing), missing)
	}

	// DEAD-ENTRY ARM: an exemption for a control that is now named, or no longer
	// exists, protects nothing.
	for key, reason := range denyDefaultExempt {
		if !seenExempt[key] {
			t.Errorf("exemption for %q (%s) matches no unnamed control — delete it", key, reason)
		}
	}
}
