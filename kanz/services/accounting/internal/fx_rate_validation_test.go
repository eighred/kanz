package accounting

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestNAVRefusesNonPositiveForeignRates(t *testing.T) {
	for _, rate := range []string{"0", "-1", "-1/100"} {
		for _, leg := range []string{"position", "cash", "accrued"} {
			t.Run(rate+"/"+leg, func(t *testing.T) {
				b := ledger.NewBook("PF")
				switch leg {
				case "position":
					b.Positions["asset"] = &ledger.Position{Qty: big.NewRat(2, 1)}
				case "cash":
					b.Cash["EUR"] = big.NewRat(50, 1)
				case "accrued":
					b.Accrued["EUR"] = big.NewRat(7, 3)
				}
				r, ok := new(big.Rat).SetString(rate)
				if !ok {
					t.Fatal("invalid test rate")
				}
				_, err := ComputeNAVInCurrency(b, "USD", time.Time{}, map[string]*big.Rat{"asset": big.NewRat(10, 1)}, InstrumentCurrency{"asset": "EUR"}, NewFXTable("USD", map[string]*big.Rat{"EUR": r}))
				if err == nil || !strings.Contains(err.Error(), "EUR") {
					t.Fatalf("NAV must refuse the named unusable rate %s, got %v", rate, err)
				}
			})
		}
	}
}

func TestNAVPreservesAnExactSmallPositiveRate(t *testing.T) {
	b := ledger.NewBook("PF")
	b.Cash["EUR"] = big.NewRat(100, 1)
	n, err := ComputeNAVInCurrency(b, "USD", time.Time{}, nil, nil, NewFXTable("USD", map[string]*big.Rat{"EUR": big.NewRat(1, 1000000000)}))
	if err != nil || n.Total.Cmp(big.NewRat(1, 10000000)) != 0 {
		t.Fatalf("small positive conversion was lost: %v %v", n.Total, err)
	}
}
