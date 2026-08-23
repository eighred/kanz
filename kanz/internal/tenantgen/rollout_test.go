package tenantgen

import (
	"strings"
	"testing"
)

// RENDERING A ROLLOUT AND ITS AUTOSCALER (#668). risk-engine is the first
// per-tenant service whose definition is not a Deployment, and two fields
// decide whether the render is correct in ways nothing downstream would catch.

const rolloutBase = `apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: risk-engine
  namespace: kanz-services
  labels:
    app: risk-engine
spec:
  replicas: 3
  selector:
    matchLabels:
      app: risk-engine
  strategy:
    canary:
      steps:
        - setWeight: 20
        - analysis:
            templates:
              - templateName: risk-engine-canary
            args:
              - name: canary-hash
                valueFrom:
                  podTemplateHashValue: Latest
  template:
    metadata:
      labels:
        app: risk-engine
    spec:
      serviceAccountName: risk-engine
      containers:
        - name: risk-engine
          image: risk-engine:latest
          env:
            - { name: RISK_ENGINE_TENANT, value: "__system__" }
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: risk-engine
  namespace: kanz-services
spec:
  scaleTargetRef:
    apiVersion: argoproj.io/v1alpha1
    kind: Rollout
    name: risk-engine
  minReplicaCount: 3
  maxReplicaCount: 12
`

func riskService() Service {
	return Service{
		Name: "risk-engine", Base: "infra/deploy/risk-engine-rollout.yaml",
		Container: "risk-engine", TenantEnv: "RISK_ENGINE_TENANT",
	}
}

func renderRisk(t *testing.T) string {
	t.Helper()
	out, err := Render([]byte(rolloutBase), riskService(), "acme")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

// THE AUTOSCALER MUST FOLLOW THE TENANT'S OWN ROLLOUT. Left pointing at
// "risk-engine", the tenant's ScaledObject scales the PLATFORM rollout from the
// tenant's own queue depth — one tenant's load driving another's capacity, with
// KEDA reporting it healthy throughout.
func TestScaledObjectTargetsTheTenantsOwnRollout(t *testing.T) {
	got := renderRisk(t)
	if !strings.Contains(got, "name: risk-engine-acme") {
		t.Fatalf("the render names no risk-engine-acme at all:\n%s", got)
	}
	// The scaleTargetRef block specifically, not just any occurrence.
	i := strings.Index(got, "scaleTargetRef")
	if i < 0 {
		t.Fatal("no scaleTargetRef in the render")
	}
	block := got[i:]
	if j := strings.Index(block, "minReplicaCount"); j > 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "risk-engine-acme") {
		t.Fatalf("scaleTargetRef does not point at the tenant's rollout — this tenant's autoscaler "+
			"would scale the PLATFORM one:\n%s", block)
	}
}

// THE ANALYSIS TEMPLATE REFERENCE MUST NOT BE SUFFIXED. The AnalysisTemplate is
// shared and deliberately not rendered per tenant — it takes the
// rollouts-pod-template-hash as an argument, so its queries already scope to one
// rollout's canary pods. A suffixed reference points at a template that does not
// exist, and Argo treats a missing template as an analysis FAILURE, which aborts
// the rollout: the tenant's risk engine becomes undeployable and the symptom is a
// rollback rather than a not-found.
func TestTheCanaryAnalysisReferenceIsNotSuffixed(t *testing.T) {
	got := renderRisk(t)
	if strings.Contains(got, "templateName: risk-engine-canary-acme") {
		t.Fatal("the canary AnalysisTemplate reference was suffixed. That template is shared and " +
			"not rendered per tenant, so this rollout now references one that does not exist — " +
			"Argo reads a missing template as a FAILED analysis and aborts the rollout")
	}
	if !strings.Contains(got, "templateName: risk-engine-canary") {
		t.Fatalf("the canary analysis reference is gone entirely:\n%s", got)
	}
}

// The ordinary Deployment-shaped fields still render: a Rollout goes through the
// same transform, and this pins that it actually ran rather than the kind being
// waved through.
func TestARolloutGetsTheDeploymentTreatment(t *testing.T) {
	got := renderRisk(t)
	for _, want := range []string{
		"serviceAccountName: risk-engine-acme", // the SA suffix
		"value: acme",                          // TenantEnv rewritten
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q — the Rollout did not go through transformDeployment:\n%s", want, got)
		}
	}
}

// A ScaledObject with no scaleTargetRef is a render-time error, not a silent
// pass: it would produce an autoscaler bound to nothing, which KEDA reports as
// healthy while scaling no workload at all.
func TestAScaledObjectWithoutATargetIsRefused(t *testing.T) {
	broken := strings.Replace(rolloutBase, `  scaleTargetRef:
    apiVersion: argoproj.io/v1alpha1
    kind: Rollout
    name: risk-engine
`, "", 1)
	if _, err := Render([]byte(broken), riskService(), "acme"); err == nil {
		t.Fatal("a ScaledObject with no scaleTargetRef rendered without error")
	}
}
