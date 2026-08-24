package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	decutil "github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	mdstore "github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/venuemargin"
	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE MARGIN WIRING (#408 control 4). What is graded here is the JOIN — whose
// accounts are read, from which observation, paired with a mark from which
// instant — and the postures the composition root takes when one of the three
// facts is missing. compute/marginrisk_test.go already grades the arithmetic
// against a fake provider; nothing graded the provider, which is where every
// input the measure trusts is actually resolved.

// --- fixtures -------------------------------------------------------------

const (
	testTenant = "acme"
	testFund   = "fund-alpha"
	testVenue  = "XNAS"
	testAcct   = "okx-sub-1"
)

var (
	observedAt = time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)
	// The fold's clock, ten seconds after the observation: current by
	// venuemargin.DefaultMaxAge, and a fixed point so no test depends on wall time.
	foldNow = func() time.Time { return observedAt.Add(10 * time.Second) }
)

func mustBindings(t *testing.T, spec string) *execution.AccountBindings {
	t.Helper()
	b, err := execution.ParseBindings(spec)
	if err != nil {
		t.Fatalf("ParseBindings(%q): %v", spec, err)
	}
	return b
}

func decimal(t *testing.T, coefficient int64, exponent int32) *commonpb.Decimal {
	t.Helper()
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

// observation is one venue margin FACT for the bound account, with the given
// open positions.
func observation(t *testing.T, positions ...*collateralpb.VenueLiquidationPrice) *collateralpb.VenueMarginState {
	t.Helper()
	return &collateralpb.VenueMarginState{
		Venue: testVenue, VenueAccountId: testAcct,
		MarginRatio:       decimal(t, 375, -2),
		LiquidationPrices: positions,
		ObservedAt:        timestamppb.New(observedAt),
		KnowledgeTime:     timestamppb.New(observedAt),
		Coverage:          &domainpb.InputCoverage{Contributed: uint32(len(positions))},
	}
}

func position(t *testing.T, symbol, instrument string, coefficient int64, exponent int32) *collateralpb.VenueLiquidationPrice {
	t.Helper()
	return &collateralpb.VenueLiquidationPrice{
		VenueSymbol: symbol, InstrumentId: instrument, Price: decimal(t, coefficient, exponent),
	}
}

// foldOf returns a view holding exactly the given observation.
func foldOf(t *testing.T, msgs ...*collateralpb.VenueMarginState) *venuemargin.View {
	t.Helper()
	v := venuemargin.New(venuemargin.WithClock(foldNow))
	for _, msg := range msgs {
		b, err := proto.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := v.Handle(context.Background(), nil, b); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	return v
}

// recordingMarks answers one mark for every instrument and records the asOf it
// was asked for — the axis this file has to prove, because a mark read at "now"
// pairs a liquidation price from one instant with a price from another.
type recordingMarks struct {
	price   *big.Rat
	ok      bool
	asOf    []time.Time
	instrs  []string
	present map[string]*big.Rat
}

func (m *recordingMarks) Mark(_ context.Context, instrumentID string, asOf time.Time) (*big.Rat, bool) {
	m.instrs = append(m.instrs, instrumentID)
	m.asOf = append(m.asOf, asOf)
	if m.present != nil {
		p, ok := m.present[instrumentID]
		return p, ok
	}
	return m.price, m.ok
}

func providerOver(t *testing.T, spec string, fold marginFold, marks markResolver) *marginProvider {
	t.Helper()
	return &marginProvider{
		tenant:   testTenant,
		bindings: mustBindings(t, spec),
		fold:     fold,
		marks:    marks,
	}
}

const boundSpec = testTenant + "/" + testFund + "@" + testVenue + "=" + testAcct

// --- the join -------------------------------------------------------------

// AN UNBOUND PORTFOLIO IS UNKNOWN, NOT UNLEVERED.
//
// With no binding the OMS may still route its orders against whatever account
// the adapter holds, alongside every other unbound portfolio — a shared
// collateral pool, in which no account's margin can be said to back THIS
// portfolio. Answering with an empty slice would let the measure conclude the
// portfolio holds nothing leveraged, which is the flattering reading of a
// question nobody can answer.
func TestAnUnboundPortfolioIsUnknownRatherThanFlat(t *testing.T) {
	p := providerOver(t, boundSpec, foldOf(t, observation(t)), &recordingMarks{})
	got, ok := p.AccountMargins(context.Background(), v1.PortfolioID("fund-nobody-bound"))
	if ok {
		t.Fatalf("AccountMargins ok = true for an unbound portfolio (%d accounts) — the measure "+
			"would report an exact zero proximity for a book it cannot see the collateral of", len(got))
	}
	if got != nil {
		t.Errorf("AccountMargins = %v alongside ok=false", got)
	}
}

// ANOTHER TENANT'S PORTFOLIO OF THE SAME NAME IS NOT THIS ONE. The engine is
// tenant-pinned, and the tenant it is pinned to is what the join uses.
func TestTheJoinIsScopedToTheEnginesTenant(t *testing.T) {
	p := providerOver(t, "rival/"+testFund+"@"+testVenue+"=other-acct", foldOf(t, observation(t)), &recordingMarks{})
	if _, ok := p.AccountMargins(context.Background(), v1.PortfolioID(testFund)); ok {
		t.Fatal("AccountMargins resolved a binding belonging to another tenant — one customer's " +
			"liquidation distance would be reported over another's collateral")
	}
}

// AN ACCOUNT NOBODY HAS OBSERVED IS RETURNED, NOT DROPPED.
//
// Dropping it would leave the measure with an empty account list for a bound
// portfolio, which it reads as "the exchange says nothing is leveraged". The
// account has to arrive with Read=false so the refusal names it.
func TestAnUnobservedAccountArrivesUnreadRatherThanMissing(t *testing.T) {
	p := providerOver(t, boundSpec, foldOf(t), &recordingMarks{})
	got, ok := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if !ok || len(got) != 1 {
		t.Fatalf("AccountMargins = %v, %v — want one account for a bound portfolio", got, ok)
	}
	if got[0].Read {
		t.Error("Read = true for an account no venue has ever reported on")
	}
	if got[0].Venue != testVenue || got[0].Account != testAcct {
		t.Errorf("account = %s/%s, want %s/%s", got[0].Venue, got[0].Account, testVenue, testAcct)
	}
	if got[0].CoverageReported || got[0].ExcludedCount != 0 || len(got[0].Positions) != 0 {
		t.Errorf("unread account carries state it cannot have: %+v", got[0])
	}
}

// AN ACCOUNT WHOSE OBSERVATIONS HAVE STOPPED IS UNREAD, on the same freshness
// bound every other reader of the fold applies. Margin moves with the mark, and
// a ratio from before the move is the "stale book" the whole #408 set is built
// on.
func TestAnAgedOutAccountIsUnread(t *testing.T) {
	stale := venuemargin.New(venuemargin.WithClock(func() time.Time {
		return observedAt.Add(venuemargin.DefaultMaxAge + time.Second)
	}))
	b, err := proto.Marshal(observation(t, position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := stale.Handle(context.Background(), nil, b); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	p := providerOver(t, boundSpec, stale, &recordingMarks{price: big.NewRat(50000, 1), ok: true})
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 || got[0].Read {
		t.Fatalf("account = %+v, want Read=false one second past venuemargin.DefaultMaxAge", got)
	}
}

// THE EXCHANGE SAYING "NOTHING LEVERAGED HERE" IS A POSITIVE STATEMENT, and it
// is the one answer that legitimately produces an exact zero. It must be
// distinguishable from every silence above by Read and the coverage alone.
func TestAnEmptyCurrentObservationIsTheExchangeSayingFlat(t *testing.T) {
	p := providerOver(t, boundSpec, foldOf(t, observation(t)), &recordingMarks{})
	got, ok := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if !ok || len(got) != 1 {
		t.Fatalf("AccountMargins = %v, %v", got, ok)
	}
	if !got[0].Read || !got[0].CoverageReported || got[0].ExcludedCount != 0 {
		t.Fatalf("account = %+v, want read + coverage reported + nothing excluded", got[0])
	}
	if len(got[0].Positions) != 0 {
		t.Errorf("positions = %v, want none", got[0].Positions)
	}
}

// THE COVERAGE TRAVELS WITH THE POSITIONS. An exchange that answered for two of
// three positions must not look like one that answered for two.
func TestAnIncompleteObservationCarriesItsExclusionCount(t *testing.T) {
	msg := observation(t, position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0))
	msg.Coverage = &domainpb.InputCoverage{Contributed: 1, ExcludedCount: 2}
	p := providerOver(t, boundSpec, foldOf(t, msg), &recordingMarks{price: big.NewRat(50000, 1), ok: true})
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 {
		t.Fatalf("accounts = %v", got)
	}
	if !got[0].CoverageReported || got[0].ExcludedCount != 2 {
		t.Errorf("coverage reported=%v excluded=%d, want true/2 — the account would be measured "+
			"over the positions the venue DID answer for, and the two it did not may be the ones "+
			"nearest the boundary", got[0].CoverageReported, got[0].ExcludedCount)
	}
}

// A PUBLISHER THAT REPORTS NO COVERAGE IS NOT ONE THAT EXCLUDED NOTHING. False
// and zero must not collapse into the same zero — the publisher that says
// nothing is the one nobody has checked.
func TestAnObservationWithNoCoverageRecordIsNotReadAsComplete(t *testing.T) {
	msg := observation(t, position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0))
	msg.Coverage = nil
	p := providerOver(t, boundSpec, foldOf(t, msg), &recordingMarks{price: big.NewRat(50000, 1), ok: true})
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 {
		t.Fatalf("accounts = %v", got)
	}
	if got[0].CoverageReported {
		t.Error("CoverageReported = true for an observation carrying no coverage record")
	}
	if got[0].ExcludedCount != 0 {
		t.Errorf("ExcludedCount = %d on an unreported coverage, want 0 — the count is meaningful "+
			"only when the record exists, and a non-zero here would be invented", got[0].ExcludedCount)
	}
}

// THE MARK IS READ AS OF THE OBSERVATION, NOT AS OF NOW.
//
// The proximity is one price over another. Read at wall-clock now, the
// denominator is minutes newer than the numerator during exactly the fast move
// the measure exists to catch, and the ratio is of two different moments.
func TestTheMarkIsReadAtTheInstantTheVenueReported(t *testing.T) {
	marks := &recordingMarks{price: big.NewRat(50000, 1), ok: true}
	p := providerOver(t, boundSpec, foldOf(t, observation(t,
		position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0))), marks)
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 || len(got[0].Positions) != 1 {
		t.Fatalf("accounts = %+v", got)
	}
	if len(marks.asOf) != 1 {
		t.Fatalf("mark resolved %d times, want once", len(marks.asOf))
	}
	if !marks.asOf[0].Equal(observedAt) {
		t.Errorf("mark asOf = %s, want the observation time %s — the liquidation price and the "+
			"mark it is divided by came from different moments", marks.asOf[0], observedAt)
	}
	if marks.instrs[0] != "BTC-PERP" {
		t.Errorf("mark asked for %q, want the attributed instrument BTC-PERP", marks.instrs[0])
	}
	pos := got[0].Positions[0]
	if pos.Liquidation == nil || pos.Liquidation.Cmp(big.NewRat(41000, 1)) != 0 {
		t.Errorf("liquidation = %v, want 41000", pos.Liquidation)
	}
	if pos.Mark == nil || pos.Mark.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Errorf("mark = %v, want 50000", pos.Mark)
	}
	if string(pos.InstrumentID) != "BTC-PERP" || pos.VenueSymbol != "BTC-USDT-SWAP" {
		t.Errorf("position identity = %+v, want both the instrument and the venue's own spelling", pos)
	}
}

