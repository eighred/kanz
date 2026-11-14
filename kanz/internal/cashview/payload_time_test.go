package cashview_test

import (
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var cashTimeBase = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func malformedCashTimes(at time.Time) []struct {
	name string
	at   *timestamppb.Timestamp
} {
	return []struct {
		name string
		at   *timestamppb.Timestamp
	}{
		{"negative-nanos", &timestamppb.Timestamp{Seconds: at.Unix(), Nanos: -1}},
		{"nanos-billion", &timestamppb.Timestamp{Seconds: at.Unix(), Nanos: 1_000_000_000}},
		{"seconds-before-minimum", &timestamppb.Timestamp{Seconds: -62_135_596_801}},
		{"seconds-after-maximum", &timestamppb.Timestamp{Seconds: 253_402_300_800}},
	}
}

func cashTimeReport(portfolio string, amount *commonpb.Decimal, at *timestamppb.Timestamp) *accountingpb.PortfolioCashBalance {
	return &accountingpb.PortfolioCashBalance{PortfolioId: portfolio, BaseCurrency: "USD", Total: amount, AsOf: at,
		KnowledgeTime: timestamppb.New(cashTimeBase), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1},
		Completeness:   &accountingpb.BalanceCompleteness{ProducedEntryTypes: []string{"cash"}},
		ByVenueAccount: []*accountingpb.VenueAccountCash{{VenueAccountId: "account", Asset: "USD", Amount: &commonpb.Decimal{Coefficient: -25, Exponent: -1}}}}
}

