package custody

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// BookScope decides WHICH SLICE of a portfolio's journal one custodian's
// reconciliation compares against (#1006).
//
// custody.Subject is (portfolio, custodian, business date) and every part of this
// control was custodian-aware except the book side: it loaded the WHOLE portfolio
// and handed it to recon.Reconcile against ONE custodian's statement. For a
// portfolio custodied in two places — which ACCOUNTING_CUSTODY_PAIRS accepts and
// the Scheduler is built to iterate — custodian A's run reports every position
// held at B as MISSING_AT_CUSTODIAN, and B's run reports every position held at A
// the same way. Every position becomes a break, twice, and the one break that
// means a fill never reached the ledger is buried in it. EXPLAINED counts as
// outstanding by design, so the noise ages, pages via CustodyBreakAgeing, and
// trains an operator to ignore the queue.
//
// THE CUSTODIAN IS DERIVED, NOT STORED. A ledger entry already carries the
// exchange account it settled against; this maps those accounts to custodians.
// See ledger.MaterializeForAccounts for why that beats putting a custodian_id on
// ledger.Event.
//
// A ZERO BookScope IS THE PRE-#1006 BEHAVIOUR, and that is deliberate rather than
// a fallback: for a portfolio with ONE custodian, the whole book against that
// custodian is correct, needs no declaration, and must not move. Only the
// configuration that was wrong changes.
type BookScope struct {
	// custodians is portfolio -> the custodians configured for it, sorted.
	custodians map[string][]string
	// accounts is portfolio -> custodian -> the exchange accounts it holds.
	accounts map[string]map[string]ledger.AccountScope
	// claimed is portfolio -> every account any of its custodians claims, which
	// is what makes an unclaimed account detectable rather than merely absent.
	claimed map[string]ledger.AccountScope
}

// NewBookScope builds the scope from the configured pairs and the account
// declaration, refusing every configuration that would reconcile against a book
// it cannot justify.
//
// THE REFUSALS ARE THE POINT. Each one is a state that would otherwise produce a
// confident wrong answer at the first tick rather than a failure at startup:
//
//   - a portfolio with TWO OR MORE custodians and no accounts declared for one of
//     them — the defect this type exists to remove, arriving as a config change
//   - an account claimed by two custodians of one portfolio — the holding would be
//     counted at both, so one of them cannot break when it should
//   - a declaration naming a (portfolio, custodian) that is not a configured pair —
//     a typo that silently scopes nothing
//
// accounts is portfolio -> custodian -> account ids.
func NewBookScope(pairs []Subject, accounts map[string]map[string][]string) (*BookScope, error) {
	s := &BookScope{
		custodians: map[string][]string{},
		accounts:   map[string]map[string]ledger.AccountScope{},
		claimed:    map[string]ledger.AccountScope{},
	}
	for _, p := range pairs {
		s.custodians[p.PortfolioID] = append(s.custodians[p.PortfolioID], p.CustodianID)
	}
	for _, cs := range s.custodians {
		sort.Strings(cs)
	}

	for portfolio, byCustodian := range accounts {
		known := s.custodians[portfolio]
		if len(known) == 0 {
			return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS declares accounts for portfolio %q, "+
				"which is not in ACCOUNTING_CUSTODY_PAIRS — the declaration would scope nothing", portfolio)
		}
		for custodian, ids := range byCustodian {
			if !slices.Contains(known, custodian) {
				return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS declares accounts for %s:%s, "+
					"which is not a configured pair (portfolio %q reconciles against %s)",
					portfolio, custodian, portfolio, strings.Join(known, ", "))
			}
			scope, err := ledger.NewAccountScope(ids...)
			if err != nil {
				return nil, fmt.Errorf("custody: %s:%s: %w", portfolio, custodian, err)
			}
			if len(scope) == 0 {
				return nil, fmt.Errorf("custody: %s:%s declares an EMPTY account set — its book would fold "+
					"to nothing and every position the custodian holds would break as MISSING_IN_IBOR",
					portfolio, custodian)
			}
			if s.accounts[portfolio] == nil {
				s.accounts[portfolio] = map[string]ledger.AccountScope{}
				s.claimed[portfolio] = ledger.AccountScope{}
			}
			for id := range scope {
				if s.claimed[portfolio][id] {
					return nil, fmt.Errorf("custody: exchange account %q is claimed by more than one custodian "+
						"of portfolio %s — the holding would be counted at both, so neither can break when it "+
						"should", id, portfolio)
				}
				s.claimed[portfolio][id] = true
			}
			s.accounts[portfolio][custodian] = scope
		}
	}

	// The refusal this type exists for: two custodians, and nothing saying which
	// holds what.
	for portfolio, cs := range s.custodians {
		if len(cs) < 2 {
			continue
		}
		for _, custodian := range cs {
			if len(s.accounts[portfolio][custodian]) == 0 {
				return nil, fmt.Errorf("custody: portfolio %s reconciles against %d custodians (%s) but "+
					"ACCOUNTING_CUSTODY_ACCOUNTS declares no exchange accounts for %s.\n\n"+
					"Without it every run compares the WHOLE portfolio book against ONE custodian's "+
					"statement, so each custodian's run reports every position held at the other as "+
					"MISSING_AT_CUSTODIAN — the entire book becomes breaks, twice, and a real break is "+
					"buried in the noise. Declare the accounts EVERY custodian of this portfolio holds "+
					"— entries separated by whitespace, one custodian's accounts by comma — and replace "+
					"each placeholder with the real exchange account ids:\n"+
					"ACCOUNTING_CUSTODY_ACCOUNTS=%s\n",
					portfolio, len(cs), strings.Join(cs, ", "), custodian, s.declTemplate(portfolio, cs))
			}
		}
	}
	return s, nil
}

