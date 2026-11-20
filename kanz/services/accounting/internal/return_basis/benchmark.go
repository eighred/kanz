package returnbasis

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/performance"
	performancepb "github.com/eighred/kanz/kanz-schemas-go/performance/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const BenchmarkFormat = "kanz.accounting.benchmark-observation.v1"

// BenchmarkEvidence attaches an independently signed total-return index
// observation. An explicit null observation records missing coverage.
type BenchmarkEvidence struct {
	Observation *BenchmarkObservation `json:"observation"`
	Signature   []byte                `json:"signature"`
}

// BenchmarkObservation is signed as compact JSON. The digest of these
// canonical bytes identifies the immutable source version.
type BenchmarkObservation struct {
	Format              string    `json:"format"`
	Tenant              string    `json:"tenant"`
	Portfolio           string    `json:"portfolio"`
	Currency            string    `json:"currency"`
	BenchmarkID         string    `json:"benchmark_id"`
	Issuer              string    `json:"issuer"`
	DefinitionEffective time.Time `json:"definition_effective"`
	Start               time.Time `json:"start"`
	End                 time.Time `json:"end"`
	KnownAt             time.Time `json:"known_at"`
	OpeningLevel        dec.Exact `json:"opening_level"`
	ClosingLevel        dec.Exact `json:"closing_level"`
}

func validBenchmark(e Evidence) bool {
	b := e.Benchmark
	if b == nil {
		return true
	}
	o := b.Observation
	if o == nil {
		return b.Signature == nil
	}
	if o.Format != BenchmarkFormat || len(b.Signature) != ed25519.SignatureSize ||
		o.Tenant != e.Tenant || o.Portfolio != e.Portfolio || o.Currency != e.Currency ||
		!identifier.MatchString(o.BenchmarkID) || !identifier.MatchString(o.Issuer) {
		return false
	}
	for _, t := range []time.Time{o.DefinitionEffective, o.Start, o.End, o.KnownAt} {
		if t.IsZero() || t.Location() != time.UTC || t.Nanosecond()%1000 != 0 || timestamppb.New(t).CheckValid() != nil {
			return false
		}
	}
	if o.DefinitionEffective.After(o.Start) || !o.Start.Before(o.End) ||
		o.KnownAt.Before(o.End) || o.KnownAt.After(e.AsOf) {
		return false
	}
	for _, level := range []dec.Exact{o.OpeningLevel, o.ClosingLevel} {
		value, err := level.Rat()
		if err != nil || value.Sign() <= 0 {
			return false
		}
		if _, ok := dec.ToProtoExact(value); !ok {
			return false
		}
	}
	return true
}

func verifyBenchmark(ctx context.Context, tx pgx.Tx, e Evidence) (string, error) {
	if e.Benchmark == nil || e.Benchmark.Observation == nil {
		return "", nil
	}
	o := e.Benchmark.Observation
	var key []byte
	err := tx.QueryRow(ctx, `SELECT public_key FROM return_basis_benchmark_authorities
		WHERE tenant_id=public.app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND currency=$3
			AND benchmark_id=$4 AND issuer_id=$5`, e.Tenant, e.Portfolio, e.Currency, o.BenchmarkID, o.Issuer).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrEvidence
	}
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(o)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(key), raw, e.Benchmark.Signature) {
		return "", ErrEvidence
	}
	return digest(raw), nil
}

func compareBenchmark(r Receipt, assessment *performancepb.DailyReturnAssessment) *performancepb.DailyBenchmarkComparison {
	if r.Evidence.Benchmark == nil {
		return nil
	}
	out := &performancepb.DailyBenchmarkComparison{}
	reason := func(value string) *performancepb.DailyBenchmarkComparison {
		out.Result = &performancepb.DailyBenchmarkComparison_UnassessedReason{UnassessedReason: value}
		return out
	}
	o := r.Evidence.Benchmark.Observation
	if o == nil {
		return reason("benchmark_missing")
	}
	out.Definition = &performancepb.BenchmarkDefinition{
		BenchmarkId: o.BenchmarkID, CurrencyCode: o.Currency, AsOf: timestamppb.New(o.DefinitionEffective),
	}
	out.Start, out.End, out.KnownAt = timestamppb.New(o.Start), timestamppb.New(o.End), timestamppb.New(o.KnownAt)
	out.SourceDigest = r.benchmarkDigest
	if !o.Start.Equal(assessment.Start.AsTime()) || !o.End.Equal(assessment.End.AsTime()) {
		return reason("benchmark_window_mismatch")
	}
	portfolio := assessment.GetMeasured()
	if portfolio == nil {
		return reason("portfolio_return_unassessed")
	}
	open, _ := o.OpeningLevel.Rat()
	close, _ := o.ClosingLevel.Rat()
	indexReturn, _ := new(big.Rat).Quo(new(big.Rat).Sub(close, open), open).Float64()
	active := performance.ActiveReturn(portfolio.ReturnValue, indexReturn)
	if math.IsInf(active, 0) || math.IsNaN(active) {
		return reason("benchmark_comparison_out_of_range")
	}
	out.Result = &performancepb.DailyBenchmarkComparison_Measured{Measured: &performancepb.BenchmarkRelativeReturn{
		BenchmarkReturn: indexReturn, ActiveReturn: active,
	}}
	return out
}
