package factormodel

import (
	"context"
	"testing"
	"time"
)

func TestDatedFactorFitsRefuseIncompleteUniverses(t *testing.T) {
	p := Providers{Returns: matrixReturns{"A": {.1, -.1, .2}}, Characteristics: staticChars{"A": {Style: map[string]float64{"Size": 1}}, "B": {Style: map[string]float64{"Size": 2}}}}
	for _, kind := range []ModelType{Statistical, Fundamental, Blend} {
		if _, err := Fit(context.Background(), Config{Type: kind, StyleFactors: []string{"Size"}}, []string{"A", "B"}, time.Now(), p); err == nil {
			t.Fatalf("%v fitted a zero-risk missing row", kind)
		}
		if _, err := Fit(context.Background(), Config{Type: kind}, []string{"A", "A"}, time.Now(), p); err == nil {
			t.Fatalf("%v accepted duplicate universe", kind)
		}
	}
}
