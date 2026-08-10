package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
)

// THE PORTFOLIOS CLAIM, FROM THE OPERATOR'S INVITE TO THE ENTITLEMENT CHECK
// (#99).
//
// #99 sat at P0 as blocked-external for months: the cross-USER ownership fix
// depends on a portfolios claim, and the Eighred SSO that was to issue it does
// not exist. The platform now issues its own (#364), so the requirement is
// finally testable — and the thing worth testing is the WHOLE JOURNEY, because
// every individual link already had a test while the chain had none:
//
//	operator's invite  ->  account  ->  minted token  ->  JWKS verification
//	  ->  auth.Principal  ->  edgePrincipal  ->  the entitlement rule
//
// That is exactly the shape of the #225 defect this file was created for: each
// end was fine and the join dropped the field. A test that asserts the join
// asserts the property nobody owns.
//
// It is deliberately NOT a mock of any step. The signer is the real one, the
// JWKS is served over HTTP and fetched by the real verifier, and the rule at the
// end is auth.PortfolioEntitled — which is literally what the OMS's entitledTo
// calls (services/oms/internal/order/service.go:1198).

// issuedTokenFor runs the real provisioning path — an operator's invite, redeemed
// into an account — and mints a token for it, served by a real JWKS endpoint.
// Returns the token and the issuer URL the gateway would be configured with.
func issuedTokenFor(t *testing.T, portfolios []string) (token, issuer string) {
	t.Helper()

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	signer, err := identity.NewSigner(key, srv.URL, "kanz-api", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(signer.JWKS())
	})

	// THE CLAIM COMES FROM THE INVITE, which is the part #99 is about: an
	// operator decides the portfolios when they create the account, and the
	// invitee supplies only a credential. Going through NewInvite and
	// UserFromInvite rather than building a User by hand is what makes this a
	// test of provisioning rather than of the signer.
	_, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatalf("NewInviteToken: %v", err)
	}
	now := time.Now().UTC()
	inv, err := identity.NewInvite("inv-99", hash, "user:alice", "acme",
		[]string{"kanz-trader"}, portfolios, "user:operator", now, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	cred, err := identity.HashCredential("a-properly-long-passphrase")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	user := identity.UserFromInvite(inv, cred, now)

	tok, _, err := signer.Mint(user)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok, srv.URL
}

// edgePrincipalFor authenticates tok exactly as the deployed gateway does, and
// maps it onto the edge principal the order path reads.
func edgePrincipalFor(t *testing.T, tok, issuer string) []string {
	t.Helper()
	authn, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{Issuer: issuer, Audience: "kanz-api"})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	shared, err := authn.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("the gateway refused a token the platform just issued: %v", err)
	}
	return edgePrincipal(shared).Portfolios
}

func TestAnInvitesPortfoliosReachTheEntitlementCheck(t *testing.T) {
	tok, issuer := issuedTokenFor(t, []string{"flagship", "research"})
	got := edgePrincipalFor(t, tok, issuer)

	if len(got) != 2 || got[0] != "flagship" || got[1] != "research" {
		t.Fatalf("edge principal portfolios = %v, want [flagship research].\n\n"+
			"The operator set these on the invite. Anywhere they are dropped between the "+
			"invite and here, the OMS refuses every cancel and amend NOT_ENTITLED — which "+
			"is #225, and it survived because each end had a test and the join did not.", got)
	}

	// The rule at the end is the OMS's: services/oms/internal/order/service.go's
	// entitledTo is a one-line call to this function.
	if !auth.PortfolioEntitled(got, "flagship") {
		t.Error("a portfolio the operator granted was refused — the caller cannot act on " +
			"an account provisioned for them")
	}
	if auth.PortfolioEntitled(got, "someone-elses-book") {
		t.Fatal("a portfolio the operator did NOT grant was allowed.\n\n" +
			"This is the cross-user ownership hole the claim exists to close: a trader " +
			"acting on a book they were never entitled to.")
	}
}

// AN ACCOUNT PROVISIONED WITH NO PORTFOLIOS IS REFUSED, NOT UNRESTRICTED.
//
// This is the case the two consumers answer differently on purpose. On the
// capital path empty DENIES: the shortest fix for a NOT_ENTITLED outage is to
// make it permissive, and that converts a trading outage into an authorization
// bypass across every portfolio in the tenant.
func TestAnInviteWithNoPortfoliosEntitlesNothing(t *testing.T) {
	tok, issuer := issuedTokenFor(t, nil)
	got := edgePrincipalFor(t, tok, issuer)

	if len(got) != 0 {
		t.Fatalf("portfolios = %v, want empty — the invite granted none", got)
	}
	if auth.PortfolioEntitled(got, "flagship") {
		t.Fatal("an account provisioned with NO portfolios was entitled to one.\n\n" +
			"Empty must deny on the capital path. Treating it as 'unrestricted' would " +
			"make every account the operator has not yet scoped a fully privileged one.")
	}
}
