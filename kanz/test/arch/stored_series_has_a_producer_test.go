package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A SERIES NOTHING PRODUCES IS AN EMPTY PARTITION, AND A QUERY FOR IT RETURNS
// "WE HOLD NONE OF THOSE" RATHER THAN "NOBODY EVER WROTE ANY".
//
// #509 a third time, one granularity below store_has_a_writer_test.go.
//
// internal/marketdata/store declares three bar resolutions — Resolution1m,
// Resolution1h, Resolution1d. The store accepts all three, the migration has a
// resolution column, the index is keyed on it, BarQuery filters on it, and
// store.Resolution.Valid() says yes to all three. Everything about the store says
// the coarse series exist.
//
// NOTHING PRODUCED THEM UNTIL 2026-08-16. Resolution1h was named nowhere in this
// module outside its own declaration and the `resolutions` interval map beside
// it. Resolution1d was named in exactly two more places, and BOTH WERE COMMENTS
// (internal/risk/liquiditysource/provider.go, which said in prose that no rollup
// job existed). marketdata.TranslateBar refuses a coarse bar at the ingest seam
// ON PURPOSE — one candle must not have two sources — so the coarse series can
// only ever be filled by a rollup, and no rollup was wired.
//
// The consequence was not an error anywhere. A consumer configured for
// Resolution1d matched zero rows for every instrument that would ever exist, and
// the two consumers that take a resolution both DEFAULT to Resolution1m
// (indicator/source.go:101, alpha/outcome/outcome.go:80), so in practice every
// caller silently read the base series instead of the one it asked the platform
// to support. internal/risk/liquiditysource paid for it: at DefaultWindow it
// walks 28 x 1440 = ~40,000 1-minute rows per instrument per call, twice per
// position, because the 1d series it was designed to read was empty. That read
// cost was the standing argument against wiring RegisterLiquidityRisk at all.
//
// # THE MOTIVATING CASE IS DISCHARGED, AND BY WHAT
//
// internal/marketdata/rollup landed the day this guard was written, so
// Resolution1h and Resolution1d resolve to a producer and carry NO exemption
// below. The producers are services/market-data/cmd/market-data/rollup.go:127
// and :128 — the composition root's target table — and NOT the rollup package
// itself, which stamps `Resolution: req.Target` from a variable (rollup/driver.
// go:238, rollup/rollup.go:153). That distinction is the whole reason this file
// says what it says about derived values: had the job been constructed with a
// target parsed from configuration instead of named at the composition root, the
// series would fill and this guard would still report both constants dark. The
// fix for that would be to NAME THE CONSTANT at the wiring site, never to widen
// the rule.
//
// IF THE ROLLUP IS EVER REVERTED, this guard goes red on exactly those two
// constants, which is correct and is the point. What belongs in the exemption if
// that happens: "#509 — the coarse bar series. TranslateBar refuses a coarse bar
// from a venue by design, so the only legitimate producer is a rollup over the 1m
// base; the rollup must name the constant at the site that stamps the output bar,
// and no_dark_capability_test.go must separately see the rollup package imported
// from a composition root, or the job that names it is one nothing runs."
//
// # Why the two guards next door are green on this
//
// no_dark_capability_test.go is IMPORT granularity. internal/marketdata/store is
// imported by nine packages. The package is bright.
//
// store_has_a_writer_test.go is WHOLE-STORE granularity. PutBars has production
// callers (backfill, the market-data ingest path), so *store.Postgres resolves to
// both a live reader and a live writer and passes — while two of its three series
// have never had a single row written to them. "The table is written" and "every
// series in the table is written" are different claims, and only the first one
// had a guard.
//
// # What this checks
//
// Every value of an enumerated constant family that a durable record carries must
// be PRODUCED somewhere in production code, or carry an argued exemption.
//
// A FAMILY is a named type T declared in a package under the module's private
// tree (kanz/internal/... or services/<svc>/internal/...) where all three hold:
//
//  1. T's underlying type is a basic type (string, int32, ...) — an enum, not a
//     struct;
//  2. the package declares TWO OR MORE exported constants of type T — one
//     constant is a spelling, two or more is a partition;
//  3. T is the type of an exported FIELD on a struct S in the same package, and S
//     appears in the signature of a function or interface method whose FIRST
//     parameter is context.Context.
//
// CLAUSE 3 IS THE DURABILITY TEST, and it is the same one store_has_a_writer_test
// .go uses for the same reason: a call that crosses to Postgres, a broker or the
// network takes a Context because it can block. Without it every internal enum in
// the module qualifies — log levels, HTTP verbs, metric label values — and a
// guard that reports those is a guard someone switches off. With it, 10 families
// and 38 constants are in scope, and each one is a label some record is stored or
// queried under.
//
// # What counts as a PRODUCER, exactly
//
// A reference to the constant that IS, verbatim, one of these expressions:
//
//	AssignStmt.Rhs           f.Resolution = store.Resolution1m
//	ValueSpec.Values         const DefaultKind = store.PriceKindClose
//	ReturnStmt.Results       return store.Resolution1m
//	CallExpr.Args            WithPriceKind(store.PriceKindMid)
//	CompositeLit element     Resolution: store.Resolution1m   (keyed)
//	                         {store.Resolution1h, lookback}   (positional)
//
// Four exclusions are load-bearing rather than incidental:
//
//   - THE MATCH IS ON THE AST, NEVER ON TEXT. Resolution1d's only two mentions
//     outside its own package are inside comments. A grep-shaped guard counts
//     those, reports "2 references", and passes on the exact constant it was
//     written for — the failure mode a-guard-that-matches-prose already produced
//     here three times. Comments are not expressions and cannot be reached by
//     ast.Inspect over CallExpr/AssignStmt/KeyValueExpr.
//   - A COMPOSITE-LITERAL KEY IS NOT A PRODUCER. store/bar.go declares
//     `var resolutions = map[Resolution]time.Duration{Resolution1m: time.Minute,
//     Resolution1h: time.Hour, Resolution1d: 24 * time.Hour}`. If keys counted,
//     the store's own interval table would vouch for all three series and this
//     guard would be green on day one having proved nothing. Only KeyValueExpr
//     .Value is read.
//   - A LITERAL OF THE FAMILY ITSELF IS A CATALOGUE, NOT A RECORD, and is skipped
//     whole. `[]store.Resolution{Resolution1d, Resolution1h, Resolution1m}` is
//     liquiditysource.CoarsestFirst — a READER's preference order, which says
//     which series it will try and nothing about whether any exists. THIS ONE
//     PRODUCED A FALSE GREEN DURING DEVELOPMENT: with slice elements counted,
//     that single line marked both coarse resolutions produced while the series
//     were still empty, on the exact defect the guard was written for. The same
//     shape covers `var kinds = []Kind{KindOption, KindSwap, KindFuture,
//     KindBond}` (terms/postgres.go:67) and `Kinds: []audit.Kind{...}` (soc2/
//     soc2.go:46) — an enumeration of a family must not vouch for its members, or
//     "is this value ever formed" degrades into "is this value ever listed".
//   - THE EXPRESSION MUST BE THE CONSTANT ITSELF, not contain it. `return k >=
//     store.PriceKindClose && k <= store.PriceKindLast` (spotsource/provider.go:
//     343) is a ReturnStmt result, but it is a BinaryExpr — a RANGE CHECK THAT
//     READS the enum, not a site that forms a value of it. Sub-expression
//     matching would make every validator vouch for every constant it validates,
//     which is precisely backwards: those validators are why the dark kinds are
//     reachable from config in the first place.
//
// A `case store.Resolution1h:` clause is not in the list at all, and that is the
// distinction the rule exists to draw. Switch arms, equality tests and range
// checks are the READ side; they prove somebody is prepared to handle the value,
// never that anything creates one. The store is full of code prepared to handle
// 1h bars.
//
// # What the rule misses, stated here rather than discovered later
//
//  1. IT FINDS CONSTANT MENTIONS, NOT VALUE FLOWS. The live 1m write path does
//     NOT name Resolution1m: marketdata.TranslateBar derives the series from the
//     candle's own interval via store.ResolutionOf(openTime, closeTime) and
//     stamps that. So a rollup job that computed `store.ResolutionOf(start,
//     start.Add(time.Hour))` would fill the 1h series while leaving Resolution1h
//     with zero producers here — a FALSE POSITIVE — and, symmetrically, a job
//     that names the constant but is never constructed at a composition root
//     would turn this green while writing nothing. The first failure is loud (a
//     guard failing on working code gets looked at); the second is the quiet one,
//     and it is why the exemption below states what "produced" must mean for the
//     rollup specifically. no_dark_capability_test.go is the guard that catches a
//     rollup package nobody imports; neither closes the loop alone.
//  2. A CONVERSION FROM A STRING IS INVISIBLE. `report.Format(f)` at services/
//     audit/internal/server/server.go:292 turns a `?format=` query parameter into
//     a Format, so FormatCSV is genuinely live and has no constant producer. That
//     is the FormatCSV exemption below, and it is a limit of the rule rather than
//     a defect in the estate. The same construct on the READ side —
//     `o.Kind = PriceKind(kind)` at store/postgres.go:173 — is a row being
//     decoded, not a value being created, and treating conversions as producers
//     would therefore silence the entire PriceKind family using its own scanner.
//     There is no syntactic difference between the two; only an exemption can
//     tell them apart, so the rule stays syntactic and the judgement stays
//     written down.
//  3. THIS DOES NOT DISTINGUISH A WRITE FROM A READ. `Resolution: store.
//     Resolution1m` inside a BarQuery (vwap.go:163) counts exactly as much as
//     `f.Resolution = store.Resolution1m` on a Bar about to be PutBars'd
//     (backfill.go:151). Separating them needs a type checker. What this guard
//     proves is narrower and still decisive: NO CODE ANYWHERE IN THE ESTATE EVER
//     FORMS THIS VALUE — which is strictly stronger evidence of an empty
//     partition than "the store has no writer", because it also rules out the
//     query ever being asked.
//  4. A PRODUCER INSIDE DEAD CODE COUNTS. Same blind spot as the whole-store
//     guard, one level finer, and the same answer: the exemption map is the
//     durable record, because a reason survives what a call graph cannot prove.
//  5. A VARIADIC OR OPTION-SHAPED CALL STILL COUNTS. Call arguments are how a
//     value enters most of these packages (WithPriceKind, WithResolution), so
//     they must count — but that means a READER's configuration counts too. The
//     catalogue exclusion above removes the slice-shaped version of this;
//     `WithResolution(store.Resolution1d)` in a consumer's wiring would still
//     vouch for a series nothing writes. No producer in this tree is reachable
//     only that way today.
//
// # The zero value is excluded BY RULE, not by exemption
//
// A constant whose literal value is the zero value of its type
// (PriceKindUnspecified = 0) is not a series. It is what a struct field carries
// when nobody set it, and in this store it means "any kind" on a Query
// (store.go:101) and is REFUSED on write (Observation.validate, store.go:88).
// Requiring a producer for it would demand that something deliberately construct
// the unset state, which is the opposite of what every consumer here does.

