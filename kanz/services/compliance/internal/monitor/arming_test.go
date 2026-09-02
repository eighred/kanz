package monitor_test

// TestARestartedMonitorSeesTheForbiddenHolding pins EXEC-M20.
//
// The post-trade monitor holds its book in memory (`books map[portfolio]map[instrument]`)
// and filled it from a DURABLE CONSUMER GROUP, which resumes at its last ack. So a
// RESTARTED monitor came back with an EMPTY book, and rebuilt it only as new position
// FACTs happened to arrive — an instrument that did not trade again was simply GONE.
//
// This test makes the consequence concrete rather than abstract. The fund holds a
// FORBIDDEN instrument, and the mandate denies it. A monitor that can see the whole book
// raises the breach. A monitor whose book came back partial never sees the holding at all,
// so it raises NOTHING — the control does not fail, it goes blind, and a fund holding an
// instrument its mandate forbids looks perfectly compliant.
//
// Replaying "the last message per subject" could not have fixed it either: every position
// rode ONE FLAT SUBJECT, and JetStream compacts PER SUBJECT — the whole platform retained
// exactly one message. That is the mandate defect (EXEC-M13) on a different stream, and
// this is the same fix: one subject per holding, and a consumer that ARMS at boot.
//
// So it is a RESTART, not a cold start — the sequence a rolling update performs.

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
)

func ratOf(n int64) *big.Rat { return big.NewRat(n, 1) }

// oneMandate is a MandateSource holding ONE mandate, and it governs ONLY the
// (tenant, portfolio) that mandate names.
//
// IT USED TO ANSWER FOR EVERY PORTFOLIO IT WAS ASKED ABOUT, and that made every
// test in this file check a weaker property than its name claims. The POSITION
// stream is persistent and compacted, so a monitor booting here replays every
// holding the estate has ever published — including earlier runs' — and a mandate
// source that answers for all of them gets all of them evaluated. comp.Engine's
// Evaluate then stamps the result's portfolio_id from the MANDATE, not from the
// book, so a FOREIGN portfolio's verdict came back labelled with THIS run's
// portfolio id and sailed straight through portfolioRecorder's filter. The
// per-run unique ids below are real; the isolation their comment claimed was
// not, because the uniqueness was erased one layer downstream.
//
// IT COST A REAL FALSE FAILURE. TestAMonitorReplayingAnOldFillReadsCurrentReferenceData
// asserted on a decision about BTC-USD-<an earlier run's suffix> — the holding
// TestARestartedMonitorSeesTheForbiddenHolding leaves on the stream — and reported
// the classified dimension as unreadable. It fails in the direction that hides,
// too: CI provisions a fresh broker, so these tests are green there and red on a
// developer box that has run them twice.
//
// REFUSING A PORTFOLIO IT DOES NOT NAME IS ALSO WHAT THE REAL REGISTRY DOES.
// comp.MandateRegistry returns ok=false for an unknown (tenant, portfolio) and
// the monitor treats that as UNGOVERNED and declines to evaluate, so a foreign
// book now produces no decision record at all. The isolation these tests need
// and the behaviour they are standing in for are the same thing.
type oneMandate struct{ m *compliancepb.Mandate }

func (o oneMandate) Mandate(_ context.Context, tenant, portfolio string, _ time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	if tenant != o.m.GetTenantId() || portfolio != o.m.GetPortfolioId() {
		return nil, comp.NeverMandated, nil
	}
	return o.m, comp.Governed, nil
}

// breachRecorder counts the post-trade decisions the monitor records for ONE
// portfolio.
//
// SCOPED, for oneMandate's reason. An unscoped counter over a persistent,
// compacted stream counts every run's book, and this test's whole assertion is
// "> 0" — which an earlier run's forbidden holding satisfies on its own. The
// scoped mandate source above already stops a foreign book being evaluated; this
// is the second lock on the same door, and it is the one that keeps working if
// somebody ever re-broadens the stub.
type breachRecorder struct {
	mu        sync.Mutex
	portfolio string
	breaches  int
}

