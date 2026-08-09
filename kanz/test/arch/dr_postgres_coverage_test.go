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
// It does NOT assert that every store is backed up. It asserts that every store
// is DECIDED. As of the commit that placed the last of #60's five, no service is
// drUnresolved — but that is a fact about drPosture today, not a property of
// this guard, and the assertion is deliberately not tightened to "everything is
// covered". A future service may legitimately be excluded, and one may sit
// unresolved while its placement is argued; a guard that forbade that would be
// answered by inventing an exclusion, which is the failure this repository keeps
// paying for wearing a green check. Assertion 5 logs any unresolved store loudly
// on every run instead.
//
// It also does NOT assert that a covered service CONNECTS to the cluster it is
// mapped to. Every DSN is read from Vault at kv/kanz/<service> and no file in
// this module sets it, so "covered" here means the cluster, the standby and the
// promotion exist — declared wiring, checked against three manifests. Whether
// the running service opens that database is settled by #59.
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

	// 5. Say the exposure out loud on every run, if there is any. Silence here is
	//    the only correct silence in this test: an unresolved store is a store
	//    with no backup and no exclusion, and it must never be able to reach a
	//    green run without printing itself.
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
// This is what made #60's placements safe to do at all. Placing a store means
// either adding a cluster or joining one, and the three places that must learn
// about a cluster — cluster.yaml, replica.yaml, failover.sh — are three files
// that no compiler relates. This guard relates them, so a cluster cannot be
// half-added: it fails until a standby exists and the failover script promotes
// it. kanz-compliance was added under exactly that constraint.
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

	"market-data": {status: drExcluded, reason: "price_observations is append-only price history with " +
		"its own retention, re-ingestable from the upstream feed, so PITR would restore what the feed " +
		"can replay. contract_terms (#345) is NOT fully re-ingestable and the difference is recorded " +
		"here deliberately: a venue publishes an instrument's CURRENT specification, not the history " +
		"of its amendments, so a reload re-derives today's strikes and multipliers and cannot re-derive " +
		"the as_of chain a historical revaluation reads. The exclusion still holds — losing it degrades " +
		"backtest fidelity rather than the live book, and the live book is what DR exists for — but " +
		"this entry must not be read as 'everything here is replayable', because half of it is not"},
	"identity": {status: drUnresolved, reason: "identity_users + identity_invites — the platform's " +
		"own accounts and their Argon2id credentials (#364). NOT EXCLUDABLE: credentials exist " +
		"nowhere else, so a region failover onto an empty store locks every operator and trader out " +
		"of the platform, including whoever would perform the recovery. It is deliberately NOT " +
		"marked covered either — the service has migrations but no deployment yet, and claiming a " +
		"cluster it has never been placed in would be exactly the invented exclusion this guard's " +
		"header warns about. The placement question is real: PITR is per-cluster, so putting " +
		"identity in kanz-books would tie a password rotation to the IBOR ledger's restore " +
		"timeline. Tracked by #364, which cannot close while this row still says NOT COVERED"},

	"audit": {status: drExcluded, reason: "AUDIT-01b WORM store with its own tamper-resistant " +
		"retention — object-locked, and deliberately not restorable-to-an-earlier-instant, which is " +
		"the point of an audit log"},

	"oms": {status: drCovered, cluster: "kanz-orders", reason: "holds orders, positions and " +
		"position_fills — the order store and position book, the money path itself. Its OWN " +
		"cluster rather than a database in kanz-books because PITR is per-cluster: rewinding the " +
		"order book to an instant before a bad sweep would otherwise rewind the IBOR ledger, the " +
		"fund journal, the household book and the golden records with it. Losing it is the " +
		"quietest failure in the estate — the OMS starts against an empty store and " +
		"SweepInterrupted logs count=0, the identical line a healthy clean start produces. NOTE: " +
		"this records that the DR wiring is DECLARED (cluster + standby + promotion). The DSN the " +
		"OMS actually opens comes from Vault at kv/kanz/oms, which no file in this repo sets, so " +
		"binding it to kanz-orders-rw.kanz-data.svc is confirmed by #59, not by this entry"},
	"venue-binance": {status: drCovered, cluster: "kanz-orders", reason: "holds venue_orders — the " +
		"exchange-order to kanz-order mapping that reconciliation and the idempotency/recovery path " +
		"depend on. Losing it means an exchange order cannot be tied back to the order that placed " +
		"it. IN kanz-orders, a database beside the OMS's, and that is the whole point rather than " +
		"convenience: the OMS admits an order at its own primary key and only then calls this " +
		"adapter, which writes the mapping row — one logical transaction on the order path. PITR is " +
		"per-cluster, so a cluster of its own would be a SEPARATE RESTORE TIMELINE: restore orders " +
		"to T while venue_orders sits at T' and you get orphan venue mappings, or orders with no " +
		"mapping back to the exchange order at all, which is exactly what recovery needs to read. " +
		"One cluster is one timeline, so the order path restores coherently or not at all. NOTE: " +
		"this records that the DR wiring is DECLARED. The DSN comes from Vault at " +
		"kv/kanz/venue-binance, which no file in this repo sets, so binding it to " +
		"kanz-orders-rw.kanz-data.svc is confirmed by #59, not by this entry"},
	"venue-okx": {status: drCovered, cluster: "kanz-orders", reason: "holds venue_orders — same " +
		"schema and same exposure as venue-binance, and in the same cluster for the same reason: " +
		"the venue mapping and the OMS order it maps must share one restore timeline, because PITR " +
		"is per-cluster and a mapping restored to a different instant than the order is worse than " +
		"no mapping — it is a wrong answer to the question reconciliation asks. NOTE: the DSN comes " +
		"from Vault at kv/kanz/venue-okx and is confirmed by #59, not by this entry"},
	"regulatory": {status: drCovered, cluster: "kanz-compliance", reason: "holds audit_chain_links — " +
		"the tamper-evidence chain. Losing it does not corrupt the chain, it makes the chain " +
		"unverifiable, which for a tamper-evidence structure is most of its value, and the canonical " +
		"signed bytes exist nowhere else so there is nothing to re-derive them from. Its OWN " +
		"cluster, kanz-compliance, deliberately NOT co-located with the order path or the books: " +
		"this chain is evidence ABOUT those stores, PITR is per-cluster, and evidence that gets " +
		"rewound whenever the thing it attests to gets rewound is not evidence — a restore of the " +
		"books would silently take the links covering that window with it, leaving a chain that " +
		"cannot distinguish 'those filings never happened' from 'the record of them was rolled " +
		"back'. Independence is the reason it is separate, not blast radius. NOTE: the DSN comes " +
		"from Vault at kv/kanz/regulatory and is confirmed by #59, not by this entry"},
	"tv-sync": {status: drCovered, cluster: "kanz-books", reason: "holds tv_facts, a projection of " +
		"the order FACT log. COVERED RATHER THAN EXCLUDED, and the rebuild story is why: it was " +
		"chased to the config and it does not hold. The 24h figure in tv-sync's own comments is " +
		"real (infra/nats/bootstrap-job.yaml:100 — the EXECUTION stream carrying order.> has " +
		"max_age 24h), and Kafka does retain order.order for 30d " +
		"(infra/kafka/topics-job.yaml:67, retention.ms=2592000000) — but the DR rebuild reads only " +
		"a bounded recent window of it: infra/dr/nats/rebuild-job.yaml:97 sets " +
		"NATS_REBUILD_SINCE=24h, and order.order is not in NATS_REBUILD_STATE_TOPICS, so it takes " +
		"the time window rather than offset 0. So at most 24h of the 30d log ever returns to the " +
		"spine. Worse, the rebuild's only sink is the bus (tools/natsrebuild/rebuild.go) — it " +
		"restores the SPINE, and nothing anywhere re-drives this projection: the sole path that " +
		"writes tv_facts is the running service, and its Rehydrate reads tv_facts itself " +
		"(services/tv-sync/internal/projection/postgres.go), which is circular when the table is " +
		"empty. Per-tenant it is worse still — infra/dr/nats/rebuild-job.yaml drains only the " +
		"un-prefixed __system__ topics and exits 0 having replayed nothing for any onboarded " +
		"tenant. And no fill is persisted anywhere else, so what is lost is not a cache of " +
		"something durable. IN kanz-books rather than kanz-orders: that cluster's contents are " +
		"already 'event-sourced or replace-on-write projections whose source of truth is the " +
		"append-only journal', which is exactly what this is, and nothing on the order path READS " +
		"tv_facts — it is the read side, so it has no transactional coupling to orders and must " +
		"not share the money path's restore timeline in either direction. NOTE: the DSN comes from " +
		"Vault at kv/kanz/tv-sync and is confirmed by #59, not by this entry"},
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
