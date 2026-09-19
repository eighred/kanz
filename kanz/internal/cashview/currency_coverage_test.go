package cashview

import (
	"context"
	"sync"
	"testing"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSpendableCurrencyCoverageConcurrentSnapshots(t *testing.T) {
	v := New(WithMaxAge(0))
	encode := func(value int64, excluded uint32) []byte {
		b, err := proto.Marshal(&accountingpb.PortfolioCashBalance{PortfolioId: "PF", BaseCurrency: "USD", Total: &commonpb.Decimal{Coefficient: value}, AsOf: timestamppb.New(t0), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1, ExcludedCount: excluded}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	complete, incomplete := encode(100, 0), encode(999, 1)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				payload := complete
				if i%2 == 0 {
					payload = incomplete
				}
				if err := v.Handle(context.Background(), nil, payload); err != nil {
					t.Error(err)
					return
				}
				if total, _, _, ok := v.Spendable("PF"); ok && total.Coefficient != 100 {
					t.Error("coverage and value came from different reports")
					return
				}
				if report, ok := v.Lookup("PF"); ok {
					report.CurrencyCoverage.ExcludedCount = 0
				}
			}
		})
	}
	wg.Wait()
}

func TestSpendableCurrencyCoverageReplacesPreviouslyCompleteBalance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coverage *domainpb.InputCoverage
		excluded []string
	}{
		{"legacy", nil, nil},
		{"omitted liability", &domainpb.InputCoverage{Contributed: 1, ExcludedCount: 1}, []string{"EUR"}},
		{"truncated sample", &domainpb.InputCoverage{ExcludedCount: 33}, nil},
		{"inconsistent sample", &domainpb.InputCoverage{}, []string{"EUR"}},
		{"exclusion without count", &domainpb.InputCoverage{Exclusions: []*domainpb.InputExclusion{{Reason: "missing"}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := New(WithMaxAge(0))
			msg := &accountingpb.PortfolioCashBalance{PortfolioId: "PF", BaseCurrency: "USD", Total: &commonpb.Decimal{Coefficient: 100}, AsOf: timestamppb.New(t0), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1}}
			fold := func() {
				b, err := proto.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				if err = v.Handle(context.Background(), nil, b); err != nil {
					t.Fatal(err)
				}
			}
			fold()
			if _, _, _, ok := v.Spendable("PF"); !ok {
				t.Fatal("complete base cash refused")
			}
			msg.CurrencyCoverage = tc.coverage
			msg.ExcludedCurrencies = tc.excluded
			fold()
			if _, _, _, ok := v.Spendable("PF"); ok {
				t.Fatal("partial or unstated cash remained spendable")
			}
			report, ok := v.Lookup("PF")
			if !ok {
				t.Fatal("qualified report lost")
			}
			if report.CurrencyCoverage != nil {
				report.CurrencyCoverage.ExcludedCount = 0
				report.CurrencyCoverage.Exclusions = nil
			}
			if len(report.ExcludedCurrencies) > 0 {
				report.ExcludedCurrencies[0] = "USD"
			}
			if _, _, _, ok := v.Spendable("PF"); ok {
				t.Fatal("caller mutation changed admission coverage")
			}
			msg.CurrencyCoverage = &domainpb.InputCoverage{}
			msg.ExcludedCurrencies = nil
			msg.Total = &commonpb.Decimal{}
			fold()
			if _, _, _, ok := v.Spendable("PF"); !ok {
				t.Fatal("measured empty book refused")
			}
		})
	}
}
