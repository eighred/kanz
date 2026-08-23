package arch

import (
	"sort"
	"strings"
	"testing"
)

// AN OVERRIDE HELD FOR A SIGNATURE NO CLIENT CAN GIVE IS A PRICING OUTAGE
// NOTHING REFUSES (#410).
//
// # The two halves, in two different services' environments
//
// The compliance-override dual control is a PAIR, and neither half can see the
// other:
//
//	datamaster    DATAMASTER_REQUIRE_DUAL_CONTROL   holds an override for a
//	                                                second signature
//	api-gateway   API_GATEWAY_APPROVE_ROLE          names the role carrying
//	                                                authz.Approve, WITHOUT WHICH
//	                                                the three override routes are
//	                                                not registered at all
//
// proxy.go registers POST /v1/exceptions/{id}/override, its /approve sibling and
// GET /v1/exceptions/pending-overrides only `if h.roles.Approve != ""` — a route
// whose capability nobody holds would answer 403 to every principal that exists,
// so it is deliberately absent instead.
//
// # The combination this exists to refuse
//
// datamaster ARMED and the gateway role UNSET. Every override is then HELD for a
// signature no client can give: the propose route does not exist, the approve
// route does not exist, and the pending queue that would at least make the
// backlog visible does not exist either. Pricing exceptions stop being resolved,
// and the book goes on being valued off prices the system itself flagged.
//
// api-gateway-deploy.yaml states the hazard in a comment and ends "Set both, or
// neither". services/api-gateway/cmd/api-gateway/main.go logs a WARN naming it.
// NEITHER CAN REFUSE IT: the flag lives in another service's environment and the
// gateway process cannot read it, so config.Load has nothing to validate
// against. A WARN in one pod's log is not a control — it is read after the
// incident, by someone who already knows what to look for.
//
// This guard is where the two manifests are read together, which is the only
// place the pair is visible before it ships. CLAUDE.md: an invariant worth
// keeping is a guard, not a paragraph.
//
// # What it does NOT assert
//
// That dual control is ARMED. It is not, on either side, and that is a stated
// posture rather than a defect — see #410's own thread, which records the
// unarmed default as argued rather than merely defaulted. "Neither" is a legal
// and shipped configuration. What is not legal is one of them.
func TestComplianceOverrideDualControlShipsAsAPair(t *testing.T) {
	armed, armedFound := workloadEnv(t, "datamaster", "DATAMASTER_REQUIRE_DUAL_CONTROL")
	role, roleFound := workloadEnv(t, "api-gateway", "API_GATEWAY_APPROVE_ROLE")

	// NON-VACUITY. datamaster's flag is set explicitly in the manifest today; if
	// the lookup stops finding it, this guard is comparing two absences and
	// passes having checked nothing.
	if !armedFound {
		t.Fatalf("DATAMASTER_REQUIRE_DUAL_CONTROL is not set in datamaster's manifest at all. It " +
			"was, explicitly, so either the workload lookup is broken or the flag was removed — " +
			"and this guard cannot tell those apart. Restore the variable or fix the lookup; do " +
			"not delete this check.")
	}

	datamasterArmed := strings.EqualFold(strings.TrimSpace(armed), "true")
	gatewayCanApprove := roleFound && strings.TrimSpace(role) != ""

	if datamasterArmed && !gatewayCanApprove {
		t.Fatalf("datamaster is ARMED for dual control (DATAMASTER_REQUIRE_DUAL_CONTROL=%q) and the "+
			"api-gateway names NO approve role (API_GATEWAY_APPROVE_ROLE %s).\n\n"+
			"Every pricing override is now HELD for a second signature that NO CLIENT CAN GIVE. "+
			"proxy.go registers the three override routes only when an approve role is named, so "+
			"the propose route, the approve route AND the pending-overrides queue are all absent — "+
			"there is not even a backlog to look at. Pricing exceptions stop being resolved and the "+
			"book goes on being valued off prices the system itself flagged.\n\n"+
			"Neither service can refuse this at boot: the flag is in datamaster's environment and "+
			"the gateway cannot read it. Set API_GATEWAY_APPROVE_ROLE, or unarm datamaster. Set "+
			"both, or neither.",
			armed, describeEnv(role, roleFound))
	}

	// THE OTHER DIRECTION IS NOT AN OUTAGE, so it is not a failure — but it is
	// worth naming, because it means the approve ROUTES exist while the second
	// signature is OPTIONAL. An operator reading the route list would reasonably
	// conclude overrides are dual-controlled, and they are not.
	if gatewayCanApprove && !datamasterArmed {
		t.Logf("api-gateway names an approve role (%q) while datamaster is UNARMED "+
			"(DATAMASTER_REQUIRE_DUAL_CONTROL=%q). The three override routes are mounted and the "+
			"second signature is OPTIONAL: an override applies on one person's authority, counted "+
			"as single-signed. Not an outage and not failed here, but the route list implies a "+
			"control that is not enforced.", role, armed)
	}
}

// describeEnv renders an env lookup for an error message, telling "absent" apart
// from "present and empty" — the distinction the caller is being asked about.
func describeEnv(value string, found bool) string {
	if !found {
		return "is not set in the manifest"
	}
	if strings.TrimSpace(value) == "" {
		return "is set to the empty string"
	}
	return "= " + value
}

// workloadEnv returns the value of an env var on a workload's own container,
// selected by the pod's `app` label and the container sharing that name.
//
// BY LABEL AND CONTAINER NAME, never positionally: a manifest may grow a sidecar
// (api-gateway carries a cloudflared one), and reading env[0] of containers[0]
// would silently start reporting the sidecar's environment.
func workloadEnv(t *testing.T, app, key string) (string, bool) {
	t.Helper()
	var (
		value string
		found bool
		seen  []string
	)
	for _, w := range allWorkloads(t) {
		if w.app() != app {
			continue
		}
		seen = append(seen, w.Metadata.Name)
		for _, c := range append(append([]podWorkloadContainer{},
			w.Spec.Template.Spec.Containers...), w.Spec.Template.Spec.InitContainers...) {
			if c.Name != app {
				continue
			}
			for _, e := range c.Env {
				if e.Name == key {
					value, found = e.Value, true
				}
			}
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no workload labelled app=%s found in infra/ — this guard is reading nothing", app)
	}
	sort.Strings(seen)
	return value, found
}
