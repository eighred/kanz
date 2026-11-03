package capital

import (
	"errors"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func coverageBalance() *accountingpb.PortfolioCashBalance {
	return &accountingpb.PortfolioCashBalance{PortfolioId: "fund", BaseCurrency: "USD", Total: &commonpb.Decimal{Coefficient: 9007199254740993},
		AsOf: timestamppb.New(time.Date(2026, 10, 2, 10, 0, 0, 123, time.UTC)), KnowledgeTime: timestamppb.New(time.Date(2026, 10, 2, 10, 0, 0, 123, time.UTC)), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1},
		Completeness: &accountingpb.BalanceCompleteness{ProducedEntryTypes: []string{"trade", "cash", "fee", "corporate_action", "accrual"}},
		CashCommit:   &accountingpb.CashCommitCoverage{Revision: 1, Complete: true, Applied: []*accountingpb.OrderCashDebit{{OrderId: "order", Debit: &commonpb.Decimal{Coefficient: 1, Exponent: -9}}}},
	}
}

func TestCashCoverageDecoderPreservesExactEvidenceAndRefusesUnknowns(t *testing.T) {
	msg := coverageBalance()
	event, err := FromBalance(msg)
	if err != nil || !event.Complete || event.Total != "9007199254740993" || len(event.Applied) != 1 || event.Applied[0].Debit != "1/1000000000" || event.ObservedAt.Nanosecond() != 123 {
		t.Fatalf("lost exact evidence: %+v %v", event, err)
	}
	for name, mutate := range map[string]func(*accountingpb.PortfolioCashBalance){
		"legacy producer":  func(m *accountingpb.PortfolioCashBalance) { m.CashCommit = nil },
		"missing amount":   func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Applied[0].Debit = nil },
		"negative debit":   func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Applied[0].Debit.Coefficient = -1 },
		"unstated time":    func(m *accountingpb.PortfolioCashBalance) { m.AsOf = nil },
		"invalid exponent": func(m *accountingpb.PortfolioCashBalance) { m.Total.Exponent = 1000000000 },
		"duplicate proof": func(m *accountingpb.PortfolioCashBalance) {
			m.CashCommit.Applied = append(m.CashCommit.Applied, proto.Clone(m.CashCommit.Applied[0]).(*accountingpb.OrderCashDebit))
		},
		"contradictory completeness": func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.UnattributedEntries = 1 },
		"incomplete with proofs":     func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Complete = false },
	} {
		t.Run(name, func(t *testing.T) {
			copy := proto.Clone(msg).(*accountingpb.PortfolioCashBalance)
			mutate(copy)
			if _, err := FromBalance(copy); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrUnknown) {
				t.Fatalf("unsafe evidence accepted: %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*accountingpb.PortfolioCashBalance){
		"unstated inputs":   func(m *accountingpb.PortfolioCashBalance) { m.Completeness = nil },
		"incomplete inputs": func(m *accountingpb.PortfolioCashBalance) { m.Completeness.UnproducedEntryTypes = []string{"fee"} },
		"unknown input":     func(m *accountingpb.PortfolioCashBalance) { m.Completeness.ProducedEntryTypes = []string{"made_up"} },
		"omitted enumeration": func(m *accountingpb.PortfolioCashBalance) {
			m.Completeness.ProducedEntryTypes = []string{"trade", "cash"}
		},
		"unstated currency": func(m *accountingpb.PortfolioCashBalance) { m.CurrencyCoverage = nil },
		"foreign liability": func(m *accountingpb.PortfolioCashBalance) { m.CurrencyCoverage.ExcludedCount = 1 },
		"unknown attribution": func(m *accountingpb.PortfolioCashBalance) {
			m.CashCommit.Complete = false
			m.CashCommit.UnattributedEntries = 1
			m.CashCommit.Applied = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := proto.Clone(msg).(*accountingpb.PortfolioCashBalance)
			mutate(copy)
			event, err := FromBalance(copy)
			if err != nil || event.Complete {
				t.Fatalf("must commit an explicit incomplete state: %+v %v", event, err)
			}
		})
	}
}
