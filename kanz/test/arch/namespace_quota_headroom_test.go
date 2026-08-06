package arch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A NAMESPACE'S QUOTA MUST BE ABLE TO FUND THE AUTOSCALE ITS OWN MANIFESTS DECLARE.
//
// infra/deploy/availability.yaml granted kanz-services requests.cpu: "24" under the
// sentence "Sized above the KEDA maxReplicaCount headroom." It was not. The declared
// ceilings sum to 86 pods and, at that file's own LimitRange defaultRequest of 500m,
// cost 43 CPU — nearly double what was granted. Two numbers in ONE file contradicted
// each other and nothing compared them, because the comparison lived in a comment
// (#231).
//
// The failure mode is the worst shape available. Under a market event KEDA raises
// the replica count, the ReplicaSet controller starts getting `exceeded quota` from
// the API server, and that surfaces ONLY as a Kubernetes Event on the ReplicaSet:
// no probe fails, no service reports unhealthy, no alert fires. The platform quietly
// refuses to scale during exactly the event scaling exists for. Meanwhile every pod
// that DID start is sized by a LimitRange default nobody measured, so a service whose
// working set exceeds 1Gi is OOMKilled on a number that was never chosen for it.
//
// So the arithmetic is the guard, and the manifests are its only input. Nothing here
// is asserted; every term is read back out of infra/deploy on each run:
//
//	ceiling(w) = max(spec.replicas, the maxReplicaCount of any ScaledObject
//	             whose scaleTargetRef names w)
//	cost(w)    = ceiling(w) x the pod's effective request, which is Kubernetes'
//	             own formula max(sum(containers), max(initContainers)) — init
//	             containers run BEFORE the app containers, so they do not add
//	             (this is how the quota actually counts a pod, and getting it
//	             wrong in the other direction would over-size every quota by the
//	             migrate initContainer every service carries)
//	surge      = the single most expensive rolling update WHILE at ceiling, per
//	             resource. One at a time, deliberately: modelling every workload
//	             surging simultaneously sizes the quota for a case nobody should
//	             ever run, and a quota that funds everything bounds nothing.
//
// A container's request comes from its own `resources:` block or, failing that,
// from its namespace's LimitRange defaults. A container with NEITHER is BestEffort:
// invisible to the quota, first evicted under node pressure, and indistinguishable
// from one that was deliberately sized — which is the "nothing configured looks like
// checked, and fine" shape this repository refuses. That is checked too, below.
func TestNamespaceQuotaFundsTheDeclaredAutoscale(t *testing.T) {
	root := moduleRoot(t)
	est := k8sEstate(t, root)

	// --- NON-VACUITY -------------------------------------------------------
	//
	// Every arm below is a loop over something parsed out of infra/deploy. A walk
	// that found nothing, a decoder that stopped at the first document, or a
	// renamed directory would make all of them pass over an empty set and report
	// a funded, well-governed estate. Fail instead.
	if len(est.workloads) < 20 {
		t.Fatalf("scanned infra/deploy and found only %d workload documents (Deployment/Rollout/"+
			"StatefulSet/DaemonSet). This estate has more than twenty. Finding almost none is a "+
			"BROKEN SCAN reported as a clean result — did infra/deploy move, or did the parse stop early?",
			len(est.workloads))
	}
	if len(est.quotas) == 0 || len(est.limitRanges) == 0 || len(est.scalers) == 0 {
		t.Fatalf("found %d ResourceQuota, %d LimitRange and %d ScaledObject documents under infra/deploy — "+
			"this guard is the comparison BETWEEN those three kinds, so missing any of them means it is "+
			"checking nothing at all.", len(est.quotas), len(est.limitRanges), len(est.scalers))
	}

	// --- DEAD-ENTRY CHECK on the exemption list ----------------------------
	live := map[string]bool{}
	for _, w := range est.workloads {
		for _, c := range append(append([]k8sContainer{}, w.initContainers...), w.containers...) {
			live[w.name+"/"+c.name] = true
		}
	}
	var dead []string
	for key := range unsizedContainerExemptions {
		if !live[key] {
			dead = append(dead, key)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("unsizedContainerExemptions names %d container(s) that no longer exist: %s\n\n"+
			"Either the workload was renamed (update the key) or it was deleted (drop the entry). "+
			"A stale exemption protects nothing while looking load-bearing, which is worse than "+
			"no exemption at all.", len(dead), strings.Join(dead, ", "))
	}

	// --- ARM 1: every ScaledObject's target must resolve --------------------
	//
	// An unresolved scaleTargetRef is not a harmless typo here: its ceiling is
	// then counted against NOTHING, so the sum below silently under-states the
	// requirement and the quota passes while being too small. market-data lived
	// in exactly this state once — the ScaledObject named a Deployment that did
	// not exist (see market-data-deploy.yaml's own header).
	var problems []string
	for _, so := range est.scalers {
		if est.workloadFor(so.namespace, so.targetKind, so.targetName) == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: ScaledObject %q targets %s/%s in namespace %q and no such workload is declared "+
					"under infra/deploy — its maxReplicaCount of %s is therefore counted against nothing "+
					"and the quota derivation below silently under-states the requirement",
				so.file, so.name, so.targetKind, so.targetName, so.namespace, k8sIntText(so.max)))
		}
	}

	// --- ARM 2: every container gets a request AND a limit from somewhere ---
	for _, w := range est.workloads {
		lr := est.limitRangeFor(w.namespace)
		for _, c := range append(append([]k8sContainer{}, w.initContainers...), w.containers...) {
			if _, ok := unsizedContainerExemptions[w.name+"/"+c.name]; ok {
				continue
			}
			for _, res := range []string{"cpu", "memory"} {
				if _, ok := c.requests[res]; !ok {
					if lr == nil || lr.defaultRequest[res] == nil {
						problems = append(problems, fmt.Sprintf(
							"%s: container %q of %s %q declares no resources.requests.%s, and namespace %q "+
								"has no LimitRange defaultRequest.%s to supply one — the pod is BestEffort for "+
								"%s: uncounted by any quota, first evicted under node pressure, and identical "+
								"on inspection to a container someone sized on purpose",
							w.file, c.name, w.kind, w.name, res, w.namespace, res, res))
					}
				}
				if _, ok := c.limits[res]; !ok {
					if lr == nil || lr.defaultLimit[res] == nil {
						problems = append(problems, fmt.Sprintf(
							"%s: container %q of %s %q declares no resources.limits.%s and namespace %q has no "+
								"LimitRange default.%s — nothing bounds what this container can take from its node",
							w.file, c.name, w.kind, w.name, res, w.namespace, res))
					}
				}
			}
		}
	}

	// --- ARM 3: a LimitRange must be internally consistent ------------------
	//
	// defaultRequest <= default <= max. A LimitRange whose default limit exceeds
	// its own max rejects EVERY pod in the namespace at admission, and the
	// rejection names the LimitRange, not the workload — so it reads as a broken
	// deployment rather than a broken policy.
	for _, lr := range est.limitRanges {
		for _, res := range []string{"cpu", "memory"} {
			req, def, max := lr.defaultRequest[res], lr.defaultLimit[res], lr.max[res]
			if req != nil && def != nil && *req > *def {
				problems = append(problems, fmt.Sprintf(
					"%s: LimitRange %q has defaultRequest.%s greater than default.%s — a request above its "+
						"own limit is rejected at admission for every container in namespace %q",
					lr.file, lr.name, res, res, lr.namespace))
			}
			if def != nil && max != nil && *def > *max {
				problems = append(problems, fmt.Sprintf(
					"%s: LimitRange %q has default.%s greater than max.%s — every pod in namespace %q that "+
						"relies on the default is rejected at admission by this same object",
					lr.file, lr.name, res, res, lr.namespace))
			}
		}
	}

	// --- ARM 4: a namespace that autoscales must have a ResourceQuota -------
	for _, so := range est.scalers {
		if est.quotaFor(so.namespace) == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: ScaledObject %q scales workloads in namespace %q up to %s replicas and that namespace "+
					"has no ResourceQuota at all — nothing bounds a runaway scale-out, and nothing can be "+
					"checked against the ceiling either",
				so.file, so.name, so.namespace, k8sIntText(so.max)))
		}
	}

	// --- ARM 5: the arithmetic ---------------------------------------------
	for _, q := range est.quotas {
		req := est.requirementFor(t, q.namespace)
		if req.pods == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: ResourceQuota %q governs namespace %q, in which this scan found no workloads at all. "+
					"Either the quota is orphaned or the workload parse missed them; a quota checked against "+
					"zero pods passes no matter what it says.", q.file, q.name, q.namespace))
			continue
		}
		for _, want := range req.demands() {
			have, ok := q.hard[want.key]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"%s: ResourceQuota %q declares no %s. An absent key is UNLIMITED, which is the one "+
						"setting indistinguishable from having thought about it: namespace %q needs %s.",
					q.file, q.name, want.key, q.namespace, want.render(want.need)))
				continue
			}
			if have < want.need {
				problems = append(problems, fmt.Sprintf(
					"%s: ResourceQuota %q grants %s = %s to namespace %q, which cannot fund the ceiling its "+
						"own manifests declare: %s.\n      %s",
					q.file, q.name, want.key, want.render(have), q.namespace,
					want.render(want.need), req.explain))
			}
			if have > want.need*k8sQuotaSlackFactor {
				problems = append(problems, fmt.Sprintf(
					"%s: ResourceQuota %q grants %s = %s to namespace %q, more than %dx the %s the declared "+
						"ceiling needs. A quota that large bounds nothing and is indistinguishable from no "+
						"quota; either the ceilings shrank and this was not followed down, or the number was "+
						"picked rather than derived.\n      %s",
					q.file, q.name, want.key, want.render(have), q.namespace,
					k8sQuotaSlackFactor, want.render(want.need), req.explain))
			}
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the namespace quotas cannot fund the scale their own manifests declare:\n\n  %s\n\n"+
			"RULE: a ResourceQuota is DERIVED from the workloads in its namespace, never chosen. Raising a "+
			"maxReplicaCount, a replica count or a per-container request without following the quota up is a "+
			"red build here rather than an `exceeded quota` Event that nothing is watching. The derivation is "+
			"written out in infra/deploy/availability.yaml next to the numbers it produces; if you change one, "+
			"change both in the same commit.",
			strings.Join(problems, "\n\n  "))
	}
}

