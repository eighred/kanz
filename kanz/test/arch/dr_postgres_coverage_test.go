package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY SERVICE THAT OWNS A POSTGRES SCHEMA MUST HAVE A DECIDED DR POSTURE.
//
// The defect this guards is not "a database is unprotected" — it is "nobody
// decided, and the absence of a decision is invisible". Five services holding
// *-db SecretProviderClasses and running migrations were named in no CNPG
// cluster and in no exclusion. Nothing said they were unprotected; they were
// simply missing from the document that would have said so.
//
// WHAT THIS GUARD ASSERTS, AND WHAT IT DELIBERATELY DOES NOT.
//
// It asserts that every migration-owning service is CLASSIFIED — covered by a
// cluster that carries the WAL+PITR contract, excluded with a written reason,
// or explicitly recorded as unresolved — and that the classification, the
// cluster manifests and the README cannot drift apart.
//
// It does NOT assert that every store is backed up, because five are not. A
// guard that claimed otherwise would be the exact failure this repository keeps
// paying for: a green check standing in for a property that does not hold. The
// unresolved five are listed below with their real exposure, this test logs
// them loudly on every run, and #60 is not closed by this file — only its
// classification half is.
func TestEveryMigrationOwningServiceHasADecidedDRPosture(t *testing.T) {
	root := moduleRoot(t)

	onDisk := migrationOwningServices(t, root)
	// NON-VACUITY. A scan that finds no services passes every assertion below
	// no matter how many schemas are undecided — broken guard, not clean estate.
	if len(onDisk) == 0 {
		t.Fatal("found zero services/*/migrations directories — the scanner is broken, not the estate")
	}

	// 1. Every service on disk must be classified. This is the assertion that
	//    closes the original hole: a NEW service with a migrations directory
	//    fails here until somebody decides its DR posture.
	for _, svc := range onDisk {
		if _, ok := drPosture[svc]; !ok {
			t.Errorf("service %q owns services/%s/migrations but has no DR classification\n\n"+
				"It persists state that a region failover would start empty, and nothing anywhere "+
				"records whether that is intended. Add it to drPosture as covered (naming the CNPG "+
				"cluster that holds it), excluded (with the reason its data does not need PITR), or "+
				"unresolved (with the exposure and the issue tracking it) — and add the matching row "+
				"to infra/dr/postgres/README.md.", svc, svc)
		}
	}

	// 2. DEAD-ENTRY CHECK. A classification naming a service that no longer owns
	//    migrations is stale: it protects nothing and makes the table read as
	//    more complete than it is. Same shape as retryCertifiedConsumers.
	present := map[string]bool{}
	for _, s := range onDisk {
		present[s] = true
	}
	var dead []string
	for svc := range drPosture {
		if !present[svc] {
			dead = append(dead, svc)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Errorf("drPosture classifies %d service(s) that no longer own migrations: %s\n\n"+
			"The service was removed or its schema was retired — delete the entry and its README row.",
			len(dead), strings.Join(dead, ", "))
	}

	// 3. Every cluster a service is mapped to must EXIST and carry the WAL+PITR
	//    contract. A mapping to a cluster with no continuous archiving, no
	//    retention or no base backup is a mapping to something that cannot
	//    restore — coverage on paper only.
	clusters := cnpgClusters(t, root)
	if len(clusters) == 0 {
		t.Fatal("parsed zero CNPG clusters from infra/dr/postgres/cluster.yaml — the parser is broken, not the manifest")
	}
	for _, svc := range sortedKeys(drPosture) {
		p := drPosture[svc]
		if p.cluster == "" {
			continue
		}
		c, ok := clusters[p.cluster]
		if !ok {
			t.Errorf("service %q is mapped to CNPG cluster %q, which cluster.yaml does not define\n\n"+
				"Either the cluster was renamed and this mapping was not, or the cluster was never "+
				"added. A service mapped to a cluster that does not exist is not covered.", svc, p.cluster)
			continue
		}
		for _, missing := range c.missingPITRParts() {
			t.Errorf("service %q is mapped to cluster %q, which does not carry the WAL+PITR contract: %s\n\n"+
				"Continuous WAL archiving bounds the RPO; the daily base backup is what the WAL "+
				"replays ONTO. Without both there is no point-in-time recovery, only a pile of WAL "+
				"segments with no base — so this mapping records coverage the cluster cannot deliver.",
				svc, p.cluster, missing)
		}
	}

	// 4. The README must name every service, including the uncovered ones. A
	//    service missing from the human-readable table is exactly how these five
	//    stayed invisible: the Go map alone is not where an operator looks
	//    during a failover.
	readme := readFile(t, filepath.Join(root, "infra", "dr", "postgres", "README.md"))
	for _, svc := range onDisk {
		if !strings.Contains(readme, "`"+svc+"`") {
			t.Errorf("infra/dr/postgres/README.md does not mention service %q\n\n"+
				"The classification exists in Go but not in the document an operator reads while "+
				"deciding what to restore. Add its row to the coverage table.", svc)
		}
	}

	// 5. Say the exposure out loud on every run. These are not passing because
	//    they are fine; they are passing because the gap is now recorded rather
	//    than invisible, which is a strictly smaller claim.
	var unresolved []string
	for svc, p := range drPosture {
		if p.status == drUnresolved {
			unresolved = append(unresolved, svc)
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Logf("DR COVERAGE GAP: %d migration-owning service(s) have NO backup and NO exclusion: %s. "+
			"A region failover starts these empty. This test passing means the gap is CLASSIFIED, "+
			"not closed — see issue #60 and infra/dr/postgres/README.md.",
			len(unresolved), strings.Join(unresolved, ", "))
	}
}

type drStatus int

const (
	drCovered drStatus = iota
	drExcluded
	drUnresolved
)

type drClassification struct {
	status  drStatus
	cluster string // set only when status is drCovered
	reason  string
}

// drPosture is the DR classification of every service that owns a Postgres
// schema. It is DEFAULT-DENY: a service with a migrations directory and no
// entry here fails the guard above.
//
// Keep it in step with the coverage table in infra/dr/postgres/README.md — the
// guard checks that every service appears in both, because the Go map is not
// where an operator looks during a failover and the README is not what fails a
// build.
var drPosture = map[string]drClassification{
	"risk-engine":     {status: drCovered, cluster: "kanz-risk", reason: "PERS-01 portfolio positions and risk state"},
	"schema-registry": {status: drCovered, cluster: "kanz-registry", reason: "EVT-16 registered payload schemas"},
	"accounting":      {status: drCovered, cluster: "kanz-books", reason: "PARITY-02 IBOR ledger journal + snapshots"},
	"alternatives":    {status: drCovered, cluster: "kanz-books", reason: "PARITY-02 fund journal"},
	"wealth":          {status: drCovered, cluster: "kanz-books", reason: "PARITY-02 household book"},
	"datamaster":      {status: drCovered, cluster: "kanz-books", reason: "PARITY-02 golden records + exception queue"},

	"market-data": {status: drExcluded, reason: "append-only price history with its own retention; " +
		"re-ingestable from the upstream feed, so PITR would restore what the feed can replay"},
	"audit": {status: drExcluded, reason: "AUDIT-01b WORM store with its own tamper-resistant " +
		"retention — object-locked, and deliberately not restorable-to-an-earlier-instant, which is " +
		"the point of an audit log"},

	"oms": {status: drUnresolved, reason: "holds orders, positions and position_fills — the order " +
		"store and position book, the money path itself. THE MOST SERIOUS OF THE FIVE: after a " +
		"failover the OMS starts against an empty store and SweepInterrupted logs count=0, the " +
		"identical line a healthy clean start produces. Nothing distinguishes 'no interrupted " +
		"orders' from 'no orders at all', so the platform reports a normal startup while holding " +
		"positions at an exchange it has no record of. This is why CLAUDE.md sequences M3 after " +
		"this issue. Tracked by #60"},
	"venue-binance": {status: drUnresolved, reason: "holds venue_orders — the exchange-order to " +
		"kanz-order mapping that reconciliation and the idempotency/recovery path depend on. " +
		"Losing it means an exchange order cannot be tied back to the order that placed it. " +
		"Tracked by #60"},
	"venue-okx": {status: drUnresolved, reason: "holds venue_orders — same exposure as " +
		"venue-binance. Tracked by #60"},
	"regulatory": {status: drUnresolved, reason: "holds audit_chain_links — the tamper-evidence " +
		"chain. Losing it does not corrupt the chain, it makes the chain unverifiable, which for a " +
		"tamper-evidence structure is most of its value. Tracked by #60"},
	"tv-sync": {status: drUnresolved, reason: "holds tv_facts, a projection of the event log. The " +
		"weakest exposure of the five because it is rebuildable from the events, but 'rebuildable' " +
		"is a claim nobody has exercised — it is listed rather than excluded for that reason. " +
		"Tracked by #60"},
}

// migrationOwningServices returns every service directory containing a
// migrations/ subdirectory — the filesystem's own answer to "who persists
// state", which is why the guard reads it rather than trusting a list.
func migrationOwningServices(t *testing.T, root string) []string {
	t.Helper()

	servicesDir := filepath.Join(root, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		t.Fatalf("read %s: %v", servicesDir, err)
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, serr := os.Stat(filepath.Join(servicesDir, e.Name(), "migrations"))
		if serr == nil && info.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// cnpgCluster is the subset of a CloudNativePG Cluster this guard checks: the
// pieces that together constitute "this database can be restored".
type cnpgCluster struct {
	name              string
	archiveTimeout    bool // continuous WAL archiving cadence — bounds the RPO
	barmanObjectStore bool // where the WAL and base backups actually go
	retentionPolicy   bool // how far back PITR can reach
	scheduledBackup   bool // the base backup the archived WAL replays onto
}

func (c cnpgCluster) missingPITRParts() []string {
	var missing []string
	if !c.barmanObjectStore {
		missing = append(missing, "no spec.backup.barmanObjectStore (nowhere to archive WAL or base backups)")
	}
	if !c.archiveTimeout {
		missing = append(missing, "no postgresql.parameters.archive_timeout (the archived-WAL RPO is unbounded on an idle DB)")
	}
	if !c.retentionPolicy {
		missing = append(missing, "no spec.backup.retentionPolicy (no stated PITR window)")
	}
	if !c.scheduledBackup {
		missing = append(missing, "no ScheduledBackup targeting it (WAL with no base backup to replay onto is not PITR)")
	}
	return missing
}

// cnpgClusters parses infra/dr/postgres/cluster.yaml into the clusters it
// defines and whether each carries the WAL+PITR contract.
//
// It splits on document separators and matches fields textually rather than
// unmarshalling into CNPG's API types, which are not a dependency of this
// module. That is a deliberate trade: the guard can be fooled by a field that
// is present but wrong (an empty destinationPath), and it CANNOT be fooled by
// the failure it exists to catch — a cluster silently missing its backup stanza
// or its ScheduledBackup entirely.
func cnpgClusters(t *testing.T, root string) map[string]cnpgCluster {
	t.Helper()

	body := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "dr", "postgres", "cluster.yaml")))
	nameRe := regexp.MustCompile(`(?m)^\s*name:\s*(\S+)`)
	targetRe := regexp.MustCompile(`cluster:\s*\{\s*name:\s*([^\s}]+)`)

	out := map[string]cnpgCluster{}
	scheduled := map[string]bool{}

	for _, doc := range strings.Split(body, "\n---") {
		switch {
		case strings.Contains(doc, "kind: Cluster"):
			m := nameRe.FindStringSubmatch(doc)
			if m == nil {
				continue
			}
			out[m[1]] = cnpgCluster{
				name:              m[1],
				archiveTimeout:    strings.Contains(doc, "archive_timeout:"),
				barmanObjectStore: strings.Contains(doc, "barmanObjectStore:"),
				retentionPolicy:   strings.Contains(doc, "retentionPolicy:"),
			}
		case strings.Contains(doc, "kind: ScheduledBackup"):
			if m := targetRe.FindStringSubmatch(doc); m != nil {
				scheduled[m[1]] = true
			}
		}
	}

	for name, c := range out {
		c.scheduledBackup = scheduled[name]
		out[name] = c
	}
	return out
}
