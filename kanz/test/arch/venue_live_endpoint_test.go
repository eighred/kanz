package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// NO ORDER-PLACING VENUE ADAPTER MAY RUN AGAINST A LIVE EXCHANGE WHILE THE
// POSTGRES STORE IT DEPENDS ON IS UNBACKED.
//
// This is the one ordering error in this platform with an UNRECOVERABLE failure
// mode, and until now it existed only as a paragraph in CLAUDE.md ("M3 must not
// precede the DR-coverage issue in M0"). A paragraph does not fail a build.
//
// The failure it prevents, concretely: a real order is accepted at a real
// exchange, the exchange-order to kanz-order mapping is written to venue_orders
// and the position to the OMS's orders/positions/position_fills — and neither
// store is in any CNPG cluster with WAL archiving. Lose the region and the
// exchange still holds the position while this platform holds no record that it
// was ever placed. There is no reconciliation path from that state, because
// reconciliation is precisely the thing that needed the mapping. Worse, the OMS
// restarts clean against the empty store and logs SweepInterrupted count=0 — the
// identical line a healthy start produces (see drPosture["oms"] below). The
// platform reports normal while holding untracked exposure at an exchange.
//
// A testnet host has the same software failure and no financial one: the orders
// were never real. That asymmetry is the whole rule — this guard does not ban
// live venues and it does not ban unbacked stores, it bans the COMBINATION.
//
// WHAT IS CHECKED, AND WHY BOTH HALVES ARE NEEDED
//
//	manifest env — what the pod is configured with today.
//	Go defaults  — what the pod falls back to when that env is UNSET.
//
// Checking only the manifest leaves the dangerous path uncovered: a Go default
// of "live exchange" turns a deleted env var, a typo'd key or a stripped-down
// dev overlay into a live-money deployment, silently. A default must be the
// SAFE option, because a default is what you get when nobody was paying
// attention. Both halves are read here for that reason.
//
// DEFAULT-DENY ON HOSTS. nonLiveExchangeHosts is an ALLOW-list. Any host not on
// it counts as LIVE, including a brand-new exchange nobody has classified. This
// is deliberately the opposite of a denylist of known-live hosts: a denylist
// answers "is this one of the exchanges we thought of", which is the question
// that passes when someone adds a venue we did not think of.
func TestNoOrderPlacingVenueRunsLiveWhileItsStoreIsUnbacked(t *testing.T) {
	root := moduleRoot(t)

	adapters := orderPlacingVenueAdapters(t, root)
	// NON-VACUITY. A scanner that finds no adapters satisfies every assertion
	// below no matter how many live venues sit on unbacked stores — a broken
	// guard reads exactly like a clean estate, which is the failure this
	// repository keeps paying for.
	if len(adapters) == 0 {
		t.Fatal("found zero services registering venuepb.RegisterVenueAdapterServiceServer — " +
			"the scanner is broken, not the estate (the RPC name changed, or the scan root moved)")
	}

	migrationOwners := map[string]bool{}
	for _, s := range migrationOwningServices(t, root) {
		migrationOwners[s] = true
	}

	// hostUse records, per host, every adapter+file that configures it. It backs
	// the dead-entry check on nonLiveExchangeHosts below: a classification for a
	// host nothing configures is a claim nobody exercises.
	hostUse := map[string][]string{}

	// exposure is one adapter's violation of the invariant, carrying everything a
	// failure message needs to be actionable without a second investigation:
	// which hosts, where they are configured, and which store cannot be restored.
	type exposure struct {
		liveHosts []string // sorted, deduped hostnames
		evidence  []string // "host (path:line)" for each live sighting
		unbacked  []string // stores with neither backup nor exclusion, with reasons
	}
	violations := map[string]exposure{}

	for _, svc := range adapters {
		sightings := configuredExchangeEndpoints(t, root, svc)
		// An adapter whose endpoints cannot be found at all is not "clean" — it
		// is unread. Fail closed rather than report a pass on an empty scan.
		if len(sightings) == 0 {
			t.Errorf("order-placing adapter %q configures no http/ws endpoint in its manifest or its Go source\n\n"+
				"It reaches an exchange somehow, so either the deploy manifest is missing "+
				"(infra/deploy/%s-deploy.yaml) or the endpoint moved somewhere this guard does not read. "+
				"An adapter whose venue endpoint cannot be located cannot be checked against the DR "+
				"posture of its store, and an unchecked money path must not pass.", svc, svc)
			continue
		}

		var ex exposure
		liveSet := map[string]bool{}
		for _, s := range sightings {
			hostUse[s.host] = append(hostUse[s.host], svc+" "+s.where)
			if _, ok := nonLiveExchangeHosts[s.host]; ok {
				continue
			}
			liveSet[s.host] = true
			ex.evidence = append(ex.evidence, s.host+" ("+s.where+")")
		}
		if len(liveSet) == 0 {
			continue
		}
		ex.liveHosts = sortedKeys(liveSet)
		sort.Strings(ex.evidence)

		// The stores a placed order lands in: the adapter's own venue_orders
		// mapping, and the OMS order book every adapter routes through. Either
		// one being unbacked is enough — losing the mapping alone already breaks
		// reconciliation, and losing the OMS book alone already loses the order.
		for _, store := range []string{svc, omsOrderStore} {
			if store == svc && store != omsOrderStore && !migrationOwners[store] {
				// Adapter owns no schema of its own — nothing of its own to lose.
				continue
			}
			p, ok := drPosture[store]
			switch {
			case !ok:
				// Unclassified is not "fine". dr_postgres_coverage_test.go fails
				// on this separately; here it must not be read as covered.
				ex.unbacked = append(ex.unbacked, store+": NOT CLASSIFIED in drPosture — "+
					"nobody has decided whether it is backed up")
			case p.status == drUnresolved:
				ex.unbacked = append(ex.unbacked, store+": "+p.reason)
			}
		}
		if len(ex.unbacked) == 0 {
			continue
		}
		violations[svc] = ex
	}

	// 1. DEAD-ENTRY CHECK on the service exemptions. An exemption that outlives
	//    the exposure it documents is worse than no exemption: it records a
	//    live-money risk that no longer exists, so the next reader budgets for a
	//    repair already done and stops trusting the list.
	for _, svc := range sortedKeys(liveVenueUnbackedExemptions) {
		ex, stillViolating := violations[svc]
		if !stillViolating {
			t.Errorf("exemption for %q is DEAD — it no longer places orders against a live exchange "+
				"on an unbacked store\n\n"+
				"Either the endpoint moved to a sandbox host, the store gained backup coverage, or the "+
				"adapter was removed. Delete the entry from liveVenueUnbackedExemptions and close or "+
				"update issue %s. An exemption must not outlive its repair: left in place it makes a "+
				"resolved risk look open and an open list look untrustworthy.",
				svc, liveVenueUnbackedExemptions[svc].issue)
			continue
		}
		// 2. The exemption records a SPECIFIC exposure. If the live host set has
		//    changed, the reviewed decision no longer describes what is deployed
		//    — a second live endpoint was never signed off just because the first
		//    one was.
		recorded := append([]string(nil), liveVenueUnbackedExemptions[svc].liveHosts...)
		sort.Strings(recorded)
		if strings.Join(recorded, ",") != strings.Join(ex.liveHosts, ",") {
			t.Errorf("exemption for %q records live host(s) %s but the estate now configures %s\n\n"+
				"The exemption covers a reviewed exposure, not the service name. Re-review the change "+
				"and update liveVenueUnbackedExemptions[%q].liveHosts, or point the new endpoint at a "+
				"sandbox host. Evidence: %s",
				svc, strings.Join(recorded, ", "), strings.Join(ex.liveHosts, ", "), svc,
				strings.Join(ex.evidence, ", "))
		}
	}

	// 3. THE INVARIANT ITSELF. Every violation that is not a named, issue-carrying
	//    exemption fails the build.
	for _, svc := range sortedKeys(violations) {
		if _, exempt := liveVenueUnbackedExemptions[svc]; exempt {
			continue
		}
		ex := violations[svc]
		t.Errorf("%s places orders against LIVE exchange host(s) %s while the Postgres store(s) it "+
			"depends on have NO backup and NO exclusion:\n  %s\n\nEvidence: %s\n\n"+
			"A real order accepted at a real exchange is recorded ONLY in those stores. Lose them and "+
			"the exchange still holds the position while this platform holds no record it was ever "+
			"placed — and reconciliation cannot recover it, because reconciliation is what needed the "+
			"record. The OMS then restarts clean and logs SweepInterrupted count=0, indistinguishable "+
			"from a healthy start, so nothing surfaces the loss.\n\n"+
			"Three ways out, in preference order:\n"+
			"  (a) point the endpoint at a sandbox host and add it to nonLiveExchangeHosts with the "+
			"reason it cannot move real assets;\n"+
			"  (b) resolve the DR posture of the store(s) above (issue #60) so drPosture marks them "+
			"drCovered — this is the fix CLAUDE.md sequences M3 behind;\n"+
			"  (c) if the live endpoint is genuinely required before (b) lands, add a named entry to "+
			"liveVenueUnbackedExemptions citing the issue that retires it and stating the real "+
			"exposure. That is a money-path decision and belongs to a human, not to whoever is making "+
			"this test green.",
			svc, strings.Join(ex.liveHosts, ", "), strings.Join(ex.unbacked, "\n  "),
			strings.Join(ex.evidence, ", "))
	}

	// 4. DEAD-ENTRY CHECK on the host allow-list. A host classified non-live that
	//    nothing configures is an unexercised claim — and the allow-list is the
	//    one place where a wrong entry fails OPEN. Classify a host when you point
	//    something at it, never before.
	for _, host := range sortedKeys(nonLiveExchangeHosts) {
		if len(hostUse[host]) == 0 {
			t.Errorf("nonLiveExchangeHosts classifies %q, which no order-placing adapter configures\n\n"+
				"Delete it. This allow-list is the only entry in this guard that fails OPEN — a wrong "+
				"host here turns a live exchange into a pass — so it holds only hosts something "+
				"actually points at, where the classification is checked against a real deployment "+
				"rather than an intention.", host)
		}
	}

	// 5. The excluded read-only exchange clients are CHECKED, not assumed. See
	//    nonOrderPlacingExchangeClients: the whole exclusion rests on "it has no
	//    order endpoint", and that is a property of today's source, not a
	//    permanent fact about the service.
	adapterSet := map[string]bool{}
	for _, s := range adapters {
		adapterSet[s] = true
	}
	for _, svc := range sortedKeys(nonOrderPlacingExchangeClients) {
		if _, err := os.Stat(filepath.Join(root, "services", svc)); err != nil {
			t.Errorf("nonOrderPlacingExchangeClients names %q, which is not a service directory — "+
				"delete the stale entry", svc)
			continue
		}
		if adapterSet[svc] {
			t.Errorf("%s is excluded as read-only market data but now registers a venue adapter "+
				"service — it can place orders\n\n"+
				"Its live exchange hosts stopped being harmless the moment that happened. Delete the "+
				"exclusion; the invariant above applies to it.", svc)
			continue
		}
		if paths := orderPlacementSites(t, root, svc); len(paths) > 0 {
			t.Errorf("%s is excluded as read-only market data but its source contains order-placement "+
				"endpoint(s): %s\n\n"+
				"The exclusion is only sound while the service cannot place an order. It now can, and "+
				"it is configured against live exchange hosts. Delete the exclusion and bring it under "+
				"the invariant.", svc, strings.Join(paths, ", "))
		}
	}

	// 6. Say the accepted exposure out loud on every run. This test passing means
	//    the live-money risk is RECORDED and MACHINE-CHECKED, not that it is gone.
	for _, svc := range sortedKeys(liveVenueUnbackedExemptions) {
		e := liveVenueUnbackedExemptions[svc]
		t.Logf("ACCEPTED LIVE-VENUE EXPOSURE: %s is configured against %s with an unbacked store. "+
			"Tracked by %s. %s", svc, strings.Join(e.liveHosts, ", "), e.issue, e.reason)
	}
}

