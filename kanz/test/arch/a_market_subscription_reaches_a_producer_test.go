package arch

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A SUBSCRIBE GRANT OVER A SUBJECT SPACE NOBODY FILLS IS A FEED THAT IS ALWAYS
// QUIET (#955).
//
// nats_subscribe_permissions_test.go asks "is what this service subscribes to
// ALLOWED". That is the #787 question, and it is only half of the pair. A grant
// can be present, correct, and worth nothing: the `acme` account granted
// oms-acme a subscribe on market.*.trade and market.*.quote while NOTHING inside
// that account published a market subject and the account imported none. The pod
// authenticated, reported Ready, logged "oms subscribing to the price spine
// (broadcast)" for both subjects, and folded an empty mark source for the life
// of the deployment.
//
// EVERY DOWNSTREAM CONTROL THEN DEGRADED TO ITS OWN HONEST-LOOKING REFUSAL.
// Every MARKET and STOP order was refused PRICE_UNAVAILABLE — which is
// fail-closed working exactly as designed, and reads identically to a desk whose
// instrument genuinely has no price. stampArrival recorded no arrival mark, so
// nothing that tenant traded was measurable. And both #875 coverage gauges sat
// at zero, which is the ONE reading OMSQuoteCoverageAbsent cannot fire on: it
// requires kanz_oms_mark_instruments_live > 0 to tell a dead feed from a cold
// pod, so the alert built to detect a price-spine gap was structurally silent in
// the worst version of it.
//
// # The rule
//
// For every account, every subject a user is granted to SUBSCRIBE inside the
// `market.` domain must be reachable in that account: either a user in the SAME
// account is granted to PUBLISH a subject space covering it, or the account
// IMPORTS one.
//
// Scoped to `market.` deliberately, and not widened on sight. Most domains fail
// loudly when they are empty — an order that never arrives is an order nobody
// placed, a fill that never arrives is a fill that did not happen. The price
// spine is the one whose absence is INDISTINGUISHABLE from a market with no
// prices, because "unknown" is a legitimate answer the gate acts on. Widening
// this to every domain is a separate calibration, not a free improvement: run
// the survey first, because a guard that fires on thirty pre-existing states
// gets an exemption map instead of a repair.
//
// # Why the accounts are the unit
//
// NATS accounts are isolated by construction, so "who publishes this" is only
// ever answerable WITHIN one account. A producer in __system__ and a subscriber
// in `acme` are as unrelated as two brokers until an export/import pair says
// otherwise — which is why the import arm is not a courtesy, it is the other
// half of the same question.
func TestEveryMarketSubscriptionReachesAProducer(t *testing.T) {
	const domain = "market."

	entries := natsAccountUserEntries(t)
	blocks := natsAccountBlocks(t)

	// NON-VACUITY, BOTH SIDES. A parse that returned no accounts, or accounts
	// with no users, would report every account clean after reading nothing —
	// which is the shape of the defect this file exists to catch.
	if len(entries) < 2 {
		t.Fatalf("parsed %d account(s) with users from tenancy.yaml — this guard is blind", len(entries))
	}
	if len(blocks) < 2 {
		t.Fatalf("parsed %d account block(s) from tenancy.yaml — the import arm of this guard is blind",
			len(blocks))
	}

	var problems []string
	checked, exempted := 0, map[string]bool{}

	for _, account := range sortedKeys(entries) {
		users := entries[account]

		// The account's own producers: the union of every user's publish
		// allow-list. Union, not per-user, because the question is whether a
		// message can EXIST on the subject inside this account, and any member
		// may be the one putting it there.
		var producers []string
		for _, svid := range sortedKeys(users) {
			producers = append(producers, parsePublishPerm(users[svid]).allow...)
		}
		lands := importLandingSubjects(blocks[account])

		for _, svid := range sortedKeys(users) {
			for _, subj := range parseSubscribePerm(users[svid]).allow {
				if !strings.HasPrefix(subj, domain) {
					continue
				}
				key := account + "/" + shortSVID(svid) + "/" + subj
				if _, ok := marketSubscriptionWithoutAProducer[key]; ok {
					exempted[key] = true
					continue
				}
				checked++
				if coveredBy(producers, subj) || coveredBy(lands, subj) {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"account %q grants %s a SUBSCRIBE on %q, and no user in that account publishes "+
						"it and the account imports nothing that lands on it. The pod authenticates, "+
						"reports Ready, subscribes, and folds NOTHING — for as long as the deployment "+
						"lives. Publishers in this account: %s. Import landings: %s",
					account, shortSVID(svid), subj,
					joinOrNone(marketOnly(producers)), joinOrNone(marketOnly(lands))))
			}
		}
	}

	// NON-VACUITY, ONCE MORE. Every market grant being exempt would leave this
	// asserting nothing while still passing.
	if checked == 0 {
		t.Fatal("no market.* subscribe grant was actually checked — every one is exempt, or the " +
			"permissions parse stopped finding them. Either way this guard is watching nothing")
	}

	// A DEAD EXEMPTION IS A REPAIR NOBODY NOTICED. Left standing, it silently
	// re-permits the defect the day the subject comes back.
	for key, why := range marketSubscriptionWithoutAProducer {
		if !exempted[key] {
			problems = append(problems, fmt.Sprintf(
				"marketSubscriptionWithoutAProducer holds %q, and tenancy.yaml no longer has that "+
					"grant (or it now has a producer). Delete the entry — a standing exemption "+
					"re-permits the gap the moment the grant returns. Reason on file: %s", key, why))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("subscriptions over an empty subject space:\n  - %s", strings.Join(problems, "\n  - "))
	}
	t.Logf("checked %d market.* subscribe grant(s) across %d account(s); %d exempt",
		checked, len(entries), len(exempted))
}

