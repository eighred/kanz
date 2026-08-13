package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A DEPLOYMENT WITH SEVERAL VENUES MUST NAME WHERE AN UNTARGETED ORDER GOES
// (#437).
//
// The router answered venues[0] — whichever adapter OMS_VENUE_ENDPOINTS happened
// to list first — while the type called itself a smart order router and its
// fallback "first configured / best venue". Two different things, and only the
// first was implemented: nothing ranked anything. The estate ships two venues, so
// the destination of every untargeted order was decided by the order of a
// comma-separated string.
//
// #437 made the router REFUSE that rather than guess, and the shipped manifest
// now names XBIN — the venue that was already receiving them. So the live
// behaviour is unchanged and the choice is finally written down.
//
// WHAT THIS GUARD STOPS IS THE NEXT ONE. Adding a third endpoint is a one-line
// diff that looks like a capacity change; without the default named alongside it,
// it is silently also a routing change. The guard makes the manifest state both.
//
// WHY A MANIFEST GUARD RATHER THAN A CODE ONE: the code half is already enforced
// by the type — execution.Router refuses an ambiguous route, and
// internal/execution/default_venue_test.go proves it by mutation. What no Go test
// can see is a YAML file that configures two venues and stops there. That
// deployment starts, WARNs, and trades perfectly well until the first untargeted
// order — which is the failure mode worth catching in a diff instead.
//
// WHAT IT CANNOT CHECK: that the named MIC is the RIGHT one. That is a trading
// decision, and the point of #437 is that it now belongs to a person rather than
// to a slice index. The OMS refuses to start if the name matches no adapter it
// holds, which is the half a machine can settle.

const defaultVenueDeployDir = "infra/deploy"

var (
	// venueEndpointsRe captures the OMS_VENUE_ENDPOINTS value.
	venueEndpointsRe = regexp.MustCompile(`name:\s*OMS_VENUE_ENDPOINTS,\s*value:\s*"([^"]*)"`)
	// defaultVenueRe matches a NON-EMPTY declared default. An empty value is the
	// absent case, not a declaration — that is exactly what the router refuses on.
	defaultVenueRe = regexp.MustCompile(`name:\s*OMS_DEFAULT_VENUE_MIC,\s*value:\s*"([^"]+)"`)
)

func TestAMultiVenueDeploymentNamesItsDefault(t *testing.T) {
	root := moduleRoot(t)
	manifests, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(defaultVenueDeployDir), "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", defaultVenueDeployDir, err)
	}
	// NON-VACUITY, the directory half: a moved deploy tree returns nothing and this
	// guard passes having checked no manifest at all.
	if len(manifests) == 0 {
		t.Fatalf("found zero manifests under %s — the scanner is broken, not the estate",
			defaultVenueDeployDir)
	}

	checked := 0
	for _, m := range manifests {
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		// Normalise line endings: git on Windows hands these over with CRLF, and a
		// guard a contributor cannot run is one they cannot trust.
		manifest := strings.ReplaceAll(string(body), "\r\n", "\n")

		hit := venueEndpointsRe.FindStringSubmatch(manifest)
		if hit == nil {
			continue
		}
		checked++

		mics := endpointMICs(hit[1])
		// ONE VENUE NEEDS NO DECLARATION. There is nothing to choose, and demanding
		// a choice would be ceremony on every single-adapter deployment.
		if len(mics) < 2 {
			continue
		}
		if named := defaultVenueRe.FindStringSubmatch(manifest); named == nil {
			t.Errorf("%s configures %d venues (%s) and names no OMS_DEFAULT_VENUE_MIC.\n"+
				"An order carrying no target venue would be REFUSED — and before #437 it went to "+
				"%q purely because that endpoint is listed first, so reordering the string or "+
				"inserting an adapter ahead of it moved every such order to another exchange's "+
				"collateral in a diff that looked like a capacity change. Name the venue.",
				filepath.Base(m), len(mics), strings.Join(mics, ", "), mics[0])
		}
	}

	// NON-VACUITY, the match half: if the env-var spelling changes, this finds no
	// endpoint lists and passes while asserting nothing.
	if checked == 0 {
		t.Fatalf("found OMS_VENUE_ENDPOINTS in no manifest — the env-var spelling changed and "+
			"this guard is asserting nothing. Expected the oms deployment under %s",
			defaultVenueDeployDir)
	}
}

// endpointMICs pulls the MICs out of an OMS_VENUE_ENDPOINTS value, whose shape is
// "MIC/account=host:port,MIC/account=host:port". The account and address are
// irrelevant here: what decides ambiguity is how many distinct venues the router
// ends up holding.
func endpointMICs(value string) []string {
	seen := map[string]bool{}
	var mics []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		mic, _, _ := strings.Cut(entry, "=")
		mic, _, _ = strings.Cut(mic, "/")
		if mic = strings.TrimSpace(mic); mic != "" && !seen[mic] {
			seen[mic] = true
			mics = append(mics, mic)
		}
	}
	return mics
}