// omsOrderStore is the OMS's schema — orders, positions, position_fills. Every
// order-placing adapter routes through it, so its DR posture is an input to
// every adapter's exposure, not just its own.
const omsOrderStore = "oms"

// nonLiveExchangeHosts is the DEFAULT-DENY allow-list of exchange hostnames that
// cannot move real assets. A host absent from this map is treated as LIVE.
//
// Entries are hostnames only — no scheme, no port, no path — because the risk
// attaches to the exchange behind the name, not to the transport in front of it.
//
// The bar for an entry: an order accepted at that host settles against fake
// money. "It is a demo account" is NOT sufficient when the hostname is shared
// with production — see the venue-okx exemption, where demo is selected by a
// request header and so cannot be established from configuration at all.
var nonLiveExchangeHosts = map[string]string{
	"testnet.binance.vision": "Binance Spot Testnet — a physically separate exchange with its own " +
		"order book and its own faucet-issued balances. A key valid here is rejected by " +
		"api.binance.com, so a misdirected order fails rather than fills.",
}

// liveVenueExemption is a reviewed, issue-carrying acceptance of live-money
// exposure on an unbacked store. liveHosts pins the exact exposure that was
// reviewed: a NEW live endpoint on the same service is a new decision and fails
// until it is re-reviewed here.
type liveVenueExemption struct {
	issue     string   // the issue that retires this exemption
	liveHosts []string // the exact live hosts the exemption was granted for
	reason    string   // why the live endpoint cannot simply be pointed elsewhere
}

