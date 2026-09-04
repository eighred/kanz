package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE ESTATE PUBLISHES AN ORDER BOOK IT DOES NOT KEEP, AND FOUR DOCUMENTS USED
// TO SAY OTHERWISE (#1005).
//
// market-ingest folds full-rate L2 depth from Binance and OKX in memory and
// publishes a bounded snapshot on market.book.snapshot once per second per
// instrument. Three separate, deliberate decisions then throw all of it away:
// the MARKET stream ages market.> out after a day, the archiver's
// DefaultSubjects names no market subject, and lake-sink's topic list excludes
// market.book. Each of the three is correct on its own terms — L2 depth is the
// highest-volume domain in the estate and the CURRENT book is re-fetchable from
// the venue (DATA-M1, archived_topics_test.go's notArchivedByDesign).
//
// What was wrong was what the code SAID about it. The book package doc, the
// market-ingest composition root and the order_book.proto package doc all
// claimed the snapshots existed "for durable replay, cross-node bootstrap, and
// audit". Nothing replays them, nothing bootstraps from them and nothing audits
// with them; the same repository states the opposite disposition one directory
// away, in the DR exemption that says the book is cold after a failover until
// the venue feeds refill it. A reader sizing "can we reconstruct the book?" would
// have read the doc and stopped — which is the stale-comment failure this
// repository has already been bitten by, in its benign form.
//
// # The decision this guard records
//
// The estate does NOT keep order-book history, and the absence is enumerated
// rather than invisible. Two other answers were available and were rejected:
//
//   - Build the consumer the doc promised. A store or replay surface for a
//     subject nothing queries is the dark seam this repository keeps filing
//     issues about, and internal/alpha/barview's exemption already names the
//     shape: a consumer written to satisfy a guard moves no capability.
//   - Stop publishing the snapshot. It is the only statement of the book that
//     leaves the process, and deleting it would strand the wire-compatibility
//     defences that exist BECAUSE it exists (mark.Handle's event-type refusal,
//     and the two price-spine subscriptions that narrow away from it) as
//     narrations of a hazard with no instance — turning "wire a reader" into
//     "rebuild the producer". The repository has already ruled against deleting
//     an unwired capability, in internal/prediction/registry's own doc.
//
// The institutionally useful artifact is a different one and is not this: a book
// capture taken on the EXECUTION path at fill time and attached to the fill's
// cost record. It is bounded by fill count rather than tick rate and lands in an
// already-archived domain, so it does not disturb the DATA-M1 volume decision.
// That is its own piece of work.
//
// # What this binds together, in both directions
//
//	services/archiver's DefaultSubjects                   the archive says so
//	infra/deploy/lake-sink-deploy.yaml LAKE_SINK_TOPICS    the lake says so
//	infra/nats/bootstrap-job.yaml's MARKET ensure_stream   the spine says so
//	the four claim sites                                   the docs say so
//
// While all three retention artifacts exclude the book, every claim site must
// carry the marker. If ANY of them starts keeping it, this guard fails the other
// way — four files telling a reader that book history does not exist while it
// does is the same defect with the sign flipped, and nothing else in the
// toolchain sees a stale disclaimer in a doc comment or in YAML.
//
// # What it cannot see, stated rather than implied
//
// It checks RETENTION, which is the half that is decided in artifacts. It cannot
// see a service that subscribes the subject and holds the book in a database of
// its own: that would be a new consumer, and there is no manifest that would
// have to change for it. The floor is worth having because the expensive failure
// — a doc asserting three consumers that never existed — is entirely on this
// side of it.

// bookHistoryMarker is the annotation every claim site must carry while the
// estate keeps no book history. One distinctive all-caps sentence rather than a
// phrase neighbouring prose could assemble by accident: guards in this package
// have passed with the checked thing deleted because they matched their own
// explanation.
const bookHistoryMarker = "BOOK HISTORY IS NOT RETAINED (#1005)"

// bookSnapshotSubjectConst is the name of the constant the producer publishes
// on. The subject STRING is read off that constant rather than written here, so
// this guard cannot end up arguing about a subject the code stopped using.
const bookSnapshotSubjectConst = "SubjectBookSnapshot"

// bookKafkaTopic is the {domain}.{entity} topic a market.book.snapshot event
// would land on if anything archived it. Provisioned today with no producer,
// which is the state notArchivedByDesign records.
const bookKafkaTopic = "market.book"

