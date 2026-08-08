// THE PRODUCTION TermsProvider (#345 item 1), against a real Postgres.
//
// Gated on TEST_POSTGRES_URL. Each test builds its own schema so the suite
// cannot collide with a shared contract_terms or a developer's real one (#212).
//
// WHY THIS PACKAGE AND NOT internal/marketdata/terms, where it started: this
// adapter must import internal/risk/compute (the seam) and internal/risk/pricing
// (the option type), and test/arch/risk_boundary_test.go forbids that from
// outside the risk module — "code OUTSIDE kanz/internal/risk/ may import only
// kanz/internal/risk/api/v*". The store stays on the marketdata side; the
// adapter lives here, the same way livequote adapts a cache to curve.QuoteSource
// from inside pricing/. The guard caught the original placement.
package termsource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/pricing"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *terms.Postgres {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the TermsProvider tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("termsource_test_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations",
		"0002_contract_terms.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(b)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return terms.NewPostgres(pool)
}

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func optionRec(id, underlying string, strike int64, asOf time.Time, mult int64) terms.Record {
	return terms.Record{
		InstrumentID: id,
		AsOf:         asOf,
		UnderlyingID: underlying,
		Kind:         terms.KindOption,
		Terms: &referencepb.ContractTerms{
			InstrumentId: id,
			AsOf:         timestamppb.New(asOf),
			Terms: &referencepb.ContractTerms_Option{Option: &referencepb.OptionTerms{
				UnderlyingId:       underlying,
				Strike:             dec(strike, 0),
				Expiry:             timestamppb.New(asOf.AddDate(0, 3, 0)),
				OptionType:         referencepb.OptionType_OPTION_TYPE_CALL,
				ExerciseStyle:      referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN,
				ContractMultiplier: dec(mult, 0),
			}},
		},
	}
}

type missingSpy struct{ seen []string }

func (m *missingSpy) observe(id string) { m.seen = append(m.seen, id) }

func providerWith(t *testing.T, recs ...terms.Record) (*Provider, *missingSpy) {
	t.Helper()
	st := newStore(t)
	if len(recs) > 0 {
		if err := st.Put(context.Background(), recs); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	spy := &missingSpy{}
	return NewProvider(st, WithMissingTermsObserver(spy.observe)), spy
}

// A STORED OPTION RESOLVES, WITH ITS TERMS INTACT THROUGH THE DECIMAL BOUNDARY.
func TestOptionTermsResolvesAStoredOption(t *testing.T) {
	p, spy := providerWith(t, optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1))

	spec, ok := p.OptionTerms(context.Background(), "BTC-60000-C", t0)
	if !ok {
		t.Fatal("a stored option did not resolve — the pricing path would treat it as linear")
	}
	if spec.Strike != 60000 {
		t.Errorf("strike = %v, want 60000", spec.Strike)
	}
	if spec.UnderlyingID != "BTC-USD" {
		t.Errorf("underlying = %q, want BTC-USD", spec.UnderlyingID)
	}
	if spec.Type != pricing.Call {
		t.Errorf("type = %v, want Call", spec.Type)
	}
	if len(spy.seen) != 0 {
		t.Errorf("the missing-terms observer fired for a resolvable option: %v", spy.seen)
	}
}

// A FRACTIONAL STRIKE SURVIVES THE CONVERSION. A conversion that read only the
// coefficient would price a 0.05 strike as 5.
func TestOptionTermsConvertsAFractionalStrike(t *testing.T) {
	rec := optionRec("ETH-0.05-C", "ETH-USD", 0, t0, 1)
	rec.Terms.GetOption().Strike = &commonpb.Decimal{Coefficient: 5, Exponent: -2}

	p, _ := providerWith(t, rec)
	spec, ok := p.OptionTerms(context.Background(), "ETH-0.05-C", t0)
	if !ok {
		t.Fatal("a fractional-strike option did not resolve")
	}
	if spec.Strike != 0.05 {
		t.Fatalf("strike = %v, want 0.05 — the exponent was dropped, so every scaled strike would be "+
			"priced at the wrong moneyness", spec.Strike)
	}
}

// AN INSTRUMENT WITH NO TERMS IS REPORTED, because the seam cannot report it.
func TestOptionTermsReportsAnInstrumentWithNoTerms(t *testing.T) {
	p, spy := providerWith(t)

	if _, ok := p.OptionTerms(context.Background(), "NEVER-LOADED", t0); ok {
		t.Fatal("an instrument with no terms resolved as an option")
	}
	if len(spy.seen) != 1 || spy.seen[0] != "NEVER-LOADED" {
		t.Fatalf("missing-terms observer saw %v, want [NEVER-LOADED].\n\n"+
			"compute.TermsProvider returns a bare false here, so this hook is the ONLY way a "+
			"derivative about to be priced as a share becomes visible.", spy.seen)
	}
}

