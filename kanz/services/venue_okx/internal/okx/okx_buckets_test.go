package okx

// Rate-limit family tests (#933).
//
// #933 asks for A MEASUREMENT, NOT AN ASSUMPTION: "today's ceiling should be
// established first, because 'measured behavior over assumption' is the rule and
// the 30→7.5 figure is arithmetic off the declared weights, not an observed
// throughput." So the ceiling here is OBSERVED — the tests drive orders through
// the real adapter path (Execute → placeOrder → queryOrder → fillsHistory) until
// the venue refuses, and count what got through.
//
// THE CLOCK IS INJECTED AND NEVER ADVANCES DURING A MEASUREMENT. A sustained-rate
// test that slept through a real 2-second window would be a flake on a loaded CI
// runner and would measure the runner as much as the adapter. Freezing the clock
// makes "how many calls fit in one window" exact and deterministic: the bucket
// refills by elapsed time, and elapsed time is zero.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// frozenClock is a clock the test moves by hand.
type frozenClock struct{ t time.Time }

func (c *frozenClock) now() time.Time          { return c.t }
func (c *frozenClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// tradedVenueOver builds a venue whose every order TRADES, so each Execute walks
// the full placement path including the fills-history fetch — the sequence whose
// ceiling #933 is about.
func tradedVenueOver(t *testing.T, f *fakeOKX, clk *frozenClock) *OKXVenue {
	t.Helper()
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000","fee":"-0.05","feeCcy":"USDT"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[{"tradeId":"t1","ordId":"312","fillSz":"1","fillPx":"50000","fee":"-0.05","feeCcy":"USDT","ts":"1700000000000"}]}`

	shared := NewWeightBucket(okxDefaultWeightBudget, okxDefaultWeightWindow, clk.now)
	buckets := newOKXBuckets(shared, clk.now)
	rest := newOKXREST(okxRestConfig{
		BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p",
		Buckets: buckets, Mode: exchangeauth.OKXDemo,
	})
	return &OKXVenue{mic: "OKX", rest: rest, symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"}, now: time.Now}
}

// measureTradedCeiling drives orders through Execute until the venue refuses,
// and returns how many completed inside one window.
func measureTradedCeiling(t *testing.T, v *OKXVenue) int {
	t.Helper()
	ctx := context.Background()
	const ceilingProbe = 500 // far above any plausible ceiling; a guard against looping forever
	for i := 0; i < ceilingProbe; i++ {
		st := okxMarket("o1")
		if _, err := v.Execute(ctx, st); err != nil {
			return i
		}
	}
	t.Fatalf("no ceiling observed in %d orders — the bucket is not metering the order path at all", ceilingProbe)
	return 0
}

// THE MEASURED CEILING, AND THE DEFECT IT CLOSES.
//
// Before #933 every endpoint drew from one bucket sized 60/2s from /trade/order's
// limit, and fills-history was charged 6 to express its real 10/2s limit in that
// bucket's units. A traded order therefore cost 1 + 1 + 6 = 8 and the adapter
// self-throttled at 60/8 = 7 completed orders per window — while OKX itself would
// have permitted 60 placements AND 10 fills-history calls concurrently, out of
// separate remote budgets.
//
// With fills-history metered against its own 10/2s bucket the binding constraint
// becomes fills-history's REAL limit, which is what it should have been all along.
func TestTradedOrderCeilingIsTheVenuesLimitNotTheAdaptersModel(t *testing.T) {
	f := newFakeOKX(t)
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	got := measureTradedCeiling(t, tradedVenueOver(t, f, clk))

	// The old shared-bucket model: 60 / (1 place + 1 query + 6 fills) = 7.
	const beforeCeiling = 7
	// fills-history's own documented budget, one call per traded order.
	const wantCeiling = okxFillsHistoryPerWindow

	if got == beforeCeiling {
		t.Fatalf("measured ceiling is %d traded orders per window — unchanged from the shared-bucket "+
			"model, so fills-history is still spending placement budget (#933)", got)
	}
	if got != wantCeiling {
		t.Fatalf("measured ceiling = %d traded orders per window, want %d.\n\n"+
			"The binding constraint should be fills-history's OWN 10-per-2s budget, one call per "+
			"traded order — not an artefact of expressing that endpoint in units of /trade/order's "+
			"60-per-2s bucket.", got, wantCeiling)
	}
}

// THE PLACEMENT PATH MUST NOT BE THROTTLED BY THE TRADE FETCHES BESIDE IT.
//
// This is #933's acceptance criterion stated directly. Exhausting fills-history's
// budget must leave placement budget intact, because they are separate remote
// budgets — an order the venue would accept must not be refused locally because a
// recovery read ran beside it.
func TestExhaustingFillsHistoryDoesNotStarvePlacement(t *testing.T) {
	f := newFakeOKX(t)
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	v := tradedVenueOver(t, f, clk)
	ctx := context.Background()

	// Drain fills-history's bucket completely.
	for i := 0; i < okxFillsHistoryPerWindow; i++ {
		if _, err := v.rest.fillsHistory(ctx, "BTC-USDT", "312"); err != nil {
			t.Fatalf("fills-history %d/%d refused early: %v", i+1, okxFillsHistoryPerWindow, err)
		}
	}
	if _, err := v.rest.fillsHistory(ctx, "BTC-USDT", "312"); err == nil {
		t.Fatal("fills-history did not refuse past its own budget — its bucket is not being metered")
	}

	// Placement budget is untouched: OKX permits 60 placements per 2s regardless
	// of how many fills-history calls were made.
	placed := 0
	for i := 0; i < okxDefaultWeightBudget; i++ {
		if _, err := v.rest.placeOrder(ctx, map[string]string{"instId": "BTC-USDT", "clOrdId": "o1"}); err != nil {
			break
		}
		placed++
	}
	if placed != okxDefaultWeightBudget {
		t.Fatalf("only %d placements admitted after fills-history was exhausted, want %d.\n\n"+
			"A trade fetch is spending placement budget: the two are separate remote budgets and "+
			"draining one must not starve the other (#933).", placed, okxDefaultWeightBudget)
	}
}

// THE CONVERSE, AND IT IS THE HALF THAT KEEPS THE SPLIT HONEST. Separating the
// buckets must not make either one UNMETERED — a bucket nobody draws down is not
// a rate limiter, and the failure would look like healthy throughput right up to
// the venue's 50011s.
func TestExhaustingPlacementDoesNotStarveFillsHistoryAndBothStillRefuse(t *testing.T) {
	f := newFakeOKX(t)
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	v := tradedVenueOver(t, f, clk)
	ctx := context.Background()

	for i := 0; i < okxDefaultWeightBudget; i++ {
		if _, err := v.rest.placeOrder(ctx, map[string]string{"instId": "BTC-USDT", "clOrdId": "o1"}); err != nil {
			t.Fatalf("placement %d/%d refused early: %v", i+1, okxDefaultWeightBudget, err)
		}
	}
	if _, err := v.rest.placeOrder(ctx, map[string]string{"instId": "BTC-USDT", "clOrdId": "o1"}); err == nil {
		t.Fatal("placement did not refuse past its budget — the shared bucket is not metering")
	}
	// fills-history still has its whole budget.
	for i := 0; i < okxFillsHistoryPerWindow; i++ {
		if _, err := v.rest.fillsHistory(ctx, "BTC-USDT", "312"); err != nil {
			t.Fatalf("fills-history %d refused after placement was exhausted: %v — the split is not real", i+1, err)
		}
	}
}

// THE BUDGET REFILLS ON ITS OWN WINDOW, per family. A bucket that never refilled
// would pass every test above and throttle the connector permanently after one
// window in production.
func TestEachFamilyRefillsOnItsOwnWindow(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	b := newOKXBuckets(NewWeightBucket(okxDefaultWeightBudget, okxDefaultWeightWindow, clk.now), clk.now)

	for i := 0; i < okxFillsHistoryPerWindow; i++ {
		if !b.allow(familyFillsHistory, 1) {
			t.Fatalf("fills-history refused at %d, inside its budget of %d", i+1, okxFillsHistoryPerWindow)
		}
	}
	if b.allow(familyFillsHistory, 1) {
		t.Fatal("fills-history admitted past its budget")
	}
	clk.advance(okxFillsHistoryWindow)
	if !b.allow(familyFillsHistory, 1) {
		t.Fatal("fills-history did not refill after its window — the connector would throttle forever")
	}
}

// AN UNVERIFIED FAMILY BEHAVES EXACTLY AS IT DID BEFORE #933.
//
// The endpoints whose real OKX limit is not established still share one bucket at
// /trade/order's budget. That is the conservative direction and is deliberate:
// giving each its own bucket would raise what this connector is willing to send
// against limits nobody has verified, which is how a connector starts collecting
// 50011s on the capital path. This test pins that so a later change cannot split
// them on invented numbers without saying so.
func TestUnverifiedFamiliesStillShareOneBudget(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	b := newOKXBuckets(NewWeightBucket(okxDefaultWeightBudget, okxDefaultWeightWindow, clk.now), clk.now)

	spent := 0
	for b.allow(familyUnverified, 1) {
		spent++
		if spent > okxDefaultWeightBudget*2 {
			t.Fatal("the unverified family is not metered at all")
		}
	}
	if spent != okxDefaultWeightBudget {
		t.Fatalf("the unverified family admitted %d calls, want %d — it no longer draws from the "+
			"shared budget it shared before #933", spent, okxDefaultWeightBudget)
	}
	// And it is genuinely the SAME bucket for two different unverified endpoints:
	// they must starve each other, because nothing has established that they do not.
	if b.bucketFor(familyUnverified) != b.shared {
		t.Fatal("the unverified family is no longer the shared bucket")
	}
}

// AN UNRECOGNISED FAMILY FALLS TO THE SHARED BUCKET, NOT TO A FRESH ONE. A fresh
// bucket would hand an unnamed endpoint a brand-new full budget — the generous
// direction on a value nobody declared.
func TestAnUnknownFamilyDrawsFromTheSharedBudget(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0).UTC()}
	b := newOKXBuckets(NewWeightBucket(okxDefaultWeightBudget, okxDefaultWeightWindow, clk.now), clk.now)
	if got := b.bucketFor(okxRateFamily(9999)); got != b.shared {
		t.Fatal("an unrecognised rate family got its own bucket — an endpoint nobody classified " +
			"would be handed a full budget rather than the conservative shared one")
	}
}

