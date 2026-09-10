package venuemargin

import (
	"context"
	"math/big"
	"testing"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rat %q", s)
	}
	return r
}

func decimal(t *testing.T, s string) *commonpb.Decimal {
	t.Helper()
	d, ok := dec.ToProtoScaled(rat(t, s))
	if !ok {
		t.Fatalf("not representable: %s", s)
	}
	return d
}

func fold(t *testing.T, v *View, msg *collateralpb.VenueMarginState) {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.Handle(context.Background(), nil, b); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

var (
	observed = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	frozen   = func() time.Time { return observed.Add(10 * time.Second) }
)

func state(t *testing.T) *collateralpb.VenueMarginState {
	t.Helper()
	return &collateralpb.VenueMarginState{
		Venue: "OKX", VenueAccountId: "acct-1",
		MaintenanceMargin: decimal(t, "1250.5"),
		MarginRatio:       decimal(t, "3.75"),
		LiquidationPrices: []*collateralpb.VenueLiquidationPrice{
			{VenueSymbol: "BTC-USDT-SWAP", Price: decimal(t, "41000")},
		},
		ObservedAt:    timestamppb.New(observed),
		KnowledgeTime: timestamppb.New(observed),
		Coverage:      &domainpb.InputCoverage{Contributed: 3},
	}
}

// TestViewServesWhatTheVenueReported is the non-vacuity arm for every refusal
// test below. Without it a View that answered ok=false to everything — a broken
// fold, a wrong key — would pass all of them.
func TestViewServesWhatTheVenueReported(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t))

	mm, ok := v.MaintenanceMargin("OKX", "acct-1")
	if !ok {
		t.Fatal("maintenance margin UNKNOWN after a complete observation")
	}
	if mm.Value().Cmp(rat(t, "1250.5")) != 0 {
		t.Errorf("maintenance margin = %s, want 1250.5", mm.Value().FloatString(4))
	}
	if !mm.ObservedAt().Equal(observed) {
		t.Errorf("observedAt = %s, want %s", mm.ObservedAt(), observed)
	}
	if got := mm.Age(frozen()); got != 10*time.Second {
		t.Errorf("age = %s, want 10s", got)
	}
	if r, ok := v.MarginRatio("OKX", "acct-1"); !ok || r.Value().Cmp(rat(t, "3.75")) != 0 {
		t.Errorf("margin ratio = %v ok=%v, want 3.75 true", r.Value(), ok)
	}
	if lp, ok := v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP"); !ok || lp.Value().Cmp(rat(t, "41000")) != 0 {
		t.Errorf("liquidation price = %v ok=%v, want 41000 true", lp.Value(), ok)
	}
}

