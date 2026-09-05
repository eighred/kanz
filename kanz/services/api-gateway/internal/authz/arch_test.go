package authz_test

// THE ARCH TEST IS THE DELIVERABLE (SEC-M2).
//
// The routes are easy to get right today and easy to get wrong in six months, when someone
// adds `POST /v1/orders/{id}/amend` and registers it next to its neighbours without thinking
// about who may call it. A capability model that depends on remembering to apply it is not a
// control — it is a convention, and conventions decay silently.
//
// So: the capability is a REQUIRED PARAMETER of registration (the compiler enforces that
// every route declares one), and these tests enforce that the declaration is the RIGHT one.
// A route with a capital effect that anyone but a trader can reach fails the build.

import (
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"net/http"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/control"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// TestEveryOrderRouteRequiresACapitalAuthority.
//
// services/api-gateway/internal/orders is THE capital path: every route it serves submits,
// cancels or releases an order at a live exchange. Rather than listing those routes (a list
// someone must remember to extend), this asserts the property over WHATEVER the package
// registers — so a route added there tomorrow is covered by this test the moment it exists.
//
// TWO CAPABILITIES ARE ADMISSIBLE HERE, AND THAT IS THE CONTROL RATHER THAN AN EXCEPTION TO
// IT. Trade originates and withdraws an order; Approve gives the second signature that
// releases one the OMS held for exceeding the dual-control threshold (#539, #410). Both move
// capital, which is why neither Read, Operate nor Fund may appear in this package — but they
// are deliberately held by DIFFERENT PEOPLE, and the composition root enforces that: the
// approver's grant carries Read and Approve and never Trade, and validateAuth refuses to
// start if the approve role collides with the trade role. Widen this set only for another
// authority that is itself segregated; collapsing Approve into Trade would let the proposer
// sign their own release and record one pair of hands as two.
//
// It stays DEFAULT-DENY: a route registered here with any capability outside the set fails,
// so the next one still has to argue its case.
// SCOPE IS THE ORDERS HANDLER, AND THE NAME NOW SAYS SO (#573).
//
// It was called TestEveryRouteOnTheCapitalPathRequiresACapitalAuthority while
// mounting one handler, so it claimed a reach it did not have —
// POST /v1/portfolios/{id}/cash-movements moves the fund's own capital and lives
// in proxy, outside it. That is the same overclaim that let eleven control
// routes sit outside the golden table for months: a guard whose name describes
// more than it inspects stops anyone asking what it misses.
//
// The scope is deliberate rather than a gap. Mounting proxy here would drag in
// its read surface, every route of which requires authz.Read and would fail the
// assertion below — so widening this guard means deciding which proxy routes are
// "capital", which is a hand-maintained list, which is the thing that rots.
//
// The funding route is pinned in three places instead, and that is the answer to
// "who checks cash-movements": the golden table below (authz.Fund, and changing
// it fails there), internal/proxy/funding_test.go, and
// internal/config/config_fund_test.go, which asserts the #535 posture that an
// unnamed fund role leaves the route UNREGISTERED rather than registered and
// refusing everybody.
func TestEveryOrderRouteRequiresACapitalAuthority(t *testing.T) {
	m := authz.NewMux(nil, nil)
	// A NON-EMPTY APPROVE ROLE, for the same reason stubOrders below is a real value: since
	// #535 the approve route is registered only when the deployment names an approver, so ""
	// here would drop the second-signature route out of this guard silently — the exact blind
	// spot the fund role's comment in the golden table describes.
	orders.New(nil, "kanz-compliance", halt.OpenGate(nil)).Routes(m)

	allowed := map[authz.Capability]bool{authz.Trade: true, authz.Approve: true}

	routes := m.Routes()
	if len(routes) == 0 {
		t.Fatal("the orders handler registered NO routes — this test would pass vacuously and guard nothing")
	}
	for _, r := range routes {
		if !allowed[r.Capability] {
			t.Errorf("%s requires %q, want one of %q or %q — it submits, cancels or releases an order "+
				"at a live exchange, and anyone holding a read token could call it",
				r.Pattern, r.Capability, authz.Trade, authz.Approve)
		}
	}
}

// TestTheWholeRouteTableIsDeclared is the golden table.
//
// Every /v1 route the gateway serves, and the capability it demands. A NEW route — anywhere,
// in any handler package — fails this test until someone writes it down here, which is the
// point: the decision "who may call this?" is made deliberately, once, in the open, rather
// than inherited by accident from whichever mux the handler happened to be registered on.
func TestTheWholeRouteTableIsDeclared(t *testing.T) {
	m := authz.NewMux(nil, nil)
	// A NON-NIL ORDERS CLIENT, DELIBERATELY. gateway.Routes registers the order
	// history only when one is configured, so passing nil here would let that
	// route escape this table entirely — the guard would pass by not looking.
	gateway.New(nil, stubOrders{}, stubInstruments{}, "kanz-compliance", nil).Routes(m)
	// A NON-EMPTY APPROVE ROLE ON THE ORDERS HANDLER TOO (#539): its approve route is
	// registered only when the deployment names an approver, so "" here would hide the
	// order-release surface from this table for the same reason "" hides funding below.
	orders.New(nil, "kanz-compliance", halt.OpenGate(nil)).Routes(m)
	// A NON-EMPTY FUND ROLE, for the same reason stubOrders is a real value: since
	// #535 the cash-movement route is registered only when the deployment names a
	// funder, so passing "" here would drop it out of this table silently and the
	// guard would pass by not looking at the one route that moves the fund's cash.
	//
	// AND A NON-EMPTY APPROVE ROLE, on the same argument (#539). The three override
	// routes are registered only when the deployment names an approver — the whole
	// point of the maker-checker repair — so an empty one here would hide the
	// second-signature surface from this table exactly as "" would hide funding.
	//
	// AND A NON-EMPTY MANDATE ROLE (#562). Act two's three routes are registered
	// only when the deployment names a mandate signatory, so "" here would hide the
	// surface that changes what governs a portfolio from the table this test's doc
	// says covers every /v1 route.
	proxy.New(nil, proxy.Roles{
		Fund: "kanz-treasury", Approve: "kanz-compliance", Mandate: "kanz-mandate-officer",
	}).Routes(m)
	// THE CONTROL PLANE, AND ITS ABSENCE WAS THIS GUARD'S OWN BLIND SPOT (#573).
	//
	// The three lines above defend carefully against a route escaping through a
	// CONDITION — a nil client, an empty role. None of them noticed a whole
	// handler escaping by never being mounted at all. Eleven routes were outside
	// the table this test's doc says covers "every /v1 route the gateway serves",
	// including POST /v1/control/nodes/{name}/drain and
	// PUT /v1/control/venues/{venue}/keys, which uploads venue API credentials.
	//
	// Nothing was mis-gated. The point is that this guard could not have told.
	//
	// A nil client is safe here BECAUSE Routes registers unconditionally — unlike
	// the three above. If that ever changes, the non-vacuity arm below is what
	// stops this silently reverting to checking three handlers out of four.
	control.New(nil, nil).Routes(m)

	want := map[string]authz.Capability{
		// Risk queries. A scenario is a POST, but it computes a what-if and moves no
		// capital — the HTTP verb is not the authority on effect.
		// The LIST is Read, and the guard's own prompt — "decide whether a read
		// token should be able to call it" — is worth answering rather than
		// waving through. It discloses strictly less than the three routes below
		// it: names and a staleness stamp, no positions, no valuations, no
		// money. What it discloses that they do not is the SET of ids, and that
		// is gated twice over — by owner_tenant through writeOwned, and per row
		// by the caller's portfolios claim (#399).
		"GET /v1/portfolios":                authz.Read,
		"GET /v1/portfolios/{id}/exposure":  authz.Read,
		"GET /v1/portfolios/{id}/measures":  authz.Read,
		"POST /v1/portfolios/{id}/scenario": authz.Read,
		// ORDER HISTORY IS A READ, and the guard's prompt is worth answering: it
		// moves no capital and places nothing — the OMS's surface accepts no
		// order, deliberately, so that admission and the compliance gate stay on
		// the bus path. It is the most IDENTIFYING of these reads, naming
		// instruments, sizes and times, which is why it is the one portfolio
		// route that also consults the caller's portfolios claim (#399).
		"GET /v1/portfolios/{id}/orders": authz.Read,
		// THE TRADEABLE-PAIR CATALOGUE IS A READ, and answering the guard's prompt:
		// it is the LEAST disclosing route in this table. It reports which pairs
		// this deployment's venue adapters are configured for — no positions, no
		// sizes, no valuations, and nothing about any portfolio. It is also the
		// only route here that is not portfolio-scoped, because there is no
		// per-portfolio answer to the question: the answer is a property of the
		// deployment. The tenant gate still applies through writeOwned (#406).
		"GET /v1/instruments": authz.Read,
		"GET /v1/health":      authz.Read,

		// THE CAPITAL PATH.
		"POST /v1/orders":             authz.Trade,
		"POST /v1/orders/{id}/cancel": authz.Trade,

		// RELEASING A HELD ORDER (#539, #410). Should a read token be able to call
		// this? No — it is the one route in this table that puts capital on a live
		// exchange without composing the order, and a read token reaching it would
		// mean the dual-control threshold is enforced against nobody.
		//
		// Should a TRADE token? Also no, and that is the half worth writing down.
		// The OMS holds an order over the threshold precisely so that a SECOND
		// PERSON decides; giving Trade this route hands the proposer their own
		// release and records one pair of hands in the trail as two. The approver's
		// grant is Read + Approve, and validateAuth refuses to start if that role
		// name collides with the trade role — the segregation is configuration the
		// process will not run without, not a convention.
		"POST /v1/orders/{id}/approve": authz.Approve,

		// MODEL PORTFOLIOS (#409), and the guard's prompt is the whole point here.
		//
		// propose is a READ: it optimizes against numbers supplied in the request
		// and moves nothing, the same reasoning that makes the scenario route a
		// read.
		//
		// orders is TRADE, and unconditionally so. It emits the order commands for
		// a proposal, and in a deployment with auto-publish enabled it puts them on
		// the bus. The capability describes what the route DOES in its most
		// permissive configuration — never what one deployment's environment
		// variable currently allows — because a route whose required capability
		// changes with a config flag is one nobody can reason about, and a read
		// token must not reach a surface that in some deployment moves capital.
		"POST /v1/model-portfolios/propose": authz.Read,
		"POST /v1/model-portfolios/orders":  authz.Trade,

		// THE AGENT-FACING MCP READ PLANE (#743, wired #762). One route: every
		// MCP method — initialize, tools/list, tools/call — arrives as a field
		// inside the JSON-RPC body.
		//
		// The guard's prompt, answered: YES, a read token should reach this, and
		// that is the whole point of the plane. It cannot do anything else. Its
		// Reader interface declares no Submit, no Cancel, no Amend and no venue
		// call; an arch guard asserts no write-side capability is reachable from
		// its import graph; and its handshake states readOnly explicitly rather
		// than leaving a caller to infer it from an absence. So Read is what the
		// route requires in its most permissive configuration, which is the
		// standard the materialize route above sets.
		//
		// It borrows Read rather than minting an "mcp" capability, per CLAUDE.md's
		// preference for existing authorization over a new security system: a
		// caller entitled to read a portfolio's risk directly is entitled to read
		// it through an agent, and the per-tool gate inside the plane is what
		// scopes the answer to that caller's tenant.
		"POST /v1/mcp": authz.Read,

		// THE ESG EXCLUSION SCREEN (#751 item 4). The guard's prompt, answered:
		// yes, a read token should reach this. It evaluates a book the CALLER
		// supplied against a policy the CALLER supplied and returns a compliance
		// result — it signs nothing, stores nothing, and appends no link to the
		// AUDIT-01 hash chain.
		//
		// THE FIVE FILING ROUTES ON THE SAME SERVICE ARE NOT HERE, and that is the
		// decision rather than an omission. A filing IS signed and DOES append to
		// the chain, so authz.Read would be wrong for it and authz.Audit is a read
		// capability. Naming the right one is its own decision with its own
		// evidence, and making it while wiring a screening route is how a
		// capability ends up meaning nothing — the failure the model-portfolios
		// entry above guards against from the other direction.
		"POST /v1/screening/esg": authz.Read,

		// THE FUNDING PATH (#415) — the fund's OWN capital, not the market's.
		//
		// authz.Fund and not authz.Trade, and the guard's prompt is exactly the
		// question to answer here: should a read token reach this? No — it WRITES
		// the book of record. Should a TRADE token? Also no, and that is the less
		// obvious half. The person who can move money is never the person who
		// trades it; giving Trade this route hands every strategy operator the
		// authority to book a redemption against the IBOR.
		"POST /v1/portfolios/{id}/cash-movements": authz.Fund,

		// THE CUSTODY RECONCILIATION BREAK QUEUE (#962) — the operator surface for
		// the control that checks the book of record against the custodian.
		//
		// authz.Fund, and the guard's prompt answered: should a READ token reach
		// this? No, and the GET is the interesting half of that answer. The queue
		// names every unresolved discrepancy between the fund's book and what is
		// actually custodied — a working surface an operator acts from, not a view
		// of settled state — and a token that can see the fund's open control
		// failures but not work them is a half-open control whose leaking half is
		// the interesting one. Same reasoning as the datamaster pending queue.
		//
		// Should a TRADE token? No: the person who investigates a custody break is
		// not the person who trades the book. Should this be its own capability?
		// It was considered and refused — working a break is middle-office FUND
		// OPERATIONS, the same function that books the redemption above, and the
		// bar written on Operate/Fund/Approve is that a new capability must be a
		// DIFFERENT authority rather than a different noun.
		//
		// NO CAPITAL MOVES HERE, and the transition that could be abused cannot
		// be: Resolve refuses while the latest run still detects the difference,
		// so an operator may record what they know and may not silence the
		// control; SaveBreak refuses to insert, so a break cannot be invented.
		"GET /v1/custody/breaks":               authz.Fund,
		"POST /v1/custody/breaks/{id}/assign":  authz.Fund,
		"POST /v1/custody/breaks/{id}/explain": authz.Fund,

		// THE AD-HOC CUSTODY COMPARISON (#1025, #967) — "here is the statement in
		// my hand; what does it say about the book right now".
		//
		// authz.Fund, and the guard's prompt is worth answering carefully because
		// this route WRITES NOTHING, which is usually the argument for Read. It is
		// not the argument here. The response is the full set of differences
		// between the fund's book and a named custodian's holdings — strictly more
		// disclosing than the queue above, which lists only the differences a
		// scheduled run already found. It is also the surface an operator acts
		// from while working a break, so a read token that can compute the fund's
		// entire custody position but not work the queue is the half-open control
		// the entry above refuses.
		//
		// Should a TRADE token? No, for the reason given above: investigating a
		// custody break is not trading the book.
		//
		// NO CAPITAL MOVES AND NO BREAK MOVES. The statement is caller-supplied, so
		// the handler deliberately records no run and upserts no break — a route
		// that did would let a caller close any break by posting a statement that
		// agrees with the book, which is the hand-resolve #966 removed.
		"POST /v1/portfolios/{id}/reconcile": authz.Fund,

		// Reference + wealth reads.
		"GET /v1/households/{id}": authz.Read,
		"GET /v1/securities/{id}": authz.Read,
		"GET /v1/prices/{id}":     authz.Read,
		"GET /v1/exceptions":      authz.Read,
		"POST /v1/ask":            authz.Read,

		// MAKER-CHECKER ON THE PRICING OVERRIDE (#539, #410 act one). Should a read
		// token be able to call these? NO, and the pending queue is the one where the
		// answer is not obvious: listing what awaits a signature looks like a report,
		// but it is the working surface an approver acts from and it names the
		// proposer of every unsigned change to the marks the book is valued at. A
		// read token that can see the queue but not sign it is a half-open control.
		//
		// The propose route is authz.Approve rather than a second capability because
		// four-eyes here is a check on the PERSON, not the role: datamaster compares
		// authenticated subjects, so two holders of this role are two signatures and
		// one holder acting twice is refused.
		// The queue an approver acts from. Should a read token be able to call it?
		// NO — same answer as the override queue, and for the same reason: it names
		// the proposer of every unsigned order awaiting a second signature and is
		// the working surface somebody signs from, not a report. A held order is
		// absent from every other read on this gateway, so this is the ONLY way to
		// discover one — which is what made the approve route unusable without it.
		"GET /v1/orders/pending-approvals": authz.Approve,

		"POST /v1/exceptions/{id}/override":         authz.Approve,
		"POST /v1/exceptions/{id}/override/approve": authz.Approve,
		"GET /v1/exceptions/pending-overrides":      authz.Approve,

		// CHANGING A MANDATE, BY TWO PEOPLE (#562, #410 act two). Should a read
		// token be able to call these? No — they rewrite the constraint the
		// pre-trade gate enforces on every order, and the queue names the proposer
		// of every unsigned change to it.
		//
		// Should a TRADE token? No, and more sharply than anywhere else in this
		// table: a trader who can change the mandate does not need to break the
		// pre-trade gate, only to widen it.
		//
		// Should an APPROVE token? THAT is the question this entry exists to
		// answer, and #539 predicted the opposite — "a third route on the same
		// capability". No, because one signatory holding both would give the second
		// signature on relaxing a mandate AND the second signature on the order
		// that mandate would have refused. Each act shows two names, each passes
		// its own self-approval check, and nothing compares the two records. The
		// capability's own doc carries the full argument; validateAuth refuses to
		// start if the two role names collide.
		//
		// PROPOSE AND APPROVE SHARE authz.Mandate, on the override path's
		// reasoning: four-eyes here is a check on the PERSON, not the role —
		// compliance compares authenticated subjects across two requests, so two
		// holders are two signatures and one holder acting twice is refused.
		// THE BY-ID READ (#606) IS authz.Mandate AND NOT authz.Read, and it is the
		// entry where "should a read token reach it?" has the sharpest answer on
		// this whole table. It serves the PROPOSED MANDATE — which rules, which
		// limits — so it is strictly more sensitive than the queue above it, which
		// carries only the shape of the change. Granting it Read would put the
		// constraint set of every pending change in front of every token in the
		// tenant, and it would do so on the one surface a signature is given from.
		"POST /v1/portfolios/{id}/mandate":               authz.Mandate,
		"POST /v1/portfolios/{id}/mandate/approve":       authz.Mandate,
		"GET /v1/mandates/pending-changes":               authz.Mandate,
		"GET /v1/mandates/pending-changes/{proposal_id}": authz.Mandate,

		// THE OPERATOR CONTROL PLANE (#573). Every one of these is authz.Operate,
		// and the capability is the whole argument: Read and Trade are
		// deliberately absent because none of this reads the book or moves
		// capital — it changes the ESTATE the book runs on. Draining a node
		// evicts running pods; uploading venue keys hands a credential to the
		// process that trades with it. A read token must never reach either, and
		// a trade token has no business in the estate's shape.
		"GET /v1/control/nodes":                  authz.Operate,
		"GET /v1/control/clusters":               authz.Operate,
		"POST /v1/control/nodes":                 authz.Operate,
		"GET /v1/control/provisions":             authz.Operate,
		"POST /v1/control/test-connection":       authz.Operate,
		"POST /v1/control/nodes/{name}/cordon":   authz.Operate,
		"POST /v1/control/nodes/{name}/uncordon": authz.Operate,
		"POST /v1/control/nodes/{name}/drain":    authz.Operate,
		"POST /v1/control/nodes/{name}/region":   authz.Operate,
		"GET /v1/control/venues":                 authz.Operate,
		"PUT /v1/control/venues/{venue}/keys":    authz.Operate,

		// The TradingView Broker API: reads of the fund's own book.
		"GET /v1/broker/accounts":                 authz.Read,
		"GET /v1/broker/accounts/{id}/state":      authz.Read,
		"GET /v1/broker/accounts/{id}/positions":  authz.Read,
		"GET /v1/broker/accounts/{id}/orders":     authz.Read,
		"GET /v1/broker/accounts/{id}/executions": authz.Read,
	}

	got := map[string]authz.Capability{}
	for _, r := range m.Routes() {
		got[r.Pattern] = r.Capability
	}

	// NON-VACUITY, PER HANDLER (#573). The bidirectional comparison below catches
	// a route that appears or disappears, but it cannot catch a handler that was
	// never mounted — the declared entries would simply report as "no longer
	// registered", which reads like someone deleted a route rather than like this
	// test stopped looking at a quarter of the gateway.
	//
	// One live route from each mounted handler, so dropping any Routes() call
	// above fails HERE with the handler named, not as eleven confusing deletions.
	for _, anchor := range []struct{ handler, pattern string }{
		{"gateway", "GET /v1/portfolios"},
		{"orders", "POST /v1/orders"},
		{"proxy", "GET /v1/exceptions"},
		{"control", "POST /v1/control/nodes/{name}/drain"},
	} {
		if _, ok := got[anchor.pattern]; !ok {
			t.Fatalf("the %s handler contributed no routes — %q is missing, so this table is "+
				"declaring a gateway smaller than the one that ships. That is #573: eleven control "+
				"routes sat outside this guard because the handler was never mounted, and the guard "+
				"passed by not looking.", anchor.handler, anchor.pattern)
		}
	}

	for pattern, cap := range got {
		switch w, ok := want[pattern]; {
		case !ok:
			t.Errorf("UNDECLARED ROUTE %q (requires %q). Add it to this table — and while you are "+
				"here, decide whether a read token should be able to call it", pattern, cap)
		case w != cap:
			t.Errorf("%q requires %q, but this table says %q", pattern, cap, w)
		}
	}
	for pattern := range want {
		if _, ok := got[pattern]; !ok {
			t.Errorf("declared route %q is no longer registered — delete it from this table", pattern)
		}
	}
}

// TestAReadCapabilityCannotBeGrantedTradeByAccident: Grants maps a role to the capabilities
// it carries, and nothing else confers one. In particular, the BASELINE role that admits a
// caller to the gateway at all (SEC-M1's API_GATEWAY_REQUIRED_ROLE) must not smuggle in
// trade — every authenticated caller carries it, so if it did, SEC-M2 would be undone in one
// line of config.
func TestAReadCapabilityCannotBeGrantedTradeByAccident(t *testing.T) {
	g := authz.Grants{"kanz.reader": {authz.Read}}
	if g.Allows([]string{"kanz.reader"}, authz.Trade) {
		t.Fatal("a read-only role was allowed to trade")
	}
	if !g.Allows([]string{"kanz.reader"}, authz.Read) {
		t.Fatal("a read role was not allowed to read")
	}
	if g.Allows(nil, authz.Read) {
		t.Fatal("a principal with NO roles was allowed to read — capabilities must be deny-by-default")
	}
}

// TestRegisteringARouteWithNoCapabilityIsImpossible documents what the compiler already
// guarantees: Handle takes the capability as a REQUIRED parameter, so there is no call that
// registers a route without declaring who may reach it. This test exists to say so out loud
// — the guarantee is the API shape, not a check that runs.
func TestRegisteringARouteWithNoCapabilityIsImpossible(t *testing.T) {
	m := authz.NewMux(nil, nil)
	// m.Handle("GET /v1/thing", h)  // does not compile: missing the capability.
	m.Handle(authz.Read, "GET /v1/thing", func(http.ResponseWriter, *http.Request) {})
	if got := m.Routes(); len(got) != 1 || got[0].Capability != authz.Read {
		t.Fatalf("routes = %+v", got)
	}
}

// stubOrders exists only so gateway.Routes registers the order-history route.
//
// It is a REAL value rather than a typed nil: a nil interface value compares
// equal to nil, so the conditional registration would skip the route and this
// guard would pass by not looking at it — the exact failure mode the table is
// meant to prevent.
type stubOrders struct {
	orderpb.OrderQueryServiceClient
}

// stubInstruments does the same for the tradeable-pair catalogue (#406), and for
// the same reason — the route is registered conditionally, so a nil here would
// quietly remove it from the golden table.
type stubInstruments struct {
	venuepb.VenueQueryServiceClient
}
