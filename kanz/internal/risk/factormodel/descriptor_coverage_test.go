package factormodel

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type rawChars map[string]Characteristics

func (c rawChars) Characteristics(_ context.Context, id string, _ time.Time) (Characteristics, bool) {
	v, ok := c[id]
	return v, ok
}

func descriptorFixture() (time.Time, rawChars, matrixReturns) {
	at := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	c := rawChars{}
	r := matrixReturns{}
	for i, id := range []string{"A", "B", "C", "D"} {
		c[id] = Characteristics{Style: map[string]float64{"Size": float64(i)}, AsOf: at, SourceDigest: "source:" + id}
		r[id] = []float64{float64(i+1) * .01, -.02, float64(i) * .003, .005}
	}
	return at, c, r
}

func TestDescriptorCoverageAndNormalization(t *testing.T) {
	at, c, r := descriptorFixture()
	cfg := Config{StyleFactors: []string{"Size"}}
	base, err := Fit(context.Background(), cfg, []string{"A", "B", "C", "D"}, at, Providers{c, r})
	if err != nil {
		t.Fatal(err)
	}
	c["MISSING"] = Characteristics{Style: map[string]float64{}, AsOf: at, SourceDigest: "missing"}
	c["NAN"] = Characteristics{Style: map[string]float64{"Size": math.NaN()}, AsOf: at, SourceDigest: "nan"}
	c["FUTURE"] = Characteristics{Style: map[string]float64{"Size": 100}, AsOf: at.Add(time.Hour), SourceDigest: "future"}
	for _, kind := range []ModelType{Fundamental, Blend} {
		cfg.Type = kind
		m, err := Fit(context.Background(), cfg, []string{"FUTURE", "C", "MISSING", "A", "NAN", "D", "B", "ABSENT"}, at, Providers{c, r})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m.Instruments, base.Instruments) {
			t.Fatalf("universe %v", m.Instruments)
		}
		for i := range base.Loadings {
			if m.Loadings[i][0] != base.Loadings[i][0] {
				t.Fatal("uncovered descriptor changed eligible normalization")
			}
		}
		var excluded map[string]string
		if err := json.Unmarshal([]byte(m.InputProvenance["descriptor_exclusions"]), &excluded); err != nil {
			t.Fatal(err)
		}
		if len(excluded) != 4 || excluded["MISSING"] != "missing_style:Size" || excluded["NAN"] != "nonfinite_style:Size" || excluded["FUTURE"] != "unproven_characteristics" {
			t.Fatalf("coverage %v", excluded)
		}
		if _, ok := m.Loading("MISSING"); ok {
			t.Fatal("missing descriptor acquired loading")
		}
		if _, ok := m.Loading("A"); !ok {
			t.Fatal("observed economic zero excluded")
		}
		if m.InputProvenance["input_digest"] == "" || !strings.Contains(m.ModelID, "DESC1") {
			t.Fatal("missing versioned identity")
		}
	}
	// Same values but changed source revision must identify a different artifact.
	c["A"] = Characteristics{Style: map[string]float64{"Size": 0}, AsOf: at, SourceDigest: "corrected"}
	cfg.Type = Fundamental
	revised, err := Fit(context.Background(), cfg, base.Instruments, at, Providers{c, r})
	if err != nil {
		t.Fatal(err)
	}
	if revised.InputProvenance["input_digest"] == base.InputProvenance["input_digest"] {
		t.Fatal("source revision absent from identity")
	}
}

