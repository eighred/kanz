// Command curve_reference captures actual Go scalar curve analytics for #1534.
// Binary64 bits are private qualification data, never canonical financial wire.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

type sample struct {
	ID       string      `json:"id"`
	Mode     int         `json:"mode"`
	Comp     int         `json:"comp"`
	Interp   int         `json:"interp"`
	Tenors   []uint64    `json:"tenors"`
	Rates    []uint64    `json:"rates"`
	Queries  []uint64    `json:"queries"`
	Forwards [][2]uint64 `json:"forwards"`
	Coupons  []uint64    `json:"coupons"`
	Outcome  string      `json:"outcome"`
	Values   []uint64    `json:"values"`
}
type corpus struct {
	Schema int      `json:"schema"`
	Method string   `json:"method"`
	Source string   `json:"source_sha"`
	Count  int      `json:"case_count"`
	Cases  []sample `json:"cases"`
}

func bits(xs []float64) []uint64 {
	out := make([]uint64, len(xs))
	for i, v := range xs {
		out[i] = math.Float64bits(v)
	}
	return out
}
func capture(id string, mode, comp, interp int, ts, rs, queries []float64, forwards [][2]float64, coupons []float64) sample {
	s := sample{ID: id, Mode: mode, Comp: comp, Interp: interp, Tenors: bits(ts), Rates: bits(rs), Queries: bits(queries), Coupons: bits(coupons), Outcome: "refused", Forwards: make([][2]uint64, len(forwards))}
	for i, f := range forwards {
		s.Forwards[i] = [2]uint64{math.Float64bits(f[0]), math.Float64bits(f[1])}
	}
	var c *curve.Curve
	var err error
	if mode == 0 {
		c, err = curve.NewZeroCurve(ts, rs, curve.Compounding(comp), curve.Interpolation(interp))
	} else if mode == 1 {
		if len(ts) != len(rs) {
			return s
		}
		qs := make([]curve.ParQuote, len(ts))
		for i, t := range ts {
			qs[i] = curve.ParQuote{Tenor: t, Rate: rs[i]}
		}
		c, err = curve.Bootstrap(qs, curve.Interpolation(interp))
	} else {
		return s
	}
	if err != nil {
		return s
	}
	values := append(c.Tenors(), c.Zeros()...)
	bumped := (curve.Parallel{Bp: 1}).Apply(c)
	for _, t := range queries {
		values = append(values, c.Zero(t), c.Discount(t), bumped.Zero(t))
	}
	for _, f := range forwards {
		values = append(values, c.Forward(f[0], f[1]))
	}
	values = append(values, c.ParRate(coupons))
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			s.Outcome = "unavailable"
			return s
		}
	}
	s.Outcome, s.Values = "value", bits(values)
	return s
}