// marketSubscriptionWithoutAProducer are the grants that are KNOWN to reach
// nothing, each carrying the issue that retires it. Keyed
// `account/serviceaccount/subject`.
//
// #955 imported the PRICE SPINE — mark.DefaultSubjects, the two subjects a
// tenant OMS values an order from — and deliberately no more. The rest of the
// `market.` domain crossing the tenant boundary is a separate decision with its
// own cost (stream quota, retention, and in the risk engine's case a whole-domain
// wildcard), and it is tracked in #959 rather than smuggled in beside the fix
// for the order path.
var marketSubscriptionWithoutAProducer = map[string]string{
	"acme/risk-engine/market.>": "#959 — the tenant's own risk engine mirrors the platform " +
		"entry's whole-domain grant (RISK_ENGINE_MARKET_SUBJECTS defaults to market.>), so its " +
		"calibration cache folds nothing. Narrowing it here would break the platform-parity guard; " +
		"the fix is a decision about which of market.> a tenant sees.",
	"acme/risk-engine-acme/market.>": "#959 — the MT-02 compute copy of the entry above, with " +
		"identical grants by design.",
	"acme/oms-acme/market.crypto.volume_profile": "#959 — #897's intraday volume profile. Its " +
		"absence refuses this tenant's VWAP and POV orders while the platform tenant's are " +
		"admitted, which is #955 one subject over.",
	"acme/accounting-acme/market.fx.>": "#959 — the FX revaluation spine. Without it this " +
		"tenant's non-base-currency cash is revalued from nothing.",
}