// storedSeriesWithoutAProducerExempt maps "<package>.<Constant>" to the argument
// for why nothing in production forms that value, and — where it is a defect —
// to the work that will. An entry saying only "unused" is how a series stays
// empty while four consumers read past it; the dead-entry arm below deletes the
// entry for you the day a producer lands.
var storedSeriesWithoutAProducerExempt = map[string]string{
	"internal/marketdata/store.PriceKindAdjustedClose": storedSeriesDarkPriceKind,
	"internal/marketdata/store.PriceKindOpen":          storedSeriesDarkPriceKind,
	"internal/marketdata/store.PriceKindVWAP":          storedSeriesDarkPriceKind,
	"internal/marketdata/store.PriceKindSettlement":    storedSeriesDarkPriceKind,
	"internal/identity.StatusDisabled": "REAL FINDING, no issue yet — THE PLATFORM CANNOT DISABLE " +
		"AN ACCOUNT. identity.User.Status gates authentication (user.go:47, Active() is what login " +
		"checks), the column exists with a CHECK constraint admitting exactly 'active' and " +
		"'disabled' (services/identity/migrations/0001_identity.sql:28,31), and the ONLY value " +
		"anything in this module ever writes is StatusActive, from UserFromInvite (user.go:62) on " +
		"the redeem path. identity.Postgres exposes CreateInvite, Redeem, UserBySubject, " +
		"UpdateCredential and InvitesFor — and no method that sets status. So an offboarded trader " +
		"or a compromised credential can only be locked out by a human running UPDATE against the " +
		"production database by hand, which is unaudited, unreplicated to any other tenant's " +
		"expectation, and not a control anybody can test. THE STATE IS HONOURED BUT UNREACHABLE: " +
		"unlike the market-data cases this is not an empty series being read past, it is a " +
		"SECURITY CONTROL WITH NO ENTRY POINT. It is exempted rather than fixed here because the " +
		"repair is a service surface (a disable endpoint on services/identity with its own authz " +
		"and its own audit record), not a line of code, and inventing one inside an arch guard's " +
		"diff is the wrong place for it. File it before deleting this entry.",
	"internal/marketdata/terms.KindBond": storedSeriesDarkTermsKind + "KindBond is the narrower " +
		"one: it was ADDED by #518 as a doc comment with no declaration, and " +
		"terms_kind_covers_the_oneof_test.go now proves the constant exists. THIS GUARD PROVES THE " +
		"NEXT THING, which that one cannot: existing is not being written. A bond is also the " +
		"variant the FI measures need — DV01, Duration, Convexity and SpreadDuration are all " +
		"registered off this store — so the label the whole read path depends on is the one " +
		"nothing produces.",
	"internal/marketdata/terms.KindSwap": storedSeriesDarkTermsKind + "KindSwap is the one that " +
		"was never in reach: OKX lists SWAP instruments, and termsload/okx.go:33 records the " +
		"decision NOT to label them terms.KindSwap — a perpetual swap is not the fixed/floating " +
		"leg structure the swap variant of the oneof describes, and storing one under that label " +
		"would put a thing ChainAsOf never finds under a name that says it should.",
	"services/audit/internal/report.FormatCSV": "NOT A DEFECT — a limit of the rule, recorded so " +
		"the next reader does not go looking for a bug. FormatCSV is reached in production through " +
		"a CONVERSION rather than the constant: services/audit/internal/server/server.go:292 does " +
		"`tmpl.Format = report.Format(f)` from the ?format= query parameter, and server.go:333 " +
		"dispatches `case report.FormatCSV` on the result, so the CSV renderer is live and " +
		"reachable by any caller who asks for it. This guard cannot see that, and MUST NOT be " +
		"taught to: the same construct on the read side is store/postgres.go:173 " +
		"`o.Kind = PriceKind(kind)`, a row being decoded out of a column, and counting conversions " +
		"as producers would let the store's own scanner vouch for all eight PriceKinds and silence " +
		"the whole family. The judgement lives here instead of in the rule. This entry goes stale " +
		"the day the templates offer a CSV default, which is fine — deleting it then is correct.",
}

