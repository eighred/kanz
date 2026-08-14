package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// STRATEGY DETAILS MUST NEVER LEAVE THE ESTATE (#109).
//
// tv-sync serves the TradingView Broker API: an OUTBOUND surface, read by a
// third party, carrying this fund's live positions and orders. #109 states the
// contract exactly:
//
//	Outbound telemetry carries instrument, side, size and entry, and NOTHING
//	about how the decision was made.
//
// What the estate sends is the WHAT. The WHY — which rule refused an order, which
// mandate governs it, which signal produced it, what the model scored — is the
// intellectual property that makes the fund worth anything, and it is worth more
// to a competitor than the positions are.
//
// # This holds today by one deliberate omission, and nothing was keeping it
//
// The projection ALREADY CARRIES A REJECTION REASON. projection.go's transition()
// takes `reason` straight from the OMS's OrderRejected FACT and stores it on
// orderRev — reasons like NOT_ENTITLED, PRICE_UNAVAILABLE, a mandate id, a
// compliance rule. It is dropped at the DTO boundary because OrderDTO simply has
// no field for it.
//
// So the leak is ONE FIELD AWAY: adding `Reason string \`json:"reason"\`` to
// OrderDTO and one line to orderDTO() would ship it, compile cleanly, pass every
// test, and look like an improvement in a review — "the chart should show why the
// order was rejected" is a reasonable-sounding request. This is what stops it.
//
// # What it checks
//
// No JSON-serialised field on tv-sync's outbound surface may name a decision,
// a policy or a model concept.
//
// # What it cannot check
//
// That a permitted field is not stuffed with strategy text — nothing stops
// somebody writing a rule id into `venue`. The names are the tractable half, and
// they are the half that gets added absent-mindedly.

const tvSyncOutboundDir = "services/tv-sync/internal"

var (
	// jsonTagRe captures the serialised name from a struct tag, without its
	// options (`,omitempty`).
	jsonTagRe = regexp.MustCompile("`json:\"([^\",]+)")
	// strategyLeakRe matches a serialised name that describes HOW a decision was
	// made rather than WHAT was traded.
	//
	// Deliberately NOT matching "type" or "status": an order type and a lifecycle
	// status are what the counterparty needs to render a chart, and they say
	// nothing about the decision behind the order.
	strategyLeakRe = regexp.MustCompile(`(?i)^(reason|rationale|why|explanation|comment|note|` +
		`strategy|signal|alert|trigger|mandate|rule|policy|compliance|breach|` +
		`model|score|confidence|conviction|edge|alpha|indicator|threshold|param|params|` +
		`metadata|meta|tags?|internal|debug)`)
)

// strategyLeakExempt maps "<file>:<jsonName>" to the reason it may appear on the
// outbound surface, and what retires the entry.
//
// EMPTY, AND THAT IS THE POINT. There is no field describing this fund's
// reasoning that a third-party charting vendor needs. An entry here has to argue
// that one exists.
var strategyLeakExempt = map[string]string{}

func TestNoStrategyDetailLeavesOnTheTradingViewSurface(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seenExempt := map[string]bool{}
	scanned, tagged := 0, 0

	for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(tvSyncOutboundDir))) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		scanned++
		for _, m := range jsonTagRe.FindAllStringSubmatch(gf.body, -1) {
			name := m[1]
			if name == "-" {
				continue // explicitly NOT serialised, which is the safe answer
			}
			tagged++
			if !strategyLeakRe.MatchString(name) {
				continue
			}
			key := filepath.ToSlash(gf.rel) + ":" + name
			if reason, ok := strategyLeakExempt[key]; ok {
				seenExempt[key] = true
				t.Logf("%s: exempt — %s", key, reason)
				continue
			}
			offenders = append(offenders, key)
		}
	}

	// NON-VACUITY, the walk half: a moved or renamed tv-sync tree scans nothing
	// and this guard passes having read no outbound DTO at all.
	if scanned < 3 {
		t.Fatalf("scanned only %d non-test files under %s — the walk is broken, not the estate",
			scanned, tvSyncOutboundDir)
	}
	// NON-VACUITY, the match half: if the DTOs stop using json tags, this finds no
	// serialised names and passes while asserting nothing about what ships.
	if tagged < 20 {
		t.Fatalf("found %d JSON-tagged field(s) under %s — expected at least 20 (the position, "+
			"order, execution and state DTOs). The scan is broken and this guard is inert",
			tagged, tvSyncOutboundDir)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d field(s) on the OUTBOUND TradingView surface name a decision, not a trade: %v.\n"+
			"#109's contract is that outbound telemetry carries instrument, side, size and entry "+
			"and NOTHING about how the decision was made. The WHY — which rule refused an order, "+
			"which mandate governs it, what a model scored — is the intellectual property that "+
			"makes this fund worth anything, and it is worth more to a competitor than the "+
			"positions are. The projection already HOLDS a rejection reason internally "+
			"(projection.go's transition); it stays in the estate only because no DTO has a field "+
			"for it. Drop the field, or add an argued entry to strategyLeakExempt.",
			len(offenders), offenders)
	}

	// DEAD-ENTRY ARM: an exemption for a field that no longer ships has outlived
	// its argument and would wave the next one through.
	for key, reason := range strategyLeakExempt {
		if !seenExempt[key] {
			t.Errorf("exemption for %q (%s) matches no serialised field — delete it", key, reason)
		}
	}
}

// AND THE REASON REALLY IS IN THERE, one field from the wire.
//
// The guard above is a rule about names. This is the evidence that the rule has
// something to hold back: if the projection ever stops carrying a reason the
// guard becomes theoretical, and a theoretical guard is one somebody deletes as
// unnecessary. If this fails because `reason` is gone, that is good news — delete
// this test and say so.
func TestTheProjectionStillHoldsAReasonThatMustNotShip(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(moduleRoot(t),
		filepath.FromSlash("services/tv-sync/internal/projection/model.go")))
	if err != nil {
		t.Fatalf("read model.go: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\s*reason\s+string`).Match(body) {
		t.Skip("the projection no longer stores a rejection reason — the guard above is now " +
			"precautionary rather than load-bearing, which is a fine state to be in")
	}
	// It is held. Confirm it is not serialised from the struct that holds it.
	if regexp.MustCompile("(?m)^\\s*[Rr]eason\\s+string\\s+`json:").Match(body) {
		t.Fatal("the rejection reason is JSON-tagged on the projection's own types — it would " +
			"ship to TradingView the moment that struct is marshalled")
	}
}
