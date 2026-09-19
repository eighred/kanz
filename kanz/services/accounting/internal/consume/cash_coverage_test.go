package consume

import (
	"fmt"
	"math/big"
	"sort"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestCashBalanceAnnounceCoverageSampleIsBoundedAndSorted(t *testing.T) {
	book := &ledger.Book{Cash: map[string]*big.Rat{}}
	for i := 39; i >= 0; i-- {
		book.Cash[fmt.Sprintf("C%02d", i)] = big.NewRat(-1, 1)
	}
	coverage, sample := cashCurrencyCoverage(book, "USD")
	if coverage.Contributed != 0 || coverage.ExcludedCount != 40 || len(sample) != 32 || !sort.StringsAreSorted(sample) || sample[0] != "C00" || sample[31] != "C31" {
		t.Fatalf("coverage=%v sample=%v", coverage, sample)
	}
}

func TestCashBalanceAnnounceCurrencyCoverage(t *testing.T) {
	for _, foreign := range []int64{50, -50, 0} {
		t.Run(big.NewInt(foreign).String(), func(t *testing.T) {
			pub := &capturingPublisher{}
			a, st := announcerOver(t, pub)
			ctx := foldCtx()
			for _, e := range []*struct {
				currency string
				amount   int64
			}{{"USD", 100}, {"EUR", foreign}, {"JPY", 0}} {
				if err := st.Append(ctx, cashEntry(e.currency, "PF", "", e.currency, e.amount), nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := announceVia(t, ctx, a, st, "PF"); err != nil {
				t.Fatal(err)
			}
			msg := pub.last()
			if msg.CurrencyCoverage == nil || msg.CurrencyCoverage.Contributed != 1 || dec.FromProto(msg.Total).Cmp(big.NewRat(100, 1)) != 0 {
				t.Fatalf("wrong base-bucket report: %v", msg)
			}
			if foreign == 0 {
				if msg.CurrencyCoverage.ExcludedCount != 0 || len(msg.ExcludedCurrencies) != 0 {
					t.Fatal("zero bucket excluded")
				}
			} else if msg.CurrencyCoverage.ExcludedCount != 1 || len(msg.ExcludedCurrencies) != 1 || msg.ExcludedCurrencies[0] != "EUR" {
				t.Fatalf("foreign asset/liability silently omitted: %v", msg)
			}
		})
	}
}

func TestCashBalanceAnnounceExactAmounts(t *testing.T) {
	for _, currency := range []string{"USD", "EUR"} {
		for _, amount := range []string{"0.000000001", "123456789012345678.9", "1/3", "9223372036854775809.1"} {
			t.Run(currency+"/"+amount, func(t *testing.T) {
				pub := &capturingPublisher{}
				a, st := announcerOver(t, pub)
				e := cashEntry("entry", "PF", "venue", currency, 1)
				e.Cash, _ = new(big.Rat).SetString(amount)
				if err := st.Append(foldCtx(), e, nil); err != nil {
					t.Fatal(err)
				}
				err := announceVia(t, foldCtx(), a, st, "PF")
				_, representable := dec.ToProtoExact(e.Cash)
				if !representable {
					if err == nil {
						t.Fatal("unsupported cash rounded instead of refused")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				msg := pub.last()
				if len(msg.ByVenueAccount) != 1 || dec.FromProto(msg.ByVenueAccount[0].Amount).Cmp(e.Cash) != 0 {
					t.Fatal("account cash rounded")
				}
				if currency == "USD" && dec.FromProto(msg.Total).Cmp(e.Cash) != 0 {
					t.Fatal("base cash rounded")
				}
			})
		}
	}
}