// A SWAP IS NOT A MISSING OPTION — reporting it would drown the signal.
func TestOptionTermsDoesNotReportANonOptionAsMissing(t *testing.T) {
	swap := terms.Record{
		InstrumentID: "IRS-5Y", AsOf: t0, Kind: terms.KindSwap,
		Terms: &referencepb.ContractTerms{
			InstrumentId: "IRS-5Y", AsOf: timestamppb.New(t0),
			Terms: &referencepb.ContractTerms_Swap{Swap: &referencepb.SwapTerms{
				EffectiveDate: timestamppb.New(t0),
				MaturityDate:  timestamppb.New(t0.AddDate(5, 0, 0)),
			}},
		},
	}
	p, spy := providerWith(t, swap)

	if _, ok := p.OptionTerms(context.Background(), "IRS-5Y", t0); ok {
		t.Fatal("a swap resolved as an option")
	}
	if len(spy.seen) != 0 {
		t.Fatalf("a swap was reported as missing terms (%v) — it is not missing, it is not an "+
			"option, and conflating them makes the signal useless", spy.seen)
	}
}

// TERMS THAT EXIST BUT CANNOT BE USED are refused AND reported.
func TestOptionTermsRefusesUnusableTerms(t *testing.T) {
	cases := []struct {
		name string
		bend func(*referencepb.OptionTerms)
	}{
		{"zero strike", func(o *referencepb.OptionTerms) {
			o.Strike = &commonpb.Decimal{Coefficient: 0, Exponent: 0}
		}},
		{"negative strike", func(o *referencepb.OptionTerms) {
			o.Strike = &commonpb.Decimal{Coefficient: -100, Exponent: 0}
		}},
		{"zero multiplier", func(o *referencepb.OptionTerms) {
			o.ContractMultiplier = &commonpb.Decimal{Coefficient: 0, Exponent: 0}
		}},
		{"unspecified option type", func(o *referencepb.OptionTerms) {
			o.OptionType = referencepb.OptionType_OPTION_TYPE_UNSPECIFIED
		}},
		{"no expiry", func(o *referencepb.OptionTerms) { o.Expiry = nil }},
		{"nil strike", func(o *referencepb.OptionTerms) { o.Strike = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := optionRec("BAD-OPT", "BTC-USD", 60000, t0, 1)
			tc.bend(rec.Terms.GetOption())
			p, spy := providerWith(t, rec)

			if _, ok := p.OptionTerms(context.Background(), "BAD-OPT", t0); ok {
				t.Fatalf("%s resolved as a usable option.\n\n"+
					"A zero strike prices as a forward and a zero multiplier makes every Greek zero — "+
					"both look like an option that carries no risk.", tc.name)
			}
			if len(spy.seen) != 1 {
				t.Errorf("%s was not reported (%v) — an unusable term has the same consequence as a "+
					"missing one", tc.name, spy.seen)
			}
		})
	}
}

// A PUT RESOLVES AS A PUT.
func TestOptionTermsMapsAPut(t *testing.T) {
	rec := optionRec("BTC-60000-P", "BTC-USD", 60000, t0, 1)
	rec.Terms.GetOption().OptionType = referencepb.OptionType_OPTION_TYPE_PUT

	p, _ := providerWith(t, rec)
	spec, ok := p.OptionTerms(context.Background(), "BTC-60000-P", t0)
	if !ok {
		t.Fatal("a put did not resolve")
	}
	if spec.Type != pricing.Put {
		t.Fatalf("type = %v, want Put — a put priced as a call is wrong by the whole put-call "+
			"parity gap", spec.Type)
	}
}

// POINT-IN-TIME REACHES THE PROVIDER, not just the store.
func TestOptionTermsIsPointInTime(t *testing.T) {
	later := t0.AddDate(0, 1, 0)
	p, _ := providerWith(t,
		optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1),
		optionRec("BTC-60000-C", "BTC-USD", 30000, later, 2),
	)

	before, ok := p.OptionTerms(context.Background(), "BTC-60000-C", later.Add(-time.Second))
	if !ok {
		t.Fatal("the pre-amendment option did not resolve")
	}
	if before.Strike != 60000 {
		t.Fatalf("strike as of before the amendment = %v, want 60000", before.Strike)
	}
	after, ok := p.OptionTerms(context.Background(), "BTC-60000-C", later)
	if !ok {
		t.Fatal("the post-amendment option did not resolve")
	}
	if after.Strike != 30000 || after.Multiplier != 2 {
		t.Fatalf("post-amendment spec = strike %v mult %v, want 30000 / 2", after.Strike, after.Multiplier)
	}
}
