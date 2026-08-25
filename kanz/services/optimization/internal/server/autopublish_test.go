package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/internal/optimization"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/auth"
)

// openGate is a kill switch an operator has resumed — the state every test
// below except the halted one needs.
//
// IT IS EXPLICIT IN EVERY CASE BECAUSE THE DEFAULT IS CLOSED. halt.NewGate
// builds a gate that refuses, so a test helper that quietly omitted the gate
// would exercise the refusal path while claiming to test publication, and every
// assertion about what reached the bus would pass on an empty slice.
func openGate() *halt.Gate {
	g := halt.NewGate(time.Now)
	g.Resume("operator:test", "fixture")
	return g
}

// fakeMaterializer records what the handler asked of it.
type fakeMaterializer struct {
	armed      bool
	tenant     string
	published  []*orderpb.SubmitOrder
	facts      []*optimizationpb.ProposalMaterialized
	publishErr error
	recordErr  error
}

func (f *fakeMaterializer) Armed() bool { return f.armed }
func (f *fakeMaterializer) Publish(_ context.Context, cmd *orderpb.SubmitOrder) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, cmd)
	return nil
}
func (f *fakeMaterializer) Record(_ context.Context, fact *optimizationpb.ProposalMaterialized) error {
	f.facts = append(f.facts, fact)
	return f.recordErr
}

func serverWith(f *fakeMaterializer) (*Server, *fakeMaterializer) {
	return serverWithGate(f, openGate())
}

func serverWithGate(f *fakeMaterializer, gate *halt.Gate) (*Server, *fakeMaterializer) {
	rd := &Readiness{}
	rd.Set(true)
	s := New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithAutoPublish(func(tenant string) Materializer {
			f.tenant = tenant
			return f
		}),
		WithHaltGate(gate))
	return s, f
}

// feasibleProposal is a proposal a mandate check has PASSED — the only state
// bridge.ToOrders admits, and the state optimization.CheckMandate alone can
// produce.
//
// IT IS BUILT IN GO RATHER THAN POSTED AS JSON, and the reason is the fix in
// #646: the /v1/orders boundary refuses a caller-supplied mandate verdict, and
// this service has no mandate source of its own, so no request body can carry a
// proposal into the capital action below. The split is deliberate — the tests in
// this file exercise what happens once a proposal IS approved (publish, tenant
// scoping, the FACT, a partial failure), and server_test.go exercises the wire
// boundary that decides whether anything gets that far. Neither substitutes for
// the other, and the gap between them is exactly the wiring #646 leaves open.
func feasibleProposal() optimization.RebalanceProposal {
	return optimization.RebalanceProposal{
		PortfolioID:   "PF",
		MandateStatus: optimization.MandateFeasible,
		Trades: []optimization.ProposedTrade{
			{InstrumentID: "A", Side: optimization.Buy, Quantity: 100},
		},
	}
}

// materializeAs drives Server.materialize with the principal handleOrders would
// have resolved. The principal comes from real headers through
// auth.PrincipalFromHeaders rather than a hand-built struct, so a change to the
// header contract breaks these tests instead of being papered over.
func materializeAs(t *testing.T, s *Server, p optimization.RebalanceProposal, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", http.NoBody)
	auth.SetPrincipalHeaders(req.Header, subject, "acme", []string{"pm"})
	principal, ok := auth.PrincipalFromHeaders(req.Header)
	if !ok || principal.Subject != subject {
		t.Fatalf("the test principal did not survive the header round trip: %+v", principal)
	}
	rec := httptest.NewRecorder()
	s.materialize(rec, req, p, principal)
	return rec
}

// THE DEFAULT POSTURE IS THE ONE THAT MUST NEVER DRIFT (#409).
//
// A server built without WithAutoPublish publishes NOTHING. The bridge's own
// header states the stance it protects — "the optimizer proposes, a human
// approves, and ONLY THEN does this bridge emit commands" — and this platform
// must never acquire the opposite behaviour because someone added a parameter
// and a caller passed the wrong thing.
func TestWithoutTheSwitchNothingIsPublished(t *testing.T) {
	rec := materializeAs(t, newTestServer(), feasibleProposal(), "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count     int
		Published bool
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Fatalf("count = %d, want 1 — the commands are still BUILT and returned", resp.Count)
	}
	if resp.Published {
		t.Error("published = true on a server with no auto-publish configured. This is the " +
			"human-in-the-loop default being reported as a live trade.")
	}
}