// storedSeriesDarkPriceKind is shared by the four PriceKind values nothing
// writes. One argument, four entries, because the dead-entry arm must be able to
// retire them independently: whichever kind gets a producer first should force
// its own line to be deleted rather than let a joint entry keep vouching for the
// other three.
const storedSeriesDarkPriceKind = "REAL FINDING, tracked under #509 as the same class — FOUR OF " +
	"THE EIGHT PriceKinds ARE ACCEPTED BY CONFIGURATION AND WRITTEN BY NOTHING. The ingest path " +
	"stamps exactly three (internal/marketdata/ingest.go:181,187,190 — Close, Last, Mid) and " +
	"AdjustedClose, Open, VWAP and Settlement are formed nowhere in the module. THE REASON THIS " +
	"IS A FINDING AND NOT A DORMANT ENUM is internal/risk/spotsource: isKnownKind (provider.go:" +
	"343) admits `k >= PriceKindClose && k <= PriceKindLast`, i.e. all seven non-zero kinds, so " +
	"WithPriceKind(store.PriceKindVWAP) passes construction validation and every option in the " +
	"book then resolves to no spot — SkipNoSpot on every position, a Gamma of zero, and a book " +
	"that looks like it holds no options. That package's own doc (provider.go:38-52) describes " +
	"exactly this failure for a DIFFERENT kind and calls it total and uniform; what it does not " +
	"say is that four of the kinds it accepts can never match, whatever the deployment does. The " +
	"same range check guards nothing on the store side either: Observation.validate refuses only " +
	"the zero kind. THE CHEAP HALF OF THE FIX is to narrow the accepted set to the kinds the " +
	"estate writes and fail construction loudly on the rest — 'nothing configured' and 'checked, " +
	"and fine' must not look the same. The expensive half is a vendor feed that supplies them, " +
	"which no path in this repository carries: backfill writes bars, market-ingest folds quotes " +
	"to a mid, and the vendor Source is an unimplemented SDK seam. Deleting the constants is NOT " +
	"the remedy — PriceKind mirrors reference.v1.PriceKind by number so the two never drift, and " +
	"renumbering a wire enum to tidy a Go const block is a data-corruption bug."