func (r *breachRecorder) Record(_ context.Context, rec comp.DecisionRecord) error {
	if rec.Result.GetPortfolioId() != r.portfolio {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.breaches++
	return nil
}

func (r *breachRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.breaches
}

func TestARestartedMonitorSeesTheForbiddenHolding(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the position arming path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// The POSITION stream's production shape: MaxMsgsPerSubject=1 and NO max-age — keep
	// exactly the holding IN FORCE and never age it out, because a position that expires
	// off the stream is a holding the next restart is blind to.
	bustest.EnsureSubjects(t, ctx, js, "POSITION_IT_"+suffix, []string{subject.PositionAll})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "it", Tenant: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	// UNIQUE per run: the POSITION stream is compacted and PERSISTENT, so reusing ids
	// would let a previous run's holdings arm this one and hide the failure being
	// tested. Unique ids are necessary and NOT sufficient — see oneMandate, which is
	// what stops a foreign portfolio's verdict being counted as this one's.
	portfolio := "pf-" + suffix
	allowed, forbidden := "BTC-USD-"+suffix, "ETH-USD-"+suffix

	// THE MANDATE FORBIDS ONE INSTRUMENT. The fund holds it anyway — that is the breach
	// the monitor exists to catch.
	mandate := &compliancepb.Mandate{
		MandateId:   "m-" + portfolio,
		TenantId:    "acme",
		PortfolioId: portfolio,
		Version:     1,
		EffectiveAt: timestamppb.New(time.Now().UTC()),
		Rules: []*compliancepb.Rule{{
			RuleId: "no-eth",
			Type:   compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
				Values:    []string{forbidden},
			}},
		}},
	}

	// The fund's holdings, published BEFORE anything is listening: they are simply the
	// state of the world by the time a monitor boots.
	for _, inst := range []string{allowed, forbidden} {
		st := &domainpb.PositionState{
			PortfolioId:  portfolio,
			InstrumentId: inst,
			Quantity:     dec.ToProto(ratOf(2)),
			AveragePrice: dec.ToProto(ratOf(100)),
			// A holding is only HELD if it has a market value — comp.heldPositions filters
			// on it, and a position with none is invisible to every rule.
			MarketValue: &commonpb.Money{Amount: dec.ToProto(ratOf(200)), CurrencyCode: "USD"},
			AsOf:        timestamppb.New(time.Now().UTC()),
		}
		if err := producer.Publish(ctx, bus.Event{
			Subject:          subject.PositionFor("acme", portfolio, inst),
			EventType:        subject.PositionChanged,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "risk",
			EventTime:        time.Now().UTC(),
			PartitionKey:     portfolio,
			PayloadSchemaRef: "domain.v1.PositionState:1",
			Payload:          st,
		}); err != nil {
			t.Fatalf("publish %s: %v", inst, err)
		}
	}

	// boot runs ONE monitor process and reports whether it raised the breach.
	boot := func() int {
		rec := &breachRecorder{portfolio: portfolio}
		mon := monitor.NewMonitor(comp.NewEngine(nil), oneMandate{mandate}, nil, nil, rec, nil)

		subCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() {
			_ = consumer.SubscribeBroadcast(subCtx, subject.PositionAll, mon.Handle)
		}()

		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if rec.count() > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return rec.count()
	}

	if got := boot(); got == 0 {
		t.Fatal("the first monitor never raised the breach — it could not even cold-start")
	}

	// THE RESTART. The pod is replaced; a durable consumer's acks would survive it. The
	// new monitor must come back knowing the fund still HOLDS the forbidden instrument.
	if got := boot(); got == 0 {
		t.Fatal("a RESTARTED monitor did not raise the breach: its book came back without the forbidden " +
			"holding, so a fund holding an instrument its mandate FORBIDS looked compliant")
	}
}

// portfolioRecorder captures the post-trade decisions recorded for ONE portfolio.
//
// SCOPED, DELIBERATELY. The monitor subscribes to the whole position subject and
// the POSITION stream is persistent and compacted, so a boot replays every
// holding the estate has ever published — including other runs'. Counting all
// records would make this test pass once and then drift; counting the ones
// naming this run's portfolio asserts on content, which is stable.
//
// THIS FILTER IS NOT SUFFICIENT ON ITS OWN, and believing it was is what let a
// foreign book be asserted on. Result.PortfolioId is stamped from the MANDATE, so
// it says which mandate was applied and not whose holdings were weighed: a
// mandate source that answers for every portfolio makes every foreign book's
// verdict match this filter exactly. oneMandate is scoped for that reason, and
// each caller also asserts on evidence only ITS OWN book can produce.
type portfolioRecorder struct {
	mu        sync.Mutex
	portfolio string
	results   []*compliancepb.ComplianceResult
}

func (r *portfolioRecorder) Record(_ context.Context, rec comp.DecisionRecord) error {
	if rec.Result.GetPortfolioId() != r.portfolio {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, rec.Result)
	return nil
}

