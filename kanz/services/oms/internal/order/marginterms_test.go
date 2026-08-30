package order

import (
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// THE COLLATERAL TERMS MUST REACH THE WIRE, NOT ONLY THE VALIDATOR (#417).
//
// This is the population half of the same class stopprice_test.go carries for
// #405, and it exists because #417 Part 1 shipped without it.
//
// Part 1 added margin_mode and leverage to SubmitOrder AND to OrderState, gated
// them at admission, refused them in both connectors, and covered them in the
// dual-control digest — and then never copied the values in Accept. So
// st.GetMarginMode() was UNSPECIFIED for every order that had ever been
// admitted, which made the connector refusals unreachable from the OMS path and
// meant a CROSS order was placed as ordinary spot with no error anywhere.
//
// WHY THE CLASS GUARD DID NOT CATCH IT. test/arch/submit_fields_reach_state_test.go
// compares proto field NAMES across the two messages. Both carry margin_mode, so
// it passed — and its own header says so in as many words: "WHAT IT CANNOT
// CHECK: that the counterpart is POPULATED." A guard that fails the build for a
// third instance of a class cannot also prove the second instance was wired,
// and reading its green as coverage is how Part 1 shipped.

func TestAccept_CarriesTheMarginModeOntoTheState(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS
	cmd.Leverage = d(10, 0)

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := st.GetMarginMode(); got != orderpb.MarginMode_MARGIN_MODE_CROSS {
		t.Fatalf("OrderState.margin_mode = %s, want CROSS.\n\n"+
			"OrderState is what Venue.Execute receives, so a regime dropped here is one the "+
			"connector can never be told. Both connectors refuse a non-spot regime by reading "+
			"st.GetMarginMode() — with this unset that refusal is unreachable, and the order is "+
			"placed as ordinary SPOT while the audit root records the regime the trader asked "+
			"for. That is #240 verbatim, one message over.", got)
	}
	if got := st.GetLeverage(); got == nil {
		t.Fatal("OrderState.leverage is nil after admission — the multiplier on capital at risk " +
			"was validated at the perimeter and then discarded")
	} else if dec.Cmp(got, d(10, 0)) != 0 {
		t.Fatalf("leverage = %v, want 10 — carried EXACTLY or not at all", got)
	}
}

// ISOLATED TOO. CROSS and ISOLATED differ in whose collateral backs the
// position, not in whether the venue must be told — and a test that only walks
// one arm is satisfied by a constant.
func TestAccept_CarriesIsolatedMarginToo(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_ISOLATED

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := st.GetMarginMode(); got != orderpb.MarginMode_MARGIN_MODE_ISOLATED {
		t.Fatalf("margin_mode = %s, want ISOLATED", got)
	}
}

// A SPOT ORDER STAYS SPOT, AND CARRIES NO LEVERAGE. Without this arm, "carry it"
// is satisfied by hardcoding CROSS — and the zero value being UNSPECIFIED is the
// whole reason copying unconditionally is safe here where StopPrice's was not.
func TestAccept_LeavesSpotOrdersUnlevered(t *testing.T) {
	st, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := st.GetMarginMode(); got != orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED {
		t.Fatalf("an order that asked for no regime carries margin_mode = %s, want UNSPECIFIED "+
			"— spot is the honest answer for an order that did not ask for margin", got)
	}
	if got := st.GetLeverage(); got != nil {
		t.Fatalf("an order that asked for no leverage carries leverage = %v, want unset", got)
	}
}

// THE DUAL-CONTROL DIGEST MUST AGREE ACROSS THE TWO SHAPES (#417).
//
// approval.TermsOfSubmit reads the COMMAND and TermsOfState reads the STORED
// ORDER, and the two must hash identically or an approval collected on a
// proposal cannot be matched to the order it authorised. With the terms dropped
// in Accept, a margin order hashed CROSS on one side and UNSPECIFIED on the
// other — so the digest silently stopped being a digest OF THAT ORDER.
//
// Asserted here rather than in the approval package because the defect is in
// Accept: the digest code was already correct and reading a field nobody wrote.
func TestAccept_KeepsTheDualControlDigestConsistent(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS
	cmd.Leverage = d(3, 0)

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if st.GetMarginMode() != cmd.GetMarginMode() {
		t.Fatalf("state margin_mode %s != command margin_mode %s — the two shapes the digest "+
			"hashes disagree, so a signature taken over the proposal cannot match the order",
			st.GetMarginMode(), cmd.GetMarginMode())
	}
	if dec.Cmp(st.GetLeverage(), cmd.GetLeverage()) != 0 {
		t.Fatalf("state leverage %v != command leverage %v — same divergence, one field over",
			st.GetLeverage(), cmd.GetLeverage())
	}
}

// THE TERMS MUST SURVIVE THE LIFECYCLE, NOT ONLY ADMISSION (#742).
//
// Everything above pins what Accept writes. What none of it pins is the STEP
// AFTER: Route() sits between admission and Venue.Execute, so a transition that
// dropped these fields would restore the original bug one step later with every
// admission test above still green. stop_price carries exactly this test
// (TestRoute_PreservesTheStopPrice) for exactly this reason, and margin mode is
// the fourth field in that class.
//
// It passes for free today, because every transition goes through cloneState and
// cloneState is proto.Clone. That is precisely why it is ASSERTED rather than
// assumed: cloneState used to be a hand-rolled field-by-field copy, and the
// first field added after it was written — venue_account_id, the exchange
// account whose collateral the order spends — was silently dropped at routing.
// A copy that must be edited whenever the message changes is a copy that will be
// forgotten, and this is the assertion that notices.
func TestRoute_PreservesTheCollateralTerms(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_ISOLATED
	cmd.Leverage = d(3, 0)

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	routed, _ := Route(st, t0)

	if got := routed.GetMarginMode(); got != orderpb.MarginMode_MARGIN_MODE_ISOLATED {
		t.Fatalf("after Route, margin_mode = %s, want ISOLATED.\n\n"+
			"Route is on the path to Venue.Execute, so a transition that drops the regime drops "+
			"it exactly where the connector reads it — and both connectors' refusals key on "+
			"st.GetMarginMode(), so the order would be placed as ordinary SPOT while the audit "+
			"root records the regime the trader asked for.", got)
	}
	if got := routed.GetLeverage(); dec.Cmp(got, d(3, 0)) != 0 {
		t.Fatalf("after Route, leverage = %v, want 3 — dropped here, the platform reserves margin "+
			"and buying power against a multiple the venue never applied", got)
	}
}

// AND THEY SURVIVE AN AMEND. Amend clones the state and rewrites quantity and
// price; a clone that lost the regime would leave an order the connector reads
// as spot after a routine resize — the divergence arriving through the one
// command whose whole purpose is to change something else.
func TestAmend_PreservesTheCollateralTerms(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS
	cmd.Leverage = d(5, 0)

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	next, err := Amend(st, &orderpb.AmendOrder{OrderId: st.GetOrderId(), NewQuantity: d(50, 0)}, t0)
	if err != nil {
		t.Fatalf("Amend: %v", err)
	}
	if got := next.GetMarginMode(); got != orderpb.MarginMode_MARGIN_MODE_CROSS {
		t.Fatalf("after Amend, margin_mode = %s, want CROSS — a resize must not silently move the "+
			"order to spot", got)
	}
	if got := next.GetLeverage(); dec.Cmp(got, d(5, 0)) != 0 {
		t.Fatalf("after Amend, leverage = %v, want 5", got)
	}
}