// k8sQuotaSlackFactor bounds a quota from ABOVE as well as below. Checking only
// the lower bound is a half-check: `requests.cpu: "10000"` satisfies it forever
// while bounding nothing, and reads on inspection exactly like a governed
// namespace. 2x leaves genuine room for the unmodelled (a debug pod, a one-shot
// Job) without letting the object stop being a blast-radius bound.
const k8sQuotaSlackFactor = 2

// unsizedContainerExemptions is the default-deny allow-list of containers
// permitted to run with no resource request from either their own manifest or a
// namespace LimitRange — i.e. permitted to be BestEffort.
//
// Keyed "<workload name>/<container name>". A dead entry fails the guard above,
// so an exemption cannot outlive the thing it excused.
//
// IT IS EMPTY, AND THAT IS THE ANSWER, NOT AN OVERSIGHT. Every kanz-services
// container is sized by availability.yaml's LimitRange; the one workload outside
// that namespace (operator, in kanz-operator) carries its own block. Nothing on
// this estate has a reason to be BestEffort, so nothing is listed. The map exists
// so that the next container which needs to be adds a written reason and an issue
// here — reviewed in — rather than a special case in the logic above.
var unsizedContainerExemptions = map[string]string{}

// ---------------------------------------------------------------------------
// the estate: every manifest under infra/deploy, parsed once
// ---------------------------------------------------------------------------

