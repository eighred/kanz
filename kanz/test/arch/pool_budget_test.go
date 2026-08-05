package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/pg"
)

// EVERY POSTGRES POOL IS SIZED IN ONE PLACE, AND max_connections MUST FUND THE
// SIZE THE ESTATE'S OWN MANIFESTS ADD UP TO (#228).
//
// Nothing in this repository set MaxConns or a statement_timeout — a grep across
// *.go, *.yaml and *.sql for MaxConns, pool_max_conns, statement_timeout,
// lock_timeout or idle_in_transaction returned ZERO. Two consequences, both
// invisible until load:
//
//   - pgx defaults MaxConns to max(4, runtime.NumCPU()), and runtime.NumCPU() in a
//     container with no CPU quota is the NODE's core count. On a 16-core node every
//     pod took 16 connections against a Postgres defaulting to max_connections=100.
//     Pools are lazy, so the first symptom is `FATAL: sorry, too many clients
//     already` on a QUERY in whichever service warmed last — not at startup, and
//     not phrased as "the pool is too big".
//   - statement_timeout=0 means one blocked query holds its connection forever,
//     which is how the pool gets exhausted in the first place.
//
// Six composition roots also called pgxpool.New directly, going around the one
// constructor that sets the tenant GUC — the "17 copies of secret(), 15 of them
// wrong" shape, on the pool that carries this platform's isolation property.
//
// This guard therefore asserts three things, none of them by grepping for a
// number someone wrote down:
//
//	ARM 1  internal/pg BEHAVIOURALLY sizes and bounds a pool. It runs the real
//	       Configure over a real parsed DSN and reads the resulting pgxpool.Config.
//	ARM 2  no composition root builds a pool any other way.
//	ARM 3  the arithmetic. Demand is re-derived from infra/deploy on every run and
//	       compared with the max_connections the Postgres manifest declares.
func TestConnectionBudgetIsDerivedAndFunded(t *testing.T) {
	root := moduleRoot(t)

	// --- ARM 1: the constructor sizes and bounds, and refuses not to -----------
	//
	// Behavioural, not textual. `cfg.MaxConns = p.MaxConns` deleted from
	// Configure, or a Profile that stopped carrying a MaxConns, both leave
	// pgxpool.ParseConfig's max(4, runtime.NumCPU()) in cfg — which on the CI
	// runner may well BE 4, so comparing against a hardcoded 4 would pass on a
	// broken Configure. The two checks below are what make that impossible: the
	// value must equal the exported budget term AND every bound must be present.
	for _, p := range []pg.Profile{pg.Service, pg.Migration} {
		cfg, err := pgxpool.ParseConfig("postgres://u:p@example.invalid:5432/db?sslmode=disable")
		if err != nil {
			t.Fatalf("parse a plain DSN: %v", err)
		}
		if err := p.Configure(cfg); err != nil {
			t.Fatalf("profile %q refused to configure a plain DSN: %v", p.Name, err)
		}
		if cfg.MaxConns != p.MaxConns {
			t.Errorf("internal/pg profile %q left cfg.MaxConns at %d, not the %d it declares.\n\n"+
				"An unsized pool inherits pgx's max(4, runtime.NumCPU()), and in a container with no CPU "+
				"quota that is the NODE's core count — every pod takes a share of max_connections nobody "+
				"chose, and the estate finds out under load as `sorry, too many clients already` on a "+
				"query. Configure MUST assign cfg.MaxConns.", p.Name, cfg.MaxConns, p.MaxConns)
		}
		for _, guc := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
			if _, ok := cfg.ConnConfig.RuntimeParams[guc]; !ok {
				t.Errorf("internal/pg profile %q sets no %s on the connection.\n\n"+
					"An unset %s is 0 — unbounded. One blocked query then holds its backend forever, and "+
					"a pool of %d is exhausted by %d of them. If this profile genuinely wants no bound it "+
					"must say so with pg.Unbounded, which still WRITES the GUC: 'nothing configured' and "+
					"'checked, and fine' must not look the same in pg_settings either.",
					p.Name, guc, guc, p.MaxConns, p.MaxConns)
			}
		}
	}
	if pg.Service.StatementTimeout <= 0 {
		t.Errorf("pg.Service.StatementTimeout is %s. The service profile is the one that must NOT be "+
			"unbounded: an unbounded service query holds a pool slot until the connection dies, which is "+
			"the exhaustion path #228 exists for. Only pg.Migration may be Unbounded, and only because "+
			"kanz-migrate's -timeout flag is already the single bound on that run.", pg.Service.StatementTimeout)
	}
	// Refusing an unsized pool is half of "fail loudly": a Configure that quietly
	// skipped a zero MaxConns would leave the node's core count in place and look
	// identical, on inspection, to a pool sized on purpose.
	if err := (pg.Profile{Name: "nameless"}).Configure(&pgxpool.Config{ConnConfig: nil}); err == nil {
		t.Error("internal/pg accepted a Profile with no MaxConns. An unsized profile must be REFUSED, " +
			"not defaulted: a pool that fell back to pgx's max(4, runtime.NumCPU()) is indistinguishable " +
			"from one somebody sized, and that is exactly how #228 stayed invisible.")
	}

	// --- ARM 2: nothing builds a pool any other way ---------------------------
	bare := barePoolConstructions(t, root)
	var offenders []string
	for f := range bare {
		if _, ok := barePoolExempt[f]; ok {
			continue
		}
		offenders = append(offenders, f)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these non-test files call pgxpool.New/NewWithConfig directly instead of internal/pg:\n  %s\n\n"+
			"A bare pool gets NO MaxConns (pgx uses the node's core count), NO statement_timeout (one "+
			"blocked query holds a backend forever), and — for a tenant-scoped store — NO AfterConnect "+
			"setting app.tenant_id, which is how accounting once shipped a ledger that silently read and "+
			"wrote nothing. Use pg.NewTenantPool, pg.NewGlobalPool (which requires a written reason the "+
			"store has no RLS) or pg.NewMigrationPool.", strings.Join(offenders, "\n  "))
	}
	// NON-VACUITY for arm 2. This scan is a regex over a file walk: a moved
	// directory, a renamed symbol or a broken walk all report "no offenders",
	// which is the same output as a clean estate. Prove the scanner can still see
	// the thing it is looking for by requiring it to find internal/pg's own
	// legitimate call.
	if !bare[filepath.ToSlash(filepath.Join("internal", "pg", "pool.go"))] {
		t.Error("the pgxpool.New scan did not find internal/pg/pool.go, which certainly does call " +
			"pgxpool.NewWithConfig. The scanner is not seeing what it claims to check, so its empty " +
			"result means nothing — did the package move, or did the walk stop early?")
	}

	// --- ARM 3: the arithmetic, for EVERY POSTGRES THIS ESTATE RUNS -----------
	//
	// The first version of this guard compared the whole estate's demand against
	// infra/deploy/postgres-dev.yaml, whose own first line says "DEV-ONLY Postgres
	// for the kind rig. NOT a production database." It went green while the five
	// CNPG clusters that hold the money path, the books and the evidence chain were
	// neither sized nor checked — a guard passing because it read the wrong file,
	// which is worse than no guard: it makes "nothing configured" and "checked, and
	// fine" look the same in the one place built to tell them apart.
	//
	// Production is not one server, so the demand is a PARTITION of the dev sum, not
	// a copy of it. A cluster boundary in cluster.yaml is a restore-timeline
	// boundary, and a service reaches only the cluster holding its own database.
	est := k8sEstate(t, root)
	pools := poolsPerService(t, root)
	if len(pools) < 10 {
		t.Fatalf("found only %d service binaries that open a Postgres pool; this estate has more than ten. "+
			"A demand sum over almost nothing passes no matter how small max_connections is", len(pools))
	}

	targets, unattributed, problems := pgTargets(t, root, est, pools)
	for _, p := range problems {
		t.Error(p)
	}
	// NON-VACUITY. Every check below loops over this slice. A parser that stopped
	// early, or a renamed directory, yields an empty list and a green run that
	// compared nothing against nothing — the exact failure this arm was written to
	// repair.
	if len(targets) < 6 {
		t.Fatalf("found only %d Postgres server(s) to check (the dev rig + five CNPG clusters + their "+
			"standbys are expected). A demand check over almost no servers passes no matter how the "+
			"estate is sized", len(targets))
	}

	for _, tgt := range targets {
		d := tgt.demand(est)
		if tgt.declared < 0 {
			t.Errorf("%s: Postgres %q declares no max_connections, and it needs %d.\n      %s\n\n%s",
				tgt.file, tgt.name, d.need, d.explain, undeclaredMaxConnectionsAdvice(tgt))
			continue
		}
		if tgt.declared < d.need {
			t.Errorf("%s: Postgres %q declares max_connections=%d and the services that reach it need "+
				"%d.\n      %s\n\nOver the ceiling, pods do not degrade: whichever one warms last gets "+
				"`FATAL: sorry, too many clients already` on a query it was serving. Raise "+
				"max_connections, lower a maxReplicaCount, or lower pg.ServiceMaxConns. Do not leave "+
				"the three disagreeing.", tgt.file, tgt.name, tgt.declared, d.need, d.explain)
		}
		if tgt.declared > d.need*poolBudgetSlackFactor {
			t.Errorf("%s: Postgres %q declares max_connections=%d, more than %dx the %d the declared "+
				"ceiling needs.\n      %s\n\nA max_connections that large bounds nothing and is "+
				"indistinguishable from not having chosen one, while still costing shared memory and a "+
				"per-backend allocation for connections that cannot arrive. Either the ceilings shrank "+
				"and this was not followed down, or the number was picked rather than derived.",
				tgt.file, tgt.name, tgt.declared, poolBudgetSlackFactor, d.need, d.explain)
		}
	}

	// --- ARM 4: A STANDBY MUST NOT BE SMALLER THAN ITS PRIMARY ----------------
	//
	// Postgres refuses to open a hot standby whose max_connections is below the
	// value recorded in the WAL it is replaying:
	//
	//	FATAL: hot standby is not possible because max_connections = 100 is a
	//	lower setting than on the primary server (its value was 200)
	//
	// So raising a primary here and leaving replica.yaml alone does not degrade
	// the DR region — it STOPS it, in the region nobody is watching, and the
	// discovery moment is the failover. cluster.yaml and replica.yaml are two files
	// no compiler relates, which is the same reason
	// dr_postgres_coverage_test.go already relates them for the PITR contract.
	byName := map[string]*pgTarget{}
	for i := range targets {
		byName[targets[i].key()] = &targets[i]
	}
	standbys := 0
	for i := range targets {
		s := &targets[i]
		if s.standbyOf == "" {
			continue
		}
		standbys++
		p, ok := byName["primary/"+s.standbyOf]
		if !ok {
			t.Errorf("%s: standby %q has no primary named %q in cluster.yaml — it replays WAL from a "+
				"cluster this guard cannot find, so its sizing is checked against nothing",
				s.file, s.name, s.standbyOf)
			continue
		}
		if s.declared >= 0 && p.declared >= 0 && s.declared < p.declared {
			t.Errorf("%s: standby %q declares max_connections=%d, BELOW its primary %q (%d).\n\n"+
				"Postgres refuses to open a hot standby with a lower max_connections than the primary "+
				"whose WAL it is replaying — \"hot standby is not possible because max_connections = %d "+
				"is a lower setting than on the primary server\". The standby stops replaying, in the DR "+
				"region, and nothing in the primary region reports it: the failure surfaces at the "+
				"failover, which is the one moment it cannot be fixed. Raise it to at least the "+
				"primary's value.", s.file, s.name, s.declared, p.name, p.declared, s.declared)
		}
	}
	if standbys == 0 {
		t.Error("parsed no standby clusters from infra/dr/postgres/replica.yaml, so the " +
			"standby-not-below-primary check above ran over nothing. Every mapped cluster has a " +
			"standby (dr_postgres_coverage_test.go asserts it), so finding none is a broken parse.")
	}

	// --- ARM 5: A POOL THAT REACHES AN UNDECLARED SERVER MUST SAY SO ----------
	//
	// market-data and audit are drExcluded — deliberately, and for good reasons
	// recorded there — so no CNPG cluster holds them. They still open pools in
	// production, against a Postgres no file in this repository declares. Dropping
	// them from the sum silently is how the partition would quietly stop adding up
	// to the whole; naming them is what keeps the gap visible while it is genuine.
	for _, key := range sortedKeys(unattributed) {
		reason, ok := undeclaredServerPools[key]
		if !ok {
			t.Errorf("%s opens a Postgres pool (%d connections at its declared ceiling) against a "+
				"server no manifest in this repository declares.\n\n"+
				"It is in no CNPG cluster in infra/dr/postgres/cluster.yaml and its DSN comes from "+
				"Vault, so its demand is counted against nothing and no max_connections anywhere is "+
				"checked against it. Either place it in a cluster, or record the exposure in "+
				"undeclaredServerPools with the issue that closes it.", key, unattributed[key])
			continue
		}
		t.Logf("UNSIZED POSTGRES: %s needs %d connections from a server this repository does not "+
			"declare. %s", key, unattributed[key], reason)
	}
	var deadUndeclared []string
	for key := range undeclaredServerPools {
		if _, ok := unattributed[key]; !ok {
			deadUndeclared = append(deadUndeclared, key)
		}
	}
	sort.Strings(deadUndeclared)
	if len(deadUndeclared) > 0 {
		t.Errorf("undeclaredServerPools names %d pool(s) that now reach a declared server, or no longer "+
			"exist: %s\n\nThe exposure was closed and the entry outlived it, which makes the estate read "+
			"as less sized than it is.", len(deadUndeclared), strings.Join(deadUndeclared, ", "))
	}

	// --- DEAD ENTRIES ---------------------------------------------------------
	var dead []string
	for key := range barePoolExempt {
		if !bare[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("barePoolExempt names %d file(s) that no longer call pgxpool.New at all: %s\n\n"+
			"Either the file was repaired (drop the entry) or it moved (update the key). A stale "+
			"exemption is standing permission for a defect nobody is committing, and it hides the next "+
			"one that lands in the same file.", len(dead), strings.Join(dead, ", "))
	}
}