// storedSeriesDarkTermsKind is the shared half of the two contract-terms labels
// nothing writes. Split into two entries for the same reason as the PriceKinds:
// whichever gets a producer first must retire its own line.
const storedSeriesDarkTermsKind = "#509 — a contract-terms LABEL nothing writes, two levels " +
	"below a store that has no production writer at all. terms.Postgres is already exempt in " +
	"store_has_a_writer_test.go because Put has no non-test caller; the only thing in the module " +
	"that produces a terms.Record is internal/marketdata/termsload, which is itself exempt in " +
	"no_dark_capability_test.go for having no importer — and even if it were wired, okx.go:262-268 " +
	"labels exactly two kinds, KindOption and KindFuture. NOTE HOW THIS DIFFERS FROM THE " +
	"CATALOGUE: terms/postgres.go:67 lists all four in `var kinds = []Kind{...}` and Kinds() " +
	"exports them, so a guard that counted a list as a producer would call all four live. "

// storedSeriesConst identifies one enumerated value by its declaring package and
// constant name. The type is carried for the failure message only; resolution is
// at package + constant name, which is exact — Go has no overloading, so a
// package-qualified constant name names one thing.
type storedSeriesConst struct {
	pkg  string
	typ  string
	name string
}

func (c storedSeriesConst) String() string { return c.pkg + "." + c.name }