// bookHistoryClaimSites are the documents that tell a reader what the book
// snapshot is for, each with what it is load-bearing for. Listed by path rather
// than discovered by grep: the claim is paraphrased differently at each site,
// and a pattern loose enough to find all four would also match this file.
var bookHistoryClaimSites = []struct {
	path string
	why  string
}{
	{
		path: filepath.Join("internal", "marketedge", "book", "book.go"),
		why: "the book package doc is where the three promised purposes were written down, and " +
			"it is the first thing anyone asking what the snapshot is for will open",
	},
	{
		path: filepath.Join("services", "market-ingest", "cmd", "market-ingest", "main.go"),
		why: "the composition root is what an operator reads to find out what this pod emits and " +
			"why, and it repeated the durable-replay claim in its own words",
	},
	{
		path: filepath.FromSlash("../kanz-schemas/proto/market/v1/order_book.proto"),
		why: "the schema doc reaches every consumer in every language, including ones outside " +
			"this module, and it additionally claimed OrderBookDelta was schema'd for a " +
			"durable-log replay consumer that does not exist",
	},
	{
		path: filepath.Join("infra", "nats", "bootstrap-job.yaml"),
		why: "the MARKET stream's max-age IS the retention policy for L2 depth, and it is read by " +
			"the person standing the estate up, who does not open Go",
	},
}

// marketStreamRow captures the MARKET row of the bootstrap script's stream
// table, max-age included. audit_covers_every_stream_test.go's ensureStream
// captures name and subjects only and is load-bearing for that test's contract;
// this needs one more field, not a different one — the same reason
// archived_topics_test.go carries topicRowPolicy beside topicRow.
var marketStreamRow = regexp.MustCompile(`(?m)^\s*ensure_stream\s+MARKET\s+"([^"]+)"\s+(\S+)`)

// lakeSinkTopicsValue captures the LAKE_SINK_TOPICS value from the deployment.
// Anchored on the env entry rather than searched for as a substring: the
// manifest explains the market.book exclusion in a comment directly above the
// list, so a scan of the raw file would find the topic in the prose that says it
// is EXCLUDED and conclude the lake keeps the book.
var lakeSinkTopicsValue = regexp.MustCompile(`(?m)^\s*-\s*name:\s*LAKE_SINK_TOPICS\s*$\n\s*value:\s*"([^"]*)"`)

// bookSnapshotSubject reads the published subject off the producer's own
// constant. Parsed without comments, so the engine's prose about the subject
// cannot stand in for the declaration.
func bookSnapshotSubject(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "internal", "marketedge", "ingest", "engine.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var subject string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if name.Name != bookSnapshotSubjectConst || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				t.Fatalf("unquote %q: %v", lit.Value, uerr)
			}
			subject = s
		}
		return true
	})
	if subject == "" {
		t.Fatalf("internal/marketedge/ingest/engine.go declares no %s constant.\n\n"+
			"If the book snapshot is no longer published at all, this guard and the four claim "+
			"sites it pins are arguing about a subject that does not exist — delete them together. "+
			"If it merely moved, move this reader with it: everything below is derived from the "+
			"subject that constant names.", bookSnapshotSubjectConst)
	}
	return subject
}

// bookHistoryRetainers returns the artifacts that would make a past book state
// recoverable, empty when none does. Derived from the three files that decide
// it rather than tabulated here — the defect this guard exists for is a claim
// that stopped matching the estate, and a list written into the guard would be a
// further copy of the thing that drifted.
func bookHistoryRetainers(t *testing.T, root, subject string) []string {
	t.Helper()
	var retainers []string

	// 1. The archiver. Its DefaultSubjects is what DATA-M1 drains into Kafka.
	subjects := archiverDefaultSubjects(t, root)
	if len(subjects) == 0 {
		t.Fatal("no archiver DefaultSubjects parsed — the archival arm of this guard is blind")
	}
	if !slices.Contains(subjects, "order.>") {
		t.Fatalf("archiver DefaultSubjects parse looks wrong — expected order.> among %v", subjects)
	}
	for _, s := range subjects {
		if subjectSpaceCovers(s, subject) {
			retainers = append(retainers, fmt.Sprintf(
				"the archiver drains %q, which covers %s — book snapshots now reach Kafka", s, subject))
		}
	}

	// 2. The lake. Kafka has no subject wildcard, so the topic set is explicit.
	lakePath := filepath.Join(root, "infra", "deploy", "lake-sink-deploy.yaml")
	lake := readFile(t, lakePath)
	m := lakeSinkTopicsValue.FindStringSubmatch(lake)
	if m == nil {
		t.Fatalf("no LAKE_SINK_TOPICS value parsed from %s — has the manifest shape changed? "+
			"The lake arm of this guard would otherwise pass by reading nothing", lakePath)
	}
	for _, topic := range strings.Split(m[1], ",") {
		if strings.TrimSpace(topic) == bookKafkaTopic {
			retainers = append(retainers, fmt.Sprintf(
				"lake-sink lands %q — the book is in the durable lake", bookKafkaTopic))
		}
	}

	// 3. The live spine itself. An unlimited MARKET max-age makes JetStream the
	// archive, which is a different answer to the same question and would make
	// the claim sites false without either of the two above changing.
	bootPath := filepath.Join(root, "infra", "nats", "bootstrap-job.yaml")
	boot := readFile(t, bootPath)
	row := marketStreamRow.FindStringSubmatch(boot)
	if row == nil {
		t.Fatalf("no `ensure_stream MARKET` row parsed from %s — has the bootstrap format changed? "+
			"The retention arm of this guard would otherwise pass by reading nothing", bootPath)
	}
	if d, err := time.ParseDuration(row[2]); err != nil || d <= 0 {
		retainers = append(retainers, fmt.Sprintf(
			"the MARKET stream is provisioned with max-age %q, which is not a bounded age — the "+
				"live spine is now the book archive", row[2]))
	}

	sort.Strings(retainers)
	return retainers
}

