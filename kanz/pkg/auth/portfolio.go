package auth

// THE PORTFOLIO ALLOW-LIST HAS TWO SEMANTICS, ON PURPOSE, AND THEY LIVE HERE
// TOGETHER SO THAT STAYS A DECISION RATHER THAN A DIVERGENCE (#225).
//
// One fact — "which portfolios may this caller reach" — is consulted by two
// paths that must answer an ABSENT list differently:
//
//	PortfolioInScope    READ PATH.    Absent ⇒ every portfolio in the tenant.
//	PortfolioEntitled   CAPITAL PATH. Absent ⇒ nothing.
//
// The two implementations used to sit in different packages with no reference
// between them (pkg/auth/authz.go and services/oms/internal/order/service.go),
// which is how #225 became dangerous: the OIDC bridge dropped the claim, cancel
// and amend started refusing everything, and the shortest repair for that
// outage — make the capital path permissive to match the read path — is an
// authorization bypass across every portfolio in the tenant. Anyone who reaches
// for that fix now has to delete the paragraph below saying no.
//
// WHY THE READ PATH CANNOT SIMPLY ADOPT THE STRICTER RULE. PortfolioInScope has
// exactly one production caller: services/copilot/internal/tools/tools.go's
// Registry.authorize, which passes Resource.Type ResourcePortfolio on every
// governed tool call. Copilot does not authenticate — it receives the caller
// through the mesh identity headers, and PrincipalFromHeaders reconstructs
// subject, tenant and roles ONLY (see the "WHAT IT CANNOT RECONSTRUCT" note on
// meshheader.go). So copilot's allow-list is structurally always empty, and a
// deny-on-empty rule there would refuse every governed tool call on the
// platform, with no configuration that could fix it — the wire has no field to
// carry the claim. lineage's governance.CheckAccess asks about
// Resource.Type ResourceDataset and never reaches this branch at all.
//
// WHY THE CAPITAL PATH CANNOT ADOPT THE LOOSER ONE. The OMS decides whose
// capital an order spends. The api-gateway stamps the authenticated principal's
// list onto every order command it publishes (orders.go's bindMetadata),
// overriding whatever the client sent, so an empty list on that path means the
// token said nothing about portfolios — not that the caller owns them all. On
// the capital path the absence of proof is not proof; it is the absence of
// entitlement, and it is the same stance the gateway takes on an absent tenant.
//
// THE HONEST CONSEQUENCE, STATED. Until the IdP actually issues the claim (#99),
// a production OIDC token carries no portfolios, so every human-issued submit,
// cancel and amend is refused NOT_ENTITLED. That is the loud failure this
// platform prefers to the quiet one, and it is visible on the first command
// rather than after a wrong fill. Service-issued orders are unaffected — see
// the delegated-command note in the OMS.

// PortfolioInScope reports whether a principal restricted to allowed may reach
// portfolio on a READ path. An EMPTY allow-list PERMITS: it means the token
// asserted no portfolio restriction, and the tenant boundary (checked
// separately, and never expressible as a policy grant) is the operative limit.
//
// Do not call this on a path that moves capital. Use PortfolioEntitled.
func PortfolioInScope(allowed []string, portfolio string) bool {
	if len(allowed) == 0 {
		return true
	}
	return portfolioListed(allowed, portfolio)
}

// PortfolioEntitled reports whether a principal scoped to allowed may act on
// portfolio on a CAPITAL path. An EMPTY allow-list DENIES, and so does an empty
// portfolio id — a command that cannot say whose capital it spends cannot be
// authorized to spend it.
//
// This is deliberately the opposite of PortfolioInScope; the file comment above
// argues why, and neither may be changed to match the other.
func PortfolioEntitled(allowed []string, portfolio string) bool {
	if portfolio == "" {
		return false
	}
	return portfolioListed(allowed, portfolio)
}

// portfolioListed is the membership test both semantics share, so the two
// differ in exactly one place — the empty case — and in nothing else.
func portfolioListed(allowed []string, portfolio string) bool {
	for _, p := range allowed {
		if p == portfolio {
			return true
		}
	}
	return false
}