// storedSeriesFamily is one enum: the type, its exported members, and which
// durable record carries it.
type storedSeriesFamily struct {
	pkg     string
	typ     string
	carrier string // the struct whose field made it durable, for the message
	members []string
	zero    map[string]bool
}

func TestEveryStoredSeriesValueHasAProducer(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// ===== 1. Discover the families =====
	families := storedSeriesFamilies(t, root, fset)

	// NON-VACUITY, FIRST DIRECTION: discovery. If the basic-type clause, the
	// two-constant clause or the ctx-first carrier clause breaks, this finds
	// nothing and the guard passes having inspected zero constants — which is the
	// same silent-emptiness failure it exists to report, committed by the guard
	// itself.
	//
	// MEASURED, not guessed: 10 families / 38 constants today. Deleting the
	// durability clause takes it the WRONG way, to 24 families / 89 constants, so
	// a ceiling would be the arm that catches that — and a ceiling is not worth
	// having, because families legitimately appear and a guard that fails when the
	// estate grows an enum is one that gets edited rather than read. Every way of
	// breaking the walk itself (the const spec walk, the type walk, the internal/
	// filter) collapses the count instead, which is what this floor catches. 6/24
	// is below today's 10/38 by enough that a family being deleted does not trip
	// it.
	total := 0
	for _, f := range families {
		total += len(f.members)
	}
	if len(families) < 6 || total < 24 {
		t.Fatalf("found only %d constant families / %d constants under internal/ — the family "+
			"discovery is broken, not the estate. internal/marketdata/store.Resolution, "+
			"internal/marketdata/store.PriceKind, internal/marketdata/terms.Kind and "+
			"services/audit/internal/audit.Kind must all be found", len(families), total)
	}

	// ===== 2. Resolve producers through each file's import block =====
	produced := storedSeriesProducers(t, root, fset, families)

	// ===== 3. Default-deny =====
	var dark []string
	seenExempt := map[string]bool{}
	live, nonZero := 0, 0

	for _, fam := range families {
		for _, name := range fam.members {
			ref := storedSeriesConst{fam.pkg, fam.typ, name}
			// The zero value is "unset", not a series. See the doc comment.
			if fam.zero[name] {
				continue
			}
			nonZero++
			if len(produced[ref]) > 0 {
				live++
				continue
			}
			if reason, ok := storedSeriesWithoutAProducerExempt[ref.String()]; ok {
				seenExempt[ref.String()] = true
				t.Logf("%s (%s carried by %s): no production producer — tracked: %s",
					ref, fam.typ, fam.carrier, reason)
				continue
			}
			dark = append(dark, ref.String()+" (field type "+fam.typ+" on "+fam.pkg+"."+fam.carrier+")")
		}
	}

	// NON-VACUITY, SECOND DIRECTION: producer resolution. If the import-scoped
	// lookup or the producer-position set breaks, every constant looks dark at
	// once and the correct response to a bug in THIS FILE would look like "add
	// thirty exemptions".
	//
	// THE THRESHOLD IS MEASURED, NOT GUESSED, and the measurement is why it is not
	// lower. 29 of 37 non-zero constants resolve to a producer today. Corrupting
	// the cross-package half of the resolution — so no import ever matches a
	// family package, which is the single most likely way for this file to break
	// silently — only takes it to 16, because IN-PACKAGE producers (audit/classify
	// .go, proxy/proxy.go, llm/stub.go, identity/user.go) hold up a floor entirely
	// on their own. Any threshold at or below 16 SURVIVES that mutation and ships
	// an arm that fires for nothing; the obvious "more than half" guess of 19
	// would have been one of them. 25 fails it, with four constants of headroom
	// for the estate to move.
	if live < 25 {
		t.Fatalf("only %d of %d non-zero enumerated constants resolved to ANY producer — the "+
			"import-scoped resolution or the producer-position set is broken, not the estate. "+
			"store.Resolution1m, store.PriceKindClose, audit.KindCommand and proxy.ServiceWealth "+
			"are all produced in production and must resolve", live, nonZero)
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d enumerated value(s) on a durable record have NO production producer: %v.\n"+
			"Nothing in this module ever forms that value, so the partition it labels is empty and "+
			"a query filtering on it returns 'we hold none of those' — indistinguishable from a "+
			"series nobody ever wrote. store.Resolution1h and store.Resolution1d are why this "+
			"guard exists (#509): both are stored, indexed, queryable and Valid(), the ingest seam "+
			"refuses them from a venue by design, and no rollup fills them — so every consumer "+
			"silently falls back to the 1m base series, which costs liquiditysource ~40,000 rows "+
			"per instrument per call.\n"+
			"Neither guard next door can see this. no_dark_capability_test.go is IMPORT "+
			"granularity and the store is imported nine times; store_has_a_writer_test.go is "+
			"WHOLE-STORE granularity and PutBars has live callers. Both are green and two of the "+
			"three series are empty.\n"+
			"Produce the value, or add an entry to storedSeriesWithoutAProducerExempt NAMING THE "+
			"ISSUE AND WHAT WILL PRODUCE IT. Deleting the constant is NOT the remedy where it "+
			"mirrors a wire enum by number (store.PriceKind mirrors reference.v1.PriceKind).",
			len(dark), dark)
	}

	// DEAD-ENTRY ARM. An exemption for a constant that now has a producer means
	// somebody built the rollup, the loader or the endpoint; one for a constant
	// that no longer exists means it was renamed or its family stopped being
	// durable. Either way the entry asserts something about the estate that is no
	// longer true, and an exemption must not outlive its repair.
	for name, reason := range storedSeriesWithoutAProducerExempt {
		if seenExempt[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — the constant now has a production producer, was "+
			"renamed or removed, or its family no longer carries a durable record. Delete the "+
			"entry (%s)", name, reason)
	}
}