// poolBudgetSlackFactor bounds max_connections from ABOVE as well as below, for
// the same reason the quota guard bounds the ResourceQuota: checking only "big
// enough" is satisfied forever by a number so large it stops being a decision.
const poolBudgetSlackFactor = 2

// barePoolExempt is the default-deny allow-list of non-test files permitted to
// construct a pgxpool without internal/pg. Keyed by module-relative,
// slash-separated path; the value is the reason and the issue that retires it. A
// dead entry fails the guard, so an exemption cannot outlive its repair.
//
// IT CONTAINS ONLY internal/pg ITSELF, AND THAT IS THE ANSWER RATHER THAN AN
// OVERSIGHT. #228 moved all six roots that went around it — kanz-migrate, audit,
// market-data, regulatory, risk-engine's price pool and schema-registry — onto
// pg.NewMigrationPool or pg.NewGlobalPool. Nothing else on this estate has a
// reason to build a pool by hand, so nothing else is listed; the map exists so
// that the next thing which needs to writes a reason and an issue here, reviewed
// in, rather than quietly reintroducing the six.
//
// TEST FILES ARE OUT OF SCOPE, deliberately: several of them build a BARE pool on
// purpose, to prove that an unscoped connection is refused by RLS
// (accounting/internal/ledger/postgres_test.go) or to act as an admin fixture.
// Forcing those through internal/pg would delete the contrast the test is made of.
var barePoolExempt = map[string]string{
	"internal/pg/pool.go": "this IS the one constructor. Everything above exists so that this is the " +
		"only line in the module that calls pgxpool.NewWithConfig.",
}

