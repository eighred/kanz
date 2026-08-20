// THE POSITION FACTs, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// This projector had NO test of its publish path at all — not a weak double, no
// caller of NewProjector in any _test.go. What it emits is the FACT the risk
// engine, the compliance monitor and the pre-trade gate all consume, so an
// envelope the broker refuses does not degrade those consumers: it leaves them
// arming on nothing while the OMS reports healthy.
//
// TIER-B: a REAL bus.Producer over a fake bus.Client, so bus.Validate runs and
// the assertions are made on the wire bytes.
//
// THE PRODUCER HERE DELIBERATELY HAS NO ProducerConfig.Tenant, mirroring the
// OMS's own. The projector sets no Event.TenantID either, so the tenant can only
// come from the CTX the consumer stashes it on (bus/consumer.go). That is
// the entire reason TestPositionPublishRefusesOutsideADelivery exists: the same
// shape — a producer with no fallback publishing outside an inbound delivery —
// has already crash-looped this service once.
package position

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// stubStore returns a fixed fold. The arithmetic is book.go's and postgres.go's
// business and is tested there; what is under test here is the ENVELOPE.
type stubStore struct{ applied *Applied }

func (s *stubStore) Apply(context.Context, string, *orderpb.Fill, time.Time) (*Applied, error) {
	return s.applied, nil
}
func (s *stubStore) Snapshot(context.Context, string, time.Time) (*domainpb.PortfolioSnapshot, error) {
	return nil, errors.New("not implemented")
}

const (
	testTenant    = "acme"
	testPortfolio = "pf-1"
	testInstr     = "BTC-USD"
	testVenue     = "BINANCE"
)

func state(qty int64) *domainpb.PositionState {
	return &domainpb.PositionState{
		PortfolioId:  testPortfolio,
		InstrumentId: testInstr,
		Quantity:     &commonpb.Decimal{Coefficient: qty, Exponent: 0},
		AsOf:         timestamppb.New(time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)),
	}
}

// newProjectorUnderTest builds the projector over a real Producer with NO
// tenant fallback — see the package doc.
func newProjectorUnderTest(t *testing.T) (*Projector, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "oms",
		ProducerVersion: "test",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	st := &stubStore{applied: &Applied{Aggregate: state(3), Venue: state(2)}}
	p, err := NewProjector(st, prod, testTenant)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	return p, cc
}

// filledDelivery builds the inbound FACT and the ctx a real consumer would hand
// the handler — including the stashed tenant.
func filledDelivery(t *testing.T, tenant string) (context.Context, *envelopepb.Envelope, []byte) {
	t.Helper()
	ev := &orderpb.OrderFilled{
		State: &orderpb.OrderState{PortfolioId: testPortfolio},
		Fill: &orderpb.Fill{
			FillId:       "f-1",
			InstrumentId: testInstr,
			Venue:        testVenue,
			Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
			Price:        &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
			ExecutedAt:   timestamppb.New(time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)),
		},
	}
	payload, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{EventType: orderEventFilled, TenantId: tenant}
	return bus.WithTenantID(context.Background(), tenant), env, payload
}

// ONE FILL, TWO FACTs, BOTH VALID (EXEC-M19a). The fund-level position is what
// the risk engine and compliance monitor consume; the per-venue holding is what
// a CLOSE must flatten. Both go out, and both must survive Validate — an
// envelope refused here leaves a consumer arming on nothing.
func TestPositionProjectorEmitsTwoValidFacts(t *testing.T) {
	p, cc := newProjectorUnderTest(t)
	ctx, env, payload := filledDelivery(t, testTenant)

	if err := p.Handle(ctx, env, payload); err != nil {
		t.Fatalf("Handle: %v — the position FACTs do not survive bus.Validate, so the risk engine, "+
			"the compliance monitor and the pre-trade gate would all be arming on nothing", err)
	}
	if len(cc.sent) != 2 {
		t.Fatalf("published %d messages, want 2 (fund-level and per-venue)", len(cc.sent))
	}

	for i, want := range []struct {
		subject   string
		eventType string
	}{
		{subject.PositionFor(testTenant, testPortfolio, testInstr), positionEventChanged},
		{subject.VenuePositionFor(testTenant, testPortfolio, testVenue, testInstr), subject.VenuePositionChanged},
	} {
		msg := cc.sent[i]
		envOut, _, err := bus.Unframe(msg.Body)
		if err != nil {
			t.Fatalf("[%d] Unframe: %v", i, err)
		}
		if err := bus.Validate(envOut); err != nil {
			t.Errorf("[%d] emitted envelope fails Validate: %v", i, err)
		}
		if msg.Subject != want.subject {
			t.Errorf("[%d] subject = %q, want %q — the subject carries the entity so the COMPACTED "+
				"position stream keeps the current state of each holding", i, msg.Subject, want.subject)
		}
		if got := envOut.GetEventType(); got != want.eventType {
			t.Errorf("[%d] event_type = %q, want %q — consumers dispatch on it, and it is what tells "+
				"them whether this is the fund's position or one venue's", i, got, want.eventType)
		}
		if got := envOut.GetTenantId(); got != testTenant {
			t.Errorf("[%d] tenant_id = %q, want %q", i, got, testTenant)
		}
		if got := envOut.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Errorf("[%d] event_class = %v, want FACT", i, got)
		}
		if got := envOut.GetPayloadSchemaRef(); got != "domain.v1.PositionState:1" {
			t.Errorf("[%d] payload_schema_ref = %q", i, got)
		}
		if got := string(msg.Key); got != testPortfolio {
			t.Errorf("[%d] partition key = %q, want the portfolio id — a book's states must stay "+
				"ordered against each other", i, got)
		}
	}

	// The two subjects must DIFFER, or the compacted stream keeps one and
	// silently discards the other — the fund's position and one venue's holding
	// overwriting each other in turn.
	if cc.sent[0].Subject == cc.sent[1].Subject {
		t.Errorf("both FACTs went to %q. On a compacted stream the second overwrites the first, so "+
			"one of the fund position and the venue holding is permanently lost", cc.sent[0].Subject)
	}
}