func (r *portfolioRecorder) first() *compliancepb.ComplianceResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return nil
	}
	return r.results[0]
}

// TestAMonitorReplayingAnOldFillFindsTheMandateInForce pins #917 against the
// REAL spine and the REAL registry, because the defect lives at the join between
// the two.
//
// The fund's last fill is a MONTH OLD and its mandate took force AFTER it —
// exactly what a portfolio that stopped trading before its mandate was published
// looks like. The position FACT arrives off DeliverLastPerSubject carrying that
// month-old as_of; the registry, armed from its own compacted subject, holds
// only the version in force (#884, #916). Resolving the mandate at the FACT's
// as_of finds NOTHING, and the portfolio is skipped as UNGOVERNED — the control
// does not fail, it declines to run, and a fund holding an instrument its
// mandate FORBIDS looks compliant.
//
// The book is a single forbidden holding, so any evaluation at all breaches.
func TestAMonitorReplayingAnOldFillFindsTheMandateInForce(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to replay a backdated position FACT over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "POSITION_917_"+suffix, []string{subject.PositionAll})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-917"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "it", Tenant: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	// UNIQUE per run: the POSITION stream is compacted and PERSISTENT, so a reused
	// id would let a previous run's holdings arm this one. Necessary and NOT
	// sufficient — see oneMandate.
	portfolio := "pf-917-" + suffix
	forbidden := "ETH-USD-" + suffix

	// THE FILL IS A MONTH OLD. Nothing has traded this portfolio since.
	lastFill := time.Now().UTC().Add(-30 * 24 * time.Hour)
	st := &domainpb.PositionState{
		PortfolioId:  portfolio,
		InstrumentId: forbidden,
		Quantity:     dec.ToProto(ratOf(2)),
		AveragePrice: dec.ToProto(ratOf(100)),
		MarketValue:  &commonpb.Money{Amount: dec.ToProto(ratOf(200)), CurrencyCode: "USD"},
		AsOf:         timestamppb.New(lastFill),
	}
	if err := producer.Publish(ctx, bus.Event{
		Subject:          subject.PositionFor("acme", portfolio, forbidden),
		EventType:        subject.PositionChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        lastFill,
		PartitionKey:     portfolio,
		PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload:          st,
	}); err != nil {
		t.Fatalf("publish the old fill: %v", err)
	}

	// THE MANDATE TOOK FORCE AFTER THE FILL, and it is the only resident version —
	// which is what the compacted mandate subject serves.
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{
		MandateId:   "m-" + portfolio,
		TenantId:    "acme",
		PortfolioId: portfolio,
		Version:     1,
		EffectiveAt: timestamppb.New(time.Now().UTC()),
		Rules: []*compliancepb.Rule{{
			RuleId: "no-eth",
			Type:   compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
				Values:    []string{forbidden},
			}},
		}},
	}); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}

	rec := &portfolioRecorder{portfolio: portfolio}
	mon := monitor.NewMonitor(comp.NewEngine(nil), reg, nil, nil, rec, nil)
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.SubscribeBroadcast(subCtx, subject.PositionAll, mon.Handle)
	}()

	deadline := time.Now().Add(20 * time.Second)
	var got *compliancepb.ComplianceResult
	for time.Now().Before(deadline) {
		if got = rec.first(); got != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the monitor recorded no decision for a portfolio holding an instrument its mandate " +
			"FORBIDS: the replayed FACT's as_of predates the mandate's effective_at, so the mandate " +
			"lookup found nothing and the portfolio was skipped as UNGOVERNED")
	}
	if got.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("decision status is %v, want BREACH", got.GetStatus())
	}
	if got.GetMandateId() != "m-"+portfolio {
		t.Fatalf("decision names mandate %q, want %q", got.GetMandateId(), "m-"+portfolio)
	}
	// THE VERDICT IS ABOUT THIS RUN'S BOOK, asserted on evidence no other run can
	// produce. mandate_id and portfolio_id are BOTH stamped from the mandate, so
	// neither can say whose HOLDINGS were weighed; the violation's instrument is
	// read off the position, and `forbidden` is unique to this run. Before
	// oneMandate was scoped this test would have asserted on an earlier run's
	// leftover holding just as happily — see oneMandate.
	var denied *compliancepb.Violation
	for _, v := range got.GetViolations() {
		if v.GetMessage() == "instrument violates restriction list" {
			denied = v
		}
	}
	if denied == nil {
		t.Fatalf("no restriction violation on a book holding a denied instrument: status=%v", got.GetStatus())
	}
	if inst := denied.GetEvidence()["instrument"]; inst != forbidden {
		t.Fatalf("the violation names instrument %q, want this run's %q — the decision asserted on is "+
			"about ANOTHER portfolio's book, replayed off the compacted POSITION stream", inst, forbidden)
	}
	// The evaluation is stamped with the OBSERVATION, not with the clock that
	// resolved the mandate — the attribution half of #917.
	if evAt := got.GetEvaluatedAt().AsTime(); !evAt.Equal(lastFill) {
		t.Fatalf("decision evaluated_at is %s, want the FACT's as_of %s", evAt, lastFill)
	}
}

