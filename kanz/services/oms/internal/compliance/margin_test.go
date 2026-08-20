package compliance

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

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venuemargin"
)

// THE JOIN #408 CONTROL 3 RESTS ON: (tenant, portfolio, venue) → exchange
// ACCOUNT → what the venue said about it.
//
// These tests drive the REAL venuemargin.View, folding real
// collateral.v1.VenueMarginState messages, so the freshness bound and the
// coverage discipline being asserted are the shipped ones and not a stand-in.

const (
	testVenue   = "XOKX"
	testAccount = "okx-sub-1"
	testSpec    = "acme/fund-alpha@XOKX=okx-sub-1"
)

func bindings(t *testing.T, spec string) *execution.AccountBindings {
	t.Helper()
	b, err := execution.ParseBindings(spec)
	if err != nil {
		t.Fatalf("ParseBindings(%q): %v", spec, err)
	}
	return b
}

// observe folds one VenueMarginState into the view.
func observe(t *testing.T, v *venuemargin.View, msg *collateralpb.VenueMarginState) {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// state builds a well-formed observation: a margin ratio, full coverage, and an
// observation time.
func state(at time.Time, ratio *commonpb.Decimal, cov *domainpb.InputCoverage) *collateralpb.VenueMarginState {
	return &collateralpb.VenueMarginState{
		Venue:          testVenue,
		VenueAccountId: testAccount,
		MarginRatio:    ratio,
		ObservedAt:     timestamppb.New(at),
		KnowledgeTime:  timestamppb.New(at),
		Coverage:       cov,
	}
}

func fullCoverage() *domainpb.InputCoverage {
	return &domainpb.InputCoverage{Contributed: 2, ExcludedCount: 0}
}

// A CURRENT, COMPLETE OBSERVATION OF A BOUND ACCOUNT RESOLVES, and it arrives
// with its observation time attached.
func TestMarginSource_ResolvesABoundAccount(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(now, &commonpb.Decimal{Coefficient: 5, Exponent: -1}, fullCoverage()))

	src := NewMarginSource(bindings(t, testSpec), view)
	got, ok := src.Margin("acme", "fund-alpha", testVenue)
	if !ok {
		t.Fatal("a current, fully covered observation of a bound account read as UNKNOWN")
	}
	if got.Account != testAccount {
		t.Errorf("Account = %q, want %q — the exchange account IS the liquidation boundary",
			got.Account, testAccount)
	}
	if got.Ratio == nil || got.Ratio.Cmp(big.NewRat(1, 2)) != 0 {
		t.Errorf("Ratio = %v, want 1/2", got.Ratio)
	}
	if !got.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v — the number must not arrive without its timestamp",
			got.ObservedAt, now)
	}
	if !got.Complete() {
		t.Errorf("Complete() = false for a coverage record with zero exclusions: %+v", got)
	}
}

// STALE IS UNKNOWN, THROUGH THE SHIPPED BOUND.
//
// venuemargin.DefaultMaxAge is two minutes — far tighter than the fifteen cash
// and risk allow — because margin moves with the mark and the whole scenario
// #408 guards against is a fast move. A margin ratio from ten minutes ago during
// one is not a margin ratio; it is the number that was true before the move that
// is about to liquidate the account.
func TestMarginSource_StaleIsUnknown(t *testing.T) {
	observedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	now := observedAt
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(observedAt, &commonpb.Decimal{Coefficient: 5, Exponent: -1}, fullCoverage()))

	src := NewMarginSource(bindings(t, testSpec), view)
	if _, ok := src.Margin("acme", "fund-alpha", testVenue); !ok {
		t.Fatal("the observation read as unknown while it was fresh — the rest of this test proves nothing")
	}

	now = observedAt.Add(venuemargin.DefaultMaxAge + time.Second)
	if got, ok := src.Margin("acme", "fund-alpha", testVenue); ok {
		t.Fatalf("an observation older than DefaultMaxAge (%s) still answered with a ratio (%v) — "+
			"this is the stale book #408 exists to remove", venuemargin.DefaultMaxAge, got.Ratio)
	}
}

// A NEVER-OBSERVED ACCOUNT IS UNKNOWN. Before the first observation there is no
// margin state, and there is no default to fall back to.
func TestMarginSource_NeverObservedIsUnknown(t *testing.T) {
	src := NewMarginSource(bindings(t, testSpec), venuemargin.New())

	if _, ok := src.Margin("acme", "fund-alpha", testVenue); ok {
		t.Fatal("an account nothing has ever observed answered as KNOWN")
	}
}

