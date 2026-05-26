package auth

import (
	"context"
	"errors"
	"strings"
)

// ClaimIssuers is the principal claim listing additional command-issuer
// identities the principal may assume beyond itself (verbatim "{type}:{id}"
// strings, e.g. "strategy:momentum-v2", "operator:risk-desk"). Absent ⇒ the
// principal may issue commands only as itself.
const ClaimIssuers = "issuers"

// ErrForgedIssuer is returned when a command's declared issuer is not one the
// authenticated principal is permitted to issue under.
var ErrForgedIssuer = errors.New("auth: command issuer not authorized for principal")

// VerifyIssuer enforces the forged-issuer guard (AUTH-01c): the command's
// issuer must resolve to the authenticated principal. The issuer wire form is
// "{type}:{id}" (command.v1). A principal may issue:
//
//   - as ITSELF — any issuer whose id-part equals the principal's Subject,
//     regardless of the type prefix (user:/operator:/...). The id is the
//     authenticated identity; it is what cannot be forged.
//   - as a DELEGATED identity it was granted — an issuer string listed verbatim
//     in the principal's `issuers` claim (e.g. a strategy it operates).
//
// Anything else is forgery. Deny-by-default: a nil principal or empty issuer is
// rejected.
func VerifyIssuer(p *Principal, issuer string) error {
	if p == nil {
		return ErrUnauthenticated
	}
	if issuer == "" {
		return errors.New("auth: command issuer required")
	}
	if p.Subject != "" && issuerID(issuer) == p.Subject {
		return nil
	}
	for _, allowed := range rolesClaim(p.Claims[ClaimIssuers]) {
		if allowed == issuer {
			return nil
		}
	}
	return ErrForgedIssuer
}

// VerifyCommandIssuer adapts VerifyIssuer to the bus.CommandIssuerFunc shape,
// pulling the authenticated principal off ctx — wire it into
// bus.ProducerConfig.VerifyCommandIssuer. A request with no principal on ctx
// (never authenticated) cannot issue a command.
func VerifyCommandIssuer(ctx context.Context, issuer string) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	return VerifyIssuer(p, issuer)
}

// issuerID returns the id portion of a "{type}:{id}" issuer (the whole string
// when there is no ":").
func issuerID(issuer string) string {
	if i := strings.IndexByte(issuer, ':'); i >= 0 {
		return issuer[i+1:]
	}
	return issuer
}