// AN UNATTRIBUTED POSITION IS KEPT WITH NO MARK, NEVER DROPPED.
//
// The venue reported a leveraged position whose symbol this deployment's map
// does not carry. It is real and it can be liquidated; dropped here, it would
// leave the measure describing an account it has not fully seen while the
// account's own coverage said so and nothing read it.
func TestAnUnattributedPositionIsKeptAndUnmarked(t *testing.T) {
	marks := &recordingMarks{price: big.NewRat(50000, 1), ok: true}
	p := providerOver(t, boundSpec, foldOf(t, observation(t,
		position(t, "MYSTERY-SWAP", "", 12, 0))), marks)
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 || len(got[0].Positions) != 1 {
		t.Fatalf("accounts = %+v — an unmappable position was dropped, and the account now reads "+
			"as one the platform has fully seen", got)
	}
	pos := got[0].Positions[0]
	if pos.Mark != nil {
		t.Errorf("mark = %v for a position with no instrument — it was resolved against something", pos.Mark)
	}
	if len(marks.instrs) != 0 {
		t.Errorf("the store was asked for %v — an empty instrument id must not reach it", marks.instrs)
	}
	if pos.VenueSymbol != "MYSTERY-SWAP" {
		t.Errorf("venue symbol = %q, want the exchange's own spelling so an operator can find it",
			pos.VenueSymbol)
	}
}

