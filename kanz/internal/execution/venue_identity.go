package execution

import (
	"context"
	"errors"
	"fmt"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

// VenueIdentity is what an adapter says it is when the OMS asks (venue.v1.Describe):
// the venue it trades, the exchange account its credential belongs to, and whether
// the exchange itself confirmed that.
type VenueIdentity struct {
	MIC     string
	Account string
	Proof   AccountProof
}

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
	}, nil
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