// k8sQuantity is a resource quantity in its smallest sensible unit: millicores
// for cpu, bytes for memory. Kept as an integer so the sums below are exact —
// floating-point milliCPU is how a quota check passes by 0.0000001.
type k8sQuantity int64

// k8sContainer is one container reduced to the two maps this guard compares,
// plus the image and the argv that pool_budget_test.go reads off the Postgres
// container to find its declared max_connections.
type k8sContainer struct {
	name     string
	image    string
	args     []string
	requests map[string]*k8sQuantity
	limits   map[string]*k8sQuantity
}

// k8sWorkload is one Deployment/Rollout/StatefulSet/DaemonSet.
type k8sWorkload struct {
	file           string // repo-relative, slash-separated — named in every message
	kind           string
	name           string
	namespace      string
	labels         map[string]string
	podLabels      map[string]string
	replicas       *int
	grace          *int
	recreate       bool   // strategy: Recreate — a rollout never surges
	maxSurge       string // the declared maxSurge, "" when the manifest leaves it default
	containers     []k8sContainer
	initContainers []k8sContainer
}

type k8sQuota struct {
	file, name, namespace string
	hard                  map[string]k8sQuantity
}

type k8sLimitRange struct {
	file, name, namespace             string
	defaultRequest, defaultLimit, max map[string]*k8sQuantity
}

