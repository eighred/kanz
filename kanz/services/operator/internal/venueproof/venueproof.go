// Package venueproof is the operator's pre-write venue-key proof: before
// SetVenueKeys writes a candidate credential to its backend, it asks the
// exchange which account the credential belongs to. It maps the operator's
// own secrets.VenueKeys type onto exchangeauth.Credential and adds the one
// piece of operator-specific policy exchangeauth does not know: which base
// URL to call for a given venue, configured per deployment.
package venueproof

import (
	"context"
	"errors"

	"net/http"

	"github.com/kanz-eng/kanz/internal/venueadapter/exchangeauth"
	"github.com/kanz-eng/kanz/services/operator/internal/secrets"
)

// ErrNoEndpoint is returned when proof is enabled but the requested venue has
// no base URL configured. Proof is deploy-time configuration only — there is
// no default endpoint, so a venue that isn't configured cannot be proved and
// its write must be refused, never written unproven.
var ErrNoEndpoint = errors.New("venueproof: no exchange endpoint configured for this venue")

// Prover asks an exchange which account a candidate key set belongs to,
// scoped to the venues present in baseURLs.
type Prover struct {
	baseURLs map[string]string
	httpc    *http.Client
}

// New returns a Prover that proves the venues present in baseURLs (venue id
// to base URL). httpc may be nil; exchangeauth applies its own default.
func New(baseURLs map[string]string, httpc *http.Client) *Prover {
	return &Prover{baseURLs: baseURLs, httpc: httpc}
}

// ProveAccount looks up venue's configured base URL and asks the exchange
// which account keys belongs to, returning the exchange's own account id.
func (p *Prover) ProveAccount(ctx context.Context, venue string, keys secrets.VenueKeys) (string, error) {
	base, ok := p.baseURLs[venue]
	if !ok {
		return "", ErrNoEndpoint
	}
	cred := exchangeauth.Credential{APIKey: keys.APIKey, APISecret: keys.APISecret, Passphrase: keys.Passphrase}
	return exchangeauth.AccountID(ctx, venue, cred, exchangeauth.Options{BaseURL: base, HTTPClient: p.httpc})
}