// AN UNRESOLVED MARK LEAVES THE MARK NIL, NEVER ZERO. A zero denominator would
// put the liquidation boundary infinitely far away and report the one position
// whose price could not be read as the safest thing on the book.
func TestAnUnresolvedMarkLeavesThePositionUnmarked(t *testing.T) {
	p := providerOver(t, boundSpec, foldOf(t, observation(t,
		position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0))),
		&recordingMarks{ok: false})
	got, _ := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if len(got) != 1 || len(got[0].Positions) != 1 {
		t.Fatalf("accounts = %+v", got)
	}
	if got[0].Positions[0].Mark != nil {
		t.Errorf("mark = %v, want nil", got[0].Positions[0].Mark)
	}
}

// EVERY BOUND VENUE IS ENUMERATED, IN A STABLE ORDER. A venue missing from the
// list is a liquidation boundary measured by nothing that still reports as fully
// covered.
func TestEveryBoundAccountIsRead(t *testing.T) {
	spec := boundSpec + "," + testTenant + "/" + testFund + "@XLON=binance-main"
	p := providerOver(t, spec, foldOf(t, observation(t)), &recordingMarks{})
	got, ok := p.AccountMargins(context.Background(), v1.PortfolioID(testFund))
	if !ok || len(got) != 2 {
		t.Fatalf("AccountMargins = %v, %v — want both bound venues", got, ok)
	}
	if got[0].Venue != "XLON" || got[1].Venue != testVenue {
		t.Errorf("venues = %s, %s — want a stable sort so two reads cannot disagree about which "+
			"account a tie is attributed to", got[0].Venue, got[1].Venue)
	}
	// The second venue has no observation: it must be UNREAD rather than absent,
	// while the first is read.
	if !got[1].Read || got[0].Read {
		t.Errorf("read flags = %v, %v — want the observed venue read and the unobserved one not",
			got[0].Read, got[1].Read)
	}
}