type k8sScaledObject struct {
	file, name, namespace  string
	targetKind, targetName string
	min, max               *int
}

type k8sPDB struct {
	file, name, namespace string
	minAvailable          string
	maxUnavailable        string
	selector              map[string]string
}

type k8sEstateDocs struct {
	workloads   []k8sWorkload
	quotas      []k8sQuota
	limitRanges []k8sLimitRange
	scalers     []k8sScaledObject
	pdbs        []k8sPDB
}

func (e *k8sEstateDocs) workloadFor(ns, kind, name string) *k8sWorkload {
	for i := range e.workloads {
		w := &e.workloads[i]
		if w.namespace == ns && w.kind == kind && w.name == name {
			return w
		}
	}
	return nil
}

func (e *k8sEstateDocs) quotaFor(ns string) *k8sQuota {
	for i := range e.quotas {
		if e.quotas[i].namespace == ns {
			return &e.quotas[i]
		}
	}
	return nil
}

func (e *k8sEstateDocs) limitRangeFor(ns string) *k8sLimitRange {
	for i := range e.limitRanges {
		if e.limitRanges[i].namespace == ns {
			return &e.limitRanges[i]
		}
	}
	return nil
}

// ceilingFor is the largest replica count a workload's own manifests declare:
// its pinned count, raised to the maxReplicaCount of any ScaledObject that
// targets it. An absent spec.replicas is Kubernetes' default of 1.
func (e *k8sEstateDocs) ceilingFor(w *k8sWorkload) int {
	n := 1
	if w.replicas != nil {
		n = *w.replicas
	}
	for _, so := range e.scalers {
		if so.namespace == w.namespace && so.targetKind == w.kind && so.targetName == w.name {
			if so.max != nil && *so.max > n {
				n = *so.max
			}
		}
	}
	return n
}

// k8sRequirement is what one namespace's declared ceiling costs.
type k8sRequirement struct {
	pods                           int64
	reqCPU, reqMem, limCPU, limMem k8sQuantity
	explain                        string
}

type k8sDemand struct {
	key    string
	need   k8sQuantity
	render func(k8sQuantity) string
}

