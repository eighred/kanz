package capital

import (
	"crypto/sha256"
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"google.golang.org/protobuf/proto"
	"strings"
)

// FromBalance decodes the durable accounting handoff, never an OMS fill.
// The caller must authenticate the producer and scope the database by envelope
// tenant. An absent handoff is unknown and must invalidate admission, not be
// silently skipped while a previous cash level remains usable.
func FromBalance(msg *accountingpb.PortfolioCashBalance) (CashEvent, error) {
	if msg.GetCashCommit() == nil {
		return CashEvent{}, ErrUnknown
	}
	c := msg.CashCommit
	if !validID(msg.PortfolioId) || !validID(msg.BaseCurrency) || msg.Total == nil || msg.AsOf == nil || msg.AsOf.CheckValid() != nil ||
		msg.KnowledgeTime == nil || msg.KnowledgeTime.CheckValid() != nil || !msg.KnowledgeTime.AsTime().Equal(msg.AsOf.AsTime()) ||
		c.Revision <= 0 || len(c.Applied) > 4096 || (c.Complete && c.UnattributedEntries != 0) || (!c.Complete && len(c.Applied) != 0) {
		return CashEvent{}, ErrInvalid
	}
	if _, ok := dec.InDomainDeep(msg); !ok {
		return CashEvent{}, ErrInvalid
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return CashEvent{}, ErrInvalid
	}
	posture, currency := msg.GetCompleteness(), msg.GetCurrencyCoverage()
	event := CashEvent{PortfolioID: msg.PortfolioId, Currency: msg.BaseCurrency, Revision: c.Revision,
		Total: dec.Exact(dec.FromProto(msg.Total).RatString()), ObservedAt: msg.AsOf.AsTime().UTC(),
		SourceDigest: sha256.Sum256(body),
		Complete: c.Complete && completeSources(posture) &&
			currency != nil && currency.GetExcludedCount() == 0 && len(msg.ExcludedCurrencies) == 0,
	}
	previous := ""
	for _, a := range c.Applied {
		if !validID(a.GetOrderId()) || a.GetOrderId() <= previous || a.GetDebit() == nil {
			return CashEvent{}, ErrInvalid
		}
		debit := dec.FromProto(a.Debit)
		if debit.Sign() < 0 {
			return CashEvent{}, ErrInvalid
		}
		event.Applied = append(event.Applied, Applied{OrderID: a.OrderId, Debit: dec.Exact(debit.RatString())})
		previous = a.OrderId
	}
	return event, nil
}

func completeSources(posture *accountingpb.BalanceCompleteness) bool {
	if posture == nil || len(posture.UnproducedEntryTypes) != 0 {
		return false
	}
	seen := map[int32]bool{}
	for _, name := range posture.ProducedEntryTypes {
		value, ok := accountingpb.EntryType_value["ENTRY_TYPE_"+strings.ToUpper(name)]
		if !ok || value == 0 || seen[value] {
			return false
		}
		seen[value] = true
	}
	return len(seen) == len(accountingpb.EntryType_name)-1
}