// declTemplate renders the ACCOUNTING_CUSTODY_ACCOUNTS value that would LIFT the
// refusal above, in the syntax ParseCustodyAccounts actually accepts.
//
// IT IS COMPLETE, AND THAT IS THE POINT (#1029). The message used to end with a
// one-entry example naming a single account of a single custodian — printed by a
// refusal that fires precisely because the portfolio has two, so the example
// covered one of them. An operator who applied it literally got the SAME exit 2
// naming the other custodian: the remediation did not produce a start, which no
// unit test of the parser can see because the syntax was never the wrong part.
//
// Every custodian of the portfolio appears here, and the ones already declared
// keep the accounts they were declared with rather than being overwritten by a
// placeholder.
func (s *BookScope) declTemplate(portfolio string, custodians []string) string {
	entries := make([]string, 0, len(custodians))
	for _, c := range custodians {
		ids := make([]string, 0, len(s.accounts[portfolio][c]))
		for id := range s.accounts[portfolio][c] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			// Named after the custodian so two undeclared custodians do not both
			// claim one placeholder id — which the refusal above would reject as
			// an account counted at both.
			ids = []string{"<accounts-held-at-" + c + ">"}
		}
		entries = append(entries, portfolio+":"+c+":"+strings.Join(ids, ","))
	}
	return strings.Join(entries, " ")
}

// Scoped reports whether this portfolio's book is folded per custodian. False
// means the whole book is compared, which is correct for a single custodian and
// is what NewBookScope guarantees is the only case that reaches it.
func (s *BookScope) Scoped(portfolio string) bool {
	return s != nil && len(s.accounts[portfolio]) > 0
}

// Custodians returns the custodians configured for a portfolio, sorted, or nil
// when this deployment reconciles that portfolio against none.
//
// IT EXISTS SO A CALLER CAN REFUSE A CUSTODIAN IT CANNOT PLACE (#1025). The
// ad-hoc reconcile endpoint takes the custodian from the REQUEST, and Scoped
// cannot check it: a single-custodian portfolio is deliberately unscoped, so
// there a wrong custodian id is indistinguishable from the right one and the
// only book the handler could answer with is the WHOLE portfolio — #1006's
// defect served with a 200. A caller that cannot place the name must refuse
// rather than compare something else.
func (s *BookScope) Custodians(portfolio string) []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.custodians[portfolio])
}

// For returns the accounts one custodian holds, and every account any custodian
// of that portfolio claims. The second is what turns an account nobody declared
// into a refusal instead of a silently missing holding.
func (s *BookScope) For(portfolio, custodian string) (scope, claimed ledger.AccountScope) {
	if s == nil {
		return nil, nil
	}
	return s.accounts[portfolio][custodian], s.claimed[portfolio]
}

