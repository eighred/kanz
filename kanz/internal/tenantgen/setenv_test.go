package tenantgen

import (
	"strings"
	"testing"
)

// A PER-TENANT RENDER ARMS A CONTROL THE PLATFORM BASE LEAVES OFF (#779).
//
// The deny-by-default family (*_REQUIRE_*) ships false on the platform bases,
// and oms-deploy.yaml states why: arming it refuses every order for every
// portfolio nobody has run kanz-mandate for yet — a trading outage dressed as a
// control. That reasoning is about the pre-tenancy __system__ book, whose
// portfolios are older than the control. A tenant provisioned today has none of
// them, so inheriting the base's value hands a new client the platform's
// grandfathering as though somebody had chosen it for them — and the client
// trades with NO compliance rule evaluated until an operator writes a mandate.
//
// SetEnv is what keeps the two postures apart. These tests hold it to the same
// dead-entry discipline DropEnv is held to, in both directions: an override that
// matches nothing, and an override that changes nothing, are each errors.

// baseWithControl is a minimal Deployment carrying a deny-by-default control in
// the shape transformDeployment walks — the base's permissive value included,
// because "the base already sets this" is one of the conditions under test.
const baseWithControl = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: oms
  namespace: kanz-services
  labels:
    app: oms
spec:
  selector:
    matchLabels:
      app: oms
  template:
    metadata:
      labels:
        app: oms
    spec:
      serviceAccountName: oms
      containers:
        - name: oms
          image: oms:latest
          env:
            - { name: OMS_TENANT, value: "__system__" }
            - { name: OMS_REQUIRE_MANDATE, value: "false" }
            - { name: OMS_SOURCE, value: "oms" }
`

func omsServiceWithSet(set []SetEnvVar) Service {
	return Service{
		Name: "oms", Base: "infra/deploy/oms-deploy.yaml",
		Container: "oms", TenantEnv: "OMS_TENANT", SetEnv: set,
	}
}

func requireMandate(value string) []SetEnvVar {
	return []SetEnvVar{{Name: "OMS_REQUIRE_MANDATE", Value: value, Why: "a tenant starts deny-by-default"}}
}

func TestAnOverriddenControlReachesTheTenantRender(t *testing.T) {
	out, err := Render([]byte(baseWithControl), omsServiceWithSet(requireMandate("true")), "acme")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)

	if strings.Contains(got, `value: "false"`) {
		t.Fatalf("the rendered tenant manifest still carries the base's permissive value — this "+
			"tenant's OMS would admit orders for portfolios no mandate governs, with no rule "+
			"evaluated:\n%s", got)
	}
	if !strings.Contains(got, "OMS_REQUIRE_MANDATE") || !strings.Contains(got, `value: "true"`) {
		t.Fatalf("OMS_REQUIRE_MANDATE was not overridden to \"true\":\n%s", got)
	}
	// THE TENANT PIN MUST STILL BE APPLIED. An override that also ate the rewrite
	// would leave a deployment wearing the tenant's name and serving __system__.
	if !strings.Contains(got, "acme") {
		t.Fatalf("OMS_TENANT was not rewritten to the tenant:\n%s", got)
	}
	// AND NOTHING ELSE MAY BE LOST — the env list is rebuilt, so an off-by-one
	// would silently drop a neighbouring variable.
	if !strings.Contains(got, "OMS_SOURCE") {
		t.Fatalf("an unrelated env var disappeared from the render:\n%s", got)
	}
}

// AN OVERRIDE THAT MATCHES NOTHING IS AN ERROR — DropEnv's dead-exemption rule,
// applied to the other direction. The base having renamed the variable is
// exactly when the entry stops arming anything, and the tenant would silently
// fall back to the base's default, which for this family is permissive.
func TestAnOverrideThatMatchesNothingFailsTheRender(t *testing.T) {
	_, err := Render([]byte(baseWithControl), omsServiceWithSet([]SetEnvVar{
		{Name: "OMS_RENAMED_SINCE", Value: "true", Why: "a stale entry"},
	}), "acme")
	if err == nil {
		t.Fatal("a SetEnv entry naming a variable the base does not set was accepted; the tenant " +
			"would run the base's default while services.go recorded a control that was armed")
	}
	if !strings.Contains(err.Error(), "OMS_RENAMED_SINCE") {
		t.Fatalf("error %q does not name the stale entry", err)
	}
}

// AN OVERRIDE THAT CHANGES NOTHING IS ALSO AN ERROR. Once the base adopts the
// same value, the entry enforces nothing while its Why goes on reading as a live
// decision — and if the base later moves again, the reader has no way to tell
// whether the entry was still meant. Deleting it is the honest outcome.
func TestAnOverrideThatChangesNothingFailsTheRender(t *testing.T) {
	_, err := Render([]byte(baseWithControl), omsServiceWithSet(requireMandate("false")), "acme")
	if err == nil {
		t.Fatal("a SetEnv entry restating the base's own value was accepted; it records a reason " +
			"for a difference that has stopped existing")
	}
	if !strings.Contains(err.Error(), "ALREADY sets") {
		t.Fatalf("error %q does not say the base already sets the value", err)
	}
}

// A CONTROL SUPPLIED BY REFERENCE CANNOT BE OVERRIDDEN WITH A LITERAL. An env
// entry carrying both value and valueFrom is rejected by the API server, so the
// render would produce a manifest that fails to APPLY rather than to render —
// a tenant whose OMS never starts, discovered at deploy time.
func TestAnOverrideOfAValueFromReferenceFailsTheRender(t *testing.T) {
	const base = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: oms
  labels: { app: oms }
spec:
  selector:
    matchLabels: { app: oms }
  template:
    metadata:
      labels: { app: oms }
    spec:
      serviceAccountName: oms
      containers:
        - name: oms
          env:
            - { name: OMS_TENANT, value: "__system__" }
            - name: OMS_REQUIRE_MANDATE
              valueFrom:
                configMapKeyRef: { name: oms-posture, key: require-mandate }
`
	_, err := Render([]byte(base), omsServiceWithSet(requireMandate("true")), "acme")
	if err == nil {
		t.Fatal("a SetEnv override of a valueFrom-sourced variable was accepted; the rendered " +
			"manifest would carry both value and valueFrom and the API server would reject it")
	}
	if !strings.Contains(err.Error(), "valueFrom") {
		t.Fatalf("error %q does not name the valueFrom reference", err)
	}
}

