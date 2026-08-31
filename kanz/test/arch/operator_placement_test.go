package arch

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// controlPlaneRoleLabel is the node label that marks a control-plane node. Only the
// KEY is pinned here, deliberately: k3s sets it to "true" and kubeadm/kind set it to
// "", and this guard is about WHICH NODES the operator may land on, not about which
// distribution the estate runs. The manifest itself carries the value and the note
// explaining that a nodeSelector is an exact string match.
const controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"

// placementDoc captures only the pod-placement fields this guard inspects from the
// multi-document operator manifest, in the same yaml.v3 multi-doc decode style as
// operator_rbac_test.go and node_exporter_test.go.
type placementDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				NodeSelector map[string]string `yaml:"nodeSelector"`
				Tolerations  []struct {
					Key      string `yaml:"key"`
					Operator string `yaml:"operator"`
					Effect   string `yaml:"effect"`
				} `yaml:"tolerations"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// TestOperatorDeploymentIsPinnedToControlPlane asserts the operator Deployment can
// only be scheduled onto a control-plane node.
//
// THE FAILURE THIS PREVENTS IS A DEADLOCK, NOT AN AESTHETIC. The operator is the
// component that performs node drains. Unpinned, the scheduler may place it on a
// worker; a drain of that worker must then evict the operator pod to complete, and
// cannot — replicas: 1 plus the `operator` PodDisruptionBudget's minAvailable: 1
// leaves zero allowed disruptions, so the Eviction API refuses for the whole
// 15-minute drainDeadline and the node never drains. Observed on the two-node k3s
// estate with the operator on worker ip-172-26-12-47.
//
// The answer is placement, never a weaker PDB: refusing to evict the only replica of
// a control-plane component is exactly what that budget is for.
func TestOperatorDeploymentIsPinnedToControlPlane(t *testing.T) {
	var found bool
	for _, d := range decodeOperatorPlacement(t) {
		if d.Kind != "Deployment" || d.Metadata.Name != "operator" {
			continue
		}
		found = true
		podSpec := d.Spec.Template.Spec

		// --- the nodeSelector: the pin itself ---
		if len(podSpec.NodeSelector) == 0 {
			t.Fatalf("the operator Deployment has NO nodeSelector, so Kubernetes may schedule "+
				"the drainer onto a worker. A drain of that worker then blocks forever on "+
				"evicting this pod (replicas: 1 + PDB minAvailable: 1 = 0 allowed disruptions). "+
				"Pin it back with nodeSelector: {%s: \"true\"} — do not weaken the PDB instead.",
				controlPlaneRoleLabel)
		}
		if _, ok := podSpec.NodeSelector[controlPlaneRoleLabel]; !ok {
			t.Errorf("the operator Deployment's nodeSelector is %v, which does not name %q. "+
				"Pinning it to some other label does not keep the drainer off the fleet it "+
				"drains; only the control-plane role label does.",
				podSpec.NodeSelector, controlPlaneRoleLabel)
		}
		for k := range podSpec.NodeSelector {
			if k != controlPlaneRoleLabel {
				t.Errorf("the operator Deployment's nodeSelector carries extra key %q. Every "+
					"nodeSelector key must match for the pod to schedule, so an additional "+
					"constraint can only narrow placement further — and narrowing it past the "+
					"one control-plane node leaves the operator Pending.", k)
			}
		}

		// --- the toleration: not needed on this cluster, required for the next one ---
		//
		// The k3s control-plane node carries no taints today, so the selector alone
		// places the pod. On a cluster whose control plane IS tainted NoSchedule
		// (kubeadm's default) the selector says "only here" and the taint says "not
		// here", and the pin becomes an unschedulable pod. Losing the toleration is
		// therefore a latent outage, invisible until someone hardens the cluster.
		var tolerated bool
		for _, tol := range podSpec.Tolerations {
			if tol.Key != controlPlaneRoleLabel {
				continue
			}
			if tol.Effect == "NoSchedule" || tol.Effect == "" {
				tolerated = true
			}
		}
		if !tolerated {
			t.Errorf("the operator Deployment is pinned to %s but does not tolerate "+
				"%s:NoSchedule. It schedules on THIS cluster only because the control-plane "+
				"node happens to be untainted; on a cluster that taints its control plane the "+
				"pin above turns into a permanently Pending pod.",
				controlPlaneRoleLabel, controlPlaneRoleLabel)
		}
	}

	// Non-vacuity: a guard that matches no document passes forever after the
	// Deployment is renamed or the manifest split.
	if !found {
		t.Fatalf("no Deployment named operator found in infra/deploy/operator-deploy.yaml")
	}
}

// decodeOperatorPlacement reads infra/deploy/operator-deploy.yaml into typed docs.
// Separate from operator_rbac_test.go's decodeOperatorManifest because that one
// projects the RBAC fields and this one projects the pod-placement fields; widening
// a shared struct would couple two unrelated guards.
func decodeOperatorPlacement(t *testing.T) []placementDoc {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var docs []placementDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d placementDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("no docs decoded from %s", path)
	}
	return docs
}

// provisionSpecFile builds, in Go, the two Jobs the operator creates at runtime.
const provisionSpecFile = "services/operator/internal/provision/provision.go"

// pinnedPodSpecs are the pod specs that MUST carry the control-plane pin, and why.
//
// ONE ENTRY, AND THAT IS THE RULING RATHER THAN AN OVERSIGHT (OPS-M2f-a, #76).
// probeJobSpec used to be here with a note saying its placement was "OPS-M2f-a's open
// question, not yet decided". It is decided: the Job holding the fleet-admission
// credential stays pinned, the Job holding nothing is unpinned so Test Connection does
// not become unschedulable the moment the k3s server is drained.
// TestProbeJobIsNotPinned below asserts the other half, so unpinning is a property with
// a guard of its own rather than merely the absence of one.
var pinnedPodSpecs = []struct {
	fn  string
	why string
}{
	{"jobSpec", "it mounts the Job-owned Secret carrying the SSH bootstrap key AND K3S_TOKEN — " +
		"the cluster-admission token. Unpinned it runs on an arbitrary worker, possibly one just " +
		"provisioned from the Add Node form, which copies the credential that admits the fleet " +
		"onto a member of that fleet"},
}

// TestProvisioningJobsArePinnedToControlPlane is the same rule as the Deployment guard
// above, applied to the two Jobs the operator builds IN GO.
//
// WHY IT IS A SECOND TEST. The guard above parses infra/deploy/operator-deploy.yaml, and
// a manifest parser cannot see a corev1.PodSpec struct literal. That blind spot is not
// theoretical: the board and a review both recorded the Jobs as pinned while neither
// spec carried a nodeSelector at all, and nothing failed, because the only executable
// guard was reading a file the Jobs do not live in.
//
// WHY IT PARSES SOURCE rather than calling provision.New and reading the built Job.
// services/operator/internal/provision is an INTERNAL package: only packages under
// services/operator may import it, and test/arch is not one. Parsing keeps the guard
// where the other cross-package placement rules live (see probe_deadline_nesting_test.go,
// which parses for the same reason) instead of relaxing an import boundary for a test.
//
// It asserts the specs USE THE SHARED VALUES rather than that they each carry some
// nodeSelector, because that is the property that cannot rot: two independent copies of a
// security constraint drift the first time one is edited alone.
func TestProvisioningJobsArePinnedToControlPlane(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(provisionSpecFile))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	consts := stringConsts(f)

	// --- the shared selector must be the SAME pin the Deployment carries ---
	//
	// Compared against the manifest rather than against a literal written here: a
	// nodeSelector is an exact string match and the value is distribution-specific
	// ("true" on k3s, empty on kubeadm/kind), so the one thing that must hold is that
	// the Jobs and the operator target the same nodes. A third copy of the value in this
	// test would just be another thing to forget when the estate is ported.
	want := deploymentControlPlaneSelector(t)
	got := mapLiteralValue(t, f, consts, "controlPlaneOnly")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s declares controlPlaneOnly = %v, but the operator Deployment's nodeSelector "+
			"is %v.\n\nThe two must be identical: a nodeSelector is an exact string match, so a "+
			"Job pinned with a different value than the Deployment does not land somewhere wrong "+
			"— it sits Pending until its ActiveDeadlineSeconds expires, holding the bootstrap-key "+
			"Secret the whole time.", provisionSpecFile, got, want)
	}

	// --- and the shared toleration must still tolerate the control-plane taint ---
	//
	// Text, not a decoded struct: the point is only that this value names the same label
	// and the NoSchedule effect. It grants nothing on this untainted cluster; it exists so
	// that hardening the control plane later cannot silently make provisioning
	// unschedulable.
	tol := declText(t, fset, f, "tolerateControlPlane")
	if !strings.Contains(tol, "controlPlaneRoleLabel") || !strings.Contains(tol, "NoSchedule") {
		t.Errorf("%s declares tolerateControlPlane as:\n\n%s\n\nIt must tolerate "+
			"%s:NoSchedule. Both Jobs are pinned to the control plane by the selector above; "+
			"on a cluster that taints its control plane (kubeadm's default) the selector says "+
			"\"only here\" and the taint says \"not here\", and every provisioning and every "+
			"Test Connection becomes a Job that never schedules.",
			provisionSpecFile, tol, controlPlaneRoleLabel)
	}

	// --- the credential-carrying pod spec must actually USE them ---
	for _, spec := range pinnedPodSpecs {
		t.Run(spec.fn, func(t *testing.T) {
			fields := podSpecFields(t, fset, f, spec.fn)
			if fields["NodeSelector"] != "controlPlaneOnly" {
				t.Errorf("the corev1.PodSpec built by %s sets NodeSelector to %q, want the shared "+
					"controlPlaneOnly.\n\nThis Job must run on the control plane: %s.",
					spec.fn, fields["NodeSelector"], spec.why)
			}
			if fields["Tolerations"] != "tolerateControlPlane" {
				t.Errorf("the corev1.PodSpec built by %s sets Tolerations to %q, want the shared "+
					"tolerateControlPlane.\n\nWithout it the pin above becomes an unschedulable Job "+
					"the moment the control plane is tainted.", spec.fn, fields["Tolerations"])
			}
		})
	}
}

// deploymentControlPlaneSelector returns the operator Deployment's nodeSelector — the
// pin TestOperatorDeploymentIsPinnedToControlPlane already guards, reused here as the
// single source of the value the Jobs must match.
func deploymentControlPlaneSelector(t *testing.T) map[string]string {
	t.Helper()
	for _, d := range decodeOperatorPlacement(t) {
		if d.Kind != "Deployment" || d.Metadata.Name != "operator" {
			continue
		}
		if len(d.Spec.Template.Spec.NodeSelector) == 0 {
			t.Fatalf("the operator Deployment has no nodeSelector, so this test has nothing to "+
				"compare the Jobs against — fix that first (see %s)",
				"TestOperatorDeploymentIsPinnedToControlPlane")
		}
		return d.Spec.Template.Spec.NodeSelector
	}
	t.Fatalf("no Deployment named operator found in infra/deploy/operator-deploy.yaml")
	return nil
}

// podSpecFields returns the field names set on the corev1.PodSpec literal built by the
// named function, mapped to the source text of their values.
//
// It FAILS when the function or its PodSpec literal cannot be found: a guard that silently
// reads an empty map from a renamed or restructured function passes forever while asserting
// nothing, which is exactly how the missing pin survived review in the first place.
func podSpecFields(t *testing.T, fset *token.FileSet, f *ast.File, fn string) map[string]string {
	t.Helper()

	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			decl = fd
		}
	}
	if decl == nil {
		t.Fatalf("%s no longer declares %s. If the Job builders were renamed or moved, point "+
			"provisionPodSpecs at them; if they were deleted, this guard is asserting nothing",
			provisionSpecFile, fn)
	}

	fields := map[string]string{}
	var found int
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isSelectorType(lit.Type, "corev1", "PodSpec") {
			return true
		}
		found++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				fields[key.Name] = exprText(t, fset, kv.Value)
			}
		}
		return true
	})
	if found != 1 {
		t.Fatalf("%s builds %d corev1.PodSpec literals, want exactly 1 — this guard reads the "+
			"placement off that literal and cannot tell which one runs", fn, found)
	}
	return fields
}

// mapLiteralValue decodes a package-level `var name = map[string]string{...}` whose keys
// and values may be string literals or names of string consts declared in the same file.
func mapLiteralValue(t *testing.T, f *ast.File, consts map[string]string, name string) map[string]string {
	t.Helper()
	lit, ok := varValue(f, name).(*ast.CompositeLit)
	if !ok {
		t.Fatalf("%s does not declare %s as a map literal — the two Job specs are supposed to "+
			"share exactly one placement value", provisionSpecFile, name)
	}
	out := map[string]string{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("%s has a non key/value element this guard cannot read", name)
		}
		k, kok := stringValue(kv.Key, consts)
		v, vok := stringValue(kv.Value, consts)
		if !kok || !vok {
			t.Fatalf("%s uses a key or value this guard cannot resolve to a string — keep it "+
				"string literals or consts declared in the same file", name)
		}
		out[k] = v
	}
	return out
}

// varValue returns the single value expression of a package-level var, or nil.
func varValue(f *ast.File, name string) ast.Expr {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 || vs.Names[0].Name != name {
				continue
			}
			return vs.Values[0]
		}
	}
	return nil
}

// declText renders the value expression of a package-level var back to source.
func declText(t *testing.T, fset *token.FileSet, f *ast.File, name string) string {
	t.Helper()
	v := varValue(f, name)
	if v == nil {
		t.Fatalf("%s no longer declares a package-level %s — the two Job specs are supposed to "+
			"share one placement value, not carry a copy each", provisionSpecFile, name)
	}
	return exprText(t, fset, v)
}

// stringConsts collects every file-level `name = "literal"` binding, so an
// identifier used as a map key or as a published subject can be resolved without
// a full type check.
//
// IT COVERS `var` AND MULTI-NAME SPECS, and that is not tidiness (#865). The
// const-only, single-name version missed a mutation outright:
// test/arch/load_harness_front_door_test.go asserts that no load harness
// publishes an order COMMAND, and a mutation binding the subject to an
// identifier survived it — the publish was there and the guard was green,
// because the value was one hop away from the literal it looked for. A resolver
// used by a default-deny guard has to resolve the ordinary shapes, and `var x =
// "s"` and `const a, b = "s", "t"` are both ordinary.
func stringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if s, ok := literalString(vs.Values[i]); ok {
					out[name.Name] = s
				}
			}
		}
	}
	return out
}

