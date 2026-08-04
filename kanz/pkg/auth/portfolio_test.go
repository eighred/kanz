package auth

import "testing"

// THE TWO SEMANTICS ARE ASSERTED SIDE BY SIDE, ON THE SAME INPUTS (#225).
//
// Not "each is tested somewhere". The failure this guards against is a
// well-meaning unification: someone hits the NOT_ENTITLED outage that an absent
// claim produces on the capital path, sees that the read path is permissive on
// empty, and makes them match — which is an authorization bypass across every
// portfolio in the tenant. One table, both columns, so the row that changes is
// the one a reviewer reads.
func TestPortfolioScope_TheTwoSemanticsDifferOnlyOnTheEmptyList(t *testing.T) {
	cases := []struct {
		name        string
		allowed     []string
		portfolio   string
		inScope     bool // READ path — PortfolioInScope
		entitled    bool // CAPITAL path — PortfolioEntitled
		theyDisagre bool
	}{
		// THE ONE ROW THEY DISAGREE ON, and the whole reason both exist.
		{"nil list", nil, "pf-1", true, false, true},
		{"empty list", []string{}, "pf-1", true, false, true},

		// Everywhere else they must give the same answer. A divergence here is
		// not a policy decision, it is a bug in one of them.
		{"listed", []string{"pf-1", "pf-2"}, "pf-1", true, true, false},
		{"listed last", []string{"pf-2", "pf-1"}, "pf-1", true, true, false},
		{"not listed", []string{"pf-2"}, "pf-1", false, false, false},
		{"case sensitive", []string{"PF-1"}, "pf-1", false, false, false},
		{"no substring match", []string{"pf-10"}, "pf-1", false, false, false},

		// An unnamed portfolio is not a portfolio. The read path treats an
		// unrestricted principal as unrestricted regardless; the capital path
		// refuses, because an order that cannot say whose capital it spends
		// cannot be authorized to spend it.
		{"empty portfolio, empty list", nil, "", true, false, true},
		{"empty portfolio, populated list", []string{"pf-1"}, "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PortfolioInScope(tc.allowed, tc.portfolio)
			if got != tc.inScope {
				t.Errorf("PortfolioInScope(%v, %q) = %v, want %v", tc.allowed, tc.portfolio, got, tc.inScope)
			}
			gotE := PortfolioEntitled(tc.allowed, tc.portfolio)
			if gotE != tc.entitled {
				t.Errorf("PortfolioEntitled(%v, %q) = %v, want %v", tc.allowed, tc.portfolio, gotE, tc.entitled)
			}
			if (got != gotE) != tc.theyDisagre {
				t.Errorf("the two semantics %s on (%v, %q) — the table says they should %s. "+
					"If this is deliberate, the argument belongs in portfolio.go before the row changes",
					disagreement(got != gotE), tc.allowed, tc.portfolio, disagreement(tc.theyDisagre))
			}
		})
	}
}

func disagreement(b bool) string {
	if b {
		return "disagree"
	}
	return "agree"
}

// The empty case is the one a hotfix reaches for, so it is asserted on its own
// as well as in the table — with the consequence of flipping either direction
// written into the failure message.
func TestPortfolioScope_EmptyListIsNotTheSameQuestionOnBothPaths(t *testing.T) {
	if !PortfolioInScope(nil, "anything") {
		t.Fatal("PortfolioInScope must PERMIT an empty allow-list. Its only production " +
			"caller is copilot's tool gate, whose principal comes off the mesh headers and " +
			"therefore never carries a portfolio list at all — deny-on-empty refuses every " +
			"governed tool call on the platform, with no configuration that could fix it")
	}
	if PortfolioEntitled(nil, "anything") {
		t.Fatal("PortfolioEntitled must DENY an empty allow-list. The api-gateway stamps the " +
			"authenticated principal's list onto every order command, so empty means the token " +
			"said nothing about portfolios — permitting it authorizes any caller against every " +
			"portfolio in the tenant, on the path that moves capital")
	}
}
