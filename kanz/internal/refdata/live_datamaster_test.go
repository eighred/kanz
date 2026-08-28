package refdata_test

import (
	"context"
	"os"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/refdata"
)

// #640'S LAST CLAUSE: A SECTOR MANDATE RESOLVED THROUGH A RUNNING DATAMASTER.
//
// #640 was a P0: no instrument classifier was constructed anywhere in
// production, so a book 100% in technology was ADMITTED under a 10% technology
// cap and the decision was recorded as a pass — an audit artifact asserting a
// check that could not have run. PR #695 built the chain (an issuer column
// through datamaster's RefRow → VendorRecord → SecurityMaster, a read path that
// serves sector and issuer, internal/refdata as the client, and all three
// composition roots wired).
//
// It stayed needs-verification for one stated reason, and the wording matters:
// "Every test drives refdata.Cache through a stubbed Source or an httptest
// server. What is unproven end-to-end is the live hop — a running datamaster
// projecting a vendor CSV drop with an issuer_id column, the OMS resolving a
// sector mandate through it."
//
// An httptest server was explicitly named as NOT sufficient. So this one talks
// to a datamaster PROCESS: its own HTTP server, its own Postgres, its own
// projection of a vendor CSV drop. Nothing here stubs anything.
//
// Gated on TEST_DATAMASTER_URL, and on the two instrument ids the drop is
// expected to carry, because the fixture lives outside this repository:
//
//	# Postgres, then the datamaster schema
//	test/backing/up.sh
//	KANZ_MIGRATE_DATABASE_URL=$TEST_POSTGRES_URL \
//	  go run ./cmd/kanz-migrate -dir services/datamaster/migrations -set datamaster
//
//	# a vendor drop carrying sector_code/sector_name/issuer_id, then the service
//	DATAMASTER_DATABASE_URL=$TEST_POSTGRES_URL DATAMASTER_LISTEN=:8099 \
//	DATAMASTER_TENANT=acme DATAMASTER_REF_FILES=VENDORA=/path/refdata.csv \
//	DATAMASTER_VENDOR_PRIORITY=VENDORA=0 go run ./services/datamaster/cmd/datamaster
//
//	TEST_DATAMASTER_URL=http://localhost:8099 \
//	TEST_DATAMASTER_TENANT=acme \
//	TEST_DATAMASTER_TECH_IDS=US0000000001,US0000000002 \
//	TEST_DATAMASTER_OTHER_ID=US0000000003 \
//	  go test ./internal/refdata/ -run LiveDatamaster
//
// THE IDS ARE ISINs, NOT SYMBOLS. DefaultIDResolver keys a mastered record by
// its strongest identifier, so a drop carrying an ISIN masters under the ISIN.
// That cost a wrong-looking "instrument not found" the first time this was run
// by hand, and it is written down here so the next person does not spend it
// again.

func liveDatamaster(t *testing.T) (*refdata.Cache, []string, string) {
	t.Helper()
	url := os.Getenv("TEST_DATAMASTER_URL")
	if url == "" {
		t.Skip("set TEST_DATAMASTER_URL to resolve a sector mandate through a RUNNING datamaster")
	}
	tech := splitIDs(os.Getenv("TEST_DATAMASTER_TECH_IDS"))
	other := os.Getenv("TEST_DATAMASTER_OTHER_ID")
	if len(tech) < 2 || other == "" {
		t.Skip("set TEST_DATAMASTER_TECH_IDS (two ids in one sector) and TEST_DATAMASTER_OTHER_ID " +
			"(one in another) to the instruments the vendor drop carries")
	}
	tenant := os.Getenv("TEST_DATAMASTER_TENANT")
	if tenant == "" {
		tenant = "acme"
	}
	cache, err := refdata.Config{DatamasterURL: url}.NewCache(tenant, "svc:oms")
	if err != nil {
		t.Fatalf("NewCache against %s: %v", url, err)
	}
	if cache == nil {
		t.Fatal("no cache was built from a configured DatamasterURL — the seam this test exists " +
			"to exercise is not there")
	}
	return cache, tech, other
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range splitComma(s) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, trimSpace(cur))
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, trimSpace(cur))
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// warm asks for each instrument and drives the refresh cycle until the cache can
// answer, exactly as the composition root's background loop does.
//
// THE FIRST LOOKUP IS EXPECTED TO MISS. The cache never dials from inside a rule
// evaluation — the rule evaluators are handed context.TODO, so a classifier that
// dialled datamaster there would put an uncancellable HTTP call on the
// order-admission path. It records what it was asked for and resolves it on the
// next cycle, which is why a cold pod REFUSES a classified-dimension rule rather
// than answering from nothing.
func warm(t *testing.T, cache *refdata.Cache, ids []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl := cache.Compliance()
	for _, id := range ids {
		_, _ = cl.Classify(ctx, id, time.Now())
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cache.Refresh(ctx); err != nil {
			t.Fatalf("refresh against the live datamaster failed: %v", err)
		}
		resolved := 0
		for _, id := range ids {
			if _, ok := cl.Classify(ctx, id, time.Now()); ok {
				resolved++
			}
		}
		if resolved == len(ids) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the cache never resolved %v from the live datamaster. Every sector, issuer and "+
		"asset-class rule is REFUSED in this state (#640) — which is the correct behaviour and "+
		"means the hop is not working", ids)
}