// A NIL SHARED BUCKET MUST NOT REACH THE ORDER PATH. It would panic on the first
// Allow, at runtime, on placement.
func TestNilSharedBucketIsReplacedRatherThanPanicking(t *testing.T) {
	b := newOKXBuckets(nil, nil)
	if b.shared == nil {
		t.Fatal("nil shared bucket survived construction — the first placement would nil-panic")
	}
	if !b.allow(familyUnverified, 1) {
		t.Fatal("the substituted bucket admits nothing")
	}
}

// EVERY METERED CALL NAMES A FAMILY, and none reaches a bucket directly.
//
// This is the guard the okxBuckets type comment promises. Without it, "add an
// endpoint and forget to classify it" is invisible: a new `c.buckets.shared.Allow(1)`
// compiles, passes every test above, and silently puts another endpoint back on
// the placement budget — reintroducing #933 one endpoint at a time.
//
// It reads the REST sources with comments STRIPPED, because a guard that greps
// raw source matches its own prose: every paragraph above naming `.Allow(` would
// satisfy or trip it for the wrong reason.
func TestEveryMeteredCallGoesThroughTheFamilySeam(t *testing.T) {
	files := []string{"okx_rest.go", "okx_margin.go"}
	seam := regexp.MustCompile(`c\.buckets\.allow\((family[A-Za-z]+),`)
	direct := regexp.MustCompile(`\.(Allow|RetryAfter)\(`)

	total := 0
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := stripGoComments(string(raw))

		if got := direct.FindAllString(src, -1); len(got) > 0 {
			t.Fatalf("%s calls a bucket directly (%v).\n\nEvery metered call must go through "+
				"c.buckets.allow(family..., n) so the endpoint's rate-limit family is named at the "+
				"call site. A direct call puts the endpoint back on whichever bucket it happened to "+
				"reach, which is how #933 happened.", name, got)
		}
		found := seam.FindAllStringSubmatch(src, -1)
		total += len(found)
		for _, m := range found {
			switch m[1] {
			case "familyUnverified", "familyFillsHistory":
			default:
				t.Fatalf("%s meters against unknown family %q — add it to okxRateFamily and to "+
					"bucketFor, or this endpoint silently draws from the shared budget", name, m[1])
			}
		}
	}
	if total == 0 {
		t.Fatal("no metered call sites found at all — the guard is matching nothing and would pass " +
			"over a REST client with its rate limiting removed entirely")
	}
	if total < 11 {
		t.Fatalf("found %d metered call sites, expected at least 11 — endpoints appear to have lost "+
			"their metering", total)
	}
}

// stripGoComments removes // and /* */ comments so a guard matches CODE.
func stripGoComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