// ARMED, the commands go out — and the response says so, so a caller never has
// to consult the deployment's environment to learn whether it just traded.
func TestArmedTheCommandsArePublished(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true})

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(f.published) != 1 {
		t.Fatalf("published %d command(s), want 1", len(f.published))
	}
	if got := f.published[0].GetMetadata().GetIssuer(); got != "alice" {
		t.Errorf("issuer = %q, want the authenticated principal alice", got)
	}
	var resp struct{ Published bool }
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Published {
		t.Error("published = false while commands went to the bus — the caller is told a live " +
			"rebalance was a dry run")
	}
}

// NOT EVEN ARMED, AND NOT EVEN THROUGH THE FRONT DOOR (#646).
//
// This is the reachability assertion the two above cannot make: it goes through
// the real router, with auto-publish armed, using the proposal shape a client
// gets back from /v1/propose — and nothing reaches the bus, because nothing has
// checked that proposal against a mandate. Before the fix this request emitted a
// live SubmitOrder command, since RebalanceProposal's constructor stamped the
// verdict the bridge gates on.
func TestArmedAnUncheckedProposalStillPublishesNothing(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unchecked proposal got %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if len(f.published) != 0 {
		t.Fatalf("%d command(s) reached the bus from a proposal no mandate check has run on",
			len(f.published))
	}
	if len(f.facts) != 0 {
		t.Errorf("a ProposalMaterialized FACT was recorded for a refused proposal: %+v", f.facts)
	}
}

// THE TENANT COMES FROM THE CALLER, per request. One handler serves every tenant
// concurrently; a materializer that carried a tenant set at construction would
// publish one customer's rebalance under another's.
func TestTheMaterializerIsScopedToTheCallersTenant(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true})

	if rec := materializeAs(t, s, feasibleProposal(), "alice"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if f.tenant != "acme" {
		t.Errorf("materializer tenant = %q, want the caller's tenant acme", f.tenant)
	}
}

// A DRY RUN IS STILL RECORDED. With a broker configured and the switch off, the
// FACT goes out with published=false — so "nobody asked" and "we asked and sent
// nothing" are distinguishable on the bus rather than identical silence.
func TestADryRunStillEmitsTheFact(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: false})

	if rec := materializeAs(t, s, feasibleProposal(), "alice"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if len(f.published) != 0 {
		t.Fatalf("a disarmed materializer published %d command(s)", len(f.published))
	}
	if len(f.facts) != 1 {
		t.Fatalf("facts = %d, want 1 — a dry run is an outcome, not an absence", len(f.facts))
	}
	if f.facts[0].GetPublished() {
		t.Error("the FACT says published=true for a dry run")
	}
	if len(f.facts[0].GetSubmittedOrderIds()) != 1 {
		t.Error("the FACT records no order ids — it must say WHICH commands were built, or it " +
			"cannot be reconciled against what the caller received")
	}
}

// THE FACT NAMES WHO AUTHORIZED IT. An automated capital action whose record
// does not identify the principal is not auditable, which is the whole reason
// this FACT exists.
func TestTheFactCarriesThePrincipalAndTheOrderIDs(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true})

	if rec := materializeAs(t, s, feasibleProposal(), "alice"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if len(f.facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(f.facts))
	}
	fact := f.facts[0]
	if fact.GetIssuer() != "alice" {
		t.Errorf("fact issuer = %q, want alice", fact.GetIssuer())
	}
	if !fact.GetPublished() {
		t.Error("fact published = false while the commands went out")
	}
	if len(fact.GetSubmittedOrderIds()) != 1 || fact.GetSubmittedOrderIds()[0] == "" {
		t.Errorf("fact order ids = %v, want the published command's id", fact.GetSubmittedOrderIds())
	}
	if fact.GetPortfolioId() != "PF" {
		t.Errorf("fact portfolio = %q, want PF", fact.GetPortfolioId())
	}
}

// A PARTIAL PUBLISH IS NOT A SUCCESS. Materialize stops at the first failure, so
// some commands are live and the rest are not — reporting 200 would tell a
// caller their whole rebalance went out, and a portfolio left with one leg of a
// pair trade on is the worst available outcome.
func TestAPartialPublishIsReportedAsAFailure(t *testing.T) {
	s, _ := serverWith(&fakeMaterializer{armed: true, publishErr: errors.New("broker unavailable")})

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 — a rebalance that published only in part must not read as OK: %s",
			rec.Code, rec.Body.String())
	}
}

