package tenantgen

import (
	"strings"
	"testing"
)

// A PER-TENANT RENDER MUST NOT INHERIT A __system__-ONLY DEPENDENCY (#640).
//
// The base OMS deployment points at datamaster, which serves ONE tenant per
// instance and answers every other caller with a no-oracle 404. Copied into a
// tenant's render, that URL makes every reference lookup come back as "the
// master does not hold this instrument" — a reference-data gap naming the
// tenant's whole book, when the truth is that no instance was ever going to
// serve them. DropEnv is what keeps the render honest instead.

// baseWithEnv is a minimal Deployment carrying the two env vars these tests
// care about, in the shape transformDeployment walks.
const baseWithEnv = `apiVersion: apps/v1
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
            - { name: OMS_DATAMASTER_URL, value: "http://datamaster.kanz-services.svc:8080" }
            - { name: OMS_SOURCE, value: "oms" }
`

func omsService(drop []DropEnvVar) Service {
	return Service{
		Name: "oms", Base: "infra/deploy/oms-deploy.yaml",
		Container: "oms", TenantEnv: "OMS_TENANT", DropEnv: drop,
	}
}

func TestADroppedEnvVarDoesNotReachTheTenantRender(t *testing.T) {
	out, err := Render([]byte(baseWithEnv), omsService([]DropEnvVar{
		{Name: "OMS_DATAMASTER_URL", Why: "the security master is __system__-only"},
	}), "acme")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)

	if strings.Contains(got, "OMS_DATAMASTER_URL") {
		t.Fatalf("the rendered tenant manifest still carries OMS_DATAMASTER_URL — it would point "+
			"this pod at a single-tenant peer that refuses it, and every lookup would read as a "+
			"reference-data gap:\n%s", got)
	}
	// THE TENANT PIN MUST STILL BE APPLIED. A drop that also ate the rewrite
	// would leave a deployment wearing the tenant's name and serving __system__,
	// which is the defect this whole package exists to close.
	if !strings.Contains(got, "acme") {
		t.Fatalf("OMS_TENANT was not rewritten to the tenant:\n%s", got)
	}
	// AND NOTHING ELSE MAY BE LOST. The rebuild replaces the env list, so an
	// off-by-one there would silently drop a neighbouring variable.
	if !strings.Contains(got, "OMS_SOURCE") {
		t.Fatalf("an unrelated env var disappeared from the render:\n%s", got)
	}
}

// A DECLARED DROP THAT MATCHES NOTHING IS AN ERROR, on the dead-exemption
// reasoning the arch guards use: the base having renamed the variable is
// exactly when the entry stops protecting anything, and a silent no-op would
// leave its reason recorded in services.go while the behaviour it describes had
// quietly ended.
func TestADropThatMatchesNothingFailsTheRender(t *testing.T) {
	_, err := Render([]byte(baseWithEnv), omsService([]DropEnvVar{
		{Name: "OMS_RENAMED_SINCE", Why: "a stale entry"},
	}), "acme")
	if err == nil {
		t.Fatal("a DropEnv entry naming a variable the base does not set was accepted; it would " +
			"sit there recording a reason for behaviour that had stopped happening")
	}
	if !strings.Contains(err.Error(), "OMS_RENAMED_SINCE") {
		t.Fatalf("error %q does not name the stale entry", err)
	}
}

// The declared OMS service must actually carry the drop — this is the entry the
// committed infra/deploy/tenants/acme/oms-acme.yaml depends on, and
// test/arch/tenant_compute_test.go compares that file against a live render.
func TestTheDeclaredOMSWithholdsTheSecurityMasterURL(t *testing.T) {
	svc, ok := ServiceByName("oms")
	if !ok {
		t.Fatal("the oms service is no longer declared")
	}
	for _, d := range svc.DropEnv {
		if d.Name == "OMS_DATAMASTER_URL" {
			if d.Why == "" {
				t.Fatal("the drop carries no reason — an operator reading the render learns that " +
					"the tenant has no classifier only if this says so")
			}
			return
		}
	}
	t.Fatal("the declared oms service no longer withholds OMS_DATAMASTER_URL, so a per-tenant " +
		"render would inherit the __system__ security master and report its 404s as a " +
		"reference-data gap covering the tenant's whole book (#640)")
}
