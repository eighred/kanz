package server

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/requestbody"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type numericReadStore struct {
	ledger.Store
	reads atomic.Int64
}

func (s *numericReadStore) LoadSnapshot(ctx context.Context, pf string) (*ledger.Snapshot, error) {
	s.reads.Add(1)
	return s.Store.LoadSnapshot(ctx, pf)
}
func (s *numericReadStore) Journal(ctx context.Context, pf string) ([]*ledger.Event, error) {
	s.reads.Add(1)
	return s.Store.Journal(ctx, pf)
}
func (s *numericReadStore) JournalSince(ctx context.Context, pf string, after int64) ([]*ledger.Event, error) {
	s.reads.Add(1)
	return s.Store.JournalSince(ctx, pf, after)
}

func seedNumericBook(t *testing.T, store ledger.Store) {
	t.Helper()
	for _, entry := range []*ledger.Event{
		{EntryID: "numeric-opening", PortfolioID: "PF", VenueAccountID: "okx-sub-1", Type: ledger.EntryCash, Cash: big.NewRat(100, 1), CashCurrency: "USD", Effective: breakT0, Knowledge: breakT0},
		{EntryID: "numeric-holding", PortfolioID: "PF", VenueAccountID: "okx-sub-1", Type: ledger.EntryTrade, InstrumentID: "AAPL", Quantity: big.NewRat(3, 1), Price: big.NewRat(1, 1), Cash: new(big.Rat), CashCurrency: "USD", Effective: breakT0, Knowledge: breakT0, SourceRef: "numeric-fill"},
	} {
		if err := store.Append(t.Context(), entry, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func numericRequest(t *testing.T, h *httptest.Server, tenant, path, body string, chunked bool, scopes ...string) (int, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "POST", h.URL+"/v1/portfolios/PF/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(r.Header, "numeric-reviewer", tenant, []string{"funder"})
	for _, scope := range scopes {
		r.Header.Add(auth.HeaderPrincipalPortfolios, scope)
	}
	if chunked {
		r.ContentLength = -1
		r.TransferEncoding = []string{"chunked"}
	}
	response, err := h.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

type numericFieldCase struct{ name, path, template, valid, oracle string }

func numericFieldCases() []numericFieldCase {
	return []numericFieldCase{
		{"prices", "nav", `{"prices":{"AAPL":"VALUE"}}`, "1/3", `"total":"101.00"`},
		{"FX", "nav", `{"prices":{"AAPL":"1/3"},"instrument_currency":{"AAPL":"EUR"},"fx":{"EUR":"VALUE"}}`, "0x1.4p0", `"total":"101.25"`},
		{"positions", "reconcile", `{"custodian_id":"CUST-A","positions":{"AAPL":"VALUE"},"cash":{"USD":"100"}}`, "3/1", `"count":0`},
		{"cash", "reconcile", `{"custodian_id":"CUST-A","positions":{"AAPL":"3"},"cash":{"USD":"VALUE"}}`, "100e0", `"count":0`},
		{"tolerance", "reconcile", `{"custodian_id":"CUST-A","positions":{"AAPL":"3.25"},"cash":{"USD":"100"},"tolerance":"VALUE"}`, ".25", `"count":0`},
	}
}

func TestAccountingNumericHTTPFields(t *testing.T) {
	store := &numericReadStore{Store: ledger.NewMemoryStore()}
	seedNumericBook(t, store)
	h := httptest.NewServer(New(&Readiness{}, nil, store, "USD", WithTenant(testTenant), WithCustodyBookScope(testScope(t))))
	defer h.Close()
	for _, field := range numericFieldCases() {
		t.Run(field.name, func(t *testing.T) {
			t.Run("bounded_exact", func(t *testing.T) {
				before := store.reads.Load()
				code, body := numericRequest(t, h, testTenant, field.path, strings.ReplaceAll(field.template, "VALUE", field.valid), false, `["PF"]`)
				if code != 200 || !strings.Contains(body, field.oracle) || store.reads.Load() <= before {
					t.Fatalf("financial oracle: status=%d %s", code, body)
				}
			})
			t.Run("out_of_bounds_before_financial_read", func(t *testing.T) {
				before := store.reads.Load()
				code, body := numericRequest(t, h, testTenant, field.path, strings.ReplaceAll(field.template, "VALUE", "1e155"), false, `["PF"]`)
				if code != 400 || store.reads.Load() != before {
					t.Fatalf("unqualified field reached financial state: status=%d reads=%d %s", code, store.reads.Load()-before, body)
				}
			})
		})
	}
	for _, tc := range []struct{ name, tolerance string }{{"default_tolerance", ""}, {"explicit_zero_tolerance", `,"tolerance":"0"`}} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := numericRequest(t, h, testTenant, "reconcile", `{"custodian_id":"CUST-A","positions":{"AAPL":"3"},"cash":{"USD":"100"}`+tc.tolerance+`}`, false, `["PF"]`)
			if code != 200 || !strings.Contains(body, `"count":0`) {
				t.Fatalf("zero tolerance changed: %d %s", code, body)
			}
		})
	}
}

func TestAccountingNumericRequestBodies(t *testing.T) {
	store := &numericReadStore{Store: ledger.NewMemoryStore()}
	seedNumericBook(t, store)
	h := httptest.NewServer(New(&Readiness{}, nil, store, "USD", WithTenant(testTenant)))
	defer h.Close()
	valid := `{"prices":{"AAPL":"1/3"}}`
	atLimit := valid + strings.Repeat(" ", requestbody.MaxBytes-len(valid))
	for _, tc := range []struct {
		name, body string
		chunked    bool
		status     int
	}{
		{"valid", valid, false, 200},
		{"trailing_whitespace", valid + " \r\n\t", false, 200},
		{"multiple_values_rejected", valid + valid, false, 400},
		{"trailing_junk_rejected", valid + "x", false, 400},
		{"malformed", "{", false, 400},
		{"JSON_number_is_not_string", `{"prices":{"AAPL":1}}`, false, 400},
		{"content_length_over_limit", atLimit + " ", false, 413},
		{"chunked_over_limit", atLimit + " ", true, 413},
		{"exact_body_limit", atLimit, false, 200},
		{"chunked_exact_body_limit", atLimit, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := store.reads.Load()
			code, body := numericRequest(t, h, testTenant, "nav", tc.body, tc.chunked, `["PF"]`)
			if code != tc.status {
				t.Fatalf("status=%d want=%d %s", code, tc.status, body)
			}
			if code != 200 && store.reads.Load() != before {
				t.Fatal("refused body reached financial state")
			}
			if code == 200 && !strings.Contains(body, `"total":"101.00"`) {
				t.Fatalf("valid body oracle %s", body)
			}
		})
	}
}

