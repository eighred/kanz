// Command option_pricing_reference captures the existing Go European pricing
// library for migration qualification. Binary64 bits are private test data,
// never a new canonical money wire or a served market-data/terms contract.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"

	"github.com/eighred/kanz/internal/risk/pricing"
)

type input [6]float64 // spot, strike, years, rate, yield, volatility

type sample struct {
	ID         string     `json:"id"`
	Right      int        `json:"right"`
	Exercise   int        `json:"exercise"`
	InputBits  [6]uint64  `json:"input_bits"`
	Outcome    string     `json:"outcome"`
	ValuesBits *[6]uint64 `json:"values_bits"`
}

type corpus struct {
	Schema    int      `json:"schema"`
	Method    string   `json:"method"`
	SourceSHA string   `json:"source_sha"`
	CaseCount int      `json:"case_count"`
	Cases     []sample `json:"cases"`
}

func capture(id string, right pricing.OptionType, exercise pricing.Exercise, v input) sample {
	s := sample{ID: id, Right: int(right), Exercise: int(exercise), Outcome: "unsupported_model"}
	for i, value := range v {
		s.InputBits[i] = math.Float64bits(value)
	}
	if exercise != pricing.European {
		return s
	}
	for _, value := range v {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			s.Outcome = "nonfinite_input"
			return s
		}
	}
	p, g := pricing.PriceGreeks(right, exercise, v[0], v[1], v[2], v[3], v[4], v[5])
	values := [6]float64{p, g.Delta, g.Gamma, g.Vega, g.Theta, g.Rho}
	var bits [6]uint64
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			s.Outcome = "nonfinite_output"
			return s
		}
		bits[i] = math.Float64bits(value)
	}
	s.Outcome, s.ValuesBits = "value", &bits
	return s
}

func cases() []sample {
	var rows []sample
	for _, right := range []pricing.OptionType{pricing.Call, pricing.Put} {
		for si, spot := range []float64{1e-6, 1, 42, 100, 1e6} {
			for ki, strikeFactor := range []float64{0.1, 1, 10} {
				for ti, years := range []float64{1e-12, 0.01, 0.5, 1, 10, 100} {
					for vi, volatility := range []float64{1e-6, 0.01, 0.2, 1, 5} {
						for ri, rate := range []float64{-0.1, 0, 0.05} {
							for qi, yield := range []float64{-0.05, 0, 0.03} {
								id := fmt.Sprintf("grid/%d/%d/%d/%d/%d/%d/%d", right, si, ki, ti, vi, ri, qi)
								rows = append(rows, capture(id, right, pricing.European, input{spot, spot * strikeFactor, years, rate, yield, volatility}))
							}
						}
					}
				}
			}
		}
		legacy := []input{{100, 95, 0, 0.05, 0.02, 0.2}, {100, 95, -1, 0.05, 0.02, 0.2},
			{100, 95, 0.5, 0.05, 0.02, 0}, {100, 95, 0.5, 0.05, 0.02, -0.2},
			{0, 95, 0.5, 0.05, 0.02, 0.2}, {-100, 95, 0.5, 0.05, 0.02, 0.2},
			{100, 0, 0.5, 0.05, 0.02, 0.2}, {100, -95, 0.5, 0.05, 0.02, 0.2}}
		for n, v := range legacy {
			rows = append(rows, capture(fmt.Sprintf("legacy/%d/%d", right, n), right, pricing.European, v))
		}
		for field := range 6 {
			for n, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
				v := input{100, 95, 0.5, 0.05, 0.02, 0.2}
				v[field] = bad
				rows = append(rows, capture(fmt.Sprintf("nonfinite/%d/%d/%d", right, field, n), right, pricing.European, v))
			}
		}
		for n, v := range []input{{100, 95, 1, 0, -1000, 0.2}, {1e308, 1e-308, 1e308, -1, 0, 1e308}} {
			rows = append(rows, capture(fmt.Sprintf("overflow/%d/%d", right, n), right, pricing.European, v))
		}
		rows = append(rows, capture(fmt.Sprintf("unsupported/%d", right), right, pricing.American, input{100, 95, 0.5, 0.05, 0.02, 0.2}))
		for n, v := range []input{{100, 95, 0.25, 0.1, 0, 0.5}, {910, 980, 0.25, 0.02, 0.025, 0.25}} {
			rows = append(rows, capture(fmt.Sprintf("published/%d/%d", right, n), right, pricing.European, v))
		}
	}
	return rows
}

func main() {
	source := flag.String("source", "", "reviewed source revision (40 lowercase hex digits)")
	flag.Parse()
	if flag.NArg() != 0 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*source) {
		fmt.Fprintln(os.Stderr, "option reference: a source revision is required")
		os.Exit(2)
	}
	rows := cases()
	if len(rows) != 8162 {
		fmt.Fprintln(os.Stderr, "option reference: incomplete case inventory")
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(corpus{Schema: 1, Method: "go_bsm_legacy_v1", SourceSHA: *source, CaseCount: len(rows), Cases: rows}); err != nil {
		fmt.Fprintln(os.Stderr, "option reference: capture failed")
		os.Exit(1)
	}
}