// TestUnobservedAccountIsUnknownNotZero: the #408 ruling's control 3 refuses on
// UNKNOWN. If a cold view answered (0, true) instead of (nil, false) the gate
// would pass every order on an account nothing has ever looked at, which is the
// confident-zero defect on the one path where it costs the fund its collateral.
func TestUnobservedAccountIsUnknownNotZero(t *testing.T) {
	v := New(WithClock(frozen))
	for _, tc := range []struct {
		name string
		got  func() (Quantity, bool)
	}{
		{"maintenance margin", func() (Quantity, bool) { return v.MaintenanceMargin("OKX", "acct-1") }},
		{"margin ratio", func() (Quantity, bool) { return v.MarginRatio("OKX", "acct-1") }},
		{"liquidation price", func() (Quantity, bool) { return v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP") }},
	} {
		q, ok := tc.got()
		if ok {
			t.Errorf("%s: an account with no observation answered ok=true", tc.name)
		}
		if q.Value() != nil {
			t.Errorf("%s: unknown answered value %v — UNKNOWN must not carry a number", tc.name, q.Value())
		}
	}
}

// TestQuantityTheVenueDidNotReportIsUnknown: an observation that arrives WITHOUT
// a field is the normal answer from an account in cash mode. The field must stay
// absent rather than materialising as a zero Decimal, which is what dec.FromProto
// hands back for a nil message and what would read as "needs no collateral".
func TestQuantityTheVenueDidNotReportIsUnknown(t *testing.T) {
	v := New(WithClock(frozen))
	msg := state(t)
	msg.MaintenanceMargin = nil
	msg.LiquidationPrices = nil
	msg.Coverage = &domainpb.InputCoverage{Contributed: 1, ExcludedCount: 2}
	fold(t, v, msg)

	if q, ok := v.MaintenanceMargin("OKX", "acct-1"); ok {
		t.Errorf("an unreported maintenance margin answered ok=true with %v", q.Value())
	}
	if q, ok := v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP"); ok {
		t.Errorf("an unreported liquidation price answered ok=true with %v", q.Value())
	}
	// The one the venue DID report is still served: a partial observation is not
	// discarded wholesale, or a cash-mode account would take its margin ratio down
	// with it.
	if _, ok := v.MarginRatio("OKX", "acct-1"); !ok {
		t.Error("a reported margin ratio was refused because a SIBLING field was missing")
	}
}

// TestStaleObservationIsUnknown is the property #408's ruling states directly:
// "a margin ratio from ten minutes ago during a fast move is not a margin ratio".
func TestStaleObservationIsUnknown(t *testing.T) {
	var staleVenue, staleAcct string
	var staleAge time.Duration
	now := observed.Add(DefaultMaxAge + time.Second)
	v := New(
		WithClock(func() time.Time { return now }),
		WithOnStale(func(venue, account string, age time.Duration) {
			staleVenue, staleAcct, staleAge = venue, account, age
		}),
	)
	fold(t, v, state(t))

	if q, ok := v.MaintenanceMargin("OKX", "acct-1"); ok {
		t.Fatalf("an observation %s old was served as current: %v", DefaultMaxAge+time.Second, q.Value())
	}
	if staleVenue != "OKX" || staleAcct != "acct-1" {
		t.Errorf("onStale got (%q,%q), want (OKX,acct-1) — an operator cannot see WHICH account went blind",
			staleVenue, staleAcct)
	}
	if staleAge <= DefaultMaxAge {
		t.Errorf("onStale age = %s, want > %s", staleAge, DefaultMaxAge)
	}
}

// TestMarginBoundIsTighterThanTheCashBound pins the decision, not the constant's
// spelling. balancerecon and riskview both allow 15 minutes; margin moves with
// the mark, and #408's whole scenario is a fast move. A future edit that
// "harmonises" this to the cash bound would be reverting the ruling.
func TestMarginBoundIsTighterThanTheCashBound(t *testing.T) {
	if DefaultMaxAge >= 15*time.Minute {
		t.Errorf("DefaultMaxAge = %s: margin is bounded no tighter than a CASH balance, "+
			"but #408 rules that a margin figure from ten minutes into a fast move is not a margin figure",
			DefaultMaxAge)
	}
	// And the poll must leave room for a retry, or one REST hiccup ages the
	// account out and a fail-closed control becomes a trading halt.
	if DefaultInterval*2 > DefaultMaxAge {
		t.Errorf("DefaultInterval %s leaves no headroom under DefaultMaxAge %s — a single missed poll "+
			"would refuse every order on the account", DefaultInterval, DefaultMaxAge)
	}
}

// TestFoldIsALevelNotADelta: a venue that STOPS reporting a field must not leave
// the last value visible. Merging would keep refreshing the timestamp a stale
// figure is judged against, so the freshness bound could never fire on it.
func TestFoldIsALevelNotADelta(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t))
	if _, ok := v.MaintenanceMargin("OKX", "acct-1"); !ok {
		t.Fatal("setup: first observation did not fold")
	}

	next := state(t)
	next.MaintenanceMargin = nil
	next.LiquidationPrices = nil
	fold(t, v, next)

	if q, ok := v.MaintenanceMargin("OKX", "acct-1"); ok {
		t.Errorf("a maintenance margin the venue stopped reporting is still served as %v", q.Value())
	}
	if q, ok := v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP"); ok {
		t.Errorf("a liquidation price for a position the venue no longer reports is still served as %v", q.Value())
	}
}

// TestUndatedObservationIsRefused: folding a figure with no observation time
// would put a number of unknown age behind a bound that could never judge it.
func TestUndatedObservationIsRefused(t *testing.T) {
	var undated int
	v := New(WithClock(frozen), WithOnUndated(func(string, string) { undated++ }))
	msg := state(t)
	msg.ObservedAt = nil
	fold(t, v, msg)

	if _, ok := v.MaintenanceMargin("OKX", "acct-1"); ok {
		t.Error("an observation carrying no observation time was folded and served")
	}
	if undated != 1 {
		t.Errorf("onUndated fired %d times, want 1 — an undatable margin figure was dropped SILENTLY", undated)
	}
}

// TestAccountsAreNotConflated: the account is the liquidation boundary, and
// serving one account's margin under another's key would report a portfolio as
// safe on collateral that backs somebody else's position.
func TestAccountsAreNotConflated(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t))

	if _, ok := v.MaintenanceMargin("OKX", "acct-2"); ok {
		t.Error("a DIFFERENT account on the same venue was served this account's margin")
	}
	if _, ok := v.MaintenanceMargin("BINANCE", "acct-1"); ok {
		t.Error("a DIFFERENT venue with the same account label was served this account's margin")
	}
}

