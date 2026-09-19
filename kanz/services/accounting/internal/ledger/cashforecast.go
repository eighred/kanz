package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

var ErrForecastInput = errors.New("ledger: invalid cash forecast horizon or currency")
var ErrForecastCapacity = errors.New("ledger: cash forecast exceeds bounded journal capacity")

// CashForecast is a projection of recorded cash legs, not a certification that
// all future obligations have been received. Expected inflows never increase
// ConservativeBalance. Complete remains false until upstream obligation/source
// coverage can be vouched for; consumers must not use this view to authorize a
// sweep or collateral transfer.
type CashForecast struct {
	PortfolioID         string             `json:"portfolio_id"`
	Currency            string             `json:"currency"`
	AsOf                time.Time          `json:"as_of"`
	Horizon             time.Time          `json:"horizon"`
	SourceVersion       string             `json:"source_version"`
	Complete            bool               `json:"complete"`
	Reasons             []string           `json:"reasons"`
	Opening             *dec.Exact         `json:"settled_opening"`
	ConservativeBalance *dec.Exact         `json:"conservative_balance"`
	Flows               []CashForecastFlow `json:"flows"`
}

type CashForecastFlow struct {
	EntryID string    `json:"entry_id"`
	Due     time.Time `json:"due"`
	Amount  dec.Exact `json:"amount"`
	Overdue bool      `json:"overdue"`
}

type forecastEntry struct {
	ID                   string
	Cash                 string
	Effective, Knowledge time.Time
	Basis                SettlementBasis
	Settlement           *time.Time
}

const forecastEntryLimit = 10000

// ForecastCash performs one RLS-scoped database statement, with a fixed row
// budget and deadline. It never loads an unbounded journal on the HTTP path.
// Knowledge-time filtering excludes late corrections unknown at AsOf.
func (p *Postgres) ForecastCash(ctx context.Context, portfolio, currency string, asOf, horizon time.Time) (CashForecast, error) {
	if portfolio == "" || len(currency) != 3 || asOf.IsZero() || asOf.After(time.Now()) || !horizon.After(asOf) || horizon.Sub(asOf) > 366*24*time.Hour {
		return CashForecast{}, ErrForecastInput
	}
	for _, c := range currency {
		if c < 'A' || c > 'Z' {
			return CashForecast{}, ErrForecastInput
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := p.q.Query(ctx, `SELECT entry_id,cash,effective_time,knowledge_time,settlement_status,settlement_date
		FROM ledger_entries WHERE portfolio_id=$1 AND cash_currency=$2 AND cash IS NOT NULL
		AND knowledge_time <= $3 AND effective_time <= $4
		ORDER BY entry_id LIMIT $5`, portfolio, currency, asOf, horizon, forecastEntryLimit+1)
	if err != nil {
		return CashForecast{}, err
	}
	defer rows.Close()
	entries := make([]forecastEntry, 0)
	for rows.Next() {
		var e forecastEntry
		if err := rows.Scan(&e.ID, &e.Cash, &e.Effective, &e.Knowledge, &e.Basis, &e.Settlement); err != nil {
			return CashForecast{}, err
		}
		e.Effective, e.Knowledge = e.Effective.UTC(), e.Knowledge.UTC()
		if e.Settlement != nil {
			utc := e.Settlement.UTC()
			e.Settlement = &utc
		}
		entries = append(entries, e)
		if len(entries) > forecastEntryLimit {
			return CashForecast{}, ErrForecastCapacity
		}
	}
	if err := rows.Err(); err != nil {
		return CashForecast{}, err
	}
	return projectRecordedCash(portfolio, currency, asOf.UTC(), horizon.UTC(), entries)
}

func projectRecordedCash(portfolio, currency string, asOf, horizon time.Time, entries []forecastEntry) (CashForecast, error) {
	result := CashForecast{PortfolioID: portfolio, Currency: currency, AsOf: asOf, Horizon: horizon,
		Reasons: []string{"source_completeness_unverified"}, Flows: []CashForecastFlow{}}
	opening, outgoing := new(big.Rat), new(big.Rat)
	reasons := map[string]bool{}
	for _, e := range entries {
		amount, err := dec.Exact(e.Cash).Rat()
		if err != nil {
			return CashForecast{}, err
		}
		value, err := dec.ExactFromRat(amount)
		if err != nil {
			return CashForecast{}, err
		}
		if e.Basis == SettlementUnknown || e.Settlement == nil || e.Settlement.IsZero() {
			reasons["settlement_unknown"] = true
			continue
		}
		if e.Basis != SettlementSettled && e.Basis != SettlementPending {
			return CashForecast{}, ErrForecastInput
		}
		if e.Basis == SettlementSettled {
			if e.Settlement.After(asOf) || e.Effective.After(asOf) {
				reasons["settlement_chronology_invalid"] = true
				continue
			}
			opening.Add(opening, amount)
			continue
		}
		if e.Settlement.After(horizon) {
			continue
		}
		overdue := !e.Settlement.After(asOf)
		if overdue {
			reasons["settlement_overdue"] = true
		}
		result.Flows = append(result.Flows, CashForecastFlow{EntryID: e.ID, Due: e.Settlement.UTC(), Amount: value, Overdue: overdue})
		if amount.Sign() < 0 {
			outgoing.Add(outgoing, amount)
		}
	}
	sort.Slice(result.Flows, func(i, j int) bool {
		if result.Flows[i].Due.Equal(result.Flows[j].Due) {
			return result.Flows[i].EntryID < result.Flows[j].EntryID
		}
		return result.Flows[i].Due.Before(result.Flows[j].Due)
	})
	for reason := range reasons {
		result.Reasons = append(result.Reasons, reason)
	}
	sort.Strings(result.Reasons)
	if len(reasons) == 0 {
		openingValue, err := dec.ExactFromRat(opening)
		if err != nil {
			return CashForecast{}, err
		}
		balanceValue, err := dec.ExactFromRat(new(big.Rat).Add(opening, outgoing))
		if err != nil {
			return CashForecast{}, err
		}
		result.Opening, result.ConservativeBalance = &openingValue, &balanceValue
	}
	// The journal is immutable. Hash both the selected rows and query boundary so
	// later replay can establish whether it used exactly the same known inputs.
	blob, err := json.Marshal(struct {
		Model, Portfolio, Currency string
		AsOf, Horizon              time.Time
		Entries                    []forecastEntry
	}{
		"recorded-cash-v1", portfolio, currency, asOf, horizon, entries})
	if err != nil {
		return CashForecast{}, err
	}
	h := sha256.Sum256(blob)
	result.SourceVersion = "recorded-cash-v1/" + hex.EncodeToString(h[:])
	return result, nil
}