// storedSeriesFamilies finds every enumerated family on a durable record, sorted
// for a deterministic failure message.
//
// It reads the SOURCE rather than importing the packages, for the reason
// terms_kind_covers_the_oneof_test.go states: test/arch may not import
// internal/risk (risk_boundary_test.go), several families live behind build tags,
// and a source-level guard keeps working when a package does not compile.
func storedSeriesFamilies(t *testing.T, root string, fset *token.FileSet) []storedSeriesFamily {
	t.Helper()

	type pkgFacts struct {
		enumTypes  map[string]bool            // named types over a basic type
		members    map[string][]string        // enum type -> exported constant names
		zero       map[string]bool            // constants whose literal is the type's zero value
		fieldTypes map[string]map[string]bool // struct name -> types of its exported fields
		durable    map[string]bool            // struct names appearing in a ctx-first signature
	}
	pkgs := map[string]*pkgFacts{}
	factsFor := func(p string) *pkgFacts {
		if pkgs[p] == nil {
			pkgs[p] = &pkgFacts{
				enumTypes:  map[string]bool{},
				members:    map[string][]string{},
				zero:       map[string]bool{},
				fieldTypes: map[string]map[string]bool{},
				durable:    map[string]bool{},
			}
		}
		return pkgs[p]
	}

	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if !isModuleInternalPkg(pkgDir) {
			return
		}
		p := factsFor(pkgDir)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// A ctx-first function makes every named type in its signature
				// durable — the store interfaces are the main source, but a
				// concrete method (Postgres.Put) qualifies its arguments too.
				if d.Type != nil && takesContextFirst(d.Type.Params) {
					for _, n := range storedSeriesSigTypes(d.Type) {
						p.durable[n] = true
					}
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						switch u := ts.Type.(type) {
						case *ast.Ident:
							if storedSeriesIsBasic(u.Name) {
								p.enumTypes[ts.Name.Name] = true
							}
						case *ast.StructType:
							if p.fieldTypes[ts.Name.Name] == nil {
								p.fieldTypes[ts.Name.Name] = map[string]bool{}
							}
							for _, fl := range u.Fields.List {
								id, ok := fl.Type.(*ast.Ident)
								if !ok {
									continue
								}
								for _, n := range fl.Names {
									if n.IsExported() {
										p.fieldTypes[ts.Name.Name][id.Name] = true
									}
								}
							}
						case *ast.InterfaceType:
							for _, m := range u.Methods.List {
								ft, ok := m.Type.(*ast.FuncType)
								if !ok || !takesContextFirst(ft.Params) {
									continue
								}
								for _, n := range storedSeriesSigTypes(ft) {
									p.durable[n] = true
								}
							}
						}
					}
				case token.CONST:
					for i, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok || vs.Type == nil {
							continue
						}
						id, ok := vs.Type.(*ast.Ident)
						if !ok {
							continue
						}
						for j, n := range vs.Names {
							if !n.IsExported() {
								continue
							}
							p.members[id.Name] = append(p.members[id.Name], n.Name)
							if j < len(vs.Values) && storedSeriesIsZeroValue(vs.Values[j], i) {
								p.zero[n.Name] = true
							}
						}
					}
				}
			}
		}
	})

	var out []storedSeriesFamily
	for pkg, p := range pkgs {
		for typ, members := range p.members {
			if len(members) < 2 || !p.enumTypes[typ] {
				continue
			}
			// The carrier: a struct in this package with a field of this type,
			// that some ctx-first signature mentions. Lowest name wins so the
			// failure message is stable when a type has several carriers (Bar and
			// BarQuery both carry a Resolution).
			carrier := ""
			for s, fields := range p.fieldTypes {
				if fields[typ] && p.durable[s] && (carrier == "" || s < carrier) {
					carrier = s
				}
			}
			if carrier == "" {
				continue
			}
			sort.Strings(members)
			out = append(out, storedSeriesFamily{
				pkg: pkg, typ: typ, carrier: carrier, members: members, zero: p.zero,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].pkg+"."+out[i].typ < out[j].pkg+"."+out[j].typ
	})
	return out
}

