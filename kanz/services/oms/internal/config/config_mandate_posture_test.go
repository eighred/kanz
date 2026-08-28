package config

// THE VALUE A TENANT'S MANIFEST RENDERS MUST ACTUALLY ARM THE CONTROL (#779).
//
// A per-tenant OMS ships OMS_REQUIRE_MANDATE=true so that an order for a
// portfolio no mandate governs is REJECTED rather than admitted with no
// compliance rule evaluated. Four things have to agree for that to be true, and
// three of them are already pinned elsewhere:
//
//	internal/tenantgen.Services declares the override      <- pinned here
//	Render writes it into infra/deploy/tenants/<t>/...     <- tenant_compute_test.go, byte-for-byte
//	the committed manifest carries "true"                  <- tenant_mandate_required_test.go
//	Load turns that string into cfg.RequireMandate         <- THIS TEST
//
// The last link is the brittle one, and not in an obvious way: Load reads this
// flag as os.Getenv(...) == "true", a RAW STRING COMPARISON. "True", "1" and
// " true" all load as FALSE — a control an operator believes they armed,
// silently off, with no error anywhere. That parsing is its own defect (#783);
// until it is repaired, the rendered spelling is what stands between a tenant
// and an unarmed gate. So the value under test is read from the
// DECLARATION rather than written out again here: a test that hard-coded "true"
// would keep passing if the generator started emitting a spelling Load does not
// accept, which is precisely the failure this is here to catch.

import (
	"testing"

	"github.com/eighred/kanz/internal/tenantgen"
)

// declaredMandatePosture is the value internal/tenantgen renders into every
// per-tenant OMS manifest, read from the declaration itself.
func declaredMandatePosture(t *testing.T) string {
	t.Helper()
	svc, ok := tenantgen.ServiceByName("oms")
	if !ok {
		t.Fatal("internal/tenantgen.Services no longer declares an \"oms\" service — the per-tenant " +
			"order path is not being rendered, and nothing downstream of this test is arming anything")
	}
	for _, o := range svc.SetEnv {
		if o.Name == "OMS_REQUIRE_MANDATE" {
			return o.Value
		}
	}
	t.Fatal("internal/tenantgen.Services declares no OMS_REQUIRE_MANDATE override, so a rendered " +
		"tenant inherits the platform base's false and trades every unmandated portfolio " +
		"unconstrained (#779)")
	return ""
}

func TestTheRenderedTenantPostureActuallyArmsTheControl(t *testing.T) {
	posture := declaredMandatePosture(t)
	t.Setenv("OMS_REQUIRE_MANDATE", posture)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RequireMandate {
		t.Fatalf("internal/tenantgen renders OMS_REQUIRE_MANDATE=%q and Load read it as NOT REQUIRED. "+
			"Load compares the raw string against \"true\", so every other spelling — \"True\", "+
			"\"1\", a stray space — loads as the PERMISSIVE posture with no error raised anywhere. "+
			"Every tenant rendered from this declaration would report Ready and admit orders for "+
			"portfolios no mandate governs, with no rule evaluated and the trail recording them as "+
			"having passed pre-trade compliance (#779).", posture)
	}
}

// AND THE PLATFORM'S OWN DEFAULT IS STILL PERMISSIVE, which is what makes the
// override above load-bearing rather than decorative.
//
// If Load ever defaulted to true, a tenant would be armed whether or not the
// generator emitted anything, the SetEnv entry would become a no-op that still
// read as a decision, and the day someone deleted it nothing would change until
// the default moved back. Asserting the default is how this test knows it is
// measuring the override and not the ambient behaviour.
func TestAnAbsentMandateFlagIsThePermissivePosture(t *testing.T) {
	t.Setenv("OMS_REQUIRE_MANDATE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequireMandate {
		t.Fatal("an unset OMS_REQUIRE_MANDATE loaded as REQUIRED. That may look like an improvement, " +
			"but it silently arms the platform OMS too — whose __system__ book holds portfolios " +
			"older than the control, and every one of them would start refusing. If this default " +
			"is being changed deliberately, oms-deploy.yaml's paragraph and the SetEnv entry in " +
			"internal/tenantgen both stop being true and must change with it (#779)")
	}
}