// THE JOIN FEEDS A REAL MEASURE, END TO END. Everything above grades the
// provider's answers; this asserts the measure computed from them is the number
// a mandate would be gated on — 41000/50000 = 0.82, exactly.
func TestTheWiredProviderProducesTheProximityAMandateGatesOn(t *testing.T) {
	p := providerOver(t, boundSpec, foldOf(t, observation(t,
		position(t, "BTC-USDT-SWAP", "BTC-PERP", 41000, 0))),
		&recordingMarks{price: big.NewRat(50000, 1), ok: true})

	registry := compute.NewRegistry()
	compute.RegisterMarginRisk(context.Background(), registry, p)
	set := compute.ComputeMeasures(marginPortfolio(), registry, nil)
	measure, ok := set.Lookup(compute.MeasureLiquidationProximity)
	if !ok {
		t.Fatal("LiquidationProximity absent from the computed set — the measure is registered and " +
			"produced nothing")
	}
	if measure.Value == nil {
		t.Fatalf("no value, coverage = %+v — the join refused a fully answered account", measure.Coverage)
	}
	if got := decutil.FromProto(measure.Value); got.Cmp(big.NewRat(41, 50)) != 0 {
		t.Errorf("proximity = %s, want exactly 41/50 = 0.82 (41000/50000)", got.FloatString(6))
	}
	if measure.Coverage.ExcludedCount != 0 {
		t.Errorf("coverage excluded %d, want 0 on a fully answered account",
			measure.Coverage.ExcludedCount)
	}
}