func (r k8sRequirement) demands() []k8sDemand {
	return []k8sDemand{
		{"pods", k8sQuantity(r.pods), k8sRenderCount},
		{"requests.cpu", r.reqCPU, k8sRenderCPU},
		{"requests.memory", r.reqMem, k8sRenderMem},
		{"limits.cpu", r.limCPU, k8sRenderCPU},
		{"limits.memory", r.limMem, k8sRenderMem},
	}
}

// requirementFor sums what every workload in a namespace costs at its declared
// ceiling, plus the single most expensive rolling update on top.
func (e *k8sEstateDocs) requirementFor(t *testing.T, ns string) k8sRequirement {
	t.Helper()
	lr := e.limitRangeFor(ns)

	var out k8sRequirement
	var surgePods int64
	var surgeCPUReq, surgeMemReq, surgeCPULim, surgeMemLim k8sQuantity
	var surgeWho string

	for i := range e.workloads {
		w := &e.workloads[i]
		if w.namespace != ns {
			continue
		}
		ceiling := int64(e.ceilingFor(w))
		reqCPU := k8sPodCost(w, lr, "requests", "cpu")
		reqMem := k8sPodCost(w, lr, "requests", "memory")
		limCPU := k8sPodCost(w, lr, "limits", "cpu")
		limMem := k8sPodCost(w, lr, "limits", "memory")

		out.pods += ceiling
		out.reqCPU += k8sQuantity(ceiling) * reqCPU
		out.reqMem += k8sQuantity(ceiling) * reqMem
		out.limCPU += k8sQuantity(ceiling) * limCPU
		out.limMem += k8sQuantity(ceiling) * limMem

		n := k8sSurgePods(w, ceiling)
		if n > surgePods {
			surgePods, surgeWho = n, w.name
		}
		surgeCPUReq = k8sMaxQ(surgeCPUReq, k8sQuantity(n)*reqCPU)
		surgeMemReq = k8sMaxQ(surgeMemReq, k8sQuantity(n)*reqMem)
		surgeCPULim = k8sMaxQ(surgeCPULim, k8sQuantity(n)*limCPU)
		surgeMemLim = k8sMaxQ(surgeMemLim, k8sQuantity(n)*limMem)
	}

	steadyPods := out.pods
	out.pods += surgePods
	out.reqCPU += surgeCPUReq
	out.reqMem += surgeMemReq
	out.limCPU += surgeCPULim
	out.limMem += surgeMemLim
	out.explain = fmt.Sprintf(
		"derivation: %d pods at the declared ceiling (spec.replicas raised by each ScaledObject's "+
			"maxReplicaCount) + %d surge pods for the largest single rolling update (%s) = %d pods, "+
			"costing %s / %s of requests and %s / %s of limits",
		steadyPods, surgePods, surgeWho, out.pods,
		k8sRenderCPU(out.reqCPU), k8sRenderMem(out.reqMem),
		k8sRenderCPU(out.limCPU), k8sRenderMem(out.limMem))
	return out
}

// k8sPodCost is Kubernetes' own effective-pod-resource formula:
// max(sum(containers), max(initContainers)). Init containers run to completion
// BEFORE the app containers start, so they never add to the total — they only
// raise the floor. Summing them instead would over-size every quota on this
// estate by the kanz-migrate initContainer that most services carry.
func k8sPodCost(w *k8sWorkload, lr *k8sLimitRange, kind, res string) k8sQuantity {
	var sum k8sQuantity
	for _, c := range w.containers {
		sum += k8sContainerCost(c, lr, kind, res)
	}
	var initMax k8sQuantity
	for _, c := range w.initContainers {
		initMax = k8sMaxQ(initMax, k8sContainerCost(c, lr, kind, res))
	}
	return k8sMaxQ(sum, initMax)
}