// THE TENANT COMES FROM THE DELIVERY, AND NOTHING ELSE.
//
// The projector sets no Event.TenantID and the OMS producer has no
// ProducerConfig.Tenant fallback, so the ctx the consumer stashes it on is the
// only source. Publishing outside an inbound delivery therefore produces an
// envelope the broker REFUSES — which is not a hypothetical: that exact shape
// crash-looped this service once.
//
// This test is what makes that dependency visible instead of load-bearing and
// unwritten.
func TestPositionPublishRefusesOutsideADelivery(t *testing.T) {
	p, cc := newProjectorUnderTest(t)
	_, env, payload := filledDelivery(t, testTenant)

	// context.Background(), NOT the delivery ctx: no stashed tenant.
	err := p.Handle(context.Background(), env, payload)
	if err == nil {
		t.Fatal("a position FACT was published with no tenant available from anywhere. On the live " +
			"spine bus.Validate refuses it, so the OMS would fail every fill it folded — which is " +
			"how this service crash-looped before")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the refusal must name the tenant; got %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}

// A FILL FROM ANOTHER TENANT IS REFUSED BEFORE IT IS FOLDED (#223). The
// projector folds into a book scoped to its own tenant and publishes on subjects
// built from it, so an unchecked cross-tenant envelope would be folded into, and
// published as, THIS tenant's position.
func TestPositionProjectorRefusesAnotherTenantsFill(t *testing.T) {
	p, cc := newProjectorUnderTest(t)
	ctx, env, payload := filledDelivery(t, "other-tenant")

	if err := p.Handle(ctx, env, payload); err == nil {
		t.Fatal("a fill from another tenant was folded and published as this tenant's position")
	}
	if len(cc.sent) != 0 {
		t.Errorf("a cross-tenant fill still produced %d published message(s)", len(cc.sent))
	}
}

// A non-fill event is acked without publishing. The projector subscribes to
// subjects that also carry other order events; treating one as a fill would
// publish a position derived from nothing.
func TestPositionProjectorIgnoresANonFillEvent(t *testing.T) {
	p, cc := newProjectorUnderTest(t)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	env := &envelopepb.Envelope{EventType: "order.order.accepted", TenantId: testTenant}

	if err := p.Handle(ctx, env, nil); err != nil {
		t.Fatalf("a non-fill event must be acked, not errored: %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a non-fill event produced %d published message(s)", len(cc.sent))
	}
}

// guard against the stub drifting from the real Applied contract: both
// projections must be present, or the two-publish assertion above is vacuous.
func TestStubStoreReturnsBothProjections(t *testing.T) {
	st := &stubStore{applied: &Applied{Aggregate: state(3), Venue: state(2)}}
	got, err := st.Apply(context.Background(), testPortfolio, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Aggregate == nil || got.Venue == nil {
		t.Fatal("the stub returns a partial Applied, so the two-publish test could pass with one")
	}
	if new(big.Rat).SetInt64(got.Aggregate.GetQuantity().GetCoefficient()).
		Cmp(new(big.Rat).SetInt64(got.Venue.GetQuantity().GetCoefficient())) == 0 {
		t.Fatal("aggregate and venue carry the same quantity, so a test asserting they are published " +
			"to different subjects cannot tell them apart in the payload")
	}
}
