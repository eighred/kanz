// Command ledger_conformance emits fresh Go-owner financial replay evidence.
// Test-only captures are not journal schemas, store receipts or authorization.
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"time"

	"github.com/eighred/kanz/internal/costbasis"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type instant struct {
	Seconds int64  `json:"seconds"`
	Nanos   uint32 `json:"nanos"`
}

func captureTime(t time.Time) instant { return instant{t.Unix(), uint32(t.Nanosecond())} }

type entry struct {
	ID              string  `json:"id"`
	Portfolio       string  `json:"portfolio"`
	Kind            int     `json:"kind"`
	Instrument      string  `json:"instrument"`
	Quantity        *string `json:"quantity"`
	Price           *string `json:"price"`
	Cash            *string `json:"cash"`
	Currency        string  `json:"currency"`
	Effective       instant `json:"effective"`
	Knowledge       instant `json:"knowledge"`
	SettlementBasis int     `json:"settlement_basis"`
	SettlementDate  instant `json:"settlement_date"`
	HasAction       bool    `json:"has_action"`
}
type lot struct {
	Quantity string `json:"quantity"`
	Average  string `json:"average"`
	Realized string `json:"realized"`
}
type book struct {
	Positions        map[string]lot    `json:"positions"`
	Cash             map[string]string `json:"cash"`
	Accrued          map[string]string `json:"accrued"`
	SettledPositions map[string]lot    `json:"settled_positions"`
	SettledCash      map[string]string `json:"settled_cash"`
	Unknown          int               `json:"unknown"`
	Pending          int               `json:"pending"`
	Complete         bool              `json:"complete"`
}
type query struct {
	Name      string  `json:"name"`
	Effective instant `json:"effective"`
	Knowledge instant `json:"knowledge"`
	Expected  book    `json:"expected"`
}
type history struct {
	Name      string   `json:"name"`
	Portfolio string   `json:"portfolio"`
	Entries   []*entry `json:"entries"`
	Queries   []query  `json:"queries"`
}
type corpus struct {
	Version   int       `json:"version"`
	Histories []history `json:"histories"`
}

