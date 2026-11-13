package server

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var navFXTime = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func navTimeQuote(instrument string, at *timestamppb.Timestamp, coefficient int64) *marketpb.MarketDataEvent {
	return &marketpb.MarketDataEvent{InstrumentId: instrument, Symbol: "EURUSD", Mic: "TEST", EventTime: at,
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: &commonpb.Decimal{Coefficient: coefficient, Exponent: -2}, AskPrice: &commonpb.Decimal{Coefficient: coefficient, Exponent: -2},
			BidSize: &commonpb.Decimal{Coefficient: 1}, AskSize: &commonpb.Decimal{Coefficient: 1},
		}}}
}

func seedFXTimeCash(t *testing.T, store ledger.Store) {
	t.Helper()
	if err := store.Append(t.Context(), &ledger.Event{EntryID: "fx-time-cash", PortfolioID: "PF1", Type: ledger.EntryCash,
		Cash: big.NewRat(100, 1), CashCurrency: "EUR", Effective: navFXTime, Knowledge: navFXTime}, nil); err != nil {
		t.Fatal(err)
	}
}

func serveFXTimeNAV(t *testing.T, store ledger.Store, live *fxfeed.LiveFX, tenant string) *httptest.Server {
	t.Helper()
	ready := &Readiness{}
	ready.Set(true)
	web := httptest.NewServer(New(ready, nil, store, "USD", WithTenant(tenant), WithLiveFX(live.Converter)))
	t.Cleanup(web.Close)
	return web
}

func assertFXTimeNAV(t *testing.T, web *httptest.Server, tenant, total string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+"/v1/portfolios/PF1/nav", strings.NewReader(`{"prices":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(req.Header, "fx-time-reviewer", tenant, []string{"reviewer"})
	if err := auth.SetPrincipalPortfolios(req.Header, []string{"PF1"}); err != nil {
		t.Fatal(err)
	}
	response, err := web.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Total, Error string }
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if total == "" {
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(result.Error, "EUR") || result.Total != "" {
			t.Fatalf("unknown FX produced a clean or unrelated NAV: status=%d body=%s", response.StatusCode, body)
		}
	} else if response.StatusCode != http.StatusOK || result.Total != total {
		t.Fatalf("EUR100 valuation got status=%d body=%s want USD%s", response.StatusCode, body, total)
	}
}

func TestNAVFXTimeRefusesConflictAndPreservesTemporalValuation(t *testing.T) {
	live := fxfeed.New("USD", map[string]string{"EURUSD": "EUR"})
	store := ledger.NewMemoryStore()
	seedFXTimeCash(t, store)
	web := serveFXTimeNAV(t, store, live, testTenant)
	assertFXTimeNAV(t, web, testTenant, "")
	for _, tc := range []struct {
		name  string
		at    time.Time
		rate  int64
		err   bool
		total string
	}{
		{"first", navFXTime, 120, false, "120.00"},
		{"older", navFXTime.Add(-time.Second), 110, false, "120.00"},
		{"same-time replay", navFXTime, 120, false, "120.00"},
		{"same-time conflict", navFXTime, 125, true, ""},
		{"older after conflict", navFXTime.Add(-time.Second), 110, false, ""},
		{"same-time cannot heal", navFXTime, 120, true, ""},
		{"newer repair", navFXTime.Add(time.Second), 130, false, "130.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := proto.Marshal(navTimeQuote("EURUSD", timestamppb.New(tc.at), tc.rate))
			if err != nil {
				t.Fatal(err)
			}
			err = live.Handler(t.Context(), &envelopepb.Envelope{EventType: "market.fx.quote"}, payload)
			if (err != nil) != tc.err {
				t.Fatalf("FX observation error=%v want refusal=%v", err, tc.err)
			}
			assertFXTimeNAV(t, web, testTenant, tc.total)
		})
	}
}
