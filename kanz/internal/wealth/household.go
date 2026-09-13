// Package wealth aggregates exact household valuations and model allocations.
// Probabilistic goal projections are estimates; they do not supply these books.
package wealth

import (
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

type Number = dec.Exact

var ErrValuation = errors.New("wealth: exact valuation unavailable")
var ErrWeights = errors.New("wealth: weights require positive total value")

type Holding struct {
	InstrumentID string
	AssetClass   string
	MarketValue  Number
}
type Account struct {
	AccountID string
	Holdings  []Holding
	Cash      Number
}
type Household struct {
	HouseholdID  string
	Accounts     []Account
	RiskProfile  RiskProfile
	CurrencyCode string
	AsOf         time.Time
	RecordedBy   string
	Reason       string
}

func (h Household) Clone() Household {
	h.Accounts = append([]Account(nil), h.Accounts...)
	for i := range h.Accounts {
		h.Accounts[i].Holdings = append([]Holding(nil), h.Accounts[i].Holdings...)
	}
	return h
}

func (h Household) ValidateValuation() error {
	if h.HouseholdID == "" || len(h.HouseholdID) > 256 || strings.TrimSpace(h.CurrencyCode) == "" || len(h.CurrencyCode) > 16 || h.AsOf.IsZero() || strings.TrimSpace(h.RecordedBy) == "" || strings.TrimSpace(h.Reason) == "" {
		return ErrValuation
	}
	_, err := Aggregate(h)
	return err
}

type VirtualPortfolio struct {
	HouseholdID string
	Holdings    map[string]Number
	Cash        Number
	TotalValue  Number
	assetClass  map[string]string
}

// Aggregate never changes its inputs. All arithmetic remains rational, and an
// absent amount refuses the valuation rather than becoming a measured zero.
func Aggregate(h Household) (VirtualPortfolio, error) {
	if len(h.Accounts) > 4096 {
		return VirtualPortfolio{}, ErrValuation
	}
	values := map[string]*big.Rat{}
	classes := map[string]string{}
	accounts := map[string]bool{}
	cash, total := new(big.Rat), new(big.Rat)
	count := 0
	for _, account := range h.Accounts {
		if account.AccountID == "" || len(account.AccountID) > 256 || accounts[account.AccountID] {
			return VirtualPortfolio{}, ErrValuation
		}
		accounts[account.AccountID] = true
		c, err := account.Cash.Rat()
		if err != nil {
			return VirtualPortfolio{}, ErrValuation
		}
		cash.Add(cash, c)
		total.Add(total, c)
		if !bounded(cash) || !bounded(total) {
			return VirtualPortfolio{}, ErrValuation
		}
		for _, holding := range account.Holdings {
			count++
			if count > 100000 || holding.InstrumentID == "" || len(holding.InstrumentID) > 256 {
				return VirtualPortfolio{}, ErrValuation
			}
			v, err := holding.MarketValue.Rat()
			if err != nil {
				return VirtualPortfolio{}, ErrValuation
			}
			if prior, ok := classes[holding.InstrumentID]; ok && prior != holding.AssetClass {
				return VirtualPortfolio{}, errors.New("wealth: conflicting asset classification")
			}
			classes[holding.InstrumentID] = holding.AssetClass
			if values[holding.InstrumentID] == nil {
				values[holding.InstrumentID] = new(big.Rat)
			}
			values[holding.InstrumentID].Add(values[holding.InstrumentID], v)
			total.Add(total, v)
			if !bounded(values[holding.InstrumentID]) || !bounded(total) {
				return VirtualPortfolio{}, ErrValuation
			}
		}
	}
	vp := VirtualPortfolio{HouseholdID: h.HouseholdID, Holdings: map[string]Number{}, assetClass: classes}
	var err error
	vp.Cash, err = dec.ExactFromRat(cash)
	if err != nil {
		return VirtualPortfolio{}, err
	}
	vp.TotalValue, err = dec.ExactFromRat(total)
	if err != nil {
		return VirtualPortfolio{}, err
	}
	for id, v := range values {
		vp.Holdings[id], err = dec.ExactFromRat(v)
		if err != nil {
			return VirtualPortfolio{}, err
		}
	}
	return vp, nil
}

func (vp VirtualPortfolio) Weights() (map[string]Number, error) {
	total, err := vp.TotalValue.Rat()
	if err != nil || total.Sign() <= 0 {
		return nil, ErrWeights
	}
	out := map[string]Number{}
	for _, id := range vp.Instruments() {
		value := vp.Holdings[id]
		r, err := value.Rat()
		if err != nil {
			return nil, err
		}
		out[id], err = dec.ExactFromRat(new(big.Rat).Quo(r, total))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (vp VirtualPortfolio) AssetClassExposure() (map[string]Number, error) {
	total, err := vp.TotalValue.Rat()
	if err != nil || total.Sign() <= 0 {
		return nil, ErrWeights
	}
	values := map[string]*big.Rat{}
	for _, id := range vp.Instruments() {
		value := vp.Holdings[id]
		r, err := value.Rat()
		if err != nil {
			return nil, err
		}
		class := vp.assetClass[id]
		if values[class] == nil {
			values[class] = new(big.Rat)
		}
		values[class].Add(values[class], r)
		if !bounded(values[class]) {
			return nil, ErrValuation
		}
	}
	cash, err := vp.Cash.Rat()
	if err != nil {
		return nil, err
	}
	if cash.Sign() != 0 {
		if values["CASH"] == nil {
			values["CASH"] = new(big.Rat)
		}
		values["CASH"].Add(values["CASH"], cash)
	}
	out := map[string]Number{}
	for class, value := range values {
		out[class], err = dec.ExactFromRat(new(big.Rat).Quo(value, total))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func bounded(r *big.Rat) bool {
	return r != nil && r.Num().BitLen() <= 512 && r.Denom().BitLen() <= 512
}

func (vp VirtualPortfolio) Instruments() []string {
	out := make([]string, 0, len(vp.Holdings))
	for id := range vp.Holdings {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
