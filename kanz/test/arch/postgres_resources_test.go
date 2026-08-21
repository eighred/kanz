package arch

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE DURABLE STATE IS NOT THE FIRST THING EVICTED (#639).
//
// # What went wrong without it
//
// The six CloudNativePG clusters — the order book, the IBOR ledger, the evidence
// chain, the venue mappings, the schema registry and the platform's own
// credentials — declared NO resources at all, in a namespace (kanz-data) with no
// LimitRange to supply one and, in this repository, no Namespace object to attach
// one to. A pod with neither requests nor limits is BestEffort: the FIRST class
// the kubelet evicts under node memory pressure and the first the scheduler
// preempts.
//
// The estate's position was therefore that the messaging spine declared its
// resources (nats.yaml, kafka.yaml both do), every service inherited a default
// from the kanz-services LimitRange, and only the DATABASES were disposable.
//
// cluster.yaml said so itself, and deferred to #231 — which is CLOSED, and whose
// scope was infra/deploy. Closing it retired the only pointer this gap had. The
// deferral also claimed sizing "needs measurement from a running estate", which
// was the second wrong half: memory follows from max_connections by arithmetic,
// exactly as #228 already derived max_connections rather than inheriting an
// operator default nobody can read.
//
// # What it checks
//
// It RE-DERIVES every value from max_connections rather than comparing against a
// table. A guard holding its own copy of the numbers would pass forever while the
// manifests drifted — and drift here is the failure: raise max_connections for a
// new consumer, leave memory alone, and the cluster OOMs at the load the raise
// was for. The derivation is written out at the top of cluster.yaml so the two
// can be read against each other.
//
// It also asserts the SHAPE of the request, not just its presence:
//
//   - memory request == memory limit, so the pod is never an eviction candidate
//     for its own memory, and cannot balloon into a neighbour.
//   - NO cpu limit, deliberately. That makes the pod Burstable rather than
//     Guaranteed, and it is the trade this estate chose: Guaranteed requires a
//     CPU ceiling, and a CPU ceiling on a database throttles it at exactly the
//     load that produced the spike. A future change that "upgrades" these to
//     Guaranteed by adding a cpu limit fails here, with the reason.
//   - priorityClassName set, and the PriorityClass actually declared, because QoS
//     answers the kubelet and says nothing to the scheduler.
func TestEveryPostgresClusterDeclaresDerivedResources(t *testing.T) {
	root := moduleRoot(t)
	primaries := pgResourcesIn(t, root, filepath.Join("infra", "dr", "postgres", "cluster.yaml"))
	standbys := pgResourcesIn(t, root, filepath.Join("infra", "dr", "postgres", "replica.yaml"))

	// NON-VACUITY. Six primaries exist. A parser that found none would pass this
	// guard however BestEffort the estate had become.
	if len(primaries) < 6 {
		t.Fatalf("parsed %d cluster(s) from infra/dr/postgres/cluster.yaml, want at least 6 — "+
			"the parser is broken and this guard proves nothing", len(primaries))
	}
	if len(standbys) == 0 {
		t.Fatal("parsed no standbys from infra/dr/postgres/replica.yaml — this guard is half blind")
	}

	var problems []string
	check := func(file string, c pgResources) {
		where := file + " " + c.Name

		if c.MaxConnections <= 0 {
			problems = append(problems, where+": no max_connections, so nothing can be derived from it")
			return
		}
		wantMem, wantSharedBuffers, wantCPU := derivePostgresResources(c.MaxConnections)

		if c.MemRequest == 0 || c.CPURequest == 0 {
			problems = append(problems, fmt.Sprintf("%s: declares no resources (cpu request %q, memory request %q). "+
				"A pod with neither requests nor limits is BestEffort — the first thing evicted under node "+
				"memory pressure and the first preempted, which for this cluster is the platform's durable state",
				where, c.RawCPUReq, c.RawMemReq))
			return
		}
		if c.MemLimit != c.MemRequest {
			problems = append(problems, fmt.Sprintf("%s: memory request %dMi != memory limit %dMi. They are pinned "+
				"equal on purpose: a pod at or under its memory request is not an eviction candidate, and a limit "+
				"above the request is room to grow into a neighbour's memory before anyone notices",
				where, c.MemRequest, c.MemLimit))
		}
		if c.CPULimit != 0 {
			problems = append(problems, fmt.Sprintf("%s: declares a cpu limit (%q). These clusters are deliberately "+
				"Burstable, not Guaranteed: Guaranteed requires this ceiling, and a CPU ceiling on a database "+
				"throttles it at exactly the load that produced the spike. If the estate is changing that trade, "+
				"change it here and in cluster.yaml's header together", where, c.RawCPULimit))
		}
		if c.MemRequest != wantMem {
			problems = append(problems, fmt.Sprintf("%s: memory %dMi, but max_connections=%d derives %dMi. "+
				"Either the memory is stale after a max_connections change — which is an OOM at the load the "+
				"raise was for — or the derivation in cluster.yaml's header changed and this guard was not "+
				"updated with it", where, c.MemRequest, c.MaxConnections, wantMem))
		}
		if c.SharedBuffers != wantSharedBuffers {
			problems = append(problems, fmt.Sprintf("%s: shared_buffers %dMB, want %dMB (25%% of the %dMi request). "+
				"The request was derived leaving shared_buffers exactly that share; a larger one is taken from "+
				"the backends' allowance and the cluster OOMs under connection load rather than at startup",
				where, c.SharedBuffers, wantSharedBuffers, wantMem))
		}
		if c.CPURequest != wantCPU {
			problems = append(problems, fmt.Sprintf("%s: cpu request %dm, but max_connections=%d derives %dm",
				where, c.CPURequest, c.MaxConnections, wantCPU))
		}
		if c.PriorityClassName != dataPriorityClass {
			problems = append(problems, fmt.Sprintf("%s: priorityClassName %q, want %q. Resources answer the "+
				"KUBELET (who gets evicted when a node is out of memory); they say nothing to the SCHEDULER, "+
				"which may still displace this database to place a batch pod",
				where, c.PriorityClassName, dataPriorityClass))
		}
	}
	for _, c := range primaries {
		check("cluster.yaml", c)
	}
	for _, c := range standbys {
		check("replica.yaml", c)
	}

	// MIRRORED, for the same reason max_connections is (#228): a standby whose
	// settings sit below the primary whose WAL it replays does not start. Deriving
	// both from max_connections makes them agree by construction ONLY while both
	// files carry the same max_connections, which the pool-budget guard enforces —
	// so what is left to check here is that the standby exists at all for each
	// primary that has one, and that nobody hand-edited one side.
	byName := map[string]pgResources{}
	for _, c := range standbys {
		byName[c.Name] = c
	}
	for _, p := range primaries {
		s, ok := byName[p.Name]
		if !ok {
			continue // not every primary has a standby declared; the DR guard owns that
		}
		if s.MemRequest != p.MemRequest || s.CPURequest != p.CPURequest || s.SharedBuffers != p.SharedBuffers {
			problems = append(problems, fmt.Sprintf("%s: standby and primary disagree — primary %dMi/%dm/%dMB "+
				"vs standby %dMi/%dm/%dMB. The DR region is the one nobody watches, so a divergence here is "+
				"discovered at the failover", p.Name, p.MemRequest, p.CPURequest, p.SharedBuffers,
				s.MemRequest, s.CPURequest, s.SharedBuffers))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d problem(s) in the Postgres resource declarations:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// THE PriorityClass THE CLUSTERS NAME MUST EXIST, AND MUST NOT BE THE DEFAULT.
//
// priorityClassName naming a class that is not declared is not a warning: the pod
// is REJECTED by the API server. A guard that only checked the field on the
// Cluster would call that state fine.
func TestDataPriorityClassIsDeclaredAndNotGlobalDefault(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "dr", "postgres", "priorityclass.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read infra/dr/postgres/priorityclass.yaml: %v\n\n"+
			"The six clusters name %q. A priorityClassName referring to a class that does not exist is "+
			"rejected by the API server, so every one of them would fail to schedule.", err, dataPriorityClass)
	}

	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc struct {
			Kind          string                `yaml:"kind"`
			Metadata      struct{ Name string } `yaml:"metadata"`
			Value         int                   `yaml:"value"`
			GlobalDefault bool                  `yaml:"globalDefault"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "PriorityClass" || doc.Metadata.Name != dataPriorityClass {
			continue
		}
		found = true
		if doc.GlobalDefault {
			t.Errorf("%s sets globalDefault: true. That raises EVERY pod that names no class — including "+
				"the batch and chaos workloads this class exists to outrank — restoring the flat ordering "+
				"it was written to end, while looking configured", dataPriorityClass)
		}
		// The two reserved bands. A workload above them competes with the control
		// plane and the CNI for the node it needs in order to run at all.
		const systemClusterCritical = 2000000000
		if doc.Value >= systemClusterCritical {
			t.Errorf("%s has value %d, which is at or above system-cluster-critical (%d). A database that "+
				"outranks the control plane still loses — on a node with no networking",
				dataPriorityClass, doc.Value, systemClusterCritical)
		}
		if doc.Value <= 0 {
			t.Errorf("%s has value %d, which is not above the 0 every unclassed pod already holds — "+
				"so it orders nothing", dataPriorityClass, doc.Value)
		}
	}
	if !found {
		t.Fatalf("infra/dr/postgres/priorityclass.yaml declares no PriorityClass named %q", dataPriorityClass)
	}
}

const dataPriorityClass = "kanz-data-critical"

// derivePostgresResources reproduces cluster.yaml's stated derivation. Kept as
// arithmetic rather than a lookup table so that changing max_connections changes
// the expected value, which is the whole point of the guard.
//
//	backends = max_connections x 10Mi   (work_mem 4MB default + ~3MB backend baseline)
//	total    = (backends + 336Mi) / 0.75
//	           336Mi = maintenance_work_mem 64 + wal_buffers 16 + OS/process 256
//	           the 0.75 leaves shared_buffers its conventional 25% share
//	request  = total rounded UP to a 512Mi boundary
//	cpu      = max(250m, max_connections x 2.5m) rounded up to 250m
func derivePostgresResources(maxConn int) (memMi, sharedBuffersMB, cpuMilli int) {
	const perBackendMi = 10
	const fixedMi = 64 + 16 + 256
	total := float64(maxConn*perBackendMi+fixedMi) / 0.75
	memMi = int(math.Ceil(total/512)) * 512
	sharedBuffersMB = memMi / 4
	cpuMilli = int(math.Ceil(float64(maxConn)*2.5/250)) * 250
	if cpuMilli < 250 {
		cpuMilli = 250
	}
	return memMi, sharedBuffersMB, cpuMilli
}

type pgResources struct {
	Name              string
	MaxConnections    int
	SharedBuffers     int // MB
	MemRequest        int // Mi
	MemLimit          int // Mi
	CPURequest        int // milli
	CPULimit          int // milli
	PriorityClassName string
	RawMemReq         string
	RawCPUReq         string
	RawCPULimit       string
}

// pgResourcesIn parses every kind: Cluster in one manifest STRUCTURALLY.
//
// A SECOND PARSER, and deliberately not an extension of dr_postgres_coverage_test.go's
// cnpgClusters: that one answers "can this database be restored" from cluster.yaml
// alone, by regex, and rewriting it to serve both questions would put a working
// PITR guard at risk for no gain. This one reads BOTH files and needs values, not
// presence. If a third consumer appears, that is the moment to merge them.
//
// Parsed with gopkg.in/yaml.v3
// (gopkg.in/yaml.v3), like deployability_test.go and dr_singleton_replicas_test.go
// and unlike a regex — this file's whole subject is comment-heavy YAML in which
// every field name it looks for also appears in prose a few lines above.
func pgResourcesIn(t *testing.T, root, rel string) []pgResources {
	t.Helper()
	body := readFile(t, filepath.Join(root, rel))

	var out []pgResources
	dec := yaml.NewDecoder(strings.NewReader(body))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				PostgreSQL struct {
					Parameters map[string]string `yaml:"parameters"`
				} `yaml:"postgresql"`
				Resources struct {
					Requests map[string]string `yaml:"requests"`
					Limits   map[string]string `yaml:"limits"`
				} `yaml:"resources"`
				PriorityClassName string `yaml:"priorityClassName"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "Cluster" {
			continue
		}
		c := pgResources{
			Name:              doc.Metadata.Name,
			PriorityClassName: doc.Spec.PriorityClassName,
			RawMemReq:         doc.Spec.Resources.Requests["memory"],
			RawCPUReq:         doc.Spec.Resources.Requests["cpu"],
			RawCPULimit:       doc.Spec.Resources.Limits["cpu"],
		}
		if v, ok := doc.Spec.PostgreSQL.Parameters["max_connections"]; ok {
			c.MaxConnections, _ = strconv.Atoi(v)
		}
		if v, ok := doc.Spec.PostgreSQL.Parameters["shared_buffers"]; ok {
			c.SharedBuffers = parseMemMB(t, rel, c.Name, "shared_buffers", v)
		}
		c.MemRequest = parseMemMB(t, rel, c.Name, "requests.memory", c.RawMemReq)
		c.MemLimit = parseMemMB(t, rel, c.Name, "limits.memory", doc.Spec.Resources.Limits["memory"])
		c.CPURequest = parseCPUMilli(t, rel, c.Name, c.RawCPUReq)
		c.CPULimit = parseCPUMilli(t, rel, c.Name, c.RawCPULimit)
		out = append(out, c)
	}
	return out
}

// parseMemMB accepts the Mi/MB/Gi forms these manifests use. Returns 0 for "".
func parseMemMB(t *testing.T, rel, cluster, field, v string) int {
	t.Helper()
	if v == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(v, "Gi"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "Gi"))
		if err != nil {
			t.Fatalf("%s %s: %s=%q is not a size", rel, cluster, field, v)
		}
		return n * 1024
	case strings.HasSuffix(v, "Mi"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "Mi"))
		if err != nil {
			t.Fatalf("%s %s: %s=%q is not a size", rel, cluster, field, v)
		}
		return n
	case strings.HasSuffix(v, "MB"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "MB"))
		if err != nil {
			t.Fatalf("%s %s: %s=%q is not a size", rel, cluster, field, v)
		}
		return n
	}
	t.Fatalf("%s %s: %s=%q has no unit this guard understands (Mi, Gi, MB)", rel, cluster, field, v)
	return 0
}

// parseCPUMilli accepts "500m" and "2". Returns 0 for "".
func parseCPUMilli(t *testing.T, rel, cluster, v string) int {
	t.Helper()
	if v == "" {
		return 0
	}
	if strings.HasSuffix(v, "m") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "m"))
		if err != nil {
			t.Fatalf("%s %s: cpu %q is not a quantity", rel, cluster, v)
		}
		return n
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s %s: cpu %q is not a quantity", rel, cluster, v)
	}
	return n * 1000
}
