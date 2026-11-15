// Command decimal-conformance exports the existing Go decimal semantics.
// It has no runtime callers, credentials, store access or financial authority.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
)

type value struct {
	Wire     *[]int `json:"wire"`
	Domain   bool   `json:"domain"`
	Positive bool   `json:"positive"`
	Zero     bool   `json:"zero"`
}
type corpus struct {
	Version     int     `json:"version"`
	Values      []value `json:"values"`
	Comparisons []int   `json:"comparisons"`
}

func run() error {
	values := []*commonpb.Decimal{nil}
	for _, c := range []int64{math.MinInt64, -922337203685477580, -100, -10, -1, 0, 1, 10, 100, math.MaxInt64} {
		for _, e := range []int32{math.MinInt32, math.MinInt32 + 1, -65, -64, -18, -2, -1, 0, 1, 18, 64, 65, math.MaxInt32 - 1, math.MaxInt32} {
			values = append(values, &commonpb.Decimal{Coefficient: c, Exponent: e})
		}
	}
	out := corpus{Version: 1}
	for _, d := range values {
		v := value{Domain: dec.InDomain(d), Positive: dec.IsPositive(d), Zero: dec.IsZero(d)}
		if d != nil {
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(d)
			if err != nil {
				return err
			}
			ints := make([]int, len(wire))
			for i, b := range wire {
				ints[i] = int(b)
			}
			v.Wire = &ints
		}
		out.Values = append(out.Values, v)
		for _, b := range values {
			out.Comparisons = append(out.Comparisons, dec.Cmp(d, b))
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
