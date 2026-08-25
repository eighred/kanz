package approval

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// order is a fully-populated SubmitOrder: every covered field set to something
// distinguishable, so a field silently dropped from the digest shows up as a
// mutation that does NOT change the hash.
func order() *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		Metadata: &commandpb.CommandMetadata{
			Issuer:              "user:alice",
			TargetId:            "ord-1",
			Reason:              "rebalance",
			ValidUntil:          timestamppb.New(time.Unix(1_700_000_000, 0)),
			PrincipalPortfolios: []string{"flagship"},
		},
		OrderId:      "ord-1",
		PortfolioId:  "flagship",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     d(5, 0),
		OrderType:    orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
		LimitPrice:   d(50_000, 0),
		StopPrice:    d(49_000, 0),
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_GTD,
		ExpireAt:     timestamppb.New(time.Unix(1_700_000_500, 250)),
		Venue:        "XNAS",
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:             orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP,
			WindowStart:      timestamppb.New(time.Unix(1_700_000_000, 0)),
			WindowEnd:        timestamppb.New(time.Unix(1_700_003_600, 0)),
			SliceCount:       12,
			MaxSliceQuantity: d(1, 0),
		},
	}
}

func digestOf(t *testing.T, cmd *orderpb.SubmitOrder) string {
	t.Helper()
	got, err := TermsOfSubmit(cmd).Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return got
}

// TestEveryCoveredFieldChangesTheDigest IS THE TEST THIS PACKAGE EXISTS FOR.
//
// A field outside the digest is a field that can be EDITED AFTER APPROVAL and
// still apply under the approver's signature — the trail shows two names and the
// order that trades is not the order that was shown. So every economically
// material field of SubmitOrder is mutated here, one at a time, and a digest that
// does not move is a field the signature does not cover.
//
// The failure message names the CONSEQUENCE rather than the mechanism, because
// the person who sees it will be adding a field, not debugging a hash.
func TestEveryCoveredFieldChangesTheDigest(t *testing.T) {
	base := digestOf(t, order())

	cases := []struct {
		field  string
		mutate func(*orderpb.SubmitOrder)
		harm   string
	}{
		{"order_id", func(c *orderpb.SubmitOrder) { c.OrderId = "ord-2" },
			"an approval for one order would cover any other with identical terms — and the slices of " +
				"one TWAP parent differ ONLY by id"},
		{"portfolio_id", func(c *orderpb.SubmitOrder) { c.PortfolioId = "research" },
			"the order could be moved onto another fund's capital after approval"},
		{"instrument_id", func(c *orderpb.SubmitOrder) { c.InstrumentId = "ETH-USD" },
			"a different instrument could be bought under the same signature"},
		{"side", func(c *orderpb.SubmitOrder) { c.Side = orderpb.Side_SIDE_SELL },
			"the position could be inverted after approval"},
		{"quantity", func(c *orderpb.SubmitOrder) { c.Quantity = d(500, 0) },
			"the size could be raised a hundredfold after approval"},
		{"order_type", func(c *orderpb.SubmitOrder) { c.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET },
			"a LIMIT could be re-signed as a MARKET order — the limit price stays in the message and " +
				"stops binding, so covering the price without the type covers nothing"},
		{"limit_price", func(c *orderpb.SubmitOrder) { c.LimitPrice = d(90_000, 0) },
			"the committed price could be raised after approval"},
		{"stop_price", func(c *orderpb.SubmitOrder) { c.StopPrice = d(10, 0) },
			"the trigger could be moved after approval — the field this platform has already " +
				"validated-and-then-dropped once (#405)"},
		{"time_in_force", func(c *orderpb.SubmitOrder) { c.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_GTC },
			"an IOC could be re-signed as a GTC and REST, holding exposure the trader asked to be gone (#486)"},
		{"expire_at", func(c *orderpb.SubmitOrder) { c.ExpireAt = timestamppb.New(time.Unix(1_900_000_000, 0)) },
			"a good-til-date order could be extended by years after approval"},
		{"venue", func(c *orderpb.SubmitOrder) { c.Venue = "XLON" },
			"the order could be re-routed onto a different exchange ACCOUNT, and an exchange " +
				"liquidates per account — a different collateral pool with no price or quantity changed"},
		{"parent_order_id", func(c *orderpb.SubmitOrder) {
			c.ExecutionSchedule = nil
			c.ParentOrderId = "ord-parent"
		},
			"the order could be re-parented under somebody else's clearance — a child is admitted " +
				"WITHOUT re-running the compliance gate"},
		{"execution_schedule.algo", func(c *orderpb.SubmitOrder) {
			c.ExecutionSchedule.Algo = orderpb.ExecutionAlgo_EXECUTION_ALGO_UNSPECIFIED
		}, "the algorithm could be changed after approval"},
		{"execution_schedule.window_start", func(c *orderpb.SubmitOrder) {
			c.ExecutionSchedule.WindowStart = timestamppb.New(time.Unix(1_700_003_500, 0))
		}, "the working window could be narrowed to seconds — a carefully worked order becomes a market sweep"},
		{"execution_schedule.window_end", func(c *orderpb.SubmitOrder) {
			c.ExecutionSchedule.WindowEnd = timestamppb.New(time.Unix(1_700_000_001, 0))
		}, "the working window could be collapsed after approval"},
		{"execution_schedule.slice_count", func(c *orderpb.SubmitOrder) { c.ExecutionSchedule.SliceCount = 1 },
			"a 12-slice schedule could become a single order in front of the venue"},
		{"execution_schedule.max_slice_quantity", func(c *orderpb.SubmitOrder) {
			c.ExecutionSchedule.MaxSliceQuantity = d(100, 0)
		}, "the largest single message the platform will place could be raised after approval"},
		{"execution_schedule (removed entirely)", func(c *orderpb.SubmitOrder) { c.ExecutionSchedule = nil },
			"an order approved to be WORKED OVER TIME could be sent to the venue whole"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			cmd := order()
			tc.mutate(cmd)
			if got := digestOf(t, cmd); got == base {
				t.Fatalf("changing %s did not change the digest, so a dual-control approval does NOT "+
					"cover it: %s", tc.field, tc.harm)
			}
		})
	}
}