func cashTimePayload(t *testing.T, msg *accountingpb.PortfolioCashBalance) []byte {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func foldCashTime(t *testing.T, view *cashview.View, msg *accountingpb.PortfolioCashBalance) {
	t.Helper()
	if err := view.Handle(t.Context(), nil, cashTimePayload(t, msg)); err != nil {
		t.Fatalf("cash no-op/ACK disposition changed: %v", err)
	}
}

func TestCashPayloadTimeMalformedPresentHasNoStateEffects(t *testing.T) {
	for _, state := range []string{"cold", "warm", "warm-expired"} {
		now := cashTimeBase
		if state == "warm-expired" {
			now = now.Add(6 * time.Minute)
		}
		for _, tc := range malformedCashTimes(now) {
			t.Run(tc.name+"-"+state, func(t *testing.T) {
				clock := now
				updates := map[string]int{}
				view := cashview.New(cashview.WithClock(func() time.Time { return clock }),
					cashview.WithOnUpdate(func(pf string) { updates[pf]++ }))
				prior := cashTimeReport("PF", &commonpb.Decimal{}, timestamppb.New(cashTimeBase))
				if state != "cold" {
					foldCashTime(t, view, prior)
				}
				foldCashTime(t, view, cashTimeReport("OTHER", &commonpb.Decimal{Coefficient: 42}, timestamppb.New(clock)))
				before := updates["PF"]
				bad := cashTimeReport("PF", &commonpb.Decimal{Coefficient: 750}, tc.at)
				bad.BaseCurrency = "EUR"
				bad.CurrencyCoverage = &domainpb.InputCoverage{ExcludedCount: 1}
				bad.ExcludedCurrencies = []string{"EUR"}
				bad.Completeness = &accountingpb.BalanceCompleteness{UnproducedEntryTypes: []string{"corporate_action"}}
				bad.ByVenueAccount = nil
				foldCashTime(t, view, bad)
				if updates["PF"] != before {
					t.Fatal("malformed present AsOf fired the update callback")
				}
				report, known := view.Lookup("PF")
				if state != "warm" {
					if known {
						t.Fatal("malformed timestamp established or revived a current balance")
					}
					if _, _, _, ok := view.Spendable("PF"); ok {
						t.Fatal("malformed timestamp converted UNKNOWN into spendable cash")
					}
				} else {
					if !known || !proto.Equal(report.Total, prior.Total) || report.Currency != "USD" || !report.AsOf.Equal(cashTimeBase) ||
						!proto.Equal(report.CurrencyCoverage, prior.CurrencyCoverage) || len(report.ExcludedCurrencies) != 0 ||
						len(report.ByVenueAccount) != 1 || !proto.Equal(report.ByVenueAccount["account"]["USD"], prior.ByVenueAccount[0].Amount) {
						t.Fatal("malformed timestamp changed the prior balance, original age, coverage or account attribution")
					}
					total, currency, completeness, ok := view.Spendable("PF")
					if !ok || total.GetCoefficient() != 0 || currency != "USD" || !completeness.Stated() || completeness.Incomplete() {
						t.Fatal("malformed timestamp changed prior measured zero/completeness")
					}
				}
				if other, ok := view.Lookup("OTHER"); !ok || other.Total.GetCoefficient() != 42 || updates["OTHER"] != 1 {
					t.Fatal("discard affected an unrelated portfolio")
				}
				if state == "warm" {
					clock = cashTimeBase.Add(cashview.DefaultMaxAge + time.Nanosecond)
					if _, _, _, ok := view.Spendable("PF"); ok {
						t.Fatal("discard extended the original cash level's lifetime")
					}
				}
				foldCashTime(t, view, cashTimeReport("PF", &commonpb.Decimal{Coefficient: 120001, Exponent: -3}, timestamppb.New(clock)))
				total, _, _, ok := view.Spendable("PF")
				if !ok || dec.FromProto(total).Cmp(big.NewRat(120001, 1000)) != 0 || updates["PF"] != before+1 {
					t.Fatal("valid exact repair did not restore the balance/update callback")
				}
			})
		}
	}
}

func TestCashPayloadTimePreservesValidAndLegacyPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, exact string
		at          *timestamppb.Timestamp
		amount      *commonpb.Decimal
		maxAge      time.Duration
		known       bool
	}{
		{"nil-unbounded", "750", nil, &commonpb.Decimal{Coefficient: 750}, 0, true},
		{"nil-bounded", "", nil, &commonpb.Decimal{Coefficient: 750}, cashview.DefaultMaxAge, false},
		{"valid-epoch", "750", &timestamppb.Timestamp{}, &commonpb.Decimal{Coefficient: 750}, 0, true},
		{"valid-minimum", "750", &timestamppb.Timestamp{Seconds: -62_135_596_800}, &commonpb.Decimal{Coefficient: 750}, 0, true},
		{"valid-maximum", "750", &timestamppb.Timestamp{Seconds: 253_402_300_799, Nanos: 999_999_999}, &commonpb.Decimal{Coefficient: 750}, 0, true},
		{"valid-fractional-exact", "1200000000000000001/1000000000000000000", timestamppb.New(cashTimeBase.Add(123456789)), &commonpb.Decimal{Coefficient: 1200000000000000001, Exponent: -18}, cashview.DefaultMaxAge, true},
		{"valid-zero-amount", "0", timestamppb.New(cashTimeBase), &commonpb.Decimal{}, cashview.DefaultMaxAge, true},
		{"valid-negative-amount", "-5/2", timestamppb.New(cashTimeBase), &commonpb.Decimal{Coefficient: -25, Exponent: -1}, cashview.DefaultMaxAge, true},
		{"valid-future", "750", timestamppb.New(cashTimeBase.Add(24 * time.Hour)), &commonpb.Decimal{Coefficient: 750}, cashview.DefaultMaxAge, true},
		{"valid-old", "", timestamppb.New(cashTimeBase.Add(-time.Hour)), &commonpb.Decimal{Coefficient: 750}, cashview.DefaultMaxAge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates := 0
			view := cashview.New(cashview.WithClock(func() time.Time { return cashTimeBase }), cashview.WithMaxAge(tc.maxAge),
				cashview.WithOnUpdate(func(string) { updates++ }))
			foldCashTime(t, view, cashTimeReport("PF", tc.amount, tc.at))
			report, known := view.Lookup("PF")
			if known != tc.known || updates != 1 {
				t.Fatalf("valid/legacy policy changed: known=%v updates=%d", known, updates)
			}
			total, _, completeness, spendable := view.Spendable("PF")
			if spendable != tc.known {
				t.Fatal("Lookup and Spendable disagree about timestamp availability")
			}
			if tc.known {
				want, ok := new(big.Rat).SetString(tc.exact)
				if !ok || dec.FromProto(total).Cmp(want) != 0 || !report.AsOf.Equal(tc.at.AsTime()) || !completeness.Stated() || completeness.Incomplete() {
					t.Fatal("valid/legacy time or exact amount changed")
				}
			}
		})
	}
}