func text(v *big.Rat) *string {
	if v == nil {
		return nil
	}
	s := v.RatString()
	return &s
}
func capture(e *ledger.Event) *entry {
	if e == nil {
		return nil
	}
	return &entry{e.EntryID, e.PortfolioID, int(e.Type), e.InstrumentID, text(e.Quantity), text(e.Price), text(e.Cash), e.CashCurrency, captureTime(e.Effective), captureTime(e.Knowledge), int(e.SettlementBasis), captureTime(e.SettlementDate), e.Action != nil}
}
func lots(values map[string]*costbasis.Lot) map[string]lot {
	out := map[string]lot{}
	for k, v := range values {
		out[k] = lot{v.Qty.RatString(), v.AvgCost.RatString(), v.Realized.RatString()}
	}
	return out
}
func amounts(values map[string]*big.Rat) map[string]string {
	out := map[string]string{}
	for k, v := range values {
		out[k] = v.RatString()
	}
	return out
}
func state(b *ledger.Book) book {
	return book{lots(b.Positions), amounts(b.Cash), amounts(b.Accrued), lots(b.SettledPositions), amounts(b.SettledCash), b.UnknownSettlementEntries(), b.PendingSettlementEntries(), b.SettlementBasisComplete()}
}
func fixture(name string, events []*ledger.Event, cuts [][2]time.Time) history {
	h := history{Name: name, Portfolio: "portfolio-a", Entries: make([]*entry, len(events)), Queries: []query{}}
	for i, e := range events {
		h.Entries[i] = capture(e)
	}
	for i, cut := range cuts {
		h.Queries = append(h.Queries, query{fmt.Sprintf("cut-%d", i), captureTime(cut[0]), captureTime(cut[1]), state(ledger.ReplayAsOf(h.Portfolio, events, cut[0], cut[1]))})
	}
	return h
}
func rat(n, d int64) *big.Rat { return big.NewRat(n, d) }
func buildCorpus() corpus {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Nanosecond) }
	event := func(id string, q, p, c *big.Rat, basis ledger.SettlementBasis) *ledger.Event {
		return &ledger.Event{EntryID: id, PortfolioID: "portfolio-a", Type: ledger.EntryTrade, InstrumentID: "asset-a", Quantity: q, Price: p, Cash: c, CashCurrency: "USD", Effective: at(1), Knowledge: at(1), SettlementBasis: basis}
	}
	gold := []*ledger.Event{nil, event("a", rat(2, 1), rat(10, 1), rat(-20, 1), ledger.SettlementSettled), event("b", rat(1, 1), rat(13, 1), rat(-13, 1), ledger.SettlementPending), event("c", rat(-1, 1), rat(15, 1), rat(15, 1), ledger.SettlementSettled), event("d", rat(-3, 1), rat(8, 1), rat(24, 1), ledger.SettlementUnknown)}
	gold[3].Knowledge = at(8)    // late knowledge, same effective time
	gold = append(gold, gold[1]) // exact redelivery
	foreign := event("a", rat(1000, 1), nil, nil, ledger.SettlementUnknown)
	foreign.PortfolioID = "portfolio-b"
	gold = append(gold, foreign)
	accrual := event("income", nil, nil, rat(7, 3), ledger.SettlementSettled)
	accrual.Type = ledger.EntryAccrual
	gold = append(gold, accrual)
	gold = append(gold, &ledger.Event{EntryID: "empty", PortfolioID: "portfolio-a"})
	cuts := [][2]time.Time{{{}, {}}, {at(1), at(1)}, {at(1), at(8)}, {at(0), {}}, {at(1), at(0)}}
	out := corpus{Version: 1, Histories: []history{fixture("hand-derived-mixed", gold, cuts)}}
	// Calendar extremes and nanos remain exact; there is no UnixNano conversion.
	extremes := []*ledger.Event{event("year-one", rat(1, 7), rat(3, 11), rat(-3, 77), ledger.SettlementSettled), event("year-last", rat(-1, 7), rat(6, 11), rat(6, 77), ledger.SettlementPending)}
	extremes[0].Effective = time.Time{}
	extremes[0].Knowledge = time.Time{}
	extremes[1].Effective = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	extremes[1].Knowledge = extremes[1].Effective
	out.Histories = append(out.Histories, fixture("calendar-extremes", extremes, [][2]time.Time{{{}, {}}, {base, base}}))
	// Fixed PRNG seed, bounded rational magnitudes and finite instrument set.
	rng := rand.New(rand.NewSource(1535))
	for h := 0; h < 16; h++ {
		events := make([]*ledger.Event, 0, 1300)
		for i := 0; i < 1024; i++ {
			q := rat(int64(rng.Intn(41)-20), int64(1<<uint(rng.Intn(4))))
			p := rat(int64(1+rng.Intn(10000)), 100)
			e := event(fmt.Sprintf("e-%04d", i), q, p, new(big.Rat).Neg(new(big.Rat).Mul(q, p)), ledger.SettlementBasis(rng.Intn(4)))
			e.InstrumentID = fmt.Sprintf("asset-%d", rng.Intn(17))
			e.Effective = at(rng.Intn(512))
			e.Knowledge = at(rng.Intn(640))
			e.SettlementDate = at(1)
			e.CashCurrency = []string{"USD", "EUR", ""}[rng.Intn(3)]
			switch i % 13 {
			case 0:
				e.Quantity = nil
				e.Price = nil
				e.Type = ledger.EntryCash
			case 1:
				e.Type = ledger.EntryFee
				e.Quantity = nil
			case 2:
				e.Type = ledger.EntryAccrual
				e.Quantity = nil
			case 3:
				e.Price = nil
			case 4:
				e.Cash = nil
			case 5:
				e.PortfolioID = "portfolio-b"
			case 6:
				e.PortfolioID = ""
			case 7:
				e.EntryID = ""
			case 8:
				e.InstrumentID = ""
			case 9:
				e.Type = ledger.EntryUnspecified
			}
			events = append(events, e)
			if i%17 == 0 {
				copy := *e
				copy.Cash = rat(999999, 1)
				events = append(events, &copy)
			} // equal-key first occurrence wins
			if i%31 == 0 {
				events = append(events, nil)
			}
		}
		rng.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
		queries := [][2]time.Time{{{}, {}}}
		for i := 0; i < 12; i++ {
			queries = append(queries, [2]time.Time{at(rng.Intn(650)), at(rng.Intn(650))})
		}
		out.Histories = append(out.Histories, fixture(fmt.Sprintf("seed-1535-history-%02d", h), events, queries))
	}
	return out
}
func main() {
	if err := json.NewEncoder(os.Stdout).Encode(buildCorpus()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
