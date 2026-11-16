// Command nav_conformance captures fresh Go-owner numeric NAV results and refusals.
// Its synthetic inputs are diagnostic fixtures, not security-master or FX authority.
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type amounts map[string]string
type quotes map[string]*string
type valuation struct {
	Total      string  `json:"total"`
	Cash       string  `json:"cash"`
	Securities string  `json:"securities"`
	Accrued    string  `json:"accrued"`
	Local      amounts `json:"local"`
}
type refusal struct {
	Kind  string   `json:"kind"`
	Names []string `json:"names"`
}
type specimen struct {
	Name       string            `json:"name"`
	Reporting  string            `json:"reporting"`
	Positions  amounts           `json:"positions"`
	Cash       amounts           `json:"cash"`
	Accrued    amounts           `json:"accrued"`
	Prices     quotes            `json:"prices"`
	Currencies map[string]string `json:"currencies"`
	Rates      quotes            `json:"rates"`
	Converter  bool              `json:"converter"`
	Expected   *valuation        `json:"expected"`
	Refusal    *refusal          `json:"refusal"`
}
type corpus struct {
	Version int        `json:"version"`
	Cases   []specimen `json:"cases"`
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("invalid fixture rational")
	}
	return r
}
func quote(s string) *string { return &s }
func convert(in amounts) map[string]*big.Rat {
	out := map[string]*big.Rat{}
	for k, v := range in {
		out[k] = rat(v)
	}
	return out
}
func convertQuotes(in quotes) map[string]*big.Rat {
	out := map[string]*big.Rat{}
	for k, v := range in {
		if v != nil {
			out[k] = rat(*v)
		} else {
			out[k] = nil
		}
	}
	return out
}
func capture(in map[string]*big.Rat) amounts {
	out := amounts{}
	for k, v := range in {
		out[k] = v.RatString()
	}
	return out
}
func empty(name string) specimen {
	return specimen{Name: name, Reporting: "USD", Positions: amounts{}, Cash: amounts{}, Accrued: amounts{}, Prices: quotes{}, Currencies: map[string]string{}, Rates: quotes{}, Converter: true}
}
func evaluate(s specimen) specimen {
	b := ledger.NewBook("portfolio-a")
	for k, v := range s.Positions {
		b.Positions[k] = &ledger.Position{Qty: rat(v)}
	}
	b.Cash = convert(s.Cash)
	b.Accrued = convert(s.Accrued)
	var fx accounting.FXConverter
	if s.Converter {
		fx = accounting.NewFXTable(s.Reporting, convertQuotes(s.Rates))
	}
	n, err := accounting.ComputeNAVInCurrency(b, s.Reporting, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), convertQuotes(s.Prices), accounting.InstrumentCurrency(s.Currencies), fx)
	if err == nil {
		s.Expected = &valuation{n.Total.RatString(), n.Cash.RatString(), n.SecurityValue.RatString(), n.Accrued.RatString(), capture(n.LocalExposure)}
		return s
	}
	message := err.Error()
	switch {
	case message == "accounting: NAV requires an FX converter":
		s.Refusal = &refusal{"converter", []string{}}
	case strings.HasPrefix(message, "accounting: NAV missing price for "):
		name, e := strconv.Unquote(strings.TrimPrefix(message, "accounting: NAV missing price for "))
		if e != nil {
			panic(e)
		}
		s.Refusal = &refusal{"price", []string{name}}
	default:
		prefix := fmt.Sprintf("accounting: NAV in %s REFUSED — the book holds value in ", s.Reporting)
		rest, ok := strings.CutPrefix(message, prefix)
		if !ok {
			panic(err)
		}
		names, _, ok := strings.Cut(rest, ", and this valuation has no FX rate")
		if !ok {
			panic(err)
		}
		s.Refusal = &refusal{"fx", strings.Split(names, ", ")}
	}
	return s
}
func buildCorpus() corpus {
	cases := make([]specimen, 0, 2061)
	s := empty("domestic-golden")
	s.Positions["equity"] = "100"
	s.Prices["equity"] = quote("160")
	s.Cash["USD"] = "85000"
	cases = append(cases, s)
	s = empty("foreign-golden")
	s.Positions["equity"] = "100"
	s.Prices["equity"] = quote("85")
	s.Currencies["equity"] = "EUR"
	s.Cash["EUR"] = "2000"
	s.Accrued["EUR"] = "5"
	s.Rates["EUR"] = quote("11/10")
	cases = append(cases, s)
	s = empty("missing-price")
	s.Positions["equity"] = "1"
	cases = append(cases, s)
	s = empty("missing-all-fx")
	s.Cash["EUR"] = "1"
	s.Cash["GBP"] = "2"
	s.Accrued["JPY"] = "3"
	cases = append(cases, s)
	s = empty("zero-buckets")
	s.Positions["equity"] = "0"
	s.Cash["EUR"] = "0"
	s.Accrued["GBP"] = "0"
	cases = append(cases, s)
	s = empty("zero-mark-needs-fx")
	s.Positions["equity"] = "1"
	s.Prices["equity"] = quote("0")
	s.Currencies["equity"] = "EUR"
	cases = append(cases, s)
	s = empty("offset-unknown-fx")
	s.Cash["EUR"] = "100"
	s.Accrued["EUR"] = "-100"
	cases = append(cases, s)
	s = empty("identity-overrides")
	s.Cash["USD"] = "1"
	s.Cash[""] = "2"
	s.Rates["USD"] = quote("-1")
	s.Rates[""] = quote("0")
	cases = append(cases, s)
	s = empty("missing-converter")
	s.Converter = false
	cases = append(cases, s)
	s = empty("zero-rate")
	s.Cash["EUR"] = "10"
	s.Rates["EUR"] = quote("0")
	cases = append(cases, s)
	s = empty("negative-rate")
	s.Accrued["GBP"] = "1"
	s.Rates["GBP"] = quote("-1")
	cases = append(cases, s)
	s = empty("nil-rate")
	s.Cash["JPY"] = "1"
	s.Rates["JPY"] = nil
	cases = append(cases, s)

	rng := rand.New(rand.NewSource(153503))
	currencies := []string{"USD", "EUR", "GBP", "JPY", "CHF", ""}
	for i := 0; i < 2048; i++ {
		s = empty(fmt.Sprintf("seed-153503-%04d", i))
		s.Reporting = currencies[i%2]
		for _, ccy := range currencies {
			s.Cash[ccy] = big.NewRat(int64(rng.Intn(200001)-100000), 100).RatString()
			s.Accrued[ccy] = big.NewRat(int64(rng.Intn(2001)-1000), 7).RatString()
			s.Rates[ccy] = quote(big.NewRat(int64(1+rng.Intn(500)), 100).RatString())
		}
		for j := 0; j < 64; j++ {
			id := fmt.Sprintf("asset-%03d", j)
			s.Positions[id] = big.NewRat(int64(rng.Intn(2001)-1000), 7).RatString()
			s.Prices[id] = quote(big.NewRat(int64(rng.Intn(20001)-10000), 100).RatString())
			if j%5 != 0 {
				s.Currencies[id] = currencies[rng.Intn(len(currencies))]
			}
		}
		if i%7 == 0 {
			s.Positions["asset-001"] = "1"
			delete(s.Prices, "asset-001")
		}
		if i%11 == 0 {
			delete(s.Rates, "JPY")
		}
		if i%13 == 0 {
			s.Rates["CHF"] = quote("0")
		}
		if i%17 == 0 {
			s.Rates["GBP"] = quote("-1/5")
		}
		cases = append(cases, s)
	}
	s = empty("large-8192-position-book")
	for j := 0; j < 8192; j++ {
		id := fmt.Sprintf("asset-%05d", j)
		s.Positions[id] = big.NewRat(int64(j%31-15), 7).RatString()
		s.Prices[id] = quote(big.NewRat(int64(j%100+1), 3).RatString())
		s.Currencies[id] = currencies[j%len(currencies)]
	}
	for _, ccy := range currencies {
		s.Cash[ccy] = "1234/7"
		s.Accrued[ccy] = "-5/13"
		s.Rates[ccy] = quote("11/10")
	}
	cases = append(cases, s)
	for i := range cases {
		cases[i] = evaluate(cases[i])
	}
	return corpus{1, cases}
}
func main() {
	if err := json.NewEncoder(os.Stdout).Encode(buildCorpus()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