func TestCashPayloadTimeArrivalAndQualificationRemainUnchanged(t *testing.T) {
	for _, name := range []string{"older-valid-replaces", "same-time-replaces", "duplicate-valid-callback", "unstated-completeness", "incomplete-completeness", "unknown-currency-coverage", "excluded-currency", "measured-empty", "decimal-domain-refusal"} {
		t.Run(name, func(t *testing.T) {
			now, updates := cashTimeBase.Add(time.Minute), 0
			view := cashview.New(cashview.WithClock(func() time.Time { return now }), cashview.WithOnUpdate(func(string) { updates++ }))
			prior := cashTimeReport("PF", &commonpb.Decimal{Coefficient: 100}, timestamppb.New(now))
			foldCashTime(t, view, prior)
			next := cashTimeReport("PF", &commonpb.Decimal{Coefficient: 250}, timestamppb.New(now))
			want, wantUpdates, spendable := int64(250), 2, true
			switch name {
			case "older-valid-replaces":
				next.AsOf = timestamppb.New(cashTimeBase)
			case "duplicate-valid-callback":
				next = proto.Clone(prior).(*accountingpb.PortfolioCashBalance)
				want = 100
			case "unstated-completeness":
				next.Completeness = nil
			case "incomplete-completeness":
				next.Completeness.UnproducedEntryTypes = []string{"accrual"}
			case "unknown-currency-coverage":
				next.CurrencyCoverage, spendable = nil, false
			case "excluded-currency":
				next.CurrencyCoverage = &domainpb.InputCoverage{ExcludedCount: 1}
				next.ExcludedCurrencies, spendable = []string{"EUR"}, false
			case "measured-empty":
				next.Total, next.CurrencyCoverage, want = &commonpb.Decimal{}, &domainpb.InputCoverage{}, 0
			case "decimal-domain-refusal":
				next.Total = &commonpb.Decimal{Coefficient: 1, Exponent: 65}
				want, wantUpdates = 100, 1
			}
			foldCashTime(t, view, next)
			report, ok := view.Lookup("PF")
			if !ok || report.Total.GetCoefficient() != want || updates != wantUpdates {
				t.Fatal("valid arrival/qualification or Decimal-domain behavior changed")
			}
			_, _, completeness, ok := view.Spendable("PF")
			if ok != spendable {
				t.Fatal("currency coverage no longer qualifies the actual cash level")
			}
			if name == "older-valid-replaces" && !report.AsOf.Equal(cashTimeBase) {
				t.Fatal("timestamp validation silently introduced ordering")
			}
			if name == "unstated-completeness" && completeness.Stated() || name == "incomplete-completeness" && !completeness.Incomplete() {
				t.Fatal("timestamp validation changed the producer's completeness statement")
			}
		})
	}
}

func TestCashPayloadTimeConcurrentDiscardAndReads(t *testing.T) {
	var updates atomic.Int64
	view := cashview.New(cashview.WithClock(func() time.Time { return cashTimeBase }), cashview.WithOnUpdate(func(string) { updates.Add(1) }))
	good := cashTimePayload(t, cashTimeReport("PF", &commonpb.Decimal{Coefficient: 100}, timestamppb.New(cashTimeBase)))
	bad := cashTimePayload(t, cashTimeReport("PF", &commonpb.Decimal{Coefficient: 999}, malformedCashTimes(cashTimeBase)[0].at))
	if err := view.Handle(t.Context(), nil, good); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		payload := good
		if i%2 != 0 {
			payload = bad
		}
		workers.Go(func() {
			for range 200 {
				if err := view.Handle(t.Context(), nil, payload); err != nil {
					t.Error(err)
					return
				}
			}
		})
		workers.Go(func() {
			for range 200 {
				balance, ok := view.Lookup("PF")
				amount, _, _, spendable := view.Spendable("PF")
				if !ok || balance.Total.GetCoefficient() != 100 || !spendable || amount.GetCoefficient() != 100 {
					t.Error("reader observed a discarded malformed cash level")
					return
				}
			}
		})
	}
	workers.Wait()
	if updates.Load() != 401 {
		t.Fatalf("updates=%d, want exactly the 401 valid deliveries", updates.Load())
	}
}