func TestDescriptorFailures(t *testing.T) {
	for _, name := range []string{"missing", "constant", "infinite", "duplicate", "ridge_nan", "unproven", "rank"} {
		t.Run(name, func(t *testing.T) {
			at, c, r := descriptorFixture()
			cfg := Config{StyleFactors: []string{"Size"}}
			for id, v := range c {
				switch name {
				case "missing":
					delete(v.Style, "Size")
				case "constant":
					v.Style["Size"] = 3
				case "infinite":
					v.Style["Size"] = math.Inf(1)
				case "unproven":
					v.SourceDigest = ""
				case "rank":
					v.Style["Copy"] = v.Style["Size"]
				}
				c[id] = v
			}
			if name == "duplicate" {
				cfg.StyleFactors = []string{"Size", "Size"}
			}
			if name == "rank" {
				cfg.StyleFactors = []string{"Size", "Copy"}
			}
			if name == "ridge_nan" {
				cfg.Ridge = math.NaN()
			}
			if _, err := Fit(context.Background(), cfg, []string{"A", "B", "C", "D"}, at, Providers{c, r}); err == nil {
				t.Fatal("invalid model fitted")
			}
		})
	}
	for _, xs := range [][]float64{{math.MaxFloat64, 0, -math.MaxFloat64}, {1e-300, 2e-300, 3e-300}} {
		z, err := descriptorScores(xs)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(mean(z)) > 1e-14 || math.Abs(sampleVar(z)-1) > 1e-14 {
			t.Fatalf("normalization %v", z)
		}
	}
}

func TestCrossSectionalFitRejectsUnidentifiedAndInvalid(t *testing.T) {
	for _, b := range [][][]float64{nil, {{1}}, {{1, 0}, {0, 1}}, {{1, 1}, {2, 2}, {3, 3}}, {{1, 1}, {1, 1 + 1e-12}, {1, 1 - 1e-12}}, {{1}, {math.NaN()}, {2}}, {{1, 2}, {3}, {4, 5}}} {
		values := make([][]float64, len(b))
		for i := range values {
			values[i] = []float64{.1, .2, .3}
		}
		if _, _, _, err := crossSectionalFit(b, values, 1e-8); err == nil {
			t.Fatalf("accepted design %v", b)
		}
	}
	// An independent one-factor OLS reference: f_t = sum(r_i,t)/3.
	cov, specific, residual, err := crossSectionalFit([][]float64{{1}, {1}, {1}}, [][]float64{{.01, .02, .03}, {.02, .04, .06}, {.03, .06, .09}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(cov[0][0]-.0004) > 1e-15 || math.Abs(specific[0]-.0001) > 1e-15 || math.Abs(residual[2][2]-.03) > 1e-15 {
		t.Fatalf("OLS reference mismatch %v %v %v", cov, specific, residual)
	}
	if _, _, _, err := crossSectionalFit([][]float64{{1}, {1}, {1}}, [][]float64{{math.MaxFloat64, -math.MaxFloat64}, {1, 2}, {3, 4}}, 0); err == nil {
		t.Fatal("overflow became a model")
	}
}

func TestCategoricalReferenceAndRank(t *testing.T) {
	at, c, r := descriptorFixture()
	for i, id := range []string{"A", "B", "C", "D"} {
		v := c[id]
		v.Industry = []string{"Bank", "Bank", "Tech", "Tech"}[i]
		v.Country = []string{"DE", "US", "DE", "US"}[i]
		c[id] = v
	}
	m, err := Fit(context.Background(), Config{}, []string{"A", "B", "C", "D"}, at, Providers{c, r})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Factors) != 3 || m.InputProvenance["country_reference"] != "DE" {
		t.Fatalf("unidentified full dummy partitions: %+v", m.Factors)
	}
	for id, v := range c {
		v.Country = v.Industry
		c[id] = v
	}
	if _, err := Fit(context.Background(), Config{}, []string{"A", "B", "C", "D"}, at, Providers{c, r}); err == nil {
		t.Fatal("disconnected category partitions accepted")
	}
}

func TestDescriptorFitDeterministicConcurrentAndCanceled(t *testing.T) {
	at, c, r := descriptorFixture()
	cfg := Config{StyleFactors: []string{"Size"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fit(ctx, cfg, []string{"A", "B", "C", "D"}, at, Providers{c, r}); err == nil {
		t.Fatal("canceled fit succeeded")
	}
	baseline, err := Fit(context.Background(), cfg, []string{"A", "B", "C", "D"}, at, Providers{c, r})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			model, err := Fit(context.Background(), cfg, []string{"D", "B", "A", "C"}, at, Providers{c, r})
			if err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(model, baseline) {
				t.Error("concurrent/permuted fit changed artifact")
			}
		})
	}
	wg.Wait()
	changed := cfg
	changed.Ridge = .01
	if DefaultModelID(cfg) == DefaultModelID(changed) {
		t.Fatal("ridge change reused model identity")
	}
}