// liveVenueUnbackedExemptions is the named-exemption list. It is not a
// suppression mechanism — an entry here is a live-money risk that a human
// accepted, and the guard checks on every run that the accepted shape is still
// the deployed shape (host set unchanged) and that the exposure still exists at
// all (dead-entry check).
// EMPTY, AND THAT IS THE POINT — see TestEveryOrderPlacingVenueCanSelectANonLiveEndpoint.
//
// venue-okx was exempted here while venue_orders and the OMS store were
// drUnresolved. #60 brought both under WAL+PITR, which removed the second half of
// this guard's condition, so the dead-entry check fired and the entry was
// deleted. That is correct: the DR objection really was resolved.
//
// It is also the trap. Nothing about the OKX adapter changed — it still points at
// the live exchange and still has no demo path — yet the interlock that had been
// holding it disappeared BECAUSE A DIFFERENT PROBLEM WAS FIXED. Deleting the
// entry and stopping there would have quietly converted "blocked by a guard" into
// "blocked by nothing", with a green suite either way.
//
// So the exposure moved rather than evaporated: it now lives against the
// invariant it always belonged to, which is #147's, not #60's.
var liveVenueUnbackedExemptions = map[string]liveVenueExemption{}

// nonOrderPlacingExchangeClients records services that configure LIVE exchange
// hosts legitimately, because they cannot place an order.
//
// They are listed rather than simply not scanned: "this service was checked and
// is read-only" and "nobody looked at this service" must not produce the same
// result. The guard re-derives the read-only claim from source on every run —
// the exclusion survives only as long as the property does.
var nonOrderPlacingExchangeClients = map[string]string{
	"market-ingest": "public market data only — order books and trades from the exchanges' " +
		"unauthenticated feeds (stream.binance.com, api.binance.com, ws.okx.com/ws/v5/public). It " +
		"holds no API key to sign an order with and registers no venue adapter service, so its live " +
		"hosts are the CORRECT configuration: pointing price ingestion at a testnet would fold a " +
		"fake book into real risk numbers, which is its own defect.",
}

