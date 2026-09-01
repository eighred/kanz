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
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
)

func ratOf(n int64) *big.Rat { return big.NewRat(n, 1) }

// oneMandate is a MandateSource that governs every portfolio with the same mandate.
type oneMandate struct{ m *compliancepb.Mandate }

func (o oneMandate) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, bool, error) {
	return o.m, true, nil
}

// breachRecorder captures the post-trade decisions the monitor records.
type breachRecorder struct {
	mu       sync.Mutex
	breaches int
}

func (r *breachRecorder) Record(_ context.Context, _ comp.DecisionRecord) error {
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
	// would let a previous run's holdings arm this one and hide the failure being tested.
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
		rec := &breachRecorder{}
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
	// id would let a previous run's holdings arm this one.
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
	// The evaluation is stamped with the OBSERVATION, not with the clock that
	// resolved the mandate — the attribution half of #917.
	if evAt := got.GetEvaluatedAt().AsTime(); !evAt.Equal(lastFill) {
		t.Fatalf("decision evaluated_at is %s, want the FACT's as_of %s", evAt, lastFill)
	}
}