// THE LIVE HOP RESOLVES A SECTOR AND AN ISSUER.
//
// Not a stub, not an httptest handler: a datamaster process that read a vendor
// CSV drop, mastered it into Postgres under RLS, and served it over HTTP.
func TestLiveDatamasterResolvesSectorAndIssuer(t *testing.T) {
	cache, tech, _ := liveDatamaster(t)
	warm(t, cache, tech)

	ctx := context.Background()
	attrs, ok := cache.Compliance().Classify(ctx, tech[0], time.Now())
	if !ok {
		t.Fatalf("%s did not resolve after warming", tech[0])
	}
	if attrs.Sector == "" {
		t.Error("the live datamaster resolved an instrument with NO SECTOR — every sector " +
			"concentration, restriction and exclusion rule is refused for it (#640)")
	}
	if attrs.Issuer == "" {
		t.Error("the live datamaster resolved an instrument with NO ISSUER — the issuer column " +
			"was the missing half of #640's chain, so an issuer exclusion cannot fire")
	}
	if attrs.AssetClass == "" {
		t.Error("the live datamaster resolved an instrument with NO ASSET CLASS")
	}
	t.Logf("live datamaster: %s -> sector=%q issuer=%q asset_class=%q",
		tech[0], attrs.Sector, attrs.Issuer, attrs.AssetClass)
}

// THE MANDATE IN #640'S OWN WORDS, THROUGH THE LIVE HOP: "no more than 10% in
// TECH", against a book that is 100% in it.
//
// This is the sentence that made the issue a P0 — "the mandate passes regardless
// of the actual holding" — evaluated through a real security master rather than
// a map a test wrote.
func TestLiveDatamasterMakesASectorCapFire(t *testing.T) {
	cache, tech, other := liveDatamaster(t)
	warm(t, cache, append(append([]string{}, tech...), other))

	ctx := context.Background()
	cl := cache.Compliance()
	sector, ok := cl.Classify(ctx, tech[0], time.Now())
	if !ok || sector.Sector == "" {
		t.Fatalf("%s has no sector on the live master; the cap below would be refused rather "+
			"than breached, which proves nothing about the cap", tech[0])
	}

	engine := comp.NewEngine(nil)
	cap10 := &compliancepb.Mandate{
		MandateId: "m-live", TenantId: "acme", PortfolioId: "p-live", Version: 1,
		Rules: []*compliancepb.Rule{{
			RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
				Bucket:    sector.Sector,
				MaxWeight: &commonpb.Decimal{Coefficient: 10, Exponent: -2},
			}},
		}},
	}

	// A BOOK WHOLLY IN THAT SECTOR must BREACH the 10% cap.
	breaching := &comp.Book{
		PortfolioID: "p-live", BaseCurrency: "USD",
		NAV:      &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 200000, Exponent: 0}, CurrencyCode: "USD"},
		NAVBasis: comp.NAVBasisEquity,
		Positions: []comp.Position{
			livePos(tech[0], 100000),
			livePos(tech[1], 100000),
		},
	}
	res := engine.Evaluate(ctx, &comp.Candidate{Book: breaching, Classifier: cl, AsOf: time.Now()}, cap10)
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("a book 100%% in %s did NOT breach a 10%% cap on %s through the live datamaster: "+
			"status=%v violations=%v.\nThis is #640's headline sentence — 'the mandate passes "+
			"regardless of the actual holding' — happening against a real security master.",
			sector.Sector, sector.Sector, res.GetStatus(), res.GetViolations())
	}

	// AND THE NON-VACUITY ARM: a book mostly OUTSIDE the sector must PASS, or the
	// rule above would be breaching for a reason that has nothing to do with the
	// classification.
	inside := &comp.Book{
		PortfolioID: "p-live", BaseCurrency: "USD",
		NAV:      &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 200000, Exponent: 0}, CurrencyCode: "USD"},
		NAVBasis: comp.NAVBasisEquity,
		Positions: []comp.Position{
			livePos(tech[0], 10000),
			livePos(other, 190000),
		},
	}
	res = engine.Evaluate(ctx, &comp.Candidate{Book: inside, Classifier: cl, AsOf: time.Now()}, cap10)
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("a book 5%% in %s BREACHED a 10%% cap through the live datamaster: %v.\n"+
			"A rule that breaches whatever the book holds is the OTHER half of #640 — the "+
			"empty-bucket case, where every portfolio breaches for a reason nobody can act on.",
			sector.Sector, res.GetViolations())
	}
}

func livePos(id string, value int64) comp.Position {
	return comp.Position{
		InstrumentID: id,
		Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: value, Exponent: 0}, CurrencyCode: "USD"},
	}
}
