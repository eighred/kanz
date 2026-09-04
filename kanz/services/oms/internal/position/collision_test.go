// TWO INSTRUMENTS, TWO SUBJECTS — AT THE PRODUCTION CALLER (#999).
//
// subject.Token used to map `.`, `*`, `>` and ` ` onto `_`, so the projector announced
// `VOD.L` and `VOD_L` on ONE subject. The POSITION stream keeps one message per subject
// forever, so one instrument's current position overwrote the other's and
// DeliverLastPerSubject gave a booting consumer one of them and silence about the other.
//
// The unit proof that Token is injective lives in internal/platform/subject. This is the
// proof at the caller that actually publishes: two fills in two instruments that used to
// collide come out on two subjects, both surviving bus.Validate on the wire.
package position

import (
	"context"
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

// projectorForInstrument is newProjectorUnderTest with the stub fold carrying the
// instrument actually under test, so the payload and the subject agree.
func projectorForInstrument(t *testing.T, instr string) (*Projector, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "oms", ProducerVersion: "test"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	fold := func(qty int64) *domainpb.PositionState {
		return &domainpb.PositionState{
			PortfolioId:  testPortfolio,
			InstrumentId: instr,
			Quantity:     &commonpb.Decimal{Coefficient: qty, Exponent: 0},
			AsOf:         timestamppb.New(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)),
		}
	}
	p, err := NewProjector(newStubStore(&Applied{Aggregate: fold(3), Venue: fold(2)}), prod, testTenant)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	return p, cc
}

// filledDeliveryFor is filledDelivery for one named instrument.
func filledDeliveryFor(t *testing.T, tenant, instr, venue string) (context.Context, *envelopepb.Envelope, []byte) {
	t.Helper()
	payload, err := proto.Marshal(&orderpb.OrderFilled{
		State: &orderpb.OrderState{PortfolioId: testPortfolio},
		Fill: &orderpb.Fill{
			FillId:       "f-" + instr,
			InstrumentId: instr,
			Venue:        venue,
			Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
			Price:        &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
			ExecutedAt:   timestamppb.New(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bus.WithTenantID(context.Background(), tenant),
		&envelopepb.Envelope{EventType: orderEventFilled, TenantId: tenant}, payload
}

// The id pairs #999 was filed on, in the shapes the estate actually holds:
//
//	VOD.L / VOD_L                    a RIC against the underscore shape MBS_A/OPT_A use
//	AAPL US Equity / AAPL_US_Equity  datamaster's DefaultIDResolver falls FIGI -> ISIN ->
//	                                 CUSIP -> Symbol, so a vendor row with none of the
//	                                 first three yields the Bloomberg ticker verbatim
func TestCollidingInstrumentIDsGetDistinctPositionSubjects(t *testing.T) {
	for _, pair := range [][2]string{
		{"VOD.L", "VOD_L"},
		{"AAPL US Equity", "AAPL_US_Equity"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			seen := map[string][]string{}
			for _, instr := range pair {
				p, cc := projectorForInstrument(t, instr)
				ctx, env, payload := filledDeliveryFor(t, testTenant, instr, testVenue)
				if err := p.Handle(ctx, env, payload); err != nil {
					t.Fatalf("Handle(%q): %v", instr, err)
				}
				if len(cc.sent) != 2 {
					t.Fatalf("Handle(%q) published %d messages, want 2", instr, len(cc.sent))
				}
				for _, msg := range cc.sent {
					envOut, _, err := bus.Unframe(msg.Body)
					if err != nil {
						t.Fatalf("Unframe: %v", err)
					}
					if err := bus.Validate(envOut); err != nil {
						t.Errorf("instrument %q emits an envelope the broker refuses: %v", instr, err)
					}
					seen[msg.Subject] = append(seen[msg.Subject], instr)
				}
			}
			// Four FACTs (aggregate + per-venue, twice) must occupy four subjects.
			if len(seen) != 4 {
				for subj, instrs := range seen {
					if len(instrs) > 1 {
						t.Errorf("subject %q carries BOTH %q and %q — on the compacted POSITION "+
							"stream one instrument's current position overwrites the other's, and "+
							"the compliance monitor's book comes back missing a holding",
							subj, instrs[0], instrs[1])
					}
				}
				t.Fatalf("two instruments produced %d distinct subjects, want 4", len(seen))
			}
		})
	}
}

// Every subject the projector builds must decode back to the ids it was built from —
// which is what makes "these two subjects differ" mean "these two instruments differ"
// rather than "these two strings happen not to match".
func TestPositionSubjectDecodesBackToTheInstrument(t *testing.T) {
	for _, instr := range []string{"VOD.L", "VOD_L", "AAPL US Equity", "BTC-USD", "a>b", "a*b"} {
		subj := subject.PositionFor(testTenant, testPortfolio, instr)
		toks := splitLast(subj)
		got, err := subject.Detoken(toks)
		if err != nil {
			t.Errorf("PositionFor(%q) ends in %q, which does not decode: %v", instr, toks, err)
			continue
		}
		if got != instr {
			t.Errorf("PositionFor(%q) ends in %q, which decodes to %q", instr, toks, got)
		}
	}
}

// splitLast returns the final dot-delimited token of a subject. It is only correct
// because Token escapes the dot — which is the property under test.
func splitLast(subj string) string {
	for i := len(subj) - 1; i >= 0; i-- {
		if subj[i] == '.' {
			return subj[i+1:]
		}
	}
	return subj
}
