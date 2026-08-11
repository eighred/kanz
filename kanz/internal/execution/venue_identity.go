package execution

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

// VenueIdentity is what an adapter says it is when the OMS asks (venue.v1.Describe):
// the venue it trades, the exchange account its credential belongs to, and whether
// the exchange itself confirmed that.
type VenueIdentity struct {
	MIC     string
	Account string
	Proof   AccountProof

	// OrderTypes is what this adapter can actually place (#405).
	//
	// NIL MEANS THE ADAPTER DID NOT SAY, and that is not the same as "none". An
	// adapter predating venue.v1's supported_order_types answers with nothing,
	// and treating that as a refusal would stop every order it has always been
	// able to work. SupportsOrderType below reads nil as "unknown, allow" and the
	// OMS is what decides whether unknown is tolerable — the same split as
	// AccountProof.Verified.
	OrderTypes []orderpb.OrderType
}

// SupportsOrderType reports whether this adapter said it can place t.
//
// UNKNOWN IS PERMISSIVE HERE AND REFUSED ONE LAYER UP. An empty list means the
// adapter never answered the question, so this cannot distinguish "cannot" from
// "did not say" — and inventing a refusal at that distance would turn a schema
// addition into a trading outage. The OMS holds the switch
// (OMS_REQUIRE_ORDER_TYPE_SUPPORT) because it is the component that can name the
// adapter, count the gap and tell an operator how to close it.
func (v VenueIdentity) SupportsOrderType(t orderpb.OrderType) bool {
	if len(v.OrderTypes) == 0 {
		return true
	}
	return ContainsOrderType(v.OrderTypes, t)
}

// DeclaresOrderTypes reports whether the adapter answered the capability
// question at all. It is separate from SupportsOrderType so a caller can tell
// "allowed" from "allowed because nobody knows".
func (v VenueIdentity) DeclaresOrderTypes() bool { return len(v.OrderTypes) > 0 }

// ErrVenueIdentityMismatch: the adapter is not who the OMS was told it is.
//
// This is never a warning. The OMS's configuration and the process holding the API
// key disagree about whose collateral the fills will margin against, and only one of
// them can be right — the one holding the key. Trading through it would post fills
// to one fund's ledger rows while the exchange debits another's, which is precisely
// the failure EXEC-M16 exists to prevent, arriving through the one door EXEC-M16 left
// open.
var ErrVenueIdentityMismatch = errors.New("execution: venue adapter identity does not match the endpoint it was declared as")

// Describe asks the adapter who it is.
func (v *GRPCVenue) Describe(ctx context.Context) (VenueIdentity, error) {
	resp, err := v.client.Describe(ctx, &venuepb.DescribeRequest{})
	if err != nil {
		return VenueIdentity{}, fmt.Errorf("execution: describe venue %s: %w", v.mic, err)
	}
	return VenueIdentity{
		MIC:     resp.GetMic(),
		Account: resp.GetAccount(),
		Proof: AccountProof{
			Verified:          resp.GetAccountVerified(),
			ExchangeAccountID: resp.GetExchangeAccountId(),
		},
		OrderTypes: resp.GetSupportedOrderTypes(),
	}, nil
}

// ListInstruments asks the adapter which pairs it is configured to trade (#406).
//
// The OMS calls this at DIAL TIME, beside Describe, and holds the answer. The set
// is the adapter's own symbol map — deploy-time configuration that only changes
// when the adapter is redeployed, and an adapter restart is already an OMS-visible
// event because Describe is asked again. Serving the catalogue from memory also
// means a pair picker does not fan out to every adapter on every page load, so one
// unreachable venue cannot take the whole picker down.
//
// AN ADAPTER WITH NO SYMBOLS RETURNS AN EMPTY LIST, WHICH IS AN ANSWER. It can
// route nothing, and the caller must be able to tell that apart from a venue that
// was never asked — so this returns the MIC the adapter answered for, and the OMS
// records the venue as present with nothing in it.
func (v *GRPCVenue) ListInstruments(ctx context.Context) ([]InstrumentSymbol, error) {
	resp, err := v.client.ListInstruments(ctx, &venuepb.ListInstrumentsRequest{})
	if err != nil {
		return nil, fmt.Errorf("execution: list instruments at venue %s: %w", v.mic, err)
	}
	out := make([]InstrumentSymbol, 0, len(resp.GetInstruments()))
	for _, in := range resp.GetInstruments() {
		out = append(out, InstrumentSymbol{
			InstrumentID: in.GetInstrumentId(),
			VenueSymbol:  in.GetVenueSymbol(),
		})
	}
	return out, nil
}

// VerifyIdentity checks an adapter's own answer against what the OMS was told in
// OMS_VENUE_ENDPOINTS, and returns ErrVenueIdentityMismatch if they disagree.
//
// An UNVERIFIED but agreeing answer is NOT a mismatch. The adapter has not proved
// itself against the exchange (no uid bound, or the venue was unreachable at its
// boot), but it is claiming to be what the manifest claims it is, and refusing that
// here would mean the OMS declines to start because an operator has not finished a
// rollout. That state is real and must be visible, so the OMS warns once per adapter
// and counts it — and OMS_REQUIRE_VERIFIED_ACCOUNT is the switch that turns it into
// a refusal, once every adapter has a uid bound. Same shape, same reasoning as
// OMS_REQUIRE_MANDATE and OMS_REQUIRE_VENUE_ACCOUNT: a control that refuses every
// order for every adapter nobody has configured yet is a trading outage, and it is
// armed WITH the list in hand, never as a default.
func VerifyIdentity(declaredMIC, declaredAccount string, id VenueIdentity) error {
	if id.MIC != declaredMIC {
		return fmt.Errorf("%w: declared as venue %q but the adapter answers for venue %q",
			ErrVenueIdentityMismatch, declaredMIC, id.MIC)
	}
	if id.Account != declaredAccount {
		return fmt.Errorf("%w: declared as account %q but the adapter HOLDS THE CREDENTIAL FOR account %q "+
			"(exchange account id %q) — every fill it produces would margin against %[3]q's collateral while the "+
			"ledger booked it to %[2]q",
			ErrVenueIdentityMismatch, declaredAccount, id.Account, id.Proof.ExchangeAccountID)
	}
	return nil
}