var (
	barePoolCall = regexp.MustCompile(`pgxpool\.New(WithConfig)?\(`)
	pgPoolCall   = regexp.MustCompile(`pg\.New(TenantPool|GlobalPool|MigrationPool)\(`)
)

// barePoolConstructions returns every non-test .go file in the module that
// constructs a pgxpool directly, keyed by module-relative slash path.
func barePoolConstructions(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures, not code that runs; vendor is not ours.
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if !barePoolCall.MatchString(readFile(t, p)) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// which pool reaches which server
// ---------------------------------------------------------------------------

// undeclaredServerPools is the default-deny record of pools whose Postgres this
// repository does not declare. Keyed "service/<dsn expression>"; the value states
// the exposure and the issue that closes it. A dead entry fails the guard.
//
// These are NOT oversights, which is the point of writing them down rather than
// dropping them from the sum. market-data and audit are classified drExcluded in
// dr_postgres_coverage_test.go with reasons — a re-ingestable price history and a
// WORM audit store that is deliberately not restorable-to-an-instant — so neither
// belongs in a PITR cluster, and consequently neither has a server declared here
// to size. Their DSNs come from Vault (#59), like every production DSN.
var undeclaredServerPools = map[string]string{
	"audit/cfg.DatabaseURL": "audit is drExcluded (AUDIT-01b WORM store with its own tamper-resistant " +
		"retention), so no CNPG cluster holds it and no manifest here declares the server it reaches. " +
		"Its demand is small — one pool at a ceiling of 2 — but it is UNSIZED, not zero. Closing this " +
		"needs the WORM store's server declared somewhere this guard can read; the DSN binding is #59.",

	"market-data/cfg.DatabaseURL": "market-data is drExcluded (append-only price history, re-ingestable " +
		"from the upstream feed), so no CNPG cluster holds it. This is the LARGEST unsized demand on the " +
		"estate: KEDA scales market-data to 16, so its pool alone wants 64 connections, and the risk " +
		"engine's price pool below lands on the same server. Whatever serves price_observations in " +
		"production is sized by nobody. DSN binding is #59.",

	"risk-engine/cfg.MarketDataURL": "risk-engine's SECOND pool, and it CROSSES A CLUSTER BOUNDARY: it " +
		"reads market-data's price_observations, not risk state, so it does NOT land on kanz-risk. " +
		"Charging it to kanz-risk would over-size that cluster and hide this gap at the same time. It " +
		"lands wherever market-data's store lives, which is the undeclared server above — at a ceiling " +
		"of 12, another 48 connections onto it.",
}

// foreignPoolReaches names, for every pool opened with a DSN that is NOT the
// service's own, whose database it actually reaches. Default-deny: a pool built
// from an unrecognised DSN expression fails the guard rather than being charged
// to its holder's cluster, because guessing that would silently over-size one
// cluster and under-size another.
//
// Keyed "service/<dsn expression>" → the service whose database is on the other
// end. A dead entry fails the guard.
var foreignPoolReaches = map[string]string{
	"risk-engine/cfg.MarketDataURL": "market-data",
}

// ownDSNExpr are the expressions that mean "this service's own database". Both
// spellings are in use: services read cfg.DatabaseURL, and kanz-migrate is handed
// a bare dsn.
var ownDSNExpr = map[string]bool{"cfg.DatabaseURL": true, "dsn": true}

// pgPoolCallArg captures the DSN expression each pool is opened with, which is
// what makes "where does this pool land" derived rather than listed.
var pgPoolCallArg = regexp.MustCompile(`pg\.New(?:TenantPool|GlobalPool|MigrationPool)\(\s*\w+\s*,\s*([^,)]+)`)

// poolsPerService lists the DSN expression of every pool each service BINARY
// opens, by reading its composition root. Derived rather than listed: a service
// that grows a second pool — as risk-engine did, for the market-data price store
// — changes this without anyone remembering to update a table.
//
// Only cmd/ trees are scanned. A pool is opened at the composition root on this
// estate by construction (arm 2 above is what keeps that true), and scanning a
// whole service tree would also match the constructor named in a comment.
//
// cmd/kanz-migrate is deliberately NOT scanned. Its initContainer runs to
// completion before its pod's app container starts, so its single connection
// never coexists with the four already reserved for that same pod, and the surge
// term already covers a rollout's worth of them.
func poolsPerService(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	services := filepath.Join(root, "services")
	entries, err := os.ReadDir(services)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		var exprs []string
		_ = filepath.WalkDir(filepath.Join(services, svc, "cmd"),
			func(p string, d fs.DirEntry, werr error) error {
				if werr != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
					return nil
				}
				for _, m := range pgPoolCallArg.FindAllStringSubmatch(readFile(t, p), -1) {
					exprs = append(exprs, strings.TrimSpace(m[1]))
				}
				return nil
			})
		if len(exprs) > 0 {
			sort.Strings(exprs)
			out[svc] = exprs
		}
	}
	return out
}