// TestTheTwoPlacementsAgreeOnTheDigest.
//
// #410's placement ruling chose a proposal in the OMS that never becomes an
// admitted order, so the propose side hashes a SubmitOrder. The APPROVED order
// then becomes an OrderState, and re-deriving the digest from the stored order is
// how anybody later proves the order that traded is the order that was signed
// for. The two derivations must agree, field for field, or that proof is
// unavailable and the approve step would fail on an order nobody changed —
// #511's defect, where effective_at defaulting to now() made the two steps hash
// different payloads and every approval failed.
func TestTheTwoPlacementsAgreeOnTheDigest(t *testing.T) {
	cmd := order()
	fromCommand := digestOf(t, cmd)

	// The order as the OMS would store it, carrying the same terms plus the
	// lifecycle fields the digest deliberately excludes.
	st := &orderpb.OrderState{
		OrderId:           cmd.GetOrderId(),
		PortfolioId:       cmd.GetPortfolioId(),
		InstrumentId:      cmd.GetInstrumentId(),
		Side:              cmd.GetSide(),
		OrderType:         cmd.GetOrderType(),
		TimeInForce:       cmd.GetTimeInForce(),
		OrderedQuantity:   cmd.GetQuantity(),
		LimitPrice:        cmd.GetLimitPrice(),
		StopPrice:         cmd.GetStopPrice(),
		ExpireAt:          cmd.GetExpireAt(),
		Venue:             cmd.GetVenue(),
		ExecutionSchedule: cmd.GetExecutionSchedule(),
		// Everything below is EXCLUDED by design, and setting it here is the
		// assertion: none of it may move the digest.
		Status:              orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED,
		FilledQuantity:      d(0, 0),
		LeavesQuantity:      cmd.GetQuantity(),
		ArrivalPrice:        d(49_900, 0),
		ArrivalAt:           timestamppb.New(time.Unix(1_700_000_010, 0)),
		VenueAccountId:      "okx-sub-1",
		AsOf:                timestamppb.New(time.Unix(1_700_000_020, 0)),
		AcceptedAnnouncedAt: timestamppb.New(time.Unix(1_700_000_021, 0)),
	}
	fromState, err := TermsOfState(st).Digest()
	if err != nil {
		t.Fatalf("digest from state: %v", err)
	}
	if fromState != fromCommand {
		t.Fatalf("the command and the stored order hash differently (%s vs %s) — an approval collected "+
			"against one could never be shown to cover the other, and every approval would fail as a "+
			"payload change for a reason that has nothing to do with the payload (#511)",
			fromCommand[:12], fromState[:12])
	}
}