// storedSeriesProducers maps each family constant to the file:line of every
// production site that FORMS it.
//
// Resolution is IMPORT-SCOPED, NEVER BY BARE NAME, the same standard
// store_has_a_writer_test.go and no_dark_measure_seam_test.go both hold: `Kind`
// constants are declared in three different packages here, and a bare-name match
// would let services/audit's KindCommand vouch for internal/marketdata/terms'
// KindBond. A file may produce a family's value only if it IMPORTS that family's
// package, or is in it.
func storedSeriesProducers(
	t *testing.T, root string, fset *token.FileSet, families []storedSeriesFamily,
) map[storedSeriesConst][]string {
	t.Helper()

	byPkg := map[string]map[string]storedSeriesConst{}
	typesByPkg := map[string]map[string]bool{}
	for _, f := range families {
		if byPkg[f.pkg] == nil {
			byPkg[f.pkg] = map[string]storedSeriesConst{}
			typesByPkg[f.pkg] = map[string]bool{}
		}
		typesByPkg[f.pkg][f.typ] = true
		for _, m := range f.members {
			byPkg[f.pkg][m] = storedSeriesConst{f.pkg, f.typ, m}
		}
	}

	out := map[storedSeriesConst][]string{}
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		self, selfTypes := byPkg[pkgDir], typesByPkg[pkgDir]
		// Local import identifier -> the family constants, and the family TYPE
		// names, it can qualify.
		viaImport := map[string]map[string]storedSeriesConst{}
		typesViaImport := map[string]map[string]bool{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			dir, ok := strings.CutPrefix(path, modulePath+"/")
			if !ok || byPkg[dir] == nil {
				continue
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			viaImport[name] = byPkg[dir]
			typesViaImport[name] = typesByPkg[dir]
		}
		if self == nil && len(viaImport) == 0 {
			return
		}

		// record accepts an expression that is in a PRODUCER POSITION and counts
		// it only if it IS a constant reference — never if it merely contains
		// one. See the doc comment: a BinaryExpr range check is a read.
		record := func(e ast.Expr) {
			var c storedSeriesConst
			var ok bool
			switch x := e.(type) {
			case *ast.Ident:
				if self == nil {
					return
				}
				c, ok = self[x.Name]
			case *ast.SelectorExpr:
				id, isIdent := x.X.(*ast.Ident)
				if !isIdent {
					return
				}
				m, known := viaImport[id.Name]
				if !known {
					return
				}
				c, ok = m[x.Sel.Name]
			}
			if !ok {
				return
			}
			out[c] = append(out[c], rel+":"+strconv.Itoa(fset.Position(e.Pos()).Line))
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, r := range x.Rhs {
					record(r)
				}
			case *ast.ValueSpec:
				for _, v := range x.Values {
					record(v)
				}
			case *ast.ReturnStmt:
				for _, r := range x.Results {
					record(r)
				}
			case *ast.CallExpr:
				// Args, never Fun: functional options are how a value enters
				// most of these packages (WithPriceKind, WithResolution).
				for _, a := range x.Args {
					record(a)
				}
			case *ast.CompositeLit:
				// A LITERAL OF THE FAMILY ITSELF IS A CATALOGUE, NOT A RECORD.
				// `[]store.Resolution{Resolution1d, Resolution1h, Resolution1m}`
				// (liquiditysource.CoarsestFirst) says which series a READER will
				// try; `map[Resolution]time.Duration{...}` (store/bar.go) says how
				// long each one lasts; `[]Kind{KindOption, KindSwap, KindFuture,
				// KindBond}` (terms/postgres.go) says which labels exist. None of
				// them writes a row, and counting them would let one line vouch
				// for an entire family — turning "is this value ever formed" into
				// "is this value ever listed". Every KeyValueExpr in the module is
				// inside a composite literal, so this is where they are read, and
				// a KEY is never a producer for the same reason.
				if storedSeriesIsCatalogue(x.Type, selfTypes, typesViaImport) {
					return true
				}
				for _, el := range x.Elts {
					if kv, keyed := el.(*ast.KeyValueExpr); keyed {
						record(kv.Value)
					} else {
						record(el)
					}
				}
			}
			return true
		})
	})
	return out
}