type numericUnreadBody struct{ read bool }

func (b *numericUnreadBody) Read([]byte) (int, error) { b.read = true; return 0, io.ErrUnexpectedEOF }
func (*numericUnreadBody) Close() error               { return nil }

func TestAccountingNumericAuthorizationBeforeBody(t *testing.T) {
	store := &numericReadStore{Store: ledger.NewMemoryStore()}
	s := New(&Readiness{}, nil, store, "USD", WithTenant(testTenant), WithCustodyBookScope(testScope(t)))
	for _, route := range []string{"nav", "reconcile"} {
		t.Run(route, func(t *testing.T) {
			for _, tc := range []struct {
				name, tenant string
				scopes       []string
			}{
				{"excluded_portfolio", testTenant, []string{`["OTHER"]`}},
				{"foreign_tenant", otherTenant, []string{`["PF"]`}},
				{"malformed_scope", testTenant, []string{`{}`}},
				{"repeated_scope", testTenant, []string{`[]`, `[]`}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := &numericUnreadBody{}
					r := httptest.NewRequest("POST", "/v1/portfolios/PF/"+route, body)
					r.ContentLength = requestbody.MaxBytes + 1
					auth.SetPrincipalHeaders(r.Header, "reviewer", tc.tenant, []string{"funder"})
					for _, scope := range tc.scopes {
						r.Header.Add(auth.HeaderPrincipalPortfolios, scope)
					}
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != 404 || body.read || store.reads.Load() != 0 {
						t.Fatalf("authorization order: status=%d bodyRead=%v financialReads=%d", w.Code, body.read, store.reads.Load())
					}
				})
			}
		})
	}
}