// TestCommandMetadataIsExcluded is the OTHER half of #511, and the reason the
// exclusion is deliberate rather than an oversight. The approve step is a
// different command with a different issuer BY DEFINITION — the approver is not
// the proposer — so a digest over metadata would make every approval fail.
func TestCommandMetadataIsExcluded(t *testing.T) {
	base := digestOf(t, order())

	cmd := order()
	cmd.Metadata = &commandpb.CommandMetadata{
		Issuer:              "user:bob", // the approver, necessarily a different person
		TargetId:            "ord-1",
		Reason:              "approving alice's order",
		ValidUntil:          timestamppb.New(time.Unix(1_900_000_000, 0)),
		PrincipalPortfolios: []string{"flagship", "research"},
	}
	if got := digestOf(t, cmd); got != base {
		t.Fatal("the approve step's own metadata changed the digest, so every approval would be " +
			"refused as a payload change — the control failing for a reason that has nothing to do " +
			"with the control (#511)")
	}

	// AND THE IDENTITY IS NOT DROPPED, it is covered by something stronger: the
	// proposer is carried on the proposal and Approve refuses an approver equal to
	// it. A hash can prove a name did not change; it cannot enforce that two
	// names differ.
	prop, err := Propose("p-1", TermsOfSubmit(order()), "user:alice", time.Unix(1_700_000_000, 0), time.Hour)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := prop.Approve("USER:Alice ", base, time.Unix(1_700_000_100, 0)); !errors.Is(err, dualcontrol.ErrSelfApproval) {
		t.Fatalf("self-approval by respelling was not refused: %v", err)
	}
}

// TestAnApprovedOrderCannotBeAlteredAfterwards is the end-to-end property: it
// runs the real propose/approve/apply sequence and then edits one economically
// material field, which is exactly the attack the digest exists to stop.
func TestAnApprovedOrderCannotBeAlteredAfterwards(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	proposed := order()

	prop, err := Propose("p-1", TermsOfSubmit(proposed), "user:alice", now, time.Hour)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	approval, err := prop.Approve("user:bob", digestOf(t, proposed), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The order Bob signed for applies.
	if err := Covers(approval, TermsOfSubmit(proposed)); err != nil {
		t.Fatalf("the approved order was refused by its own approval: %v", err)
	}

	// Now the quantity is raised a hundredfold between approval and submission.
	altered := proto.Clone(proposed).(*orderpb.SubmitOrder)
	altered.Quantity = d(500, 0)
	err = Covers(approval, TermsOfSubmit(altered))
	if err == nil {
		t.Fatal("an approval collected for 5 units authorised submitting 500 — the second signature " +
			"covers the request rather than the VALUE, which makes dual control ceremonial")
	}
	if !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Fatalf("refused for the wrong reason (%v); the operator must be told the payload changed, "+
			"not something else", err)
	}
}

// TestAnApprovalForAnotherActCannotSubmitAnOrder. The three acts share
// internal/dualcontrol precisely so the rule is written once, and that sharing is
// what would otherwise let a pricing-override approval submit an order.
func TestAnApprovalForAnotherActCannotSubmitAnOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	terms := TermsOfSubmit(order())
	digest, err := terms.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// Same subject, same digest, WRONG ACT.
	other, err := dualcontrol.Propose("p-1", dualcontrol.ActPricingOverride, terms.OrderID,
		"user:alice", digest, now, time.Hour)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	a, err := other.Approve("user:bob", digest, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := Covers(a, terms); !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Fatalf("a pricing-override approval covered an order submission: %v", err)
	}
}

