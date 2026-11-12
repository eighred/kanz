package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

type mixedQuote struct {
	Kind  int    `json:"kind"`
	Tenor uint64 `json:"tenor"`
	Span  uint64 `json:"span"`
	Value uint64 `json:"value"`
}
type mixedSample struct {
	ID      string       `json:"id"`
	Interp  int          `json:"interp"`
	Quotes  []mixedQuote `json:"quotes"`
	Queries []uint64     `json:"queries"`
	Outcome string       `json:"outcome"`
	Values  []uint64     `json:"values"`
}

func mixedCapture(id string, interp int, qs []curve.RateQuote) mixedSample {
	s := mixedSample{ID: id, Interp: interp, Quotes: make([]mixedQuote, len(qs)), Queries: bits([]float64{.1, 1, 7}), Outcome: "refused"}
	for i, q := range qs {
		s.Quotes[i] = mixedQuote{int(q.Kind), math.Float64bits(q.Tenor), math.Float64bits(q.Span), math.Float64bits(q.Value)}
	}
	c, err := curve.Calibrate(qs, curve.Interpolation(interp))
	if err != nil {
		return s
	}
	v := append(c.Tenors(), c.Zeros()...)
	for _, q := range qs {
		v = append(v, c.QuoteRate(q))
	}
	for _, t := range []float64{.1, 1, 7} {
		v = append(v, c.Zero(t), c.Discount(t))
	}
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			s.Outcome = "unavailable"
			return s
		}
	}
	s.Outcome, s.Values = "value", bits(v)
	return s
}

func captureMixed(source string) error {
	var rows []mixedSample
	for interp := 0; interp < 2; interp++ {
		for ri, r := range []float64{-.03, -1e-20, 0, 1e-20, 1e-100, .001, .03, .25} {
			families := [][]curve.RateQuote{
				{{Kind: curve.Deposit, Tenor: 1, Value: r}},
				{{Kind: curve.Deposit, Tenor: .25, Value: r}, {Kind: curve.Future, Tenor: .25, Span: .25, Value: r}},
				{{Kind: curve.Deposit, Tenor: .25, Value: r}, {Kind: curve.Future, Tenor: .25, Span: .25, Value: r * 1.1}, {Kind: curve.Swap, Tenor: 1, Value: r * 1.2}, {Kind: curve.Swap, Tenor: 2, Value: r * 1.3}, {Kind: curve.Swap, Tenor: 5, Value: r * 1.4}},
				{{Kind: curve.Swap, Tenor: 1, Value: r}, {Kind: curve.Swap, Tenor: 2, Value: r}, {Kind: curve.Swap, Tenor: 3, Value: r}, {Kind: curve.Swap, Tenor: 5, Value: r}},
				{{Kind: curve.Deposit, Tenor: .125, Value: r}, {Kind: curve.Future, Tenor: .125, Span: .375, Value: r * 1.1}, {Kind: curve.Swap, Tenor: 1.25, Value: r * 1.2}, {Kind: curve.Swap, Tenor: 3.5, Value: r * 1.3}},
			}
			for family, qs := range families {
				for order := 0; order < 2; order++ {
					copyQuotes := slices.Clone(qs)
					if order == 1 {
						slices.Reverse(copyQuotes)
					}
					rows = append(rows, mixedCapture(fmt.Sprintf("mixed/%d/%d/%d/%d", interp, ri, family, order), interp, copyQuotes))
				}
			}
		}
		invalid := [][]curve.RateQuote{
			nil, {{Kind: 99, Tenor: 1, Value: .03}}, {{Kind: curve.Deposit, Tenor: 1, Value: math.NaN()}},
			{{Kind: curve.Deposit, Tenor: math.Inf(1), Value: .03}}, {{Kind: curve.Future, Tenor: 1, Span: -1, Value: .03}},
			{{Kind: curve.Deposit, Tenor: 1, Value: .03}, {Kind: curve.Swap, Tenor: 1, Value: .03}},
			{{Kind: curve.Future, Tenor: 1, Value: .03}}, {{Kind: curve.Deposit, Tenor: .25, Value: .03}, {Kind: curve.Future, Tenor: 1, Span: .25, Value: .035}},
			{{Kind: curve.Deposit, Tenor: 0, Value: .03}}, {{Kind: curve.Swap, Tenor: 4097, Value: .03}}, {{Kind: curve.Swap, Tenor: 5, Value: 3}},
			{{Kind: curve.Deposit, Tenor: 1, Value: -1}}, {{Kind: curve.Deposit, Tenor: 1, Value: -2}},
			{{Kind: curve.Deposit, Tenor: 1e-300, Value: 1e-300}}, {{Kind: curve.Future, Tenor: -1, Value: .03}}, {{Kind: curve.Future, Tenor: 1, Span: math.Inf(1), Value: .03}},
		}
		for i, qs := range invalid {
			rows = append(rows, mixedCapture(fmt.Sprintf("invalid/%d/%d", interp, i), interp, qs))
		}
		var large []curve.RateQuote
		for i := 1; i <= 256; i++ {
			large = append(large, curve.RateQuote{Kind: curve.Deposit, Tenor: float64(i) / 4, Value: .02})
		}
		rows = append(rows, mixedCapture(fmt.Sprintf("capacity/%d", interp), interp, large))
		for i, r := range []float64{-.39, 1.7} {
			rows = append(rows, mixedCapture(fmt.Sprintf("edge/%d/%d", interp, i), interp, []curve.RateQuote{{Kind: curve.Swap, Tenor: 1, Value: r}}))
		}
	}
	if len(rows) != 198 {
		return fmt.Errorf("mixed curve reference: incomplete inventory")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Schema int           `json:"schema"`
		Method string        `json:"method"`
		Source string        `json:"source_sha"`
		Count  int           `json:"case_count"`
		Cases  []mixedSample `json:"cases"`
	}{1, "go_mixed_v1", source, len(rows), rows})
}