// A VARIABLE CANNOT BE BOTH REMOVED AND GIVEN A VALUE. Which one won would
// depend on the order transformDeployment happens to check them in — a
// descriptor whose meaning is decided by an implementation detail.
func TestAVariableInBothDropEnvAndSetEnvIsRefused(t *testing.T) {
	svc := omsServiceWithSet(requireMandate("true"))
	svc.DropEnv = []DropEnvVar{{Name: "OMS_REQUIRE_MANDATE", Why: "contradiction"}}
	_, err := Render([]byte(baseWithControl), svc, "acme")
	if err == nil {
		t.Fatal("a variable declared in both DropEnv and SetEnv was accepted")
	}
	if !strings.Contains(err.Error(), "both DropEnv and SetEnv") {
		t.Fatalf("error %q does not name the contradiction", err)
	}
}

// THE TENANT PIN IS RENDERED, NOT CONFIGURED. An override naming it would decide
// which tenant the deployment serves — the one defect this package exists to
// close, arriving through the mechanism added to close a different one.
func TestAnOverrideOfTheTenantPinIsRefused(t *testing.T) {
	_, err := Render([]byte(baseWithControl), omsServiceWithSet([]SetEnvVar{
		{Name: "OMS_TENANT", Value: "somebody-else", Why: "should never be allowed"},
	}), "acme")
	if err == nil {
		t.Fatal("a SetEnv override of the tenant pin was accepted; a per-tenant deployment could " +
			"be rendered serving another tenant's book")
	}
	if !strings.Contains(err.Error(), "tenant pin") {
		t.Fatalf("error %q does not name the tenant pin", err)
	}
}

// A REASONLESS OVERRIDE IS REFUSED. Why is the only thing that tells the next
// reader whether the difference is still meant; without it the entry is a bare
// value nobody can retire with confidence.
func TestAnOverrideWithoutAReasonIsRefused(t *testing.T) {
	_, err := Render([]byte(baseWithControl), omsServiceWithSet([]SetEnvVar{
		{Name: "OMS_REQUIRE_MANDATE", Value: "true"},
	}), "acme")
	if err == nil {
		t.Fatal("a SetEnv entry with no reason was accepted")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error %q does not name the incomplete entry", err)
	}
}

// The declared OMS service must actually carry the override — this is the entry
// infra/deploy/tenants/<t>/oms-<t>.yaml depends on, and
// test/arch/tenant_mandate_required_test.go asserts both ends of it.
func TestTheDeclaredOMSArmsTheMandateRequirement(t *testing.T) {
	svc, ok := ServiceByName("oms")
	if !ok {
		t.Fatal("the oms service is no longer declared")
	}
	for _, o := range svc.SetEnv {
		if o.Name != "OMS_REQUIRE_MANDATE" {
			continue
		}
		if o.Value != "true" {
			t.Fatalf("the declared oms service renders OMS_REQUIRE_MANDATE=%q; services/oms reads "+
				"this as os.Getenv(...) == \"true\", so anything else is the PERMISSIVE posture and "+
				"the tenant admits orders no mandate governs (#779)", o.Value)
		}
		if o.Why == "" {
			t.Fatal("the override carries no reason — an operator meeting their first " +
				"MANDATE_MISSING learns whether it is the gate working only if this says so")
		}
		return
	}
	t.Fatal("the declared oms service no longer arms OMS_REQUIRE_MANDATE, so a per-tenant render " +
		"would inherit the platform base's false and the tenant would trade with NO compliance " +
		"constraint evaluated for any portfolio nobody has published a mandate for (#779)")
}
