package arch

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"path/filepath"

	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/platform/subject"
)

// A SUBSCRIPTION THE BROKER DENIES IS A FEATURE THAT SILENTLY DOES NOTHING (#787).
//
// nats_service_permissions_test.go derives each service's PUBLISH set from code
// and fails the build when tenancy.yaml does not allow it. There was no
// equivalent for SUBSCRIBE, and the two failures are not symmetric — the
// subscribe side is the worse one.
//
// A denied publish errors on every attempt: the code sees it, the DLQ sees it,
// somebody is paged. A denied SUBSCRIBE returns no messages, which is
// indistinguishable from a quiet feed. The pod authenticates, reports Ready,
// logs "subscribing to ..." and folds nothing forever. Every derived control
// then degrades to its own honest-looking refusal — the compliance monitor would
// report "leverage cannot be verified: cash is unknown" for every portfolio on
// the estate, which is exactly what it reports when accounting has genuinely
// never announced.
//
// THIS WAS NOT HYPOTHETICAL. #787 added two subscriptions to the compliance
// service and its tenancy.yaml allow-list named neither. The full arch suite
// passed on that state.
//
// WHAT IT CHECKS: for each service below, every subject it subscribes to is
// allowed by its own account's subscribe list. The expected subjects come from
// the CONSTANTS THE CODE SUBSCRIBES WITH — cashview.Subject, mark.DefaultSubjects,
// subject.PositionAll — never from strings retyped here, so the guard cannot
// drift from the code it is guarding. That is the same stance the publish-side
// guard takes and the reason it is worth having.
//
// THIS GUARD STAYS ALONGSIDE THE AST-DERIVED ONE (#788), which needs justifying
// rather than assuming. nats_subscribe_permissions_test.go now derives subscriptions from
// the AST for every service, so most of what this file used to be the only cover
// for is covered better there. Two things keep it:
//
//   - IT NAMES SUBJECTS THE RESOLVER CANNOT REACH. compliance's price
//     subscription comes from cfg.PriceSubjects, defaulted from
//     mark.DefaultSubjects in a SHARED package — four hops from a literal, and
//     outside the per-service tree the AST guard walks. The general guard
//     resolves one compliance subject; this file asserts all four.
//   - IT IS A REQUIREMENT, NOT A PERMISSION CHECK. The AST guard asks "is what
//     this service subscribes to allowed"; a service that stops subscribing
//     satisfies it vacuously. This file says these subjects MUST be granted,
//     which is the closest thing in the tree to "the pre-trade and post-trade
//     halves of one control read the same feeds".
//
// It is deliberately small and hand-written for exactly that reason. Do not grow
// it into a second copy of the general guard: a subject the resolver CAN reach
// belongs there, not here.
func TestEverySubscribedSubjectIsGranted(t *testing.T) {
	// PARSED BY THE HELPER THE PUBLISH-SIDE GUARD ALREADY USES. A second scanner
	// over this hand-written file is how the two directions would come to disagree
	// about the shape of the same user entry.
	root := moduleRoot(t)
	grants := serviceSubscribeAllow(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(grants) == 0 {
		t.Fatal("no subscribe permission blocks parsed from tenancy.yaml — this guard verified " +
			"NOTHING. The permissions block format changed; fix the parser rather than relaxing " +
			"the check")
	}

	// EXPECTED SETS COME FROM THE CODE'S OWN CONSTANTS.
	want := map[string][]string{
		// The post-trade monitor: holdings, then the two folds #787 added so a book
		// can be valued rather than merely counted.
		"compliance": append([]string{subject.PositionAll, cashview.Subject}, mark.DefaultSubjects...),
		// The pre-trade gate folds the same two, for the same reason one layer
		// earlier. Listing it here is what makes "both halves value a book off the
		// same feed" a build failure rather than a comment.
		"oms": append([]string{cashview.Subject}, mark.DefaultSubjects...),
	}

	var problems []string
	for service, subjects := range want {
		// The account is keyed by the workload's SVID, which SPIRE templates from
		// its namespace and ServiceAccount — the same string tenantgen derives for
		// a per-tenant render. Building it here rather than retyping the URI keeps
		// one spelling of the convention.
		svid := "spiffe://kanz.internal/ns/kanz-services/sa/" + service
		perm, ok := grants[svid]
		allowed := perm.allow
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"tenancy.yaml has no subscribe permissions for %q — a service with no account "+
					"grant does not fail loudly, it receives nothing", service))
			continue
		}
		for _, subj := range subjects {
			if !subjectAllowed(subj, allowed) {
				problems = append(problems, fmt.Sprintf(
					"%s subscribes to %q and its tenancy.yaml account does not allow it. The pod will "+
						"authenticate, report Ready, and receive NOTHING on that subject — which reads "+
						"as a quiet feed rather than a missing grant. Allowed: %s",
					service, subj, strings.Join(allowed, ", ")))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("subscriptions the broker would deny:\n  - %s", strings.Join(problems, "\n  - "))
	}
}

// subjectAllowed reports whether one concrete subject is covered by a grant
// list, honouring NATS wildcards in the GRANT (never in the subject).
//
// `*` matches exactly one token and `>` matches one or more trailing tokens —
// the same semantics the broker applies. Implementing it here rather than
// comparing strings is what lets a grant of "risk.position.changed.>" cover the
// subject the monitor actually binds, and what stops a grant of
// "market.*.trade" being read as covering "market.book.snapshot".
func subjectAllowed(subj string, grants []string) bool {
	for _, g := range grants {
		if subjectMatches(g, subj) {
			return true
		}
	}
	return false
}

func subjectMatches(grant, subj string) bool {
	if grant == subj {
		return true
	}
	gt := strings.Split(grant, ".")
	st := strings.Split(subj, ".")
	for i, g := range gt {
		if g == ">" {
			// `>` is only a wildcard as the final token, and it needs at least one
			// token to match.
			return i == len(gt)-1 && len(st) > i
		}
		if i >= len(st) {
			return false
		}
		if g != "*" && g != st[i] {
			return false
		}
	}
	return len(gt) == len(st)
}
