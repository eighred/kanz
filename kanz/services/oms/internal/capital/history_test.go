package capital

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
)

func historyPage(t *testing.T, revision, through int64) cashview.CommitPage {
	t.Helper()
	return cashview.CommitPage{TenantID: "tenant-a", PortfolioID: "fund", Currency: "USD", Through: through, Next: revision, HasMore: revision < through,
		Records: []cashview.CommitRecord{{Revision: revision, Payload: sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Revision = revision })}}}
}

func TestHistoryClientRejectsUnsafeConfiguration(t *testing.T) {
	provider := func(context.Context) (string, error) { return "test", nil }
	for _, endpoint := range []string{"http://gateway", "https://user@gateway", "https://gateway/path", "https://gateway?query=1", "https://gateway#fragment", ""} {
		if _, err := NewHistoryClient(endpoint, nil, provider); err == nil {
			t.Fatalf("accepted unsafe history origin %q", endpoint)
		}
	}
	if _, err := NewHistoryClient("https://gateway", nil, nil); err == nil {
		t.Fatal("accepted absent credential provider")
	}
}

func TestHistoryClientCancellationClosesRequest(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	}))
	defer server.Close()
	client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test", nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), "tenant-a"), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ReadCashCommits(ctx, "fund", "USD", 0, 1, 1); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("request never reached the TLS server")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost request cancellation: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled HTTP request leaked its server handler")
	}
}

func TestHistoryClientRefusesMissingScopeAndCredentialsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	for _, token := range []string{"", "\t", "bad\nvalue", "bad value"} {
		client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return token, nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.ReadCashCommits(bus.WithTenantID(t.Context(), "tenant-a"), "fund", "USD", 0, 1, 1); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadCashCommits(t.Context(), "fund", "USD", 0, 1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing scope: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("unscoped or unauthenticated request reached the network")
	}
}

func TestHistoryClientPreservesExactCursorAndRotatesCredentials(t *testing.T) {
	const revision int64 = 9007199254740993
	page := historyPage(t, revision, revision)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-"+strconv.Itoa(int(call)) || r.URL.Query().Get("after") != "9007199254740992" || r.URL.Query().Get("through") != "9007199254740993" || r.URL.Path != "/v1/portfolios/fund/cash-commits" {
			t.Error("request changed identity, credential rotation or exact cursor")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	var credentials atomic.Int32
	client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test-" + strconv.Itoa(int(credentials.Add(1))), nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx := bus.WithTenantID(t.Context(), "tenant-a")
	for range 2 {
		got, err := client.ReadCashCommits(ctx, "fund", "USD", revision-1, revision, 64)
		if err != nil || got.Next != revision {
			t.Fatalf("exact history: %+v %v", got, err)
		}
	}
}

func TestHistoryClientRefusesContradictoryPages(t *testing.T) {
	for name, change := range map[string]func(*cashview.CommitPage){
		"other tenant":          func(p *cashview.CommitPage) { p.TenantID = "tenant-b" },
		"no tenant":             func(p *cashview.CommitPage) { p.TenantID = "" },
		"other portfolio":       func(p *cashview.CommitPage) { p.PortfolioID = "other" },
		"other currency":        func(p *cashview.CommitPage) { p.Currency = "EUR" },
		"moving head":           func(p *cashview.CommitPage) { p.Through = 4 },
		"skipped cursor":        func(p *cashview.CommitPage) { p.Next = 2 },
		"wrong completion":      func(p *cashview.CommitPage) { p.HasMore = false },
		"empty incomplete page": func(p *cashview.CommitPage) { p.Records = nil; p.Next = 0 },
		"out of order":          func(p *cashview.CommitPage) { p.Records[0].Revision = 2 },
		"foreign payload": func(p *cashview.CommitPage) {
			p.Records[0].Payload = sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.PortfolioId = "other" })
		},
		"different payload revision": func(p *cashview.CommitPage) {
			p.Records[0].Payload = sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Revision = 2 })
		},
		"malformed payload": func(p *cashview.CommitPage) { p.Records[0].Payload = []byte{0xff} },
		"oversized record":  func(p *cashview.CommitPage) { p.Records[0].Payload = make([]byte, (512<<10)+1) },
	} {
		t.Run(name, func(t *testing.T) {
			page := historyPage(t, 1, 3)
			change(&page)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(page) }))
			defer server.Close()
			client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test", nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ReadCashCommits(bus.WithTenantID(t.Context(), "tenant-a"), "fund", "USD", 0, 3, 64); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted contradictory history: %v", err)
			}
		})
	}
}

func TestHistoryClientRefusesRedirectsAndOversizedHTTPBodies(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Store(true) }))
	defer destination.Close()
	for _, mode := range []string{"redirect", "oversized", "refused", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", (6<<20)+1)))
				case "refused":
					http.Error(w, "do not reflect this body", http.StatusForbidden)
				case "truncated":
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("{"))
				}
			}))
			defer server.Close()
			client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test", nil })
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ReadCashCommits(bus.WithTenantID(t.Context(), "tenant-a"), "fund", "USD", 0, 3, 64)
			if err == nil || strings.Contains(err.Error(), "do not reflect") {
				t.Fatalf("unsafe HTTP response handling: %v", err)
			}
			if mode == "truncated" && err.Error() != "capital: history response read failed" {
				t.Fatalf("body transport diagnostic escaped: %v", err)
			}
		})
	}
	if redirected.Load() {
		t.Fatal("credential-bearing request followed redirect")
	}
}

func TestPostgresRetainedRecoveryResumesAfterHTTPFailure(t *testing.T) {
	pool := database(t)
	ctx := bus.WithTenantID(t.Context(), "tenant-a")
	pages := []cashview.CommitPage{historyPage(t, 1, 3), historyPage(t, 2, 3), historyPage(t, 3, 3)}
	if err := ApplyEnvelope(ctx, pool, "tenant-a", cashEnvelope(), pages[2].Records[0].Payload); !errors.Is(err, ErrUnknown) {
		t.Fatalf("missing prefix was not refused: %v", err)
	}
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if err != nil || after < 0 || after > 2 || r.URL.Query().Get("through") != "3" || r.Header.Get("Authorization") != "Bearer test" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if after == 1 && fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(pages[after])
	}))
	defer server.Close()
	client, err := NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverCashPrefix(ctx, pool, client, "fund", "USD", 3); err == nil {
		t.Fatal("HTTP failure was ignored")
	}
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM capital_balances`).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("partial prefix: %d %v", revision, err)
	}
	if err := transaction(pool, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "blocked", "1", coverageBalance().AsOf.AsTime(), time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("gap released admission: %v", err)
	}
	fail.Store(false)
	restarted := tenantPool(t, "tenant-a")
	if err := RecoverCashPrefix(ctx, restarted, client, "fund", "USD", 3); err != nil {
		t.Fatal(err)
	}
	if err := transaction(restarted, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "after-recovery", "1", coverageBalance().AsOf.AsTime(), time.Minute)
	}); err != nil {
		t.Fatalf("complete recovered source unusable: %v", err)
	}
	var receipts int
	if err := restarted.QueryRow(ctx, `SELECT count(*) FROM capital_cash_events`).Scan(&receipts); err != nil || receipts != 3 {
		t.Fatalf("lost immutable receipts: %d %v", receipts, err)
	}
}
