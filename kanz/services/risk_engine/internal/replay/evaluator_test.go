package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Unit-only sink; durable atomicity and tenant isolation are tested against PG.
type recordSink struct {
	record Record
	err    error
}

func (s *recordSink) Save(_ context.Context, r Record) error { s.record = r; return s.err }
func (s *recordSink) Load(context.Context, v1.PortfolioID, time.Time) (Record, error) {
	return s.record, s.err
}

func testPortfolio() *domain.Portfolio {
	p := domain.NewPortfolio("book", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: time.Date(2026, 1, 2, 3, 4, 5, 123, time.UTC), BaseCurrency: "USD", PositionCount: 1})
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 12345, Exponent: -2}}})
	return p
}

func TestEvaluationRoundTripAndIntegrity(t *testing.T) {
	ctx := context.Background()
	sink := new(recordSink)
	e, err := New(compute.DefaultRegistry(), sink, "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	live, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range live.Measures.Names() {
		m, _ := live.Measures.Lookup(name)
		if m.Provenance.InputManifestDigest != sink.record.Digest {
			t.Fatal("missing manifest provenance")
		}
	}
	packed, objects, err := pack(sink.record.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var packedValue packedManifest
	if err := json.Unmarshal(packed, &packedValue); err != nil {
		t.Fatal(err)
	}
	restored, err := unpack(packedValue, objects)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, sink.record.Manifest) {
		t.Fatal("packing changed the input manifest")
	}
	if _, err := reconstruct(ctx, sink.record); err != nil {
		t.Fatal(err)
	}
	bad := sink.record
	bad.Measures = append([]byte(nil), bad.Measures...)
	bad.Measures[0] ^= 1
	if _, err := reconstruct(ctx, bad); err == nil {
		t.Fatal("changed output accepted")
	}
	bad = sink.record
	bad.Digest = "wrong"
	if _, err := reconstruct(ctx, bad); err == nil {
		t.Fatal("changed digest accepted")
	}
	sink.err = errors.New("storage unavailable")
	if result, err := e.Compute(ctx, p, nil); err == nil || result.Measures != nil {
		t.Fatal("unstored evaluation escaped")
	}
}

func TestMissingInputNeverConsultsLiveSource(t *testing.T) {
	in := &inputs{replay: true, values: make(map[string]json.RawMessage)}
	called := false
	resolve(withInputs(context.Background(), in), inputKey{Kind: "test"}, func() int { called = true; return 99 })
	if called || in.failure() == nil {
		t.Fatal("missing input did not fail closed")
	}
}
