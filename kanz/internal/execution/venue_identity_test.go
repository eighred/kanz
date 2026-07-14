package execution

// SOV-02a: the OMS must not BELIEVE its own manifest about whose money an adapter
// spends.
//
// EXEC-M16 made the exchange account the collateral boundary, but the OMS learned
// the account from OMS_VENUE_ENDPOINTS ("MIC/account=addr") — a string a human
// typed. The adapter HOLDS the credential and is the only process that can be asked.
// These tests drive the ask over real gRPC and pin the refusal: an adapter that says
// it is somebody else must stop the OMS from starting, not be silently trusted.

import (
	"context"
	"errors"
	"strings"
	"testing"

	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"
)

// describingAdapter answers Describe with whatever identity it was given — the
// out-of-process adapter, telling the OMS who it really is.
type describingAdapter struct {
	stubAdapter
	id *venuepb.DescribeResponse
}

func (s *describingAdapter) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	return s.id, nil
}

func dialDescribing(t *testing.T, id *venuepb.DescribeResponse) *GRPCVenue {
	t.Helper()
	conn := dialStub(t, &describingAdapter{id: id})
	return NewGRPCVenue(id.GetMic(), id.GetAccount(), conn, "acme")
}

// TestDescribeReadsTheAdapterIdentity: the round trip works over real gRPC.
func TestDescribeReadsTheAdapterIdentity(t *testing.T) {
	v := dialDescribing(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
	})

	id, err := v.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if id.MIC != "XBIN" || id.Account != "binance-main" {
		t.Errorf("identity = %+v", id)
	}
	if !id.Proof.Verified || id.Proof.ExchangeAccountID != "12345678" {
		t.Errorf("proof = %+v", id.Proof)
	}
}

// TestVerifyIdentityRefusesAnotherAccount is THE test for this task.
//
// The OMS was told this endpoint is okx-sub-1 (fund Beta's account). The adapter —
// which holds the API key — says it is okx-sub-2 (fund Alpha's). Exactly one of
// them is right about whose collateral the fills margin against, and the OMS is not
// the one holding the credential. Without this refusal the OMS registers the venue
// under the DECLARED account, the router happily matches Beta's orders to it, and
// every fill posts to Beta's ledger rows while the exchange debits Alpha.
func TestVerifyIdentityRefusesAnotherAccount(t *testing.T) {
	err := VerifyIdentity("XOKX", "okx-sub-1", VenueIdentity{
		MIC:     "XOKX",
		Account: "okx-sub-2",
		Proof:   AccountProof{Verified: true, ExchangeAccountID: "99999"},
	})
	if err == nil {
		t.Fatal("VerifyIdentity accepted an adapter holding a DIFFERENT account's credential")
	}
	if !errors.Is(err, ErrVenueIdentityMismatch) {
		t.Errorf("err = %v, want ErrVenueIdentityMismatch", err)
	}
	// The operator reading the crash must see both sides of the disagreement, the
	// right way round: which account the manifest declared, which one the CREDENTIAL
	// actually belongs to, and the exchange's own id for it. An error that swaps them
	// sends the operator to fix the wrong deployment.
	for _, want := range []string{
		`declared as account "okx-sub-1"`,
		`HOLDS THE CREDENTIAL FOR account "okx-sub-2"`,
		`exchange account id "99999"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not read %q:\n  %v", want, err)
		}
	}
}

// TestVerifyIdentityRefusesAnotherVenue: an adapter answering for a different
// exchange entirely. Same class, and it means the endpoint map is wired wrong.
func TestVerifyIdentityRefusesAnotherVenue(t *testing.T) {
	err := VerifyIdentity("XOKX", "okx-sub-1", VenueIdentity{MIC: "XBIN", Account: "okx-sub-1"})
	if !errors.Is(err, ErrVenueIdentityMismatch) {
		t.Errorf("err = %v, want ErrVenueIdentityMismatch", err)
	}
}

// TestVerifyIdentityAcceptsAgreement: the adapter is who the manifest says.
func TestVerifyIdentityAcceptsAgreement(t *testing.T) {
	err := VerifyIdentity("XBIN", "binance-main", VenueIdentity{
		MIC:     "XBIN",
		Account: "binance-main",
		Proof:   AccountProof{Verified: true, ExchangeAccountID: "12345678"},
	})
	if err != nil {
		t.Errorf("VerifyIdentity = %v, want nil for an adapter that agrees", err)
	}
}

// TestVerifyIdentityAcceptsUnverifiedButAgreeing: the adapter never proved itself
// against the exchange (no uid configured, or the venue could not be reached). Its
// claim still AGREES with the manifest, so this is not a mismatch — it is an
// unproven agreement, and refusing it here would be the OMS declining to start
// because nobody has finished a rollout. The OMS warns and counts it instead, and
// OMS_REQUIRE_VERIFIED_ACCOUNT is what turns it into a refusal.
func TestVerifyIdentityAcceptsUnverifiedButAgreeing(t *testing.T) {
	err := VerifyIdentity("XBIN", "binance-main", VenueIdentity{MIC: "XBIN", Account: "binance-main"})
	if err != nil {
		t.Errorf("VerifyIdentity = %v, want nil (unverified is not a mismatch)", err)
	}
}