// AN UNBOUND PORTFOLIO IS UNKNOWN, NOT EXEMPT.
//
// With no binding the OMS may still route the order, against whatever account
// the adapter holds and alongside every other unbound portfolio — a shared
// collateral pool. There is then no account whose margin backs THIS portfolio,
// so no per-portfolio answer could be true, and the honest answer is unknown.
func TestMarginSource_UnboundPortfolioIsUnknown(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(now, &commonpb.Decimal{Coefficient: 5, Exponent: -1}, fullCoverage()))

	src := NewMarginSource(bindings(t, testSpec), view)
	if _, ok := src.Margin("acme", "fund-beta", testVenue); ok {
		t.Fatal("a portfolio bound to NO account was given another portfolio's margin state — " +
			"the collateral-segregation failure the whole #408 set is built on")
	}
	if _, ok := src.Margin("other-tenant", "fund-alpha", testVenue); ok {
		t.Fatal("a DIFFERENT TENANT's identically named portfolio resolved to this tenant's " +
			"exchange account (#243, one door over)")
	}
}

// ABSENT COVERAGE SURVIVES THE FOLD AS ABSENT.
//
// InputCoverage's contract is that presence is the signal: absent means "this
// publisher does not report coverage", present-with-zero means "it reports, and
// everything was covered". A uint32 renders both as zero, and the collapse hides
// the worse of the two.
func TestMarginSource_AbsentCoverageIsNotZeroExclusions(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(now, &commonpb.Decimal{Coefficient: 5, Exponent: -1}, nil))

	src := NewMarginSource(bindings(t, testSpec), view)
	got, ok := src.Margin("acme", "fund-alpha", testVenue)
	if !ok {
		t.Fatal("a current observation with no coverage record read as entirely unknown")
	}
	if got.CoverageReported {
		t.Error("an observation carrying NO coverage record reported CoverageReported=true")
	}
	if got.Complete() {
		t.Error("an observation that does not say what it left out was reported COMPLETE — this " +
			"is InputCoverage's zero value being read as 'everything resolved'")
	}
}

// REPORTED EXCLUSIONS SURVIVE THE FOLD WITH THEIR COUNT.
func TestMarginSource_ExclusionsArriveWithTheirCount(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(now, &commonpb.Decimal{Coefficient: 5, Exponent: -1},
		&domainpb.InputCoverage{Contributed: 1, ExcludedCount: 2}))

	src := NewMarginSource(bindings(t, testSpec), view)
	got, ok := src.Margin("acme", "fund-alpha", testVenue)
	if !ok {
		t.Fatal("a current observation with exclusions read as entirely unknown")
	}
	if !got.CoverageReported || got.ExcludedCount != 2 {
		t.Errorf("got %+v, want CoverageReported=true ExcludedCount=2", got)
	}
	if got.Complete() {
		t.Error("an observation the venue could not answer in full was reported COMPLETE")
	}
}

// AN ABSENT RATIO STAYS ABSENT. dec.FromProto would hand back a zero for a nil
// Decimal, and that zero would read as an active claim about the account.
func TestMarginSource_AbsentRatioIsNilNotZero(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	view := venuemargin.New(venuemargin.WithClock(func() time.Time { return now }))
	observe(t, view, state(now, nil, &domainpb.InputCoverage{ExcludedCount: 1}))

	src := NewMarginSource(bindings(t, testSpec), view)
	got, ok := src.Margin("acme", "fund-alpha", testVenue)
	if !ok {
		t.Fatal("a current observation without a ratio read as entirely unknown")
	}
	if got.Ratio != nil {
		t.Errorf("Ratio = %v, want nil — a margin ratio the venue never reported must not "+
			"materialise as zero", got.Ratio)
	}
}

// NO VIEW AND NO VENUE ARE BOTH UNKNOWN. A deployment that observes no margin
// must answer unknown rather than nil-panic or invent a state.
func TestMarginSource_NoViewOrNoVenueIsUnknown(t *testing.T) {
	if _, ok := NewMarginSource(bindings(t, testSpec), nil).Margin("acme", "fund-alpha", testVenue); ok {
		t.Error("a source with no view answered as KNOWN")
	}
	view := venuemargin.New()
	if _, ok := NewMarginSource(bindings(t, testSpec), view).Margin("acme", "fund-alpha", ""); ok {
		t.Error("an order naming no venue resolved to an account")
	}
	if _, ok := NewMarginSource(nil, view).Margin("acme", "fund-alpha", testVenue); ok {
		t.Error("a source with no bindings answered as KNOWN")
	}
}