func marginPortfolio() *domain.Portfolio { return domain.NewPortfolio(testFund, "USD") }

// --- the composition root -------------------------------------------------

func testConfig(spec string) config.Config {
	return config.Config{Tenant: testTenant, VenueAccounts: spec}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePrices is a market-data store that answers nothing. It exists so
// registerMarginRisk can be given a non-nil store without a database.
type fakePrices struct{}

func (fakePrices) LatestAsOf(context.Context, string, mdstore.PriceKind, time.Time) (mdstore.Observation, bool, error) {
	return mdstore.Observation{}, false, nil
}

// NO BINDINGS ⇒ NOTHING IS REGISTERED, AND NO SERIES IS CREATED.
//
// A registered measure that refuses on every portfolio forever reads as live. A
// zeroed kanz_risk_margin_skipped_total beside it reads as "wired, and quiet".
// Both are the state an operator must not be shown, so the absence of the
// measure and the absence of the series are the signal.
func TestNoBindingsRegistersNothingAndCreatesNoSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.NewRegistry()
	fold, err := registerMarginRisk(context.Background(), testConfig(""), registry, fakePrices{}, reg, quietLogger())
	if err != nil {
		t.Fatalf("registerMarginRisk: %v", err)
	}
	if fold != nil {
		t.Error("a fold was returned with no accounts bound — the engine would subscribe and hold " +
			"margin state nothing reads")
	}
	if liqRegistered(registry, compute.MeasureLiquidationProximity) {
		t.Error("LiquidationProximity was registered with no bindings: it would refuse every " +
			"portfolio forever while kanz_risk_measure_live reported the margin family live")
	}
	for _, name := range families(t, reg) {
		if strings.HasPrefix(name, "kanz_risk_margin_") || strings.HasPrefix(name, "kanz_risk_venue_margin_") {
			t.Errorf("%s exists on a deployment that registered no margin measure", name)
		}
	}
}

// BOUND ACCOUNTS WITH NOTHING TO MARK THEM AGAINST IS A STARTUP FAILURE.
//
// Degrading would leave a deployment that bound accounts looking exactly like
// one that never asked: every position excluded, uniformly, forever, which at
// the measure is indistinguishable from a book holding nothing leveraged.
func TestBoundAccountsWithNoPriceStoreRefuseToStart(t *testing.T) {
	fold, err := registerMarginRisk(context.Background(), testConfig(boundSpec),
		compute.NewRegistry(), nil, prometheus.NewRegistry(), quietLogger())
	if err == nil {
		t.Fatal("registerMarginRisk accepted bound accounts with no price store — the margin " +
			"measure would refuse every position and read as a flat book")
	}
	if fold != nil {
		t.Error("a fold was returned alongside the refusal")
	}
	if !strings.Contains(err.Error(), "RISK_ENGINE_MARKETDATA_DATABASE_URL") {
		t.Errorf("error does not name the variable to set: %v", err)
	}
}

// A SHARED ACCOUNT REFUSES THE START HERE TOO. One account backing two
// portfolios is one collateral pool the exchange liquidates as one, and a
// proximity measured on it attributes one portfolio's distance to another.
func TestASharedAccountRefusesToWire(t *testing.T) {
	spec := boundSpec + "," + testTenant + "/fund-beta@" + testVenue + "=" + testAcct
	if _, err := registerMarginRisk(context.Background(), testConfig(spec),
		compute.NewRegistry(), fakePrices{}, prometheus.NewRegistry(), quietLogger()); err == nil {
		t.Fatal("registerMarginRisk accepted a binding set giving one exchange account to two " +
			"portfolios")
	}
}