// exchangeEndpoint is one configured http/ws endpoint and where it was found.
// The location travels with the host because a failure message that names a
// live host without naming the file that sets it is a search, not a finding.
type exchangeEndpoint struct {
	host  string // lowercased hostname, port and path stripped
	where string // repo-relative path:line
}

// endpointURLRe matches http/https/ws/wss URLs. Deliberately limited to those
// four schemes: nats:// and unix:// endpoints in the same manifests are cluster
// plumbing, not exchanges.
var endpointURLRe = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"',}\]]+`)

// goStringURLRe matches the same URLs inside a Go double-quoted string literal,
// so a URL merely mentioned in prose does not register as configuration.
var goStringURLRe = regexp.MustCompile(`(?i)"(?:https?|wss?)://[^"]*"`)

// orderPathRe matches the exchange REST paths that place, query or cancel an
// order (/api/v3/order, /api/v5/trade/order). It backs the re-derivation of the
// read-only claim in nonOrderPlacingExchangeClients.
var orderPathRe = regexp.MustCompile(`/api/v\d+/(?:trade/)?order\b`)

// configuredExchangeEndpoints returns every http/ws endpoint an adapter is
// configured with, from BOTH the deploy manifest env and the Go source defaults.
//
// The two sources answer different questions and neither substitutes for the
// other: the manifest is what the pod runs with, the Go literal is what it falls
// back to when that env var is absent. A live default is the more dangerous of
// the two precisely because nothing in the deployment mentions it.
func configuredExchangeEndpoints(t *testing.T, root, svc string) []exchangeEndpoint {
	t.Helper()

	var out []exchangeEndpoint

	// Manifest env. Comments are stripped first: a commented-out live URL is a
	// note about a possibility, not a deployed endpoint (infra/deploy/operator-deploy.yaml
	// carries exactly that shape).
	manifest := filepath.Join(root, "infra", "deploy", svc+"-deploy.yaml")
	if b, err := os.ReadFile(manifest); err == nil {
		rel := "infra/deploy/" + svc + "-deploy.yaml"
		for i, line := range strings.Split(stripYAMLComments(string(b)), "\n") {
			for _, m := range endpointURLRe.FindAllString(line, -1) {
				out = append(out, exchangeEndpoint{host: hostOf(m), where: relLine(rel, i+1)})
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", manifest, err)
	}

	// Go source defaults, across the WHOLE service rather than just its config
	// package — a hardcoded live URL in a client constructor is the same defect
	// with a better hiding place. Test files are excluded: their fake servers are
	// httptest URLs, and a test pointing at a real exchange is a different guard's
	// problem.
	svcDir := filepath.Join(root, "services", svc)
	err := filepath.WalkDir(svcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		for i, line := range strings.Split(string(b), "\n") {
			for _, lit := range goStringURLRe.FindAllString(line, -1) {
				out = append(out, exchangeEndpoint{
					host:  hostOf(strings.Trim(lit, `"`)),
					where: relLine(rel, i+1),
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", svcDir, err)
	}
	return out
}

// orderPlacingVenueAdapters returns every service that serves the OMS-facing
// venue adapter RPC — the structural definition of "an order can reach an
// exchange through this pod". It is read from source rather than from a list so
// a NEW adapter is in scope the day it is written, not the day someone
// remembers to add it here.
//
// Test files are excluded: services/oms/cmd/oms/venues_test.go registers a fake
// adapter, which places nothing anywhere.
func orderPlacingVenueAdapters(t *testing.T, root string) []string {
	t.Helper()

	const needle = "RegisterVenueAdapterServiceServer"
	servicesDir := filepath.Join(root, "services")
	found := map[string]bool{}

	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		t.Fatalf("read %s: %v", servicesDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		werr := filepath.WalkDir(filepath.Join(servicesDir, svc), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), needle) {
				found[svc] = true
			}
			return nil
		})
		if werr != nil {
			t.Fatalf("walk services/%s: %v", svc, werr)
		}
	}
	return sortedKeys(found)
}

// orderPlacementSites returns the non-test source locations where a service
// addresses an exchange order endpoint. Used to re-derive — not assume — the
// read-only claim behind every nonOrderPlacingExchangeClients entry.
func orderPlacementSites(t *testing.T, root, svc string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(filepath.Join(root, "services", svc), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		for i, line := range strings.Split(string(b), "\n") {
			if orderPathRe.MatchString(line) {
				out = append(out, relLine(rel, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk services/%s: %v", svc, err)
	}
	return out
}

// hostOf reduces a URL to the hostname the risk attaches to: scheme, userinfo,
// port and path removed, lowercased. wss://ws.okx.com:8443/ws/v5/public and
// https://ws.okx.com are the same exchange and must classify identically.
func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// relLine renders the repo-relative file:line every failure message quotes, in
// the form an editor and `git grep` both accept.
func relLine(rel string, line int) string {
	return rel + ":" + strconv.Itoa(line)
}

// AN ADAPTER THAT CAN PLACE AN ORDER MUST BE ABLE TO BE POINTED SOMEWHERE THAT IS
// NOT THE LIVE EXCHANGE.
//
// TestNoOrderPlacingVenueRunsLiveWhileItsStoreIsUnbacked is DR-scoped: it fires on
// live host AND unbacked store. Bringing venue_orders and the OMS store under
// WAL+PITR (#60) removed the second half and legitimately retired it — a real
// problem was really fixed.
//
// That is exactly why this guard exists separately. The OKX adapter did not
// change: it still names the live exchange, and OKX selects demo by the
// `x-simulated-trading: 1` request header rather than by hostname, so there is no
// sandbox host to point it at. Had the DR fix simply deleted the exemption, the
// only thing standing between this platform and the live order book would have
// stopped standing there — and the suite would have gone green at that moment.
// A safety property that disappears when an unrelated property is repaired was
// never being enforced; it was being inferred.
//
// The question here is capability, not configuration: CAN this adapter be run
// against something that is not real money? Binance can (testnet.binance.vision).
// OKX cannot, until the header exists.
func TestEveryOrderPlacingVenueCanSelectANonLiveEndpoint(t *testing.T) {
	root := moduleRoot(t)
	adapters := orderPlacingVenueAdapters(t, root)
	if len(adapters) == 0 {
		t.Fatal("found no order-placing venue adapters — the discovery scan is broken, " +
			"and a guard that examines nothing passes for the wrong reason")
	}

	seen := map[string]bool{}
	for _, svc := range adapters {
		live := map[string]string{} // host -> where
		for _, ep := range configuredExchangeEndpoints(t, root, svc) {
			if _, ok := nonLiveExchangeHosts[ep.host]; !ok {
				live[ep.host] = ep.where
			}
		}
		if len(live) == 0 {
			continue // every configured host is a sandbox: it can be run without real money
		}
		if tok, ok := demoModeSelectors[svc]; ok && serviceMentions(t, root, svc, tok) {
			continue // a demo path exists in the code, so the live host is a choice, not a cage
		}

		ex, exempted := noDemoPathExemptions[svc]
		if !exempted {
			t.Errorf("%s can place orders and is configured against live exchange host(s) %s, "+
				"with no way to select a non-live endpoint\n\n"+
				"Evidence: %s\n\n"+
				"Every order this adapter accepts settles against real money, and no configuration "+
				"change available today makes that untrue. Either give it a sandbox host and record "+
				"that host in nonLiveExchangeHosts, or wire the venue's demo-selection mechanism and "+
				"register its marker in demoModeSelectors. If it must stay this way, add a named "+
				"entry to noDemoPathExemptions citing the issue that retires it — which is a "+
				"live-money decision for a human, not for whoever is making this test green.",
				svc, strings.Join(sortedKeys(live), ", "), whereList(live))
			continue
		}
		seen[svc] = true
		if got, want := sortedKeys(live), append([]string(nil), ex.liveHosts...); !equalStrings(got, want) {
			sort.Strings(want)
			t.Errorf("%s's exemption was reviewed for live host(s) %s but it now configures %s\n\n"+
				"A new live endpoint is a new decision. Re-review it and update the entry, or remove "+
				"the endpoint.", svc, strings.Join(want, ", "), strings.Join(got, ", "))
		}
		t.Logf("ACCEPTED NO-DEMO-PATH EXPOSURE: %s runs against %s and cannot be pointed elsewhere. "+
			"Tracked by %s. %s", svc, strings.Join(sortedKeys(live), ", "), ex.issue, ex.reason)
	}

	for _, svc := range sortedKeys(noDemoPathExemptions) {
		if !seen[svc] {
			t.Errorf("exemption for %q is DEAD — it can now select a non-live endpoint, or it no "+
				"longer places orders.\n\nDelete the entry from noDemoPathExemptions and close or "+
				"update issue %s. An exemption that outlives its repair is how the next real one "+
				"gets ignored.", svc, noDemoPathExemptions[svc].issue)
		}
	}
}

// demoModeSelectors maps a service to the source marker that proves it can run
// against the venue's demo environment. It exists because not every exchange
// separates demo by hostname: OKX shares www.okx.com between demo and production
// and switches on the `x-simulated-trading: 1` request header, so for OKX the
// hostname genuinely cannot answer the question and only the code can.
//
// This is the EXIT CONDITION for the exemption below. Wiring the header — and it
// must actually be sent, not merely named in a comment — is what turns this guard
// green, which is why the marker is checked in source rather than assumed.
var demoModeSelectors = map[string]string{
	"venue-okx": "x-simulated-trading",
}

// noDemoPathExemptions records adapters that can place live orders and have no
// way to be pointed at anything else.
//
// IT IS EMPTY (#147). venue-okx was the only entry, and it is gone because the
// thing it documented is gone: `x-simulated-trading: 1` is now sent by
// exchangeauth.SignOKX, driven by an explicit OKX_TRADING_MODE, so the adapter
// CAN be run against something that is not real money. demoModeSelectors above
// is what proves that — it looks for the header as a string literal in shipped
// source, so this entry could not have been deleted by writing a comment.
//
// The dead-entry check below is what keeps this honest in the other direction:
// re-adding an entry for an adapter that can already select a non-live endpoint
// fails the build. An exemption here is a live-money decision by a human, and the
// only reason to add one back is that a NEW adapter genuinely cannot be pointed
// anywhere safe.
var noDemoPathExemptions = map[string]liveVenueExemption{}

// goStringLiteralRe matches double-quoted Go string literals, handling escapes.
var goStringLiteralRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// serviceMentions reports whether a service's non-test Go source contains a
// marker INSIDE A STRING LITERAL. Non-test only, and the shared venueadapter
// package is included because the header would be set where the request is
// signed, not in the service's own tree.
//
// STRING LITERALS, NOT RAW TEXT — and this guard learned that the hard way. The
// first version matched anywhere in the file, and passed venue-okx immediately:
// the fix for #147 had added a comment to its config.go EXPLAINING that
// `x-simulated-trading: 1` is absent. Prose describing a control's absence read
// as the control's presence, and the exemption below was declared dead by a
// guard whose whole subject is the difference between the two.
//
// A header that is actually sent appears as an argument to Header.Set — that is,
// as a literal. A header that is only discussed appears in a comment. Reading
// only literals is what makes the check about behaviour rather than about
// whether anyone has written the words down.
func serviceMentions(t *testing.T, root, svc, marker string) bool {
	t.Helper()
	marker = strings.ToLower(marker)

	var sources []string
	for _, dir := range []string{
		filepath.Join(root, "services", svc),
		filepath.Join(root, "internal", "venueadapter"),
	} {
		if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil //nolint:nilerr // a missing optional dir is not a failure
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sources = append(sources, string(b))
			return nil
		}); err != nil && !os.IsNotExist(err) {
			t.Fatalf("scan %s for %q: %v", dir, marker, err)
		}
	}

	// A DECLARED CONSTANT IS NOT A SENT HEADER, and this is the second time this
	// helper has had to learn that the evidence it reads is not the property it
	// claims.
	//
	// Round one: it matched raw file text, so a COMMENT explaining that
	// `x-simulated-trading` was absent read as proof it was present. Fixed by
	// reading only string literals.
	//
	// Round two (found by mutation-proving #147): deleting the h.Set() call while
	// leaving `const simulatedTradingHeader = "x-simulated-trading"` in place still
	// passed — the literal survived in the declaration. The adapter would have gone
	// back to placing every order on the LIVE book with this guard green, which is
	// the precise failure it exists to prevent.
	//
	// So a bare literal is no longer enough: the marker must reach a Header.Set
	// call, either written inline or through the identifier it is bound to. That
	// is still textual and still approximate — it cannot follow a value through a
	// helper — but it is the difference between "somebody wrote the words down"
	// and "a request carries the header", and every regression seen so far lives in
	// exactly that gap. The exact wire assertion is
	// exchangeauth.TestSignOKXSendsSimulatedTradingHeaderOnlyInDemo; this guard is
	// the estate-wide net that notices when an adapter has no demo path at all.
	bindingRe := regexp.MustCompile(`(?i)(\w+)\s*(?::=|=)\s*"[^"]*` + regexp.QuoteMeta(marker) + `[^"]*"`)
	idents := map[string]bool{}
	for _, src := range sources {
		for _, m := range bindingRe.FindAllStringSubmatch(src, -1) {
			idents[m[1]] = true
		}
	}

	// Inline: h.Set("x-simulated-trading", "1")
	inline := regexp.MustCompile(`(?i)\.Set\(\s*"[^"]*` + regexp.QuoteMeta(marker) + `[^"]*"`)
	for _, src := range sources {
		if inline.MatchString(src) {
			return true
		}
		for ident := range idents {
			// Via the bound identifier: h.Set(simulatedTradingHeader, "1")
			if regexp.MustCompile(`\.Set\(\s*` + regexp.QuoteMeta(ident) + `\b`).MatchString(src) {
				return true
			}
		}
	}
	return false
}

func whereList(live map[string]string) string {
	parts := make([]string, 0, len(live))
	for _, h := range sortedKeys(live) {
		parts = append(parts, h+" ("+live[h]+")")
	}
	return strings.Join(parts, ", ")
}

func equalStrings(a, b []string) bool {
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