// TestUncoveredObservationIsAnnounced: an operator watching a margin control
// refuse must be able to tell "the exchange answered with gaps" from "the feed
// went quiet". By lookup time the two are identical.
func TestUncoveredObservationIsAnnounced(t *testing.T) {
	var gotVenue, gotAcct string
	var gotExcluded uint32
	v := New(WithClock(frozen), WithOnUncovered(func(venue, account string, excluded uint32) {
		gotVenue, gotAcct, gotExcluded = venue, account, excluded
	}))
	msg := state(t)
	msg.MaintenanceMargin = nil
	msg.Coverage = &domainpb.InputCoverage{Contributed: 2, ExcludedCount: 1}
	fold(t, v, msg)

	if gotVenue != "OKX" || gotAcct != "acct-1" || gotExcluded != 1 {
		t.Errorf("onUncovered got (%q,%q,%d), want (OKX,acct-1,1)", gotVenue, gotAcct, gotExcluded)
	}
}

// TestUnattributedObservationIsDropped: a margin figure with no account cannot
// be bound to the portfolio it gates, so it must not become a global answer.
func TestUnattributedObservationIsDropped(t *testing.T) {
	v := New(WithClock(frozen))
	msg := state(t)
	msg.VenueAccountId = ""
	fold(t, v, msg)

	if held, _ := v.Stats(); held != 0 {
		t.Errorf("view holds %d accounts after an observation naming none", held)
	}
}

func TestContradictoryOrInvalidSupportStateIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*collateralpb.VenueMarginState)
	}{
		{
			name: "unsupported field carries value",
			mutate: func(msg *collateralpb.VenueMarginState) {
				msg.MaintenanceMarginSupport = collateralpb.SupportStatus_SUPPORT_STATUS_UNSUPPORTED
			},
		},
		{
			name: "unknown enum value",
			mutate: func(msg *collateralpb.VenueMarginState) {
				msg.MarginRatioSupport = collateralpb.SupportStatus(99)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := New(WithClock(frozen))
			msg := state(t)
			tc.mutate(msg)
			fold(t, v, msg)
			if held, _ := v.Stats(); held != 0 {
				t.Errorf("view holds %d accounts after contradictory support state, want 0", held)
			}
		})
	}
}

// TestStatsSeparatesHeldFromCurrent: the gap between the two is what tells an
// operator a margin control is about to refuse every order, minutes before
// anyone files a ticket about rejected orders.
func TestStatsSeparatesHeldFromCurrent(t *testing.T) {
	now := observed
	v := New(WithClock(func() time.Time { return now }))
	fold(t, v, state(t))
	if held, live := v.Stats(); held != 1 || live != 1 {
		t.Fatalf("fresh: held=%d live=%d, want 1/1", held, live)
	}
	now = observed.Add(DefaultMaxAge + time.Second)
	if held, live := v.Stats(); held != 1 || live != 0 {
		t.Errorf("stale: held=%d live=%d, want 1/0 — an aged-out account must still be HELD, "+
			"or the posture gauge cannot show the gap", held, live)
	}
}

func TestCoverageStatsDescribesCurrentPosture(t *testing.T) {
	now := observed
	v := New(WithClock(func() time.Time { return now }))
	fold(t, v, state(t))
	if current, incomplete := v.CoverageStats(); current != 1 || incomplete != 0 {
		t.Fatalf("complete: current=%d incomplete=%d, want 1/0", current, incomplete)
	}

	incomplete := state(t)
	incomplete.Coverage = &domainpb.InputCoverage{Contributed: 1, ExcludedCount: 1}
	fold(t, v, incomplete)
	if current, gaps := v.CoverageStats(); current != 1 || gaps != 1 {
		t.Fatalf("incomplete: current=%d incomplete=%d, want 1/1", current, gaps)
	}

	now = observed.Add(DefaultMaxAge + time.Second)
	if current, gaps := v.CoverageStats(); current != 0 || gaps != 0 {
		t.Errorf("stale: current=%d incomplete=%d, want 0/0", current, gaps)
	}

	now = observed
	unreported := state(t)
	unreported.Coverage = nil
	fold(t, v, unreported)
	if current, gaps := v.CoverageStats(); current != 1 || gaps != 1 {
		t.Errorf("unreported: current=%d incomplete=%d, want 1/1", current, gaps)
	}
}

// TestValueIsACopy: the view hands the same fold to every caller, and *big.Rat
// is mutable. One caller subtracting in place would silently rewrite the stored
// maintenance margin for everybody else.
func TestValueIsACopy(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t))

	q, _ := v.MaintenanceMargin("OKX", "acct-1")
	q.Value().Sub(q.Value(), rat(t, "1250.5"))

	again, ok := v.MaintenanceMargin("OKX", "acct-1")
	if !ok || again.Value().Cmp(rat(t, "1250.5")) != 0 {
		t.Errorf("maintenance margin = %v after a caller mutated its own copy, want 1250.5", again.Value())
	}
}