// poolWorkload finds the Deployment/Rollout named after a service. Matching by
// name is safe here because every workload under infra/deploy is named for its
// service; a service whose workload cannot be found is REPORTED, not skipped —
// silently skipping it is what would make the demand sum under-state.
func poolWorkload(est *k8sEstateDocs, svc string) *k8sWorkload {
	for i := range est.workloads {
		if est.workloads[i].name == svc {
			return &est.workloads[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the servers
// ---------------------------------------------------------------------------

// pgTarget is one Postgres server this estate runs, and the pools that reach it.
type pgTarget struct {
	name      string         // kanz-books, or the dev Deployment's name
	file      string         // the manifest that declares it
	declared  int            // its max_connections, or -1 when the manifest is silent
	holders   map[string]int // service -> how many of ITS pools land here
	standbyOf string         // the primary this replays, for a replica cluster
	note      string         // what this server is, for the derivation text
}

func (t pgTarget) key() string {
	if t.standbyOf != "" {
		return "standby/" + t.name
	}
	return "primary/" + t.name
}

type pgDemand struct {
	need    int
	explain string
}

// demand is the same model the dev rig already used, applied per server:
// Σ ceiling(w) x pools(w) x ServiceMaxConns, plus the largest single rolling
// update while at ceiling, plus the operator reserve.
func (tg pgTarget) demand(est *k8sEstateDocs) pgDemand {
	var (
		steady   int
		surge    int
		surgeWho string
		lines    []string
	)
	for _, svc := range sortedKeys(tg.holders) {
		n := tg.holders[svc]
		w := poolWorkload(est, svc)
		if w == nil {
			continue
		}
		ceiling := est.ceilingFor(w)
		conns := ceiling * n * int(pg.ServiceMaxConns)
		steady += conns
		lines = append(lines, fmt.Sprintf("%s: %d pods at ceiling x %d pool(s) x %d = %d",
			svc, ceiling, n, pg.ServiceMaxConns, conns))
		if s := int(k8sSurgePods(w, int64(ceiling))) * n * int(pg.ServiceMaxConns); s > surge {
			surge, surgeWho = s, svc
		}
	}
	if surgeWho == "" {
		surgeWho = "none"
	}
	need := steady + surge + pg.OperatorReserve
	return pgDemand{
		need: need,
		explain: fmt.Sprintf("%s\n      derivation (pg.ServiceMaxConns = %d):\n        %s\n"+
			"      steady = %d\n      + surge %d for the largest single rolling update while at "+
			"ceiling (%s)\n      + reserve %d (superuser_reserved_connections, an operator, a DR "+
			"restore, a human with psql)\n      = %d",
			tg.note, pg.ServiceMaxConns, strings.Join(lines, "\n        "),
			steady, surge, surgeWho, pg.OperatorReserve, need),
	}
}

// undeclaredMaxConnectionsAdvice says what to do about a server that names no
// max_connections, and deliberately does NOT quote a default to compare against.
//
// THE DEFAULT IS NOT ESTABLISHABLE FROM THIS REPOSITORY. For the dev rig it is
// the postgres:16 image's, which is at least pinned by the image tag. For a CNPG
// cluster it is whatever the operator applies, and nothing here pins an operator
// version: there is no install manifest, no Helm chart, no kustomize reference
// and no terraform for CloudNativePG anywhere in the tree — only
// `apiVersion: postgresql.cnpg.io/v1` and the kubectl plugin in the runbooks. So
// an unset parameter here is not "the default, which is fine"; it is a number
// nobody reading this repository can name, which is the one state CLAUDE.md
// forbids. Declaring it is what makes it checkable at all.
func undeclaredMaxConnectionsAdvice(tg pgTarget) string {
	if tg.standbyOf != "" {
		return "A standby inherits nothing from its primary here — replica.yaml declares its own " +
			"postgresql.parameters — and Postgres REFUSES to open a hot standby whose max_connections " +
			"is below the primary's. Declare it at least equal to " + tg.standbyOf + "'s."
	}
	return "Declare `postgresql.parameters.max_connections`. Leaving it unset does not mean " +
		"\"the default, which is adequate\": no CloudNativePG version is pinned anywhere in this " +
		"repository — no install manifest, no Helm chart, no terraform — so the value that would " +
		"apply cannot be read from here, and an unknowable number cannot be compared with a demand. " +
		"Set it, with the arithmetic beside it, so that raising a maxReplicaCount is a red build " +
		"rather than `sorry, too many clients already` during the market event the scale-out was for."
}

// pgTargets builds every Postgres this estate runs: the dev rig from
// infra/deploy, and the CNPG primaries and standbys from infra/dr/postgres.
//
// It returns the unattributable pools separately rather than dropping them,
// because a partition that quietly loses a term is how the sum stops meaning
// anything.
func pgTargets(t *testing.T, root string, est *k8sEstateDocs, pools map[string][]string) (
	targets []pgTarget, unattributed map[string]int, problems []string) {
	t.Helper()
	unattributed = map[string]int{}

	// --- THE DEV RIG: one server, every pool ---------------------------------
	//
	// On the kind rig every DSN resolves to postgres.kanz-services.svc — the
	// separate databases in its initdb script (kanzapp, tvsync, venuebinance) are
	// databases on ONE server, and max_connections is per server. So the rig's
	// demand is the WHOLE estate's, including the pools that are unattributable in
	// production.
	dev := pgTarget{
		declared: -1,
		holders:  map[string]int{},
		note: "the kind rig runs every service against ONE Postgres (its initdb script creates " +
			"kanzapp, tvsync and venuebinance as databases on one server, and max_connections is " +
			"per server), so its demand is the whole estate's",
	}
	for _, w := range est.workloads {
		for _, c := range w.containers {
			if !strings.HasPrefix(c.image, "postgres:") {
				continue
			}
			dev.name, dev.file = w.name, w.file
			dev.declared = argMaxConnections(t, w.file, c.args)
		}
	}
	if dev.name == "" {
		t.Fatal("no Postgres workload found under infra/deploy (no container whose image starts " +
			"with \"postgres:\"). The dev rig is one of the servers this arm compares against, so " +
			"not finding it means part of the arithmetic is checked against nothing.")
	}
	for svc, exprs := range pools {
		dev.holders[svc] += len(exprs)
	}
	targets = append(targets, dev)

	// --- PRODUCTION: one target per CNPG cluster -----------------------------
	primaries := cnpgMaxConnections(t, root, filepath.Join("infra", "dr", "postgres", "cluster.yaml"))
	replicas := cnpgMaxConnections(t, root, filepath.Join("infra", "dr", "postgres", "replica.yaml"))
	if len(primaries) == 0 {
		t.Fatal("parsed zero clusters from infra/dr/postgres/cluster.yaml — the parser is broken, " +
			"not the manifest")
	}

	byCluster := map[string]map[string]int{}
	for name := range primaries {
		byCluster[name] = map[string]int{}
	}
	for _, svc := range sortedKeys(pools) {
		for _, expr := range pools[svc] {
			// Whose database does this pool reach? Its own, or a named other's.
			reaches := svc
			if !ownDSNExpr[expr] {
				key := svc + "/" + expr
				target, known := foreignPoolReaches[key]
				if !known {
					problems = append(problems, fmt.Sprintf(
						"%s opens a pool from DSN expression %q, and nothing records whose database "+
							"that reaches.\n\nIt is not the service's own (cfg.DatabaseURL), so charging "+
							"it to this service's cluster would over-size that cluster and under-size "+
							"whichever one actually serves it — silently, in both directions. Add it to "+
							"foreignPoolReaches naming the service whose database is on the other end.",
						key, expr))
					continue
				}
				reaches = target
			}
			// And which cluster holds that service's database?
			cluster := drPosture[reaches].cluster
			if cluster == "" {
				unattributed[svc+"/"+expr] += poolCost(est, svc)
				continue
			}
			if _, ok := byCluster[cluster]; !ok {
				problems = append(problems, fmt.Sprintf(
					"service %q is mapped by drPosture to cluster %q, which cluster.yaml does not "+
						"define — its connection demand is counted against no server at all",
					reaches, cluster))
				continue
			}
			byCluster[cluster][svc]++
		}
	}

	for _, name := range sortedKeys(primaries) {
		targets = append(targets, pgTarget{
			name: name, file: "infra/dr/postgres/cluster.yaml",
			declared: primaries[name], holders: byCluster[name],
			note: "the production primary; a cluster boundary here is a restore-timeline boundary, so " +
				"only the services whose own database it holds reach it",
		})
		if mc, ok := replicas[name]; ok {
			targets = append(targets, pgTarget{
				name: name, file: "infra/dr/postgres/replica.yaml",
				declared: mc, holders: byCluster[name], standbyOf: name,
				note: "the DR-region warm standby, which is promoted to serve exactly the demand its " +
					"primary serves — so it is sized against the same services",
			})
		}
	}

	// DEAD ENTRIES in foreignPoolReaches: a mapping for a pool that no longer
	// exists reads as an accounted-for crossing that nothing crosses.
	live := map[string]bool{}
	for svc, exprs := range pools {
		for _, e := range exprs {
			live[svc+"/"+e] = true
		}
	}
	var dead []string
	for key := range foreignPoolReaches {
		if !live[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		problems = append(problems, fmt.Sprintf(
			"foreignPoolReaches names %d pool(s) that no longer exist: %s\n\nThe crossing was removed "+
				"and the entry outlived it.", len(dead), strings.Join(dead, ", ")))
	}
	return targets, unattributed, problems
}

// poolCost is what one pool of one service costs at that service's declared
// ceiling — used for the unattributable pools, which have no server to sum onto
// but whose size is the whole reason for naming them.
func poolCost(est *k8sEstateDocs, svc string) int {
	w := poolWorkload(est, svc)
	if w == nil {
		return 0
	}
	return est.ceilingFor(w) * int(pg.ServiceMaxConns)
}

var maxConnectionsArg = regexp.MustCompile(`^max_connections\s*=\s*(\d+)$`)

// argMaxConnections reads `-c max_connections=N` out of a container's argv,
// returning -1 when it is not there.
func argMaxConnections(t *testing.T, file string, args []string) int {
	t.Helper()
	for _, a := range args {
		if m := maxConnectionsArg.FindStringSubmatch(strings.TrimSpace(a)); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s: max_connections=%q is not a number", file, m[1])
			}
			return n
		}
	}
	return -1
}

var cnpgMaxConnRe = regexp.MustCompile(`(?m)^\s*max_connections:\s*"?(\d+)"?\s*$`)

// cnpgMaxConnections parses a CNPG manifest into cluster name ->
// postgresql.parameters.max_connections, or -1 where the cluster declares none.
//
// Textual, for the same reason cnpgClusters in dr_postgres_coverage_test.go is:
// CNPG's API types are not a dependency of this module. Comments are stripped
// first so a number written in the derivation prose cannot be read as a setting.
func cnpgMaxConnections(t *testing.T, root, rel string) map[string]int {
	t.Helper()
	body := stripYAMLComments(readFile(t, filepath.Join(root, filepath.FromSlash(rel))))
	nameRe := regexp.MustCompile(`(?m)^\s*name:\s*(\S+)`)

	out := map[string]int{}
	for _, doc := range strings.Split(body, "\n---") {
		if !strings.Contains(doc, "kind: Cluster") {
			continue
		}
		m := nameRe.FindStringSubmatch(doc)
		if m == nil {
			continue
		}
		n := -1
		if mc := cnpgMaxConnRe.FindStringSubmatch(doc); mc != nil {
			v, err := strconv.Atoi(mc[1])
			if err != nil {
				t.Fatalf("%s: max_connections %q in cluster %q is not a number", rel, mc[1], m[1])
			}
			n = v
		}
		out[m[1]] = n
	}
	return out
}
