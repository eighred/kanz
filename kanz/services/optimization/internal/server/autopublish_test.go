package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

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
	rd := &Readiness{}
	rd.Set(true)
	s := New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAutoPublish(func(tenant string) Materializer {
		f.tenant = tenant
		return f
	}))
	return s, f
}

// THE DEFAULT POSTURE IS THE ONE THAT MUST NEVER DRIFT (#409).
//
// A server built without WithAutoPublish publishes NOTHING. The bridge's own
// header states the stance it protects — "the optimizer proposes, a human
// approves, and ONLY THEN does this bridge emit commands" — and this platform
// must never acquire the opposite behaviour because someone added a parameter
// and a caller passed the wrong thing.
func TestWithoutTheSwitchNothingIsPublished(t *testing.T) {
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", ordersBody, "alice")
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

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
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

// THE TENANT COMES FROM THE CALLER, per request. One handler serves every tenant
// concurrently; a materializer that carried a tenant set at construction would
// publish one customer's rebalance under another's.
func TestTheMaterializerIsScopedToTheCallersTenant(t *testing.T) {
	s, f := serverWith(&fakeMaterializer{armed: true})

	if rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice"); rec.Code != http.StatusOK {
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

	if rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice"); rec.Code != http.StatusOK {
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

	if rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice"); rec.Code != http.StatusOK {
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

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
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

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: the orders were published; failing here would tell the caller "+
			"the trade did not happen", rec.Code)
	}
	if len(f.published) != 1 {
		t.Errorf("published %d, want 1", len(f.published))
	}
}