// natsAccountBlocks returns each account's block body from tenancy.yaml, with
// COMMENTS STRIPPED FIRST.
//
// accountBlocks (tenant_bridge_test.go) reads the raw file, which is safe for
// what it looks for and is not safe here: this file argues about subjects in
// prose at length, and three guards in this package have already passed while
// the thing they checked was deleted, because a scan matched their own
// commentary. The permissions side (natsAccountUserEntries) strips for exactly
// that reason; the import side has to as well or the two halves are reading
// different documents.
func natsAccountBlocks(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "nats", "tenancy.yaml")
	var cm tenantConfigMap
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &cm); err != nil {
		t.Fatalf("parse %s as YAML: %v", path, err)
	}
	conf, ok := cm.Data["tenants.conf"]
	if !ok {
		t.Fatalf(`%s has no data["tenants.conf"] key — has the ConfigMap shape changed?`, path)
	}
	conf = stripYAMLComments(strings.ReplaceAll(conf, "\r\n", "\n"))

	idx := strings.Index(conf, "accounts")
	if idx < 0 {
		t.Fatalf("%s: tenants.conf has no `accounts` block — has the format changed?", path)
	}
	open := strings.IndexByte(conf[idx:], '{')
	if open < 0 {
		t.Fatalf("%s: the `accounts` block has no opening brace — has the format changed?", path)
	}

	out := map[string]string{}
	account, start := "", -1
	depth := 1 // positioned just past `accounts {`
	for i := idx + open + 1; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
			if depth == 2 {
				account, start = trailingIdentifier(conf[:i]), i
			}
		case '}':
			if depth == 2 && start >= 0 && account != "" {
				out[account] = conf[start : i+1]
				account, start = "", -1
			}
			depth--
			if depth == 0 {
				return out
			}
		}
	}
	t.Fatalf("%s: the `accounts` block never closes — the brace walk ran off the end of the file", path)
	return nil
}

// coveredBy reports whether any grant covers the whole subject SPACE named by
// subj — not merely one concrete subject inside it.
//
// subjectAllowed (nats_subscribe_grants_test.go) answers the concrete question:
// is THIS subject permitted by that list. Here both sides may be patterns, and
// the distinction is load-bearing: a producer of `market.*.trade` does not fill
// `market.>`, so reading a subscribe grant as if it were a concrete subject
// would credit a whole-domain subscription to a producer covering one variant of
// it — passing the guard in precisely the state the issue describes.
func coveredBy(grants []string, subj string) bool {
	for _, g := range grants {
		if subjectSpaceCovers(g, subj) {
			return true
		}
	}
	return false
}

// subjectSpaceCovers reports whether the subject space `outer` contains every
// subject the space `inner` admits. Both may carry NATS wildcards.
//
//	market.>        covers market.*.trade    -> true
//	market.*.trade  covers market.*.trade    -> true
//	market.*.trade  covers market.>          -> FALSE, `>` reaches further
//	market.*.trade  covers market.crypto.bar -> false
func subjectSpaceCovers(outer, inner string) bool {
	ot := strings.Split(outer, ".")
	it := strings.Split(inner, ".")
	for i, o := range ot {
		if o == ">" {
			// `>` is a wildcard only as the final token, and it needs at least
			// one token to match.
			return i == len(ot)-1 && len(it) > i
		}
		if i >= len(it) {
			return false
		}
		if o == "*" {
			// A single-token wildcard cannot contain a multi-token one.
			if it[i] == ">" {
				return false
			}
			continue
		}
		// A literal token covers only itself — never `*` or `>`, both of which
		// admit subjects this producer does not publish.
		if o != it[i] {
			return false
		}
	}
	return len(ot) == len(it)
}

// shortSVID reduces a SPIFFE URI to its ServiceAccount, which is what the
// exemption keys and the failure message name. The full URI is unambiguous and
// unreadable in a table of four.
func shortSVID(svid string) string {
	if i := strings.LastIndex(svid, "/"); i >= 0 {
		return svid[i+1:]
	}
	return svid
}

func marketOnly(subjects []string) []string {
	var out []string
	for _, s := range subjects {
		if strings.HasPrefix(s, "market.") {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func joinOrNone(subjects []string) string {
	if len(subjects) == 0 {
		return "(none)"
	}
	return strings.Join(subjects, ", ")
}