// THE WIRED PATH REGISTERS THE MEASURE, RETURNS A FOLD TO SUBSCRIBE, AND ZEROES
// EVERY REASON. A counter that appears only on its first increment reads as
// no-data to an alert, so the alert cannot fire on the transition that matters.
func TestTheWiredPathRegistersTheMeasureAndItsSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.NewRegistry()
	fold, err := registerMarginRisk(context.Background(), testConfig(boundSpec), registry,
		fakePrices{}, reg, quietLogger())
	if err != nil {
		t.Fatalf("registerMarginRisk: %v", err)
	}
	if fold == nil {
		t.Fatal("no fold returned — nothing would subscribe to the margin observations, and every " +
			"account would stay UNKNOWN forever")
	}
	if !liqRegistered(registry, compute.MeasureLiquidationProximity) {
		t.Fatal("LiquidationProximity was not registered on the wired path")
	}
	names := families(t, reg)
	for _, want := range []string{
		"kanz_risk_margin_skipped_total",
		"kanz_risk_margin_mark_unresolved_total",
		"kanz_risk_venue_margin_uncovered_total",
		"kanz_risk_venue_margin_undated_total",
		"kanz_risk_venue_margin_accounts_held",
		"kanz_risk_venue_margin_accounts_current",
	} {
		if !contains(names, want) {
			t.Errorf("%s was not registered: %v", want, names)
		}
	}
	// no_margin_provider must NOT have a series here: it fires only when the
	// measure was never registered, which is a path this metric does not exist on.
	reasons := counters(t, reg, "kanz_risk_margin_skipped_total")
	if _, present := reasons[compute.SkipNoMarginProvider]; present {
		t.Errorf("kanz_risk_margin_skipped_total carries a %s series that can never increment",
			compute.SkipNoMarginProvider)
	}
	if _, present := reasons[compute.SkipMarginUnknown]; !present {
		t.Errorf("kanz_risk_margin_skipped_total has no %s series to alert on: %v",
			compute.SkipMarginUnknown, reasons)
	}
}

// families lists the metric families a registry gathered, so a test can assert
// both that a series exists and that one does NOT.
func families(t *testing.T, g prometheus.Gatherer) []string {
	t.Helper()
	got, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make([]string, 0, len(got))
	for _, f := range got {
		out = append(out, f.GetName())
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- the call site --------------------------------------------------------

// Everything above grades registerMarginRisk in isolation, and a helper the
// composition root never calls is a control the platform does not have, tested
// to four decimal places.
//
// test/arch/no_dark_measure_seam_test.go cannot see the deletion: it resolves
// callers of compute.RegisterMarginRisk, and THIS FILE is one — a seam called
// only by another unreachable seam still counts as live there, which is the
// weakness that guard's own doc names.
//
// PARSED, NOT GREPPED. A regexp over main.go would match this file's own
// comments and the log lines beside the call — the failure mode that has already
// produced three green guards in this repository checking nothing.
func TestRunEngineCallsTheMarginWiring(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var runEngineDecl *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "runEngine" {
			runEngineDecl = fn
		}
	}
	if runEngineDecl == nil {
		t.Fatal("runEngine was not found in main.go — this guard is asserting nothing")
	}
	var registered, subscribed bool
	ast.Inspect(runEngineDecl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "registerMarginRisk" {
			registered = true
		}
		// The fold is useless unwired: SubscribeBroadcast on venuemargin.Subject is
		// what fills it, and without it every account is UNKNOWN forever.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SubscribeBroadcast" {
			return true
		}
		for _, arg := range call.Args {
			if s, ok := arg.(*ast.SelectorExpr); ok && s.Sel.Name == "Subject" {
				if pkg, ok := s.X.(*ast.Ident); ok && pkg.Name == "venuemargin" {
					subscribed = true
				}
			}
		}
		return true
	})
	if !registered {
		t.Error("runEngine does not call registerMarginRisk. Every test above still passes: the " +
			"wiring is correct and unreachable, LiquidationProximity is absent from the registry, " +
			"and a mandate naming it refuses every order it checks (#408 control 4)")
	}
	if !subscribed {
		t.Error("runEngine does not SubscribeBroadcast venuemargin.Subject. The fold would stay " +
			"empty, every account would be UNKNOWN, and the measure would refuse forever — " +
			"fail-closed, and useless")
	}
}