// k8sContainerCost resolves one container's request/limit, falling back to the
// namespace LimitRange exactly as the API server's admission plugin does. A
// container with neither is zero here AND is reported by ARM 2 above, so it
// cannot vanish from the sum unnoticed.
func k8sContainerCost(c k8sContainer, lr *k8sLimitRange, kind, res string) k8sQuantity {
	own := c.requests
	if kind == "limits" {
		own = c.limits
	}
	if q, ok := own[res]; ok && q != nil {
		return *q
	}
	if lr == nil {
		return 0
	}
	fallback := lr.defaultRequest
	if kind == "limits" {
		fallback = lr.defaultLimit
	}
	if q, ok := fallback[res]; ok && q != nil {
		return *q
	}
	return 0
}

// k8sSurgePods is how many EXTRA pods a rolling update of this workload creates
// while it sits at its ceiling. Recreate never surges — that is the whole point
// of the strategy, and every singleton on this estate uses it. Everything else
// gets its declared maxSurge, or Kubernetes' default of 25% when the manifest
// leaves it unset.
func k8sSurgePods(w *k8sWorkload, ceiling int64) int64 {
	if w.recreate {
		return 0
	}
	spec := w.maxSurge
	if spec == "" {
		spec = "25%"
	}
	if strings.HasSuffix(spec, "%") {
		pct, err := strconv.ParseFloat(strings.TrimSuffix(spec, "%"), 64)
		if err != nil {
			return 0
		}
		return int64(math.Ceil(float64(ceiling) * pct / 100))
	}
	n, err := strconv.ParseInt(spec, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func k8sMaxQ(a, b k8sQuantity) k8sQuantity {
	if a > b {
		return a
	}
	return b
}

func k8sRenderCount(q k8sQuantity) string { return strconv.FormatInt(int64(q), 10) }

func k8sRenderCPU(q k8sQuantity) string {
	if q%1000 == 0 {
		return strconv.FormatInt(int64(q)/1000, 10) + " CPU"
	}
	return strconv.FormatInt(int64(q), 10) + "m CPU"
}

func k8sRenderMem(q k8sQuantity) string {
	const gi = 1024 * 1024 * 1024
	if q%gi == 0 {
		return strconv.FormatInt(int64(q)/gi, 10) + "Gi"
	}
	return strconv.FormatInt(int64(q)/(1024*1024), 10) + "Mi"
}

func k8sIntText(p *int) string {
	if p == nil {
		return "<unset>"
	}
	return strconv.Itoa(*p)
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

type k8sResourceBlock struct {
	Requests map[string]any `yaml:"requests"`
	Limits   map[string]any `yaml:"limits"`
}

type k8sContainerDoc struct {
	Name      string           `yaml:"name"`
	Image     string           `yaml:"image"`
	Args      []string         `yaml:"args"`
	Resources k8sResourceBlock `yaml:"resources"`
}

// k8sDoc is every field this package reads out of infra/deploy, in one struct.
// yaml.v3 ignores the keys a given kind does not have, and each arm filters by
// kind before touching a field, so one decode serves all five kinds.
type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type          string `yaml:"type"`
			RollingUpdate struct {
				MaxSurge any `yaml:"maxSurge"`
			} `yaml:"rollingUpdate"`
			Canary struct {
				MaxSurge any `yaml:"maxSurge"`
			} `yaml:"canary"`
		} `yaml:"strategy"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				TerminationGracePeriodSeconds *int              `yaml:"terminationGracePeriodSeconds"`
				Containers                    []k8sContainerDoc `yaml:"containers"`
				InitContainers                []k8sContainerDoc `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`

		Hard map[string]any `yaml:"hard"` // ResourceQuota

		Limits []struct { // LimitRange
			Type           string         `yaml:"type"`
			Default        map[string]any `yaml:"default"`
			DefaultRequest map[string]any `yaml:"defaultRequest"`
			Max            map[string]any `yaml:"max"`
		} `yaml:"limits"`

		ScaleTargetRef struct { // ScaledObject
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"scaleTargetRef"`
		MinReplicaCount *int `yaml:"minReplicaCount"`
		MaxReplicaCount *int `yaml:"maxReplicaCount"`

		MinAvailable   any `yaml:"minAvailable"` // PodDisruptionBudget
		MaxUnavailable any `yaml:"maxUnavailable"`
		Selector       struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"selector"`
	} `yaml:"spec"`
}

// k8sWorkloadKinds are the kinds that own a pod template. Argo's Rollout is here
// because risk-engine — the only workload with a canary strategy and the one with
// the largest KEDA ceiling — is one, and leaving it out would drop 12 of the
// namespace's 86 declared pods out of the sum.
var k8sWorkloadKinds = map[string]bool{
	"Deployment": true, "Rollout": true, "StatefulSet": true, "DaemonSet": true,
}

// k8sEstate parses every YAML document under infra/deploy (recursively — MT-02
// renders per-tenant workloads into infra/deploy/tenants/<tenant>/).
//
// Parsed structurally with gopkg.in/yaml.v3, never grepped: `replicas: 1`,
// `maxReplicaCount` and whole quota blocks all appear inside the prose comments
// in these files, and a regex cannot tell a Deployment's own metadata.labels from
// its pod template's.
func k8sEstate(t *testing.T, root string) *k8sEstateDocs {
	t.Helper()
	base := filepath.Join(root, "infra", "deploy")
	out := &k8sEstateDocs{}

	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		dec := yaml.NewDecoder(strings.NewReader(readFile(t, p)))
		for {
			var doc k8sDoc
			derr := dec.Decode(&doc)
			if errors.Is(derr, io.EOF) {
				break
			}
			if derr != nil {
				t.Fatalf("%s: parse as YAML: %v", rel, derr)
			}
			switch {
			case k8sWorkloadKinds[doc.Kind]:
				surge := k8sScalarText(doc.Spec.Strategy.RollingUpdate.MaxSurge)
				if surge == "" {
					surge = k8sScalarText(doc.Spec.Strategy.Canary.MaxSurge)
				}
				out.workloads = append(out.workloads, k8sWorkload{
					file:           rel,
					kind:           doc.Kind,
					name:           doc.Metadata.Name,
					namespace:      doc.Metadata.Namespace,
					labels:         doc.Metadata.Labels,
					podLabels:      doc.Spec.Template.Metadata.Labels,
					replicas:       doc.Spec.Replicas,
					grace:          doc.Spec.Template.Spec.TerminationGracePeriodSeconds,
					recreate:       doc.Spec.Strategy.Type == "Recreate",
					maxSurge:       surge,
					containers:     k8sContainers(t, rel, doc.Spec.Template.Spec.Containers),
					initContainers: k8sContainers(t, rel, doc.Spec.Template.Spec.InitContainers),
				})
			case doc.Kind == "ResourceQuota":
				hard := map[string]k8sQuantity{}
				for k, v := range doc.Spec.Hard {
					hard[k] = k8sParseQuantity(t, rel, k, k8sScalarText(v))
				}
				out.quotas = append(out.quotas, k8sQuota{
					file: rel, name: doc.Metadata.Name, namespace: doc.Metadata.Namespace, hard: hard,
				})
			case doc.Kind == "LimitRange":
				lr := k8sLimitRange{file: rel, name: doc.Metadata.Name, namespace: doc.Metadata.Namespace}
				for _, l := range doc.Spec.Limits {
					if l.Type != "Container" {
						continue
					}
					lr.defaultRequest = k8sQuantities(t, rel, l.DefaultRequest)
					lr.defaultLimit = k8sQuantities(t, rel, l.Default)
					lr.max = k8sQuantities(t, rel, l.Max)
				}
				out.limitRanges = append(out.limitRanges, lr)
			case doc.Kind == "ScaledObject":
				out.scalers = append(out.scalers, k8sScaledObject{
					file: rel, name: doc.Metadata.Name, namespace: doc.Metadata.Namespace,
					targetKind: doc.Spec.ScaleTargetRef.Kind, targetName: doc.Spec.ScaleTargetRef.Name,
					min: doc.Spec.MinReplicaCount, max: doc.Spec.MaxReplicaCount,
				})
			case doc.Kind == "PodDisruptionBudget":
				out.pdbs = append(out.pdbs, k8sPDB{
					file: rel, name: doc.Metadata.Name, namespace: doc.Metadata.Namespace,
					minAvailable:   k8sScalarText(doc.Spec.MinAvailable),
					maxUnavailable: k8sScalarText(doc.Spec.MaxUnavailable),
					selector:       doc.Spec.Selector.MatchLabels,
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/deploy: %v", err)
	}

	sort.Slice(out.workloads, func(i, j int) bool {
		if out.workloads[i].file != out.workloads[j].file {
			return out.workloads[i].file < out.workloads[j].file
		}
		return out.workloads[i].name < out.workloads[j].name
	})
	return out
}

func k8sContainers(t *testing.T, file string, docs []k8sContainerDoc) []k8sContainer {
	t.Helper()
	var out []k8sContainer
	for _, d := range docs {
		out = append(out, k8sContainer{
			name:     d.Name,
			image:    d.Image,
			args:     d.Args,
			requests: k8sQuantities(t, file, d.Resources.Requests),
			limits:   k8sQuantities(t, file, d.Resources.Limits),
		})
	}
	return out
}

func k8sQuantities(t *testing.T, file string, in map[string]any) map[string]*k8sQuantity {
	t.Helper()
	out := map[string]*k8sQuantity{}
	for k, v := range in {
		q := k8sParseQuantity(t, file, k, k8sScalarText(v))
		out[k] = &q
	}
	return out
}

// k8sScalarText renders a YAML scalar as the text Kubernetes would see. `pods:
// "90"` and `pods: 90` are the same quantity to the API server, and both must
// parse here or the guard's answer depends on whether someone used quotes.
func k8sScalarText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// k8sParseQuantity turns a Kubernetes quantity into millicores (cpu) or bytes
// (memory), by the resource name it is keyed under.
//
// It FAILS the test on anything it does not recognise rather than returning
// zero. A silently-zero quantity is the exact shape that would make the sums
// above under-count and the quota look adequate — the failure this guard exists
// to catch would then be produced BY the guard.
func k8sParseQuantity(t *testing.T, file, key, text string) k8sQuantity {
	t.Helper()
	text = strings.TrimSpace(text)
	if text == "" {
		t.Fatalf("%s: resource key %q has an empty value", file, key)
	}
	isMem := strings.Contains(key, "memory") || strings.Contains(key, "storage")
	if !isMem && !strings.Contains(key, "cpu") {
		// pods, count/*, and the like: a plain integer.
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatalf("%s: %s = %q is not an integer count", file, key, text)
		}
		return k8sQuantity(n)
	}
	if isMem {
		for _, s := range []struct {
			suffix string
			mult   int64
		}{
			{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
			{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000},
		} {
			if strings.HasSuffix(text, s.suffix) {
				n, err := strconv.ParseFloat(strings.TrimSuffix(text, s.suffix), 64)
				if err != nil {
					t.Fatalf("%s: %s = %q is not a memory quantity", file, key, text)
				}
				return k8sQuantity(int64(math.Round(n * float64(s.mult))))
			}
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatalf("%s: %s = %q is not a memory quantity this guard understands "+
				"(expected a plain byte count or a Ki/Mi/Gi/Ti/K/M/G suffix)", file, key, text)
		}
		return k8sQuantity(n)
	}
	if strings.HasSuffix(text, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(text, "m"), 10, 64)
		if err != nil {
			t.Fatalf("%s: %s = %q is not a millicore quantity", file, key, text)
		}
		return k8sQuantity(n)
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatalf("%s: %s = %q is not a cpu quantity (expected cores or a `m` suffix)", file, key, text)
	}
	return k8sQuantity(int64(math.Round(n * 1000)))
}