// TestBookHistoryAbsenceIsStated pins the four claim sites against the three
// artifacts that decide whether a past book state is recoverable.
func TestBookHistoryAbsenceIsStated(t *testing.T) {
	root := moduleRoot(t)
	self := filepath.Join("test", "arch", "book_history_is_not_retained_test.go")

	// Non-vacuity, before anything is read. A shortened table is a guard that
	// checks less than it says, and this file carries the marker in its own
	// source — listing itself would make one site vouch for the other three by
	// reading the guard.
	if len(bookHistoryClaimSites) < 4 {
		t.Fatalf("bookHistoryClaimSites has %d entries, want the four documents that say what the "+
			"book snapshot is for", len(bookHistoryClaimSites))
	}
	for _, site := range bookHistoryClaimSites {
		if site.path == self {
			t.Fatalf("%s lists itself as a claim site — it carries the marker in its own source "+
				"and would vouch for nothing", self)
		}
		if site.why == "" {
			t.Errorf("%s is listed with no reason: an entry that does not say what it is "+
				"load-bearing for cannot be judged when someone wants to delete it", site.path)
		}
	}

	subject := bookSnapshotSubject(t, root)
	retainers := bookHistoryRetainers(t, root, subject)

	for _, site := range bookHistoryClaimSites {
		body, err := os.ReadFile(filepath.Join(root, site.path))
		if err != nil {
			t.Errorf("read %s: %v", site.path, err)
			continue
		}
		// Non-vacuity: an emptied or moved file satisfies the "marker removed"
		// arm for entirely the wrong reason.
		if len(strings.TrimSpace(string(body))) == 0 {
			t.Errorf("%s is empty — this guard would pass by reading nothing", site.path)
			continue
		}
		has := strings.Contains(string(body), bookHistoryMarker)

		switch {
		case len(retainers) == 0 && !has:
			t.Errorf("%s does not carry %q.\n\n"+
				"Nothing keeps %s: the archiver drains no market subject, lake-sink excludes %s, "+
				"and the MARKET stream ages it out — so no past book state is recoverable and the "+
				"question \"what did the book look like when this order filled\" has no answer. %s. "+
				"A document that says, or implies, that these snapshots are there for replay or "+
				"audit sends a reader away believing the estate holds evidence it discards within "+
				"a day (#1005).",
				site.path, bookHistoryMarker, subject, bookKafkaTopic, site.why)
		case len(retainers) > 0 && has:
			t.Errorf("%s still carries %q, but book history IS now retained:\n  - %s\n\n"+
				"A stale disclaimer is the same defect as a stale claim. Remove the marker from "+
				"every claim site, and state what the retention now buys — including whether the "+
				"per-second snapshot cadence is fine enough for what the retention is for.",
				site.path, bookHistoryMarker, strings.Join(retainers, "\n  - "))
		}
	}

	// Guarded on t.Failed as well as on the state: this line is the record that
	// every site was checked and every site stated it, and printing it beside a
	// failure would say the opposite of what the failure just reported.
	if len(retainers) == 0 && !t.Failed() {
		t.Logf("book history is not retained; %d claim site(s) state it", len(bookHistoryClaimSites))
	}
}
