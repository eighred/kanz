package server

import (
	"encoding/json"
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
	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func navUnusableFXTuples() []struct {
	name     string
	bid, ask *commonpb.Decimal
} {
	return []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"negative", &commonpb.Decimal{Coefficient: -100}, &commonpb.Decimal{Coefficient: 110}},
		{"zero", &commonpb.Decimal{}, &commonpb.Decimal{Coefficient: 2}},
		{"crossed", &commonpb.Decimal{Coefficient: 2}, &commonpb.Decimal{Coefficient: 1}},
		{"present-out-of-domain", &commonpb.Decimal{Coefficient: 1, Exponent: 65}, &commonpb.Decimal{Coefficient: 2}},
	}
}

func navTupleQuote(instrument string, at time.Time, bid, ask *commonpb.Decimal) *marketpb.MarketDataEvent {
	return &marketpb.MarketDataEvent{InstrumentId: instrument, EventTime: timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: bid, AskPrice: ask}}}
}

func seedFXTupleLeg(t *testing.T, store ledger.Store, portfolio, leg, currency string, amount int64) {
	t.Helper()
	event := &ledger.Event{EntryID: "fx-tuple-" + portfolio, PortfolioID: portfolio, Effective: navFXTime, Knowledge: navFXTime}
	switch leg {
	case "cash":
		event.Type, event.Cash, event.CashCurrency = ledger.EntryCash, big.NewRat(amount, 1), currency
	case "security":
		event.Type, event.InstrumentID, event.Quantity, event.Price = ledger.EntryTrade, "SAP", big.NewRat(1, 1), big.NewRat(amount, 1)
	case "accrual":
		event.Type, event.Cash, event.CashCurrency = ledger.EntryAccrual, big.NewRat(amount, 1), currency
	default:
		t.Fatal("unknown FX tuple fixture leg")
	}
	if err := store.Append(t.Context(), event, nil); err != nil {
		t.Fatal(err)
	}
}

func fxTupleServer(store ledger.Store, live *fxfeed.LiveFX, tenant string) *Server {
	ready := &Readiness{}
	ready.Set(true)
	return New(ready, nil, store, "USD", WithTenant(tenant), WithLiveFX(live.Converter),
		WithInstrumentCurrency(accounting.InstrumentCurrency{"SAP": "EUR"}))
}

func assertFXTupleNAV(t *testing.T, s *Server, tenant, portfolio, total, missing string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/portfolios/"+portfolio+"/nav", strings.NewReader(`{"prices":{"SAP":"100"}}`))
	auth.SetPrincipalHeaders(req.Header, "fx-tuple-reviewer", tenant, []string{"reviewer"})
	if err := auth.SetPrincipalPortfolios(req.Header, []string{portfolio}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var result struct{ Total, Currency, Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if missing != "" {
		if rec.Code != http.StatusBadRequest || result.Total != "" || !strings.Contains(result.Error, missing) {
			t.Fatalf("unknown FX returned a value or unrelated error: status=%d body=%s", rec.Code, rec.Body.String())
		}
	} else if rec.Code != http.StatusOK || result.Total != total || result.Currency != "USD" {
		t.Fatalf("NAV status=%d body=%s, want USD%s", rec.Code, rec.Body.String(), total)
	}
}

func TestNAVFXQuoteTupleQualificationProtectsEveryValuationLeg(t *testing.T) {
	for _, leg := range []string{"cash", "security", "accrual"} {
		for _, bad := range navUnusableFXTuples() {
			t.Run(leg+"-"+bad.name, func(t *testing.T) {
				store := ledger.NewMemoryStore()
				seedFXTupleLeg(t, store, "PF1", leg, "EUR", 100)
				live := fxfeed.New("USD", map[string]string{"EURUSD": "EUR"})
				s := fxTupleServer(store, live, testTenant)
				feed := func(event *marketpb.MarketDataEvent) {
					t.Helper()
					payload, err := proto.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					if err := live.Handler(t.Context(), &envelopepb.Envelope{EventType: "market.fx.quote"}, payload); err != nil {
						t.Fatalf("tuple disposition changed from nil/ACK: %v", err)
					}
				}
				assertFXTupleNAV(t, s, testTenant, "PF1", "", "EUR")
				feed(navTupleQuote("EURUSD", navFXTime.Add(time.Hour), bad.bid, bad.ask))
				assertFXTupleNAV(t, s, testTenant, "PF1", "", "EUR")
				feed(navTimeQuote("EURUSD", timestamppb.New(navFXTime), 120))
				assertFXTupleNAV(t, s, testTenant, "PF1", "120.00", "")
				frozen := live.Converter()
				feed(navTupleQuote("EURUSD", navFXTime.Add(time.Hour), bad.bid, bad.ask))
				assertFXTupleNAV(t, s, testTenant, "PF1", "120.00", "")
				feed(navTimeQuote("EURUSD", timestamppb.New(navFXTime.Add(time.Second)), 130))
				assertFXTupleNAV(t, s, testTenant, "PF1", "130.00", "")
				if rate, known := frozen.Rate("EUR"); !known || rate.Cmp(big.NewRat(6, 5)) != 0 {
					t.Fatal("served NAV repair changed its prior immutable converter")
				}
			})
		}
	}
}
