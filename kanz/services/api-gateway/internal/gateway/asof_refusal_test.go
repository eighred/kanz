package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// THE LAST LINK: A PINNED QUERY MUST REACH THE CALLER AS A 400 THAT SAYS WHY
// (#859).
//
// The refusal lives in the engine, two hops upstream, and everything between is
// mapping: engine → ErrAsOfNotSupported → grpcsrv mapError → InvalidArgument →
// httpStatus → 400 + message. The middle hop is proven against the real engine
// in services/risk-engine/internal/grpcsrv; this end asserts the two properties
// that are the gateway's own and that nothing upstream can hold for it.
//
// Both matter, and they fail in opposite directions:
//
//   - the gateway must FORWARD as_of, or the refusal never happens and the
//     caller silently receives live state — #859 itself;
//   - the gateway must RELAY the 4xx message, or the caller gets a bare 400 and
//     cannot tell which field to drop, which is the same "you cannot tell"
//     failure one layer out.

// asOfRefusal is the status the real chain produces: risk-engine's mapError
// wraps ErrAsOfNotSupported — which wraps ErrInvalidRequest — into
// codes.InvalidArgument carrying the sentinel's own text. Built from the real
// sentinel rather than a hand-typed string so a reworded refusal cannot leave
// this test asserting a message the platform no longer emits.
var asOfRefusal = status.Error(codes.InvalidArgument, riskv1.ErrAsOfNotSupported.Error())

func TestAPinnedExposureQueryIsForwardedSoItCanBeRefused(t *testing.T) {
	fc := &fakeClient{err: asOfRefusal}
	ts := serve(t, fc)

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure?as_of=2026-08-25T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// FORWARDED. If the gateway dropped as_of the upstream would answer a
	// latest-state query normally, and the caller would receive today's book for
	// a request that asked for last Tuesday — with a 200.
	if fc.gotExposure == nil {
		t.Fatal("the gateway made no upstream call at all")
	}
	if fc.gotExposure.GetAsOf() == nil {
		t.Fatal("the gateway dropped ?as_of= before calling upstream.\n\n" +
			"The engine's refusal can only fire on a field it receives, so dropping it here " +
			"restores #859 exactly: the caller's pin is discarded, the query is answered from " +
			"live state, and the response carries a 200 and a plausible timestamp.")
	}
}

func TestAPinnedQueryIsRefusedWithA400ThatNamesTheField(t *testing.T) {
	for _, path := range []string{
		"/v1/portfolios/PF1/exposure?as_of=2026-08-25T09:00:00Z",
		"/v1/portfolios/PF1/measures?as_of=2026-08-25T09:00:00Z",
	} {
		fc := &fakeClient{err: asOfRefusal}
		ts := serve(t, fc)

		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(readAll(t, resp))
		resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s ⇒ HTTP %d, want 400.\n\nA 5xx would blame the platform and invite a "+
				"retry that cannot help; a 404 would send the caller looking for a portfolio. "+
				"Only a 400 says the caller can fix this.\n\nbody: %s", path, resp.StatusCode, body)
			continue
		}
		// A 4xx describes what the CALLER sent, so httpStatus relays its message —
		// the rule #440 established. Asserted here because a bare 400 leaves the
		// caller guessing which of their fields was the problem, which is the same
		// silence #859 is about, moved one hop.
		if !strings.Contains(body, "as_of") {
			t.Errorf("%s ⇒ 400 whose body does not name the field: %s\n\n"+
				"The caller is told their request was bad and not which part. #440's rule is "+
				"that a 4xx keeps its reason precisely so this does not happen.", path, body)
		}
	}
}

// AN UNPINNED QUERY IS UNAFFECTED. The absent parameter must stay absent
// upstream: parseAsOf returning a zero timestamp rather than nil would make
// every ordinary query look pinned and the engine would refuse all of them.
func TestAnUnpinnedQuerySendsNoAsOfUpstream(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: testTenant}}
	ts := serve(t, fc)

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if fc.gotExposure == nil {
		t.Fatal("no upstream call")
	}
	if fc.gotExposure.GetAsOf() != nil {
		t.Fatalf("an unpinned request carried as_of=%v upstream — the engine refuses any "+
			"non-zero pin, so every ordinary exposure query on the platform would now be "+
			"rejected with a 400", fc.gotExposure.GetAsOf().AsTime())
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unpinned query ⇒ HTTP %d, want 200", resp.StatusCode)
	}
}