// ParseCustodyAccounts parses the WHOLE ACCOUNTING_CUSTODY_ACCOUNTS declaration:
//
//	ACCOUNTING_CUSTODY_ACCOUNTS=PF1:CUST-A:okx-sub-1,okx-sub-2 PF1:CUST-B:bin-main
//
// ENTRIES ARE SEPARATED BY WHITESPACE, a custodian's ACCOUNTS BY COMMA, one entry
// per (portfolio, custodian).
//
// IT TAKES THE RAW ENVIRONMENT VALUE BECAUSE THE TWO LEVELS OF THIS GRAMMAR MUST
// HAVE ONE OWNER (#1029). The outer split used to live in config as
// env.SplitList, which splits on comma — the same character that separates one
// custodian's accounts. A custodian holding two accounts therefore had its entry
// cut in half: the value "PF1:CUST-A:okx-sub-1,okx-sub-2 PF1:CUST-B:bin-main",
// written with commas throughout as that split required, arrived here as three
// entries rather than two, the middle one a bare "okx-sub-2", and the pod exited
// 2 naming a fragment the operator never typed.
//
// #1006's custodian scoping was then reachable ONLY for single-account
// custodians, which is not the shape an institutional portfolio is in: the repair
// was unreachable for the case it was written for. A grammar split in one package
// and interpreted in another has no owner; this one has.
//
// Whitespace is the outer separator because no portfolio, custodian or exchange
// account id contains any, so the two levels cannot collide again — and because
// it lets a manifest write one entry per line in a YAML block scalar instead of
// one unreadable run.
//
// A MALFORMED ENTRY IS AN ERROR AND NOT A SKIP, exactly as parseCustodyPairs
// treats a malformed pair: dropping one would un-scope a custodian while the
// service reported a healthy start, which is the defect this declaration exists
// to prevent, reintroduced by the parser.
func ParseCustodyAccounts(decl string) (map[string]map[string][]string, error) {
	out := map[string]map[string][]string{}
	seen := map[string]bool{}
	// Fields, not Split: an operator writes this across lines in a manifest, and
	// leading indentation must not become an empty entry.
	for _, entry := range strings.Fields(decl) {
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS entry %q is not "+
				"\"portfolio:custodian:account[,account...]\".\n\n%s", entry, entryHint(entry))
		}
		portfolio, custodian := parts[0], parts[1]
		if portfolio == "" || custodian == "" {
			return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS entry %q names an empty "+
				"portfolio or custodian", entry)
		}
		key := portfolio + "|" + custodian
		if seen[key] {
			return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS names %s twice — one of the two "+
				"account sets would win silently", key)
		}
		seen[key] = true

		var ids []string
		for _, id := range strings.Split(parts[2], ",") {
			if id == "" {
				return nil, fmt.Errorf("custody: ACCOUNTING_CUSTODY_ACCOUNTS entry %q has an empty account id", entry)
			}
			ids = append(ids, id)
		}
		if out[portfolio] == nil {
			out[portfolio] = map[string][]string{}
		}
		out[portfolio][custodian] = ids
	}
	return out, nil
}

// entryHint names the SEPARATOR MISTAKE behind a malformed entry, because the two
// that #1029 makes likely are both invisible in the fragment the operator is
// shown: they never wrote "okx-sub-2", the splitter did.
func entryHint(entry string) string {
	switch {
	case !strings.Contains(entry, ":"):
		return "It has no ':' at all, so it is the tail of an account list that was split. " +
			"ENTRIES ARE SEPARATED BY WHITESPACE and a custodian's ACCOUNTS BY COMMA — write " +
			"\"PF1:CUST-A:okx-sub-1,okx-sub-2\" with no space after the comma."
	case strings.Count(entry, ":") > 2:
		return "It has more than two ':', so two entries ran together. ENTRIES ARE SEPARATED BY " +
			"WHITESPACE, not by comma — the comma separates one custodian's ACCOUNTS. Write " +
			"\"PF1:CUST-A:okx-sub-1 PF1:CUST-B:bin-main\"."
	default:
		return "An entry names a portfolio, a custodian and at least one exchange account, e.g. " +
			"\"PF1:CUST-A:okx-sub-1,okx-sub-2\"."
	}
}
