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

	// 1b. Every classification must SAY SOMETHING. Assertion 1 only proves a key
	//     exists, and a key is not a decision: `{status: drExcluded}` with no
	//     reason, or `{status: drCovered}` with no cluster, both satisfy it while
	//     recording nothing. The empty cluster is the worse of the two, because
	//     assertion 3 below skips entries with `cluster == ""` — a covered service
	//     naming no cluster was checked against no cluster and reported green.
	//     That is "nothing configured" and "checked, and fine" looking the same,
	//     inside the very guard that exists to tell them apart.
	for _, svc := range sortedKeys(drPosture) {
		p := drPosture[svc]
		if strings.TrimSpace(p.reason) == "" {
			t.Errorf("service %q is classified in drPosture with no written reason\n\n"+
				"The classification is the decision record. Without the reason, the next operator "+
				"cannot tell a considered exclusion from an oversight that happened to be typed in.", svc)
		}
		switch p.status {
		case drCovered:
			if p.cluster == "" {
				t.Errorf("service %q is classified drCovered but names no CNPG cluster\n\n"+
					"Assertion 3 checks the WAL+PITR contract of the cluster a service is mapped to, "+
					"and skips entries that name none — so this entry claims coverage while being "+
					"checked against nothing. Name the cluster, or classify it drExcluded/drUnresolved.", svc)
			}
		case drExcluded, drUnresolved:
			if p.cluster != "" {
				t.Errorf("service %q is classified as not covered yet names cluster %q\n\n"+
					"A service is in a cluster or it is not. Naming one here reads as coverage in "+
					"every listing of this map while the status says the opposite.", svc, p.cluster)
			}
		}
		// An unresolved entry is an OPEN GAP, not an exclusion, and the thing that
		// distinguishes the two is that a gap has an owner. Without an issue number
		// it is indistinguishable from an exclusion whose author ran out of words —
		// and it would sit here forever, because nothing would ever come back to it.
		if p.status == drUnresolved && !issueRefPattern.MatchString(p.reason) {
			t.Errorf("service %q is classified drUnresolved but its reason names no issue\n\n"+
				"An unresolved store has no backup and no exclusion. That is a tracked gap or it is "+
				"an abandoned one; the issue number is the only difference. Cite the issue that "+
				"retires it (e.g. \"Tracked by #60\").", svc)
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

	// 4. The README must carry a row for every service, including the uncovered
	//    ones, AND that row must AGREE with the classification. A service missing
	//    from the human-readable table is exactly how these five stayed invisible:
	//    the Go map alone is not where an operator looks during a failover.
	//
	//    Agreement, not mere mention, is the assertion. A README that says
	//    "covered" beside a service the Go map calls unresolved is worse than a
	//    README that omits it: the omission is a hole an operator may notice,
	//    while the wrong row is an answer they will act on. Reading the table
	//    mid-incident is the one moment nobody re-derives it from the code.
	readme := readFile(t, filepath.Join(root, "infra", "dr", "postgres", "README.md"))
	rows := readmeCoverageRows(readme)
	for _, svc := range onDisk {
		row, ok := rows[svc]
		if !ok {
			t.Errorf("infra/dr/postgres/README.md has no coverage-table row for service %q\n\n"+
				"The classification exists in Go but not in the document an operator reads while "+
				"deciding what to restore. Add its row to the coverage table.", svc)
			continue
		}
		p, classified := drPosture[svc]
		if !classified {
			continue // assertion 1 already reported it; the zero value is not a claim
		}
		want := readmeStatusWord[p.status]
		if !strings.EqualFold(row.status, want) {
			t.Errorf("infra/dr/postgres/README.md says service %q is %q; drPosture says %q\n\n"+
				"The table and the classification disagree, so one of them is wrong and an operator "+
				"restoring during an incident reads the table. Make them say the same thing.",
				svc, row.status, want)
		}
		if p.status == drCovered && !strings.Contains(row.detail, "`"+p.cluster+"`") {
			t.Errorf("infra/dr/postgres/README.md's row for %q does not name cluster `%s`\n\n"+
				"drPosture maps it there. A covered row that names no cluster, or names a different "+
				"one, tells the operator to promote and repoint at the wrong database.", svc, p.cluster)
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

// EVERY CLUSTER A SERVICE IS MAPPED TO MUST HAVE SOMEWHERE TO FAIL OVER TO, AND
// SOMETHING THAT PROMOTES IT.
//
// TestEveryMigrationOwningServiceHasADecidedDRPosture proves the PRIMARY carries
// the WAL+PITR contract. That is backup, and backup is not failover. The failure
// #60 describes is not "the backup was missing" — it is "the service came up
// against an empty store and said nothing". A cluster that archives WAL
// perfectly, but has no DR-region standby replaying it, or has one that
// failover.sh never promotes, produces that outcome exactly: the services start,
// they reach a database, and it is not theirs.
//
// This matters most for the work that is still OPEN on #60. Placing the five
// unresolved stores means adding a cluster, and the three places that must learn
// about it — cluster.yaml, replica.yaml, failover.sh — are three separate files
// that no compiler relates. This guard relates them, so the next cluster cannot
// be half-added: it fails until a standby exists and the failover script
// promotes it.
//
// WHAT IT DELIBERATELY DOES NOT ASSERT. That any of this has ever been executed.
// The DR drill (docs/runbooks/dr-drill.md) is the only thing that proves a
// promotion works, it needs two regions, and this box has neither. Green here
// means the three files agree — not that a failover has been rehearsed.
func TestEveryMappedDRClusterHasAStandbyAndIsPromotedOnFailover(t *testing.T) {
	root := moduleRoot(t)

	var mapped []string
	for _, svc := range sortedKeys(drPosture) {
		if c := drPosture[svc].cluster; c != "" {
			mapped = append(mapped, c)
		}
	}
	// NON-VACUITY. With no mapped clusters every loop below is empty and the test
	// passes while asserting nothing about anything.
	if len(mapped) == 0 {
		t.Fatal("drPosture maps no service to any cluster — the classification is broken, not the estate")
	}

	standbys := replicaClusters(t, root)
	if len(standbys) == 0 {
		t.Fatal("parsed zero replica clusters from infra/dr/postgres/replica.yaml — the parser is broken, not the manifest")
	}
	promoted := failoverPromotedClusters(t, root)
	if len(promoted) == 0 {
		t.Fatal("parsed no promote loop from infra/dr/failover.sh — the parser is broken, not the script")
	}

	seen := map[string]bool{}
	for _, c := range mapped {
		if seen[c] {
			continue
		}
		seen[c] = true

		if !standbys[c] {
			t.Errorf("CNPG cluster %q holds mapped service data but replica.yaml defines no standby for it\n\n"+
				"Its WAL is archived to the object store and nothing in the DR region is replaying it. "+
				"A failover has nothing to promote, so recovery means restoring from scratch inside the "+
				"RTO — which is the DR-01e target this cluster is counted against.", c)
		}
		if !promoted[c] {
			t.Errorf("CNPG cluster %q holds mapped service data but infra/dr/failover.sh never promotes it\n\n"+
				"failover.sh promotes a hardcoded list and then scales the services up. A cluster left "+
				"out of that list is not promoted, so its standby stays read-only while the services "+
				"that depend on it start anyway — the silent-empty-store failure mode in #60, reached "+
				"by a different route than a missing backup.", c)
		}
	}
}

type drStatus int

const (
	drCovered drStatus = iota
	drExcluded
	drUnresolved
)

// readmeStatusWord is the exact token the README coverage table must use for
// each status, so the document and the classification cannot drift into saying
// different things about the same service.
var readmeStatusWord = map[drStatus]string{
	drCovered:    "covered",
	drExcluded:   "excluded",
	drUnresolved: "NOT COVERED",
}

// issueRefPattern matches the "#123" an unresolved classification must cite.
var issueRefPattern = regexp.MustCompile(`#\d+`)

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

// readmeRow is one parsed row of the README's coverage table.
type readmeRow struct {
	status string // the status cell, ** stripped
	detail string // the cluster/reason cell
}

// readmeCoverageRows parses the coverage table out of infra/dr/postgres/README.md,
// keyed by the service named in backticks in the first column.
//
// It keys on `service` in backticks specifically so the other tables in that file
// (the DB-level summary at the top, the file manifest at the bottom) cannot be
// mistaken for coverage rows — neither names a service that way.
func readmeCoverageRows(readme string) map[string]readmeRow {
	out := map[string]readmeRow{}
	for _, line := range strings.Split(readme, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		name := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(name, "`") || !strings.HasSuffix(name, "`") {
			continue
		}
		svc := strings.Trim(name, "`")
		out[svc] = readmeRow{
			status: strings.TrimSpace(strings.ReplaceAll(cells[2], "*", "")),
			detail: strings.TrimSpace(cells[3]),
		}
	}
	return out
}

// replicaClusters returns the DR-region standby clusters defined in replica.yaml
// — the things a failover has to promote.
//
// A Cluster document only counts if it actually declares `replica:`; a plain
// Cluster in that file would be a second primary, not a standby, and promoting
// it is meaningless.
func replicaClusters(t *testing.T, root string) map[string]bool {
	t.Helper()

	body := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "dr", "postgres", "replica.yaml")))
	nameRe := regexp.MustCompile(`(?m)^\s*name:\s*(\S+)`)
	// Anchored to the start of a key, not a substring search: `strings.Contains`
	// on "replica:" also matches "xreplica:" and "readReplica:", so a stanza that
	// had been renamed out of effect still counted as a standby. Found by a
	// mutation that this guard was supposed to fail and did not.
	replicaRe := regexp.MustCompile(`(?m)^\s+replica:\s*$`)

	out := map[string]bool{}
	for _, doc := range strings.Split(body, "\n---") {
		if !strings.Contains(doc, "kind: Cluster") || !replicaRe.MatchString(doc) {
			continue
		}
		if m := nameRe.FindStringSubmatch(doc); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// failoverPromotedClusters returns the clusters infra/dr/failover.sh promotes in
// its postgres step.
//
// The script iterates a HARDCODED list (`for c in kanz-risk kanz-registry
// kanz-books`), which is why this is read at all: adding a cluster to
// cluster.yaml and replica.yaml leaves that list untouched and nothing in the
// build relates the three files. Same reason onboarding_test.go reads
// provision-tenant.sh rather than trusting that someone updated it.
func failoverPromotedClusters(t *testing.T, root string) map[string]bool {
	t.Helper()

	script := readFile(t, filepath.Join(root, "infra", "dr", "failover.sh"))
	m := regexp.MustCompile(`(?m)^\s*for\s+c\s+in\s+([^;]+);\s*do`).FindStringSubmatch(script)
	if m == nil {
		return nil
	}
	out := map[string]bool{}
	for _, f := range strings.Fields(m[1]) {
		out[f] = true
	}
	return out
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