// storedSeriesIsCatalogue reports whether a composite literal's type is a slice,
// array or map whose ELEMENT is one of the enum families — the shape that
// enumerates a family rather than labelling a record.
//
// A nil literal type is an elided inner element (`[]struct{...}{{Resolution1h,
// lag}}` at services/market-data/cmd/market-data/rollup.go:127) and is NOT a
// catalogue: the outer literal already answered that question, and treating it as
// one would lose the composition root that names both coarse resolutions.
func storedSeriesIsCatalogue(
	lit ast.Expr, selfTypes map[string]bool, viaImport map[string]map[string]bool,
) bool {
	var elem ast.Expr
	switch x := lit.(type) {
	case *ast.ArrayType:
		elem = x.Elt
	case *ast.MapType:
		elem = x.Value
	default:
		return false
	}
	switch e := elem.(type) {
	case *ast.Ident:
		return selfTypes[e.Name]
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && viaImport[id.Name][e.Sel.Name]
	}
	return false
}

// storedSeriesIsBasic reports whether a type name is one of Go's basic types —
// the underlying type an enum is defined over.
func storedSeriesIsBasic(name string) bool {
	switch name {
	case "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune", "float32", "float64":
		return true
	}
	return false
}

// storedSeriesIsZeroValue reports whether a constant's initialiser is its type's
// zero value — "" or 0, or the first `iota` in a block.
//
// specIndex is needed for the iota form: `A T = iota` at index 0 IS zero, while
// the same expression could not appear later. Constants declared without a value
// (the implicit-repetition form) are not reached here at all, and none of the
// families in this tree use it for their first member.
func storedSeriesIsZeroValue(v ast.Expr, specIndex int) bool {
	switch x := v.(type) {
	case *ast.BasicLit:
		s := strings.Trim(x.Value, `"`+"`")
		return s == "" || s == "0"
	case *ast.Ident:
		return x.Name == "iota" && specIndex == 0
	}
	return false
}

// storedSeriesSigTypes returns the named types a signature mentions, seeing
// through pointers and slices so `PutBars(ctx, []Bar)` makes Bar durable.
func storedSeriesSigTypes(ft *ast.FuncType) []string {
	var out []string
	add := func(e ast.Expr) {
		for {
			switch x := e.(type) {
			case *ast.StarExpr:
				e = x.X
			case *ast.ArrayType:
				e = x.Elt
			case *ast.Ident:
				out = append(out, x.Name)
				return
			default:
				return
			}
		}
	}
	for _, fl := range []*ast.FieldList{ft.Params, ft.Results} {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			add(f.Type)
		}
	}
	return out
}