// TestTheZeroApprovalCoversNothing. Go permits dualcontrol.Approval{} anywhere,
// so a caller that merely accepted the TYPE would read "nobody approved this" as
// approved.
func TestTheZeroApprovalCoversNothing(t *testing.T) {
	if err := Covers(dualcontrol.Approval{}, TermsOfSubmit(order())); !errors.Is(err, dualcontrol.ErrMalformed) {
		t.Fatalf("the zero Approval was accepted as evidence: %v", err)
	}
}

// TestProposeProducesTheOrderSubmissionAct — #410's third act, which was declared
// and constructed by nothing from #495 until this package.
func TestProposeProducesTheOrderSubmissionAct(t *testing.T) {
	prop, err := Propose("p-1", TermsOfSubmit(order()), "user:alice", time.Unix(1_700_000_000, 0), time.Hour)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if prop.Act != dualcontrol.ActOrderSubmission {
		t.Fatalf("act = %q, want %q", prop.Act, dualcontrol.ActOrderSubmission)
	}
	if prop.Subject != "ord-1" {
		t.Fatalf("subject = %q, want the order id — it is what an approver is shown and what a "+
			"pending list is keyed on", prop.Subject)
	}
	if prop.Digest == "" {
		t.Fatal("proposal carries no digest, so an approval would cover nothing")
	}
}

// TestTheDigestIsStableAcrossDecimalEncodings. common.v1.Decimal has more than
// one encoding per value; a signature over the ENCODING would be invalidated by a
// store that normalises an exponent on the way through, for an order nobody
// changed.
func TestTheDigestIsStableAcrossDecimalEncodings(t *testing.T) {
	a, b := order(), order()
	b.Quantity = d(500, -2) // 5, written differently
	b.LimitPrice = d(5_000_000, -2)
	if digestOf(t, a) != digestOf(t, b) {
		t.Fatal("the same quantity and price written at a different exponent produced a different " +
			"digest — a store that re-scales a decimal would invalidate a signature over an order " +
			"nobody edited")
	}
}

// TestUnsetAndZeroDoNotHashAlike. A MARKET order has no limit price; an order
// with a limit price of zero is a different (and invalid) instruction, and the
// two must not be interchangeable under one signature.
func TestUnsetAndZeroDoNotHashAlike(t *testing.T) {
	unset, zero := order(), order()
	unset.LimitPrice = nil
	zero.LimitPrice = d(0, 0)
	if digestOf(t, unset) == digestOf(t, zero) {
		t.Fatal("an unset limit price and a zero limit price hash identically")
	}
}

// TestTheDigestRefusesAValueItCannotRead. Substituting zero, or the raw
// coefficient, would sign a value nobody can reproduce — and this is a signature,
// so an unreproducible value means every later approval fails for a reason no
// operator can see.
func TestTheDigestRefusesAValueItCannotRead(t *testing.T) {
	cmd := order()
	cmd.Quantity = d(1, 2_000_000_000)

	got, err := TermsOfSubmit(cmd).Digest()
	if err == nil {
		t.Fatalf("an out-of-domain exponent was hashed to %s rather than refused", got[:12])
	}
	if got != "" {
		t.Error("a digest was returned alongside the error — a caller ignoring the error would sign it")
	}
}

// TestTheDigestHashesAFixedNumberOfParts. dualcontrol.Digest length-prefixes each
// part, so a VARIABLE part count is the one remaining way two different orders
// could produce one hash. The count is asserted rather than assumed because
// adding a field to the append chain and forgetting digestParts is the obvious
// mistake, and it would silently change every digest already signed.
func TestTheDigestHashesAFixedNumberOfParts(t *testing.T) {
	full := order()
	empty := &orderpb.SubmitOrder{}
	// Both must produce a digest at all — the empty order exercises the nil arms
	// of every renderer, which is where a missing part would appear.
	if _, err := TermsOfSubmit(full).Digest(); err != nil {
		t.Fatalf("full order: %v", err)
	}
	if _, err := TermsOfSubmit(empty).Digest(); err != nil {
		t.Fatalf("empty order: %v", err)
	}
	if digestParts != 20 {
		t.Fatalf("digestParts = %d, want 20 — the covered field set changed. That is allowed, but it "+
			"INVALIDATES EVERY DIGEST ALREADY SIGNED, so it must be a deliberate edit here and not a "+
			"side effect of adding a field", digestParts)
	}
}
