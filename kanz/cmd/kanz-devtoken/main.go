// Command kanz-devtoken mints an HS256 bearer token for a LOCAL api-gateway.
//
// The gateway refuses to start unauthenticated (SEC-M1): with no OIDC issuer and
// no JWT secret it exits 2 rather than serving /v1/* — POST /v1/orders included —
// to anyone. So a local gateway runs the bundled dev validator against a shared
// secret (API_GATEWAY_JWT_SECRET, plus API_GATEWAY_ALLOW_DEV_HS256=true — since
// #242 that arm is not a valid configuration without the explicit opt-in, so it
// cannot be reached merely by leaving the OIDC issuer unset), and every caller
// needs a token it will accept:
//
//	export TOKEN=$(kanz-devtoken --secret dev-secret --tenant acme --role kanz-user)
//	curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/exposure?portfolio=PF1
//
// A read token cannot trade (SEC-M2): POST /v1/orders needs API_GATEWAY_TRADE_ROLE.
// The --role must be the one the gateway requires (API_GATEWAY_REQUIRED_ROLE), or
// every route answers 403 — authenticated, and authorized for nothing.
//
// AND A TRADE ROLE ALONE IS STILL NOT ENOUGH TO PLACE AN ORDER. The role says the
// caller may trade; --portfolio says WHAT they may trade, and the OMS treats an
// empty entitlement as no entitlement (#225), so submit, cancel and amend all
// come back NOT_ENTITLED without it:
//
//	export TOKEN=$(kanz-devtoken --tenant acme --role kanz-user,kanz-trader --portfolio PF1)
//
// That refusal is loud on the first command by design. Reads are unaffected.
//
// THIS IS NOT A PRODUCTION CREDENTIAL PATH, and it cannot become one: production
// identity is OIDC/JWKS against Eighred SSO, where kanz holds no signing key and
// mints nothing. A shared HMAC secret is symmetric — every holder can forge every
// other holder's token — which is exactly why it is confined to a developer's
// laptop and a load harness. The tool is standalone and is built into no service
// image.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/devtoken"
)

func main() {
	var (
		secret  = flag.String("secret", os.Getenv("API_GATEWAY_JWT_SECRET"), "HS256 secret the gateway validates against (default $API_GATEWAY_JWT_SECRET)")
		subject = flag.String("subject", "dev-user", "token subject (sub)")
		tenant  = flag.String("tenant", "", "tenant the caller acts as — the RLS scope every query runs under (required)")
		roles   = flag.String("role", "kanz-user", "comma-separated roles; must include API_GATEWAY_REQUIRED_ROLE or every route 403s. To TRADE, add API_GATEWAY_TRADE_ROLE too (SEC-M2): --role kanz-user,kanz-trader")
		pfs     = flag.String("portfolio", "", "comma-separated portfolio ids this caller may trade. REQUIRED TO PLACE OR PULL AN ORDER: the OMS denies an empty entitlement, so without it submit, cancel and amend all answer NOT_ENTITLED (#225). Reads and non-order routes do not need it.")
		ttl     = flag.Duration("ttl", time.Hour, "how long the token is valid")
	)
	flag.Parse()

	tok, err := devtoken.Mint(*secret, devtoken.Claims{
		Subject:    *subject,
		Tenant:     *tenant,
		Roles:      splitRoles(*roles),
		Portfolios: splitRoles(*pfs),
		TTL:        *ttl,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}
	fmt.Println(tok)
}

func splitRoles(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