// stringValue resolves a string literal or a same-file string const to its value.
func stringValue(e ast.Expr, consts map[string]string) (string, bool) {
	if s, ok := literalString(e); ok {
		return s, true
	}
	if id, ok := e.(*ast.Ident); ok {
		s, ok := consts[id.Name]
		return s, ok
	}
	return "", false
}

func literalString(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// isSelectorType reports whether an expression is the type pkg.Name, through a pointer.
func isSelectorType(e ast.Expr, pkg, name string) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// exprText renders an expression back to source, for failure messages and for the
// value-identity assertions above.
func exprText(t *testing.T, fset *token.FileSet, e ast.Expr) string {
	t.Helper()
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		t.Fatalf("render expression: %v", err)
	}
	return b.String()
}

// TestProbeJobIsNotPinned is the other half of OPS-M2f-a's ruling (#76), and it exists
// because "unpinned" is otherwise the ABSENCE of a line — which no test can notice
// being added back.
//
// The probe Job carries no credential of any kind, so credential confinement never
// applied to it, and its only other justification (the registry gap) was retired by
// OPS-M2f-b. Pinned, it made Test Connection unschedulable the moment the k3s server
// was drained: a reachability probe that cannot run while you drain a node is
// unavailable exactly when an operator needs it.
//
// THE TOLERATION MUST STAY, and that is the subtle half. It is not a pin — it grants
// permission, never preference. Without it, an estate whose control plane is tainted
// and which has no other node (a single-node k3s rig, kubeadm's default posture) could
// not schedule the probe at all: removing the selector would widen placement in
// principle and narrow it to nothing in practice.
func TestProbeJobIsNotPinned(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(provisionSpecFile))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	fields := podSpecFields(t, fset, f, "probeJobSpec")

	if sel, ok := fields["NodeSelector"]; ok {
		t.Errorf("the corev1.PodSpec built by probeJobSpec sets NodeSelector to %q.\n\n"+
			"The probe Job is deliberately UNPINNED (#76): it carries no credential, so "+
			"confinement never applied to it, and pinning it made Test Connection "+
			"unschedulable the moment the k3s server was drained — the eviction deadlock this "+
			"was supposed to remove. If a placement constraint is genuinely needed here again, "+
			"the reason belongs in provision.go beside it and in this test.", sel)
	}
	if fields["Tolerations"] != "tolerateControlPlane" {
		t.Errorf("the corev1.PodSpec built by probeJobSpec sets Tolerations to %q, want the "+
			"shared tolerateControlPlane.\n\nThe toleration is NOT the pin and must survive "+
			"unpinning: it grants permission rather than preference. Without it, a tainted "+
			"control plane with no other node cannot schedule the probe at all — which turns "+
			"widening its placement into eliminating it.", fields["Tolerations"])
	}
}
