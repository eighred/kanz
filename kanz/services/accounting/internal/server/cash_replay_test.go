package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresCashHistoryHTTPRequiresTenantPortfolioAndBoundedCursors(t *testing.T) {
	pool := newServerPool(t)
	var bypass bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("requires non-bypass role: %v %v", bypass, err)
	}
	store := ledger.NewPostgres(pool)
	at := time.Now().UTC().Truncate(time.Microsecond)
	e := &ledger.Event{EntryID: "replay:opening", PortfolioID: "PF", Type: ledger.EntryCash, Cash: big.NewRat(250, 1), CashCurrency: "USD", Effective: at, Knowledge: at}
	if err := store.Append(t.Context(), e, func(ctx context.Context, txStore ledger.Store) ([]outbox.Record, error) {
		projection, err := ledger.MaterializeCash(ctx, txStore, "PF", at)
		if err != nil {
			return nil, err
		}
		total, ok := dec.ToProtoExact(projection.Book.CashBalance("USD"))
		if !ok {
			return nil, ledger.ErrCashCommit
		}
		msg := &accountingpb.PortfolioCashBalance{PortfolioId: "PF", BaseCurrency: "USD", Total: total, AsOf: timestamppb.New(at), KnowledgeTime: timestamppb.New(at)}
		return nil, txStore.(*ledger.Postgres).SealCashBalance(ctx, msg, projection)
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(&Readiness{}, nil, store, "USD", WithTenant("__system__")))
	defer srv.Close()
	base := "/v1/portfolios/PF/cash-commits"
	valid := "?currency=USD&after=0&through=0&limit=2"
	for _, tc := range []struct {
		name, tenant, portfolio, query string
		status                         int
	}{
		{"authorized", "__system__", "PF", valid, 200},
		{"anonymous", "", "PF", valid, 404},
		{"foreign tenant", "other", "PF", valid, 404},
		{"foreign portfolio", "__system__", "OTHER", valid, 404},
		{"unentitled malformed cursor", "other", "PF", "?after=no", 404},
		{"missing bounds", "__system__", "PF", "?currency=USD", 400},
		{"ambiguous cursor", "__system__", "PF", valid + "&after=1", 400},
		{"negative cursor", "__system__", "PF", "?currency=USD&after=-1&through=0&limit=2", 400},
		{"future watermark", "__system__", "PF", "?currency=USD&after=0&through=2&limit=2", 400},
		{"unbounded page", "__system__", "PF", "?currency=USD&after=0&through=0&limit=65", 400},
		{"integer overflow", "__system__", "PF", "?currency=USD&after=9223372036854775808&through=0&limit=2", 400},
		{"unknown currency", "__system__", "PF", "?currency=EUR&after=0&through=0&limit=2", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+base+tc.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			auth.SetPrincipalHeaders(r.Header, "operator", tc.tenant, nil)
			if err := auth.SetPrincipalPortfolios(r.Header, []string{tc.portfolio}); err != nil {
				t.Fatal(err)
			}
			response, err := srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != tc.status || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, err)
			}
			if tc.status != 200 {
				if bytes.Contains(body, []byte("payload")) {
					t.Fatal("refusal leaked financial evidence")
				}
				return
			}
			var page ledger.CashCommitPage
			if err := json.Unmarshal(body, &page); err != nil || page.Next != 1 || page.Through != 1 || page.HasMore || len(page.Records) != 1 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			var retained []byte
			if err := pool.QueryRow(t.Context(), `SELECT payload FROM cash_commit_history WHERE portfolio_id='PF' AND currency='USD' AND revision=1`).Scan(&retained); err != nil || !bytes.Equal(retained, page.Records[0].Payload) {
				t.Fatalf("HTTP changed retained evidence: %v", err)
			}
		})
	}
	// History reads must not generate fresh cash facts or source revisions.
	var head, queued int64
	if err := pool.QueryRow(t.Context(), `SELECT revision FROM cash_commit_heads WHERE portfolio_id='PF'`).Scan(&head); err != nil || head != 1 {
		t.Fatalf("read changed source revision: %d %v", head, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("read republished facts: %d %v", queued, err)
	}
}

func TestCashHistoryRefusesNonDurableStore(t *testing.T) {
	s, _ := newServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF/cash-commits?currency=USD&after=0&through=0&limit=2", nil)
	auth.SetPrincipalHeaders(r.Header, "operator", testTenant, nil)
	if err := auth.SetPrincipalPortfolios(r.Header, []string{"PF"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-durable store claimed replay evidence: %d %s", w.Code, w.Body.String())
	}
}
