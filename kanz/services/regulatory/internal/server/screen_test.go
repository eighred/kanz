package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/compliance"
)

// THE ESG SCREEN NOW HAS A CALLER (#751 item 4).
//
// internal/sustainability.Screen was complete and unreached: this service
// imported the package for FileTCFD and FileSFDR only, mounted no screening
// route, and nothing else imported it — so an ESG exclusion policy had nowhere
// to be submitted. These tests are that caller's contract.

// staticClassifier resolves a fixed instrument→sector/issuer map. It stands in
// for the reference-data cache the composition root wires, so the SERVER does
// the classifying — the caller sends raw holdings and never a bucket.
type staticClassifier struct{ bySector, byIssuer map[string]string }

func (c staticClassifier) Classify(_ context.Context, instrumentID string, _ time.Time) (compliance.Attributes, bool) {
	a := compliance.Attributes{
		Sector: c.bySector[instrumentID],
		Issuer: c.byIssuer[instrumentID],
	}
	// ok=false for an instrument the master does not carry, which is what a real
	// cache reports for an unknown id — not an empty Attributes with ok=true.
	return a, a.Sector != "" || a.Issuer != ""
}

func screenServer(t *testing.T, cl compliance.Classifier) *Server {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	opts := []Option{}
	if cl != nil {
		opts = append(opts, WithClassifier(cl))
	}
	return New(r, nil, signer.New(""), opts...)
}

func decodeResult(t *testing.T, body string) *compliancepb.ComplianceResult {
	t.Helper()
	var res compliancepb.ComplianceResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode result: %v (body=%s)", err, body)
	}
	return &res
}

func tobaccoClassifier() staticClassifier {
	return staticClassifier{
		bySector: map[string]string{"BAT": "TOBACCO", "AAPL": "TECH"},
		byIssuer: map[string]string{"BAT": "ISS-BAT", "AAPL": "ISS-APPLE"},
	}
}

// A BOOK HOLDING AN EXCLUDED SECTOR BREACHES, and the violation names the
// instrument — which is the only part an operator can act on.
func TestESGScreen_AnExcludedSectorBreaches(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "AAPL", "quantity": "10", "market_value": "100000", "currency": "USD"},
			{"instrument_id": "BAT", "quantity": "5", "market_value": "50000", "currency": "USD"},
		},
		"excluded_sectors": []string{"TOBACCO"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeResult(t, rec.Body.String())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status = %v, want BREACH — the book holds TOBACCO under a TOBACCO exclusion: %s",
			res.GetStatus(), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BAT") {
		t.Errorf("the violation does not name the offending instrument: %s", rec.Body.String())
	}
}

// NON-VACUITY: a clean book passes. Without this the assertion above is
// satisfied by a route that breaches on everything.
func TestESGScreen_ACleanBookPasses(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "AAPL", "quantity": "10", "market_value": "100000", "currency": "USD"},
		},
		"excluded_sectors": []string{"TOBACCO"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if res := decodeResult(t, rec.Body.String()); res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("status = %v, want PASS for a tech-only book: %s", res.GetStatus(), rec.Body.String())
	}
}

// THE POLICY MUST ACTUALLY ARRIVE. sustainability.ExclusionPolicy carries no json
// tags, so decoding "excluded_sectors" straight into it would leave it EMPTY and
// every book would pass — the worst failure available to a control whose whole
// output is "you hold something you may not". The request type carries its own
// tags for that reason; this is the test that says so.
func TestESGScreen_ThePolicyIsNotSilentlyEmpty(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "BAT", "quantity": "5", "market_value": "50000", "currency": "USD"},
		},
		"excluded_issuers": []string{"ISS-BAT"},
	})
	res := decodeResult(t, rec.Body.String())
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("an issuer exclusion naming the only holding PASSED — the policy arrived empty, "+
			"which means the wire name and the struct field stopped matching: %s", rec.Body.String())
	}
}

// NO CLASSIFIER IS A REFUSAL, NOT A PASS (#640). This is the production posture
// of a deployment with no reference-data source, and it must not read as "your
// book holds nothing excluded".
func TestESGScreen_WithoutAClassifierTheScreenRefusesRatherThanPasses(t *testing.T) {
	s := screenServer(t, nil)
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "BAT", "quantity": "5", "market_value": "50000", "currency": "USD"},
		},
		"excluded_sectors": []string{"TOBACCO"},
	})
	res := decodeResult(t, rec.Body.String())
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a sector exclusion PASSED with no classifier wired — the dimension cannot be "+
			"resolved on this deployment and must not read as satisfied: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot be verified") {
		t.Errorf("the refusal does not say the check could not be made: %s", rec.Body.String())
	}
}

// AN UNPRICED HOLDING IS REFUSED, NOT SCREENED AS WORTHLESS (#760). Omitting
// market_value is not the same as sending zero, and the engine says so — this
// route inherits that rather than substituting a number of its own.
func TestESGScreen_AnUnpricedHoldingIsRefused(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "AAPL", "quantity": "10", "market_value": "100000", "currency": "USD"},
			{"instrument_id": "BAT", "quantity": "5"}, // no market_value
		},
		"excluded_sectors": []string{"TOBACCO"},
	})
	res := decodeResult(t, rec.Body.String())
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a book carrying a holding nobody priced PASSED: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no market value") {
		t.Errorf("the refusal does not name the unpriced holding as the cause: %s", rec.Body.String())
	}
}

// AN UNPARSEABLE DECIMAL IS A 400, NEVER A ZERO. Substituting one would silently
// shrink the book a compliance screen runs over.
func TestESGScreen_AnUnreadableDecimalIsRefusedAtTheEdge(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id":  "p1",
		"base_currency": "USD",
		"positions": []map[string]any{
			{"instrument_id": "BAT", "quantity": "5", "market_value": "not-a-number", "currency": "USD"},
		},
		"excluded_sectors": []string{"TOBACCO"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unreadable market_value: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "market_value") {
		t.Errorf("the error does not name the field: %s", rec.Body.String())
	}
}

func TestESGScreen_PortfolioIDIsRequired(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{"base_currency": "USD"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no portfolio_id", rec.Code)
	}
}
