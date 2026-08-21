package server

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/regulatory"
	"github.com/eighred/kanz/internal/regulatory/frtb"
	"github.com/eighred/kanz/internal/sustainability"
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
//
// This is a COMPLETENESS test and nothing more. It asserts 200-and-a-signature
// over an all-zero filing, and 0 is the one value for which every rendering of a
// big.Rat agrees — so it stayed green for the whole life of #633. The rendering
// contract is TestClimateFilingServesTheDecimalItSigned below; do not read this
// one as covering it.
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

// plainDecimal is what a filed number must look like on the wire: an optionally
// signed integer with an optional fractional part. Nothing else. A regulator's
// parser is a decimal parser.
var plainDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// TestClimateFilingServesTheDecimalItSigned is #633's verification.
//
// The old TestClimateDisclosures posted an EMPTY book, and every line item came
// back 0 — the one value for which big.Rat's RatString() and dec.Str() agree. So
// it asserted 200-and-a-signature over the only fixture structurally incapable of
// showing the defect: with a real book, climateReport handed the *big.Rat to
// encoding/json, which marshals it through TextMarshaler == RatString(), and a
// WACI of 1234.5678 was SERVED as "5429686605511341/4398046511104" while the
// signature in the same response committed to "1234.5678".
//
// The test therefore does what the recipient of a signed filing must be able to
// do and could not: read the values, check they are decimals, re-canonicalize
// them, and reproduce the signature the filing carries. It signs with the
// content-hash HashSigner rather than the chain signer for exactly that reason —
// a recipient can run SHA-256; it cannot run our audit chain.
//
// The expected strings are derived from the model arithmetic in float64, outside
// Go, not read off this implementation:
//
//	WACI      = (|MV|/Σ|MV|)·((S1+S2)/Revenue) = 1·(1234567.8/1000)      = 1234.5678
//	FINANCED  = (MV/EVIC)·(S1+S2+S3) = 0.5·1234667.8                     = 617333.9
//	TEMP_RISE = 1.5 + 3·((617333.9 − 500)/500); target(2035) = 500       = 3702.5034
//	CLIMATE_VAR = |MV|·|−(0.5·1234567.8)/2000000 − 0.125|                = 433641.95
//	FOOTPRINT = FINANCED / (Σ|MV|/1e6) = 617333.9/1                      = 617333.9
//	FOSSIL    = 1000000/1000000                                          = 1
//
// TEMP_RISE is the interesting one: the double is 3702.5033999999996, and dec.Str
// renders at the platform's fixed scale of 8, half-up — so "3702.5034" is the
// filed figure and the signed figure both.
func TestClimateFilingServesTheDecimalItSigned(t *testing.T) {
	ready := &Readiness{}
	ready.Set(true)
	s := New(ready, nil, filing.HashSigner{})

	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	in := sustainability.DisclosureInputs{
		Holdings: []sustainability.Holding{{
			InstrumentID: "ISS-1",
			MarketValue:  1000000,
			Carbon: sustainability.CarbonMetrics{
				Scope1: 1234567.8, Scope3: 100, Revenue: 1000, EVIC: 2000000,
			},
		}},
		GlidePath:       sustainability.GlidePath{BaseYear: 2020, TargetYear: 2050, BaseEmissions: 1000},
		Year:            2035,
		Scenario:        sustainability.ClimateScenario{CarbonPrice: 0.5, PhysicalSeverity: 0.125},
		TempSensitivity: 3,
		FossilFuel:      map[string]bool{"ISS-1": true},
	}

	for _, tc := range []struct {
		path  string
		want  map[string]string
		frame string
	}{
		{
			path:  "/v1/filings/tcfd",
			frame: "TCFD",
			want: map[string]string{
				"TCFD_WACI":               "1234.5678",
				"TCFD_FINANCED_EMISSIONS": "617333.9",
				"TCFD_IMPLIED_TEMP_RISE":  "3702.5034",
				"TCFD_CLIMATE_VAR":        "433641.95",
			},
		},
		{
			path:  "/v1/filings/sfdr",
			frame: "SFDR",
			want: map[string]string{
				"SFDR_GHG_INTENSITY":        "1234.5678",
				"SFDR_CARBON_FOOTPRINT":     "617333.9",
				"SFDR_FOSSIL_FUEL_EXPOSURE": "1",
			},
		},
	} {
		t.Run(tc.frame, func(t *testing.T) {
			rec := post(t, s, tc.path, disclosureRequest{AsOf: &at, DisclosureInputs: in})
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200 got %d (%s)", rec.Code, rec.Body.String())
			}

			// The whole point: the recipient parses the body it was served. If a
			// value arrives as a JSON number this decode fails, which is also the
			// contract — money and quantities are never JSON floats.
			var out struct {
				Framework string `json:"framework"`
				AsOf      string `json:"as_of"`
				Signature string `json:"signature"`
				LineItems []struct {
					Code  string `json:"code"`
					Label string `json:"label"`
					Value string `json:"value"`
				} `json:"line_items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("the served filing does not decode as decimal strings: %v\nbody: %s", err, rec.Body.String())
			}
			if out.Framework != tc.frame || out.Signature == "" {
				t.Fatalf("framework=%q signature=%q", out.Framework, out.Signature)
			}
			if len(out.LineItems) != len(tc.want) {
				t.Fatalf("line items = %d, want %d (complete)", len(out.LineItems), len(tc.want))
			}

			// Rebuild the filing from what was SERVED, then re-sign it. A recipient
			// with the body and nothing else must land on the signature the body
			// carries.
			parsedAsOf, err := time.Parse(time.RFC3339, out.AsOf)
			if err != nil {
				t.Fatalf("as_of %q does not parse as RFC-3339: %v", out.AsOf, err)
			}
			served := filing.Report{Framework: out.Framework, AsOf: parsedAsOf}
			for _, li := range out.LineItems {
				want, ok := tc.want[li.Code]
				if !ok {
					t.Fatalf("unexpected line item %q", li.Code)
				}
				if !plainDecimal.MatchString(li.Value) {
					t.Errorf("%s served as %q — that is not a decimal. big.Rat marshals as "+
						"RatString(), so an unrouted renderer files the rational a/b form while the "+
						"signature commits to the decimal, and the filing cannot be verified by the "+
						"party that received it (#633)", li.Code, li.Value)
					continue
				}
				if li.Value != want {
					t.Errorf("%s served as %q, want %q", li.Code, li.Value, want)
				}
				v, ok := new(big.Rat).SetString(li.Value)
				if !ok {
					t.Fatalf("%s: %q is not a rational at all", li.Code, li.Value)
				}
				served.LineItems = append(served.LineItems, filing.LineItem{Code: li.Code, Label: li.Label, Value: v})
			}
			if t.Failed() {
				return
			}

			rederived, err := filing.HashSigner{}.Sign(served.Canonical())
			if err != nil {
				t.Fatalf("re-sign: %v", err)
			}
			if rederived != out.Signature {
				t.Fatalf("the filing does not verify: re-deriving the signature from the served body "+
					"gives %s, the body carries %s.\ncanonical bytes re-derived from the body:\n%s",
					rederived, out.Signature, served.Canonical())
			}
		})
	}
}