// A LOST FACT DOES NOT FAIL THE REQUEST. By the time it is written the commands
// are already in flight, so refusing would misreport what happened — but it is
// counted and logged at the composition root, never swallowed.
func TestALostFactDoesNotFailTheRequest(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true, recordErr: errors.New("bus down")})

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: the orders were published; failing here would tell the caller "+
			"the trade did not happen", rec.Code)
	}
	if len(f.published) != 1 {
		t.Errorf("published %d, want 1", len(f.published))
	}
}

// THE PLATFORM HALT REACHES THIS SERVICE (#739).
//
// It did not, and the reason it went unnoticed for as long as it did is worth
// keeping: this service mints order.v1.SubmitOrder commands in internal/bridge
// and publishes them on order.order.submit from internal/publish — the same
// subject the gateway and webhook-ingest write to — while
// TestEveryOrderPlacingServiceHonoursTheHalt classified order origins from a
// hand-kept list that named neither package. So the guard never asked, and an
// operator running `kanz-halt` stopped five processes and not this one.
//
// The three tests below are the three states the gate can be in, because a
// refusal test alone would pass against a handler that refused unconditionally.
func TestAHaltedPlatformRefusesToPublishTheRebalance(t *testing.T) {
	// RESUMED FIRST, THEN HALTED — the real sequence, and the only one that
	// proves anything. A fresh gate is ALREADY closed ("gate has not been opened
	// since startup"), so a test that halted one would assert against the
	// startup latch and pass even if Observe did nothing at all.
	halted := openGate()
	halted.Observe(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:risk",
		Reason:    "risk breach",
	})
	s, f := serverWithGate(&fakeMaterializer{armed: true}, halted)

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusLocked {
		t.Fatalf("got %d, want 423 — a halted platform must refuse a capital action outright: %s",
			rec.Code, rec.Body.String())
	}
	// THE PART THAT MATTERS. A 423 whose orders went out anyway is worse than no
	// brake, because the operator now believes the platform is stopped.
	if len(f.published) != 0 {
		t.Fatalf("%d order(s) were published through a declared halt", len(f.published))
	}
	// AND NO FACT EITHER. recordMaterialization runs after the publish loop, so a
	// check placed inside that loop would record a ProposalMaterialized for a
	// rebalance that was refused — an audit trail claiming a capital action that
	// never occurred.
	if len(f.facts) != 0 {
		t.Fatalf("a materialization FACT was recorded for a rebalance that never happened: %v", f.facts)
	}
	if body := rec.Body.String(); !strings.Contains(body, "risk breach") {
		t.Fatalf("the refusal does not carry the operator's reason, which is the only thing that "+
			"tells the caller this is a halt and not a bug: %s", body)
	}
}

// A GATE NOBODY WIRED IS A HALTED GATE. This is the case that makes
// WithHaltGate safe to be an option rather than a constructor parameter: a
// composition root that arms auto-publish and forgets the gate refuses to
// publish, rather than trading with no brake at all.
func TestAnUnwiredHaltGateRefusesToPublish(t *testing.T) {
	rd := &Readiness{}
	rd.Set(true)
	f := &fakeMaterializer{armed: true}
	s := New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithAutoPublish(func(tenant string) Materializer { f.tenant = tenant; return f }))

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusLocked {
		t.Fatalf("got %d, want 423 — auto-publish with no halt gate must fail CLOSED: %s",
			rec.Code, rec.Body.String())
	}
	if len(f.published) != 0 {
		t.Fatalf("%d order(s) were published by a service with no brake", len(f.published))
	}
}

// AND THE DRY RUN IS NOT GATED, deliberately. A halt refuses new execution
// exposure; a proposal that reaches no bus is not exposure, and a portfolio
// manager must still be able to see what a rebalance WOULD do while the platform
// is stopped — that is often exactly what the incident needs.
func TestAHaltDoesNotStopADryRun(t *testing.T) {
	// RESUMED FIRST, THEN HALTED — the real sequence, and the only one that
	// proves anything. A fresh gate is ALREADY closed ("gate has not been opened
	// since startup"), so a test that halted one would assert against the
	// startup latch and pass even if Observe did nothing at all.
	halted := openGate()
	halted.Observe(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:risk",
		Reason:    "risk breach",
	})
	s, f := serverWithGate(&fakeMaterializer{armed: false}, halted)

	rec := materializeAs(t, s, feasibleProposal(), "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a dry run publishes nothing and has nothing to brake: %s",
			rec.Code, rec.Body.String())
	}
	if len(f.published) != 0 {
		t.Fatalf("a DISARMED materializer published %d order(s)", len(f.published))
	}
}