func cases() []sample {
	var rows []sample
	for mode := 0; mode < 2; mode++ {
		for interp := 0; interp < 2; interp++ {
			comps := []int{0}
			rates := []float64{-.02, -.001, -1e-20, 0, 1e-20, .02, .1}
			sets := [][]float64{{.25, 1, 3, 10}, {.125, .25, .5, 1}, {1, 2, 3, 4}}
			if mode == 0 {
				comps = []int{0, 1, 2}
				rates = []float64{-.99, -.1, -.01, -1e-20, 0, 1e-20, .001, .03, .7}
				sets = [][]float64{{.25, 1, 3, 10}, {1e-8, .5, 1, 100}, {1, 2, 3, 4}}
			}
			for _, comp := range comps {
				for si, ts := range sets {
					for ri, r := range rates {
						for shape := 0; shape < 2; shape++ {
							for order := 0; order < 2; order++ {
								times := slices.Clone(ts)
								rs := make([]float64, len(ts))
								for i := range rs {
									rs[i] = r + float64(shape*i)*math.Abs(r)*.01
								}
								if order == 1 {
									slices.Reverse(times)
									slices.Reverse(rs)
								}
								queries := []float64{-1, 0, ts[0] / 2, ts[0], (ts[0] + ts[len(ts)-1]) / 2, ts[len(ts)-1], ts[len(ts)-1] + 1}
								id := fmt.Sprintf("grid/%d/%d/%d/%d/%d/%d/%d", mode, interp, comp, si, ri, shape, order)
								rows = append(rows, capture(id, mode, comp, interp, times, rs, queries, [][2]float64{{.5, 5}, {2, math.Nextafter(2, math.Inf(1))}}, ts))
							}
						}
					}
				}
			}
			// Candidate bound, also the existing bond shadow's maximum pillar count.
			ts, rs := make([]float64, 256), make([]float64, 256)
			for i := range ts {
				ts[i], rs[i] = float64(i+1)/4, .02
			}
			rows = append(rows, capture(fmt.Sprintf("maximum/%d/%d", mode, interp), mode, 0, interp, ts, rs, []float64{0, .5, 32, 64, 65}, [][2]float64{{0, 65}}, ts))
		}
	}
	for mode := 0; mode < 2; mode++ {
		for i, v := range []struct{ t, r []float64 }{{nil, nil}, {[]float64{1}, nil}, {[]float64{1, 1}, []float64{.03, .04}}, {[]float64{0, 1}, []float64{.03, .04}}, {[]float64{-1, 1}, []float64{.03, .04}}} {
			rows = append(rows, capture(fmt.Sprintf("invalid/%d/%d", mode, i), mode, 0, 0, v.t, v.r, nil, nil, nil))
		}
		for field := 0; field < 2; field++ {
			for i, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
				ts, rs := []float64{1}, []float64{.03}
				if field == 0 {
					ts[0] = bad
				} else {
					rs[0] = bad
				}
				rows = append(rows, capture(fmt.Sprintf("nonfinite/%d/%d/%d", mode, field, i), mode, 0, 0, ts, rs, nil, nil, nil))
			}
		}
		rows = append(rows, capture(fmt.Sprintf("unknown-interp/%d", mode), mode, 0, 99, []float64{1}, []float64{.03}, nil, nil, nil))
	}
	for i, comp := range []int{1, 2, 99} {
		rate := -1.0
		if comp == 2 {
			rate = -2
		}
		rows = append(rows, capture(fmt.Sprintf("bad-comp/%d", i), 0, comp, 0, []float64{1}, []float64{rate}, nil, nil, nil))
	}
	for i, rs := range [][]float64{{-1}, {1, 10}} {
		ts := []float64{1}
		if len(rs) == 2 {
			ts = []float64{1, 2}
		}
		rows = append(rows, capture(fmt.Sprintf("unsolved/%d", i), 1, 0, 0, ts, rs, nil, nil, nil))
	}
	for i, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		rows = append(rows, capture(fmt.Sprintf("query/%d", i), 0, 0, 0, []float64{1, 3}, []float64{.02, .04}, []float64{bad}, nil, []float64{1, 3}))
	}
	for i, f := range [][2]float64{{2, 1}, {1, 1}, {math.NaN(), 2}, {1, math.Inf(1)}} {
		rows = append(rows, capture(fmt.Sprintf("forward/%d", i), 0, 0, 0, []float64{1, 3}, []float64{.02, .04}, nil, [][2]float64{f}, []float64{1, 3}))
	}
	for i, cp := range [][]float64{nil, {1, 1}, {2, 1}, {math.NaN()}, {math.Inf(1)}, {0}} {
		rows = append(rows, capture(fmt.Sprintf("schedule/%d", i), 0, 0, 0, []float64{1, 3}, []float64{.02, .04}, nil, nil, cp))
	}
	for i, z := range []float64{1000, -1000} {
		rows = append(rows, capture(fmt.Sprintf("numerical/%d", i), 0, 0, 0, []float64{1}, []float64{z}, []float64{1}, nil, []float64{1}))
	}
	return rows
}

func main() {
	source := flag.String("source", "", "reviewed 40-hex source revision")
	mixed := flag.Bool("mixed", false, "capture mixed deposit/future/swap calibration")
	flag.Parse()
	if flag.NArg() != 0 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*source) {
		fmt.Fprintln(os.Stderr, "curve reference: source required")
		os.Exit(2)
	}
	if *mixed {
		if err := captureMixed(*source); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	rows := cases()
	if len(rows) != 864 {
		fmt.Fprintln(os.Stderr, "curve reference: incomplete inventory")
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(corpus{1, "go_zero_par_v1", *source, len(rows), rows}) != nil {
		fmt.Fprintln(os.Stderr, "curve reference: capture failed")
		os.Exit(1)
	}
}