// refMaster is a refdata.Source over a fixed set of golden records — the one
// seam refdata.Cache needs faked. The cache, its as-of rule and the compliance
// projection over it are the REAL ones the composition root wires
// (services/compliance/cmd/compliance, refCache.Compliance).
type refMaster map[string]refdata.Record

func (m refMaster) Fetch(_ context.Context, instrumentID string) (refdata.Record, bool, error) {
	rec, ok := m[instrumentID]
	if !ok {
		return refdata.Record{}, false, nil
	}
	return rec, true, nil
}

// armedRefCache builds the production classifier over master and primes it, so
// the cache is warm exactly as a running pod's is. The cache is demand-driven:
// an instrument nothing has asked about is not resident, and would refuse for a
// reason that is not the one under test.
func armedRefCache(ctx context.Context, t *testing.T, master refMaster) comp.Classifier {
	t.Helper()
	cache, err := refdata.NewCache(master, refdata.Options{})
	if err != nil {
		t.Fatalf("refdata.NewCache: %v", err)
	}
	cl := cache.Compliance()
	for id := range master {
		_, _ = cl.Classify(ctx, id, time.Now().UTC()) // records the want
	}
	if _, err := cache.Refresh(ctx); err != nil {
		t.Fatalf("refdata.Refresh: %v", err)
	}
	for id := range master {
		if _, ok := cl.Classify(ctx, id, time.Now().UTC()); !ok {
			t.Fatalf("the cache did not arm %s, so this test would prove nothing about the as-of rule", id)
		}
	}
	return cl
}

