package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kanz-eng/kanz/internal/audit/signer"
	"github.com/kanz-eng/kanz/internal/regulatory"
	"github.com/kanz-eng/kanz/internal/regulatory/frtb"
	"github.com/kanz-eng/kanz/internal/sustainability"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, signer.New(""))
}

// post marshals body to JSON, POSTs it, and returns the recorder.
func post(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec
}

func TestHealthAndReady(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", path, rec.Code)
		}
	}
}

// A complete Form PF input yields a signed, completeness-gated filing.
func TestFormPFFiling(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/formpf", map[string]any{
		"GrossAssetValue": "1000000",
		"NetAssetValue":   "900000",
		"VaR":             "50000",
		"GrossExposure":   "1500000",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Framework string           `json:"framework"`
		Signature string           `json:"signature"`
		LineItems []map[string]any `json:"line_items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Framework != "FORM_PF" {
		t.Fatalf("framework = %q, want FORM_PF", out.Framework)
	}
	if out.Signature == "" {
		t.Fatal("filing is unsigned")
	}
	if len(out.LineItems) != 4 {
		t.Fatalf("line items = %d, want 4 (complete)", len(out.LineItems))
	}
}

// A book error (gross NAV below net) is refused, never signed.
func TestFormPFRejectsGrossBelowNet(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/formpf", map[string]any{
		"GrossAssetValue": "800000", "NetAssetValue": "900000", "VaR": "1", "GrossExposure": "1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d (%s)", rec.Code, rec.Body.String())
	}
}

// AIFMD leverage is undefined for a non-positive NAV — the filing is refused.
func TestAIFMDRejectsZeroNAV(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/aifmd", map[string]any{
		"NAV": "0", "GrossExposure": "1", "CommitmentExposure": "1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestAIFMDFiling(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/aifmd", map[string]any{
		"NAV": "1000000", "GrossExposure": "2500000", "CommitmentExposure": "1800000",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	// The breakdown comes back as exact decimal STRINGS, not JSON floats. Leverage
	// is a compliance threshold — a regulator must not have to guess which double
	// we meant.
	var out struct {
		Breakdown map[string]string `json:"breakdown"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Breakdown["gross_leverage"] != "2.5" {
		t.Fatalf("gross leverage = %q, want \"2.5\"", out.Breakdown["gross_leverage"])
	}
}

// TestMoneyMustNotArriveAsAJSONFloat pins the contract this change bought.
//
// Money on this API is an EXACT decimal string. A JSON number is an IEEE-754
// double by definition, so accepting one would silently round the ledger's figure
// on its way into a regulatory filing — and a filed NAV that does not reconcile to
// the book of record is a reportable discrepancy. The API refuses it outright
// rather than filing a number nobody chose.
func TestMoneyMustNotArriveAsAJSONFloat(t *testing.T) {
	rec := post(t, newServer(t), "/v1/filings/formpf", map[string]any{
		"GrossAssetValue": 1000000.0, // a JSON float — not acceptable for money
		"NetAssetValue":   "900000",
		"VaR":             "50000",
		"GrossExposure":   "1500000",
	})
	if rec.Code == http.StatusOK {
		t.Fatal("the API accepted a JSON float for a money field — the ledger's exact figure would be rounded into the filing")
	}
}

// FRTB assembles the SBM+DRC+RRAO total into a signed capital filing. Empty
// sensitivities with valid supervisory params yield a complete zero-charge
// filing (the total is present, so it is complete by construction).
func TestFRTBFiling(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/frtb", frtbRequest{
		FRTBInputs: regulatory.FRTBInputs{Params: frtb.DefaultParams()},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Framework string             `json:"framework"`
		Signature string             `json:"signature"`
		Breakdown map[string]float64 `json:"breakdown"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Framework != "FRTB" || out.Signature == "" {
		t.Fatalf("framework=%q signature=%q", out.Framework, out.Signature)
	}
	if _, ok := out.Breakdown["total"]; !ok {
		t.Fatal("breakdown missing total")
	}
}

// Invalid FRTB supervisory params are refused before a capital number is signed.
func TestFRTBRejectsInvalidParams(t *testing.T) {
	s := newServer(t)
	rec := post(t, s, "/v1/filings/frtb", regulatory.FRTBInputs{}) // empty params
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TCFD/SFDR disclosures assemble from the live holdings surface; an empty book
// still yields a complete (all-zero) disclosure.
func TestClimateDisclosures(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{"/v1/filings/tcfd", "/v1/filings/sfdr"} {
		rec := post(t, s, path, disclosureRequest{
			DisclosureInputs: sustainability.DisclosureInputs{Year: 2030},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d (%s)", path, rec.Code, rec.Body.String())
		}
		var out struct {
			Signature string `json:"signature"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Signature == "" {
			t.Fatalf("%s: unsigned", path)
		}
	}
}

// Successive filings chain: each signature is a distinct position in the audit
// hash chain the ChainSigner advances.
func TestSignaturesAdvanceChain(t *testing.T) {
	s := newServer(t)
	sig := func() string {
		rec := post(t, s, "/v1/filings/aifmd", map[string]any{
			"NAV": "1", "GrossExposure": "1", "CommitmentExposure": "1",
		})
		var out struct {
			Signature string `json:"signature"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Signature
	}
	if a, b := sig(), sig(); a == b {
		t.Fatal("identical inputs produced the same signature — the chain did not advance")
	}
}
