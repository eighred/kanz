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

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/services/operator/internal/secrets"
)

// ErrNoEndpoint is returned when proof is enabled but the requested venue has
// no base URL configured. Proof is deploy-time configuration only — there is
// no default endpoint, so a venue that isn't configured cannot be proved and
// its write must be refused, never written unproven.
var ErrNoEndpoint = errors.New("venueproof: no exchange endpoint configured for this venue")

// ErrNoOKXTradingMode is returned when an OKX key is proved without a stated
// trading mode. It is the same policy as ErrNoEndpoint, applied to the axis the
// endpoint cannot express (#147): OKX serves demo and production from the same
// host and separates them by the `x-simulated-trading: 1` request header, so a
// base URL alone cannot say which account this key is being proved against.
//
// Demo and live are DIFFERENT OKX accounts with different uids. Proving against
// the wrong one is not a near miss — it writes a credential whose verified uid
// belongs to a book the orders will never reach, which is a proof that has been
// performed and means nothing. Refuse the write instead.
var ErrNoOKXTradingMode = errors.New("venueproof: OKX key proof requires a trading mode (demo or live) — " +
	"the base URL cannot distinguish them")

// Prover asks an exchange which account a candidate key set belongs to,
// scoped to the venues present in baseURLs.
type Prover struct {
	baseURLs map[string]string
	okxMode  exchangeauth.OKXTradingMode
	httpc    *http.Client
}

// New returns a Prover that proves the venues present in baseURLs (venue id
// to base URL). httpc may be nil; exchangeauth applies its own default.
//
// okxMode is required only to prove an OKX key and is ignored for every other
// venue — an operator that never proves OKX keys needs no new configuration, and
// one that does gets a refusal rather than a guess. Empty is a legitimate value
// here for exactly that reason; it becomes an error at ProveAccount, where it is
// known whether OKX is actually in play.
func New(baseURLs map[string]string, okxMode exchangeauth.OKXTradingMode, httpc *http.Client) *Prover {
	return &Prover{baseURLs: baseURLs, okxMode: okxMode, httpc: httpc}
}

// ProveAccount looks up venue's configured base URL and asks the exchange
// which account keys belongs to, returning the exchange's own account id.
func (p *Prover) ProveAccount(ctx context.Context, venue string, keys secrets.VenueKeys) (string, error) {
	base, ok := p.baseURLs[venue]
	if !ok {
		return "", ErrNoEndpoint
	}
	if venue == "okx" && p.okxMode == "" {
		return "", ErrNoOKXTradingMode
	}
	cred := exchangeauth.Credential{APIKey: keys.APIKey, APISecret: keys.APISecret, Passphrase: keys.Passphrase}
	return exchangeauth.AccountID(ctx, venue, cred, exchangeauth.Options{
		BaseURL:    base,
		HTTPClient: p.httpc,
		OKXTrading: p.okxMode,
	})
}