// TestAMonitorReplayingAnOldFillReadsCurrentReferenceData pins #930 against the
// REAL spine and the REAL refdata.Cache, because the defect lives at the join
// between the replayed FACT's as_of and the cache's as-of rule.
//
// The fund's last fill is a MONTH OLD; its reference record was refreshed an
// hour ago, which is the ordinary state of a security master beside a fund that
// is not trading. refdata.Cache holds ONE snapshot per instrument and REFUSES a
// record whose own as_of is after the question's, so a classifier asked at the
// replayed FACT's as_of resolved NOTHING — and comp.unresolvedDimension turns
// that into a violation. A concentration cap on a classified dimension therefore
// reported a breach whose reason was "the dimension cannot be verified" rather
// than the cap it names, on every boot, and AUTO-01 halts and escalates on that
// FACT.
//
// THE ASSERTION IS ON CONTENT, NOT ON A COUNT, and that is load-bearing three
// times over. The POSITION stream is persistent with 24h retention, so a test
// counting events passes once and fails forever after. Under the defect this
// book breached TOO — a count of one would have been green before and after the
// fix; what separates them is WHICH violation, the cap firing on an issuer the
// classifier resolved or the refusal that says it could not read one. And the
// content chosen is a bucket minted from this run's suffix, so no other
// portfolio the stream replays can satisfy it — which is the half this test got
// wrong on its first run against a used broker, and see oneMandate for why the
// per-run ids alone did not carry it.
func TestAMonitorReplayingAnOldFillReadsCurrentReferenceData(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to replay a backdated position FACT against real reference data")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "POSITION_930_"+suffix, []string{subject.PositionAll})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-930"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "it", Tenant: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	// UNIQUE per run: the POSITION stream is compacted and PERSISTENT, so a reused
	// id would let a previous run's holdings arm this one. Necessary and NOT
	// sufficient — see oneMandate.
	portfolio := "pf-930-" + suffix
	held := "TECH-" + suffix
	// THE ISSUER DIMENSION, AND THE ISSUER ID IS UNIQUE TO THIS RUN. That is the
	// point of choosing it over SECTOR here: a violation's `bucket` evidence is the
	// resolved classification itself, so a per-run issuer makes the passing
	// assertion one that NO other portfolio on this stream can satisfy. A shared
	// "GICS:45" would have been satisfiable by any run. ISSUER, SECTOR and
	// ASSET_CLASS are the same classified path through comp.bucketKey and the same
	// refdata.Lookup as-of rule; the unit tests in classifier_clock_test.go cover
	// SECTOR.
	issuer := "ISS-" + suffix

	// THE FILL IS A MONTH OLD. THE REFERENCE RECORD IS AN HOUR OLD. That gap is
	// the whole defect: the record post-dates the question the FACT's as_of asks.
	lastFill := time.Now().UTC().Add(-30 * 24 * time.Hour)
	classifier := armedRefCache(ctx, t, refMaster{held: {
		InstrumentID: held,
		AssetClass:   "EQUITY",
		Sector:       refdata.Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
		IssuerID:     issuer,
		AsOf:         time.Now().UTC().Add(-time.Hour),
	}})

	st := &domainpb.PositionState{
		PortfolioId:  portfolio,
		InstrumentId: held,
		Quantity:     dec.ToProto(ratOf(2)),
		AveragePrice: dec.ToProto(ratOf(100)),
		MarketValue:  &commonpb.Money{Amount: dec.ToProto(ratOf(200)), CurrencyCode: "USD"},
		AsOf:         timestamppb.New(lastFill),
	}
	if err := producer.Publish(ctx, bus.Event{
		Subject:          subject.PositionFor("acme", portfolio, held),
		EventType:        subject.PositionChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        lastFill,
		PartitionKey:     portfolio,
		PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload:          st,
	}); err != nil {
		t.Fatalf("publish the old fill: %v", err)
	}

	// The cap names the issuer the fund is WHOLLY exposed to, so a monitor that can
	// read the classification breaches on the cap itself — which is the observable
	// that tells "the issuer was resolved" from "the issuer could not be read". The
	// monitor records only breaches, so this is how the evaluation is made visible
	// at all.
	mandate := &compliancepb.Mandate{
		MandateId:   "m-" + portfolio,
		TenantId:    "acme",
		PortfolioId: portfolio,
		Version:     1,
		EffectiveAt: timestamppb.New(time.Now().UTC().Add(-time.Hour)),
		Rules: []*compliancepb.Rule{{
			RuleId: "issuer-cap",
			Type:   compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_ISSUER,
				Bucket:    issuer,
				MaxWeight: &commonpb.Decimal{Coefficient: 10, Exponent: -2}, // 10%
			}},
		}},
	}

	rec := &portfolioRecorder{portfolio: portfolio}
	mon := monitor.NewMonitor(comp.NewEngine(nil), oneMandate{mandate}, classifier, nil, rec, nil)
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.SubscribeBroadcast(subCtx, subject.PositionAll, mon.Handle)
	}()

	deadline := time.Now().Add(20 * time.Second)
	var got *compliancepb.ComplianceResult
	for time.Now().Before(deadline) {
		if got = rec.first(); got != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the monitor recorded no decision for a portfolio 100% exposed to an issuer capped at 10%")
	}
	var fired *compliancepb.Violation
	for _, v := range got.GetViolations() {
		if strings.Contains(v.GetMessage(), "cannot be verified") {
			t.Fatalf("the ISSUER dimension was REFUSED rather than read: %q, evidence %v. The "+
				"classifier was asked at the replayed FACT's as_of (%s) instead of at the monitor's "+
				"clock, and the reference record is an hour old — so a fund that is not breaching "+
				"its issuer cap raises a breach FACT on every boot",
				v.GetMessage(), v.GetEvidence(), lastFill)
		}
		if v.GetMessage() == "concentration exceeds limit" {
			fired = v
		}
	}
	if fired == nil {
		t.Fatalf("no concentration violation on a book 100%% exposed to a capped issuer: status=%v",
			got.GetStatus())
	}
	// THE BUCKET IS THE WHOLE ASSERTION, and it carries two claims at once: the
	// classifier RESOLVED this holding rather than dropping it into the empty key,
	// AND the book weighed was this run's, because no other portfolio on this
	// persistent stream is exposed to an issuer id minted from this run's suffix.
	if b := fired.GetEvidence()["bucket"]; b != issuer {
		t.Fatalf("the violation names bucket %q, want this run's issuer %q", b, issuer)
	}
	// The attribution half of #917, unmoved: only the LOOKUPS ask the monitor's
	// clock; the evaluation is still stamped with the observation.
	if evAt := got.GetEvaluatedAt().AsTime(); !evAt.Equal(lastFill) {
		t.Fatalf("decision evaluated_at is %s, want the FACT's as_of %s", evAt, lastFill)
	}
}
