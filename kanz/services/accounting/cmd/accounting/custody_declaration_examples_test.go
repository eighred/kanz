package main

// THE SYNTAX THIS SERVICE ADVERTISES MUST BE THE SYNTAX ITS BINARY ACCEPTS (#1029).
//
// ACCOUNTING_CUSTODY_ACCOUNTS is declared in one place and interpreted in
// another, and for the whole life of #1006 the two disagreed: config split it
// with env.SplitList — comma — while the parser used comma as the separator
// between ONE custodian's accounts. A custodian holding two accounts therefore
// had its entry cut in half and the pod exited 2 naming a fragment the operator
// never typed, which made #1006's custodian scoping reachable only for
// single-account custodians. That is not the shape an institutional portfolio is
// in, so the repair was unreachable for the case it was written for.
//
// Three places told an operator the syntax — this package's doc comments, the
// refusal NewBookScope prints, and the deployment manifest — and all three were
// copies of one claim with nothing keeping them equal. THIS FILE MAKES THEM
// EXECUTABLE. Every advertised example is extracted from the source that
// advertises it and pushed through the real parser; the refusal's remediation is
// applied to the refusal that printed it.
//
// THE EXAMPLE SET IS DERIVED, NOT LISTED. A list of expected strings written here
// would be a fourth copy of the thing that broke. The walk finds every comment in
// services/accounting that advertises a value for either variable, so a new copy
// is covered the moment somebody writes it.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

const (
	accountsKey = "ACCOUNTING_CUSTODY_ACCOUNTS="
	pairsKey    = "ACCOUNTING_CUSTODY_PAIRS="

	accountingRoot = "../.."
	deployManifest = "../../../../infra/deploy/accounting-deploy.yaml"
)

// advertised is one example an operator could copy out of the tree.
type advertised struct {
	source string
	key    string
	value  string
}

// valueAfter returns the rest of the LINE following key, or "" when key is absent.
// The line is the unit because that is how an operator copies one: everything up
// to the newline is the value, and a value that has to be reassembled from two
// lines is not one somebody can paste.
func valueAfter(line, key string) string {
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(line[i+len(key):]), `"'`)
}

// advertisedDeclarations collects every example of either variable from the Go
// COMMENTS under services/accounting and from the deployment manifest.
//
// Comments, not string literals, for the Go side: the one advertised value that
// is not a comment is NewBookScope's remediation, and it is a format string whose
// verbs are filled at runtime. Reading its source text would prove only that "%s"
// is not a valid declaration. It is checked instead by
// TestTheRemediationARefusalPrintsActuallyLiftsIt, which applies the rendered
// message — a stronger property than this walk could assert.
func advertisedDeclarations(t *testing.T) []advertised {
	t.Helper()
	var out []advertised

	fset := token.NewFileSet()
	err := filepath.WalkDir(accountingRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, cg := range f.Comments {
			for _, line := range strings.Split(cg.Text(), "\n") {
				for _, key := range []string{accountsKey, pairsKey} {
					if v := valueAfter(line, key); v != "" {
						out = append(out, advertised{source: filepath.ToSlash(path), key: key, value: v})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for advertised declarations: %v", accountingRoot, err)
	}

	for _, line := range strings.Split(readManifest(t), "\n") {
		for _, key := range []string{accountsKey, pairsKey} {
			if v := valueAfter(line, key); v != "" {
				out = append(out, advertised{source: deployManifest, key: key, value: v})
			}
		}
	}
	return out
}

func readManifest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(deployManifest)
	if err != nil {
		t.Fatalf("reading %s: %v — the deployment manifest is one of the three places that tell an "+
			"operator this syntax, and this guard cannot check a file it cannot find. Repoint it "+
			"rather than deleting it.", deployManifest, err)
	}
	return string(b)
}

// TestEverySyntaxThisServiceAdvertisesParses is the invariant: what the tree
// tells an operator to write is what the binary accepts.
func TestEverySyntaxThisServiceAdvertisesParses(t *testing.T) {
	examples := advertisedDeclarations(t)

	// NON-VACUITY. Every assertion below is per-example, so a walk that found
	// nothing would pass by not looking — which is exactly how three copies of one
	// claim stayed wrong. Four are expected today: the ParseCustodyAccounts doc,
	// the config.Config field doc, and both manifest lines.
	var fromManifest, accounts, pairs int
	for _, e := range examples {
		if e.source == deployManifest {
			fromManifest++
		}
		switch e.key {
		case accountsKey:
			accounts++
		case pairsKey:
			pairs++
		}
	}
	if len(examples) < 3 || fromManifest == 0 || accounts == 0 || pairs == 0 {
		t.Fatalf("found %d advertised declaration(s) (%d from the manifest, %d accounts, %d pairs): %+v.\n\n"+
			"This guard derives its example set by reading the sources that advertise the syntax. "+
			"Finding almost none means it is reading nothing — repair the derivation rather than "+
			"trusting the pass.", len(examples), fromManifest, accounts, pairs, examples)
	}

	for _, e := range examples {
		switch e.key {
		case accountsKey:
			decl, err := custody.ParseCustodyAccounts(e.value)
			if err != nil {
				t.Errorf("%s advertises %s%s, which the binary REFUSES: %v\n\n"+
					"An operator following this gets a pod that will not start, with a message naming "+
					"something they did not write. That is #1029.", e.source, e.key, e.value, err)
				continue
			}
			// AN EXAMPLE THAT DEGRADES TO ONE ACCOUNT PER CUSTODIAN STILL PARSES,
			// and would have parsed before #1029 too — it is the shape the defect
			// forced every example into. The multi-account custodian is the case
			// #1006 exists for, so the advertised example has to show it.
			if !anyCustodianHasSeveralAccounts(decl) {
				t.Errorf("%s advertises %s%s, in which no custodian holds more than one account.\n\n"+
					"That is the single-account shape the #1029 defect forced, and it demonstrates "+
					"nothing about the separator an operator has to get right. Show a custodian with "+
					"two accounts.", e.source, e.key, e.value)
			}
		case pairsKey:
			// env.SplitList is what config actually applies to this variable, so
			// the example goes through it rather than through a split written here.
			if _, err := parseCustodyPairs(env.SplitList(e.value)); err != nil {
				t.Errorf("%s advertises %s%s, which the binary REFUSES: %v",
					e.source, e.key, e.value, err)
			}
		}
	}
}

func anyCustodianHasSeveralAccounts(decl map[string]map[string][]string) bool {
	for _, byCustodian := range decl {
		for _, ids := range byCustodian {
			if len(ids) > 1 {
				return true
			}
		}
	}
	return false
}

// TestTheManifestsOwnExampleStartsTheService pushes the deployment manifest's two
// lines through the composition root, together, as an operator would.
//
// The manifest is the copy furthest from the parser and the only one an operator
// edits under pressure. Parsing its lines separately would miss the failure that
// matters: a PAIRS list and an ACCOUNTS list that each parse and do not agree —
// a custodian declared that is not a pair, or a pair with no accounts — is
// refused at boot, and the manifest would then be advertising a pod that does not
// start.
func TestTheManifestsOwnExampleStartsTheService(t *testing.T) {
	var pairsSpec, accountsSpec string
	for _, line := range strings.Split(readManifest(t), "\n") {
		if v := valueAfter(line, pairsKey); v != "" {
			pairsSpec = v
		}
		if v := valueAfter(line, accountsKey); v != "" {
			accountsSpec = v
		}
	}
	if pairsSpec == "" || accountsSpec == "" {
		t.Fatalf("%s no longer shows both %s and %s (pairs=%q accounts=%q).\n\n"+
			"A control nobody can find in the manifest is a control nobody knows is absent — and this "+
			"guard has just stopped checking the copy an operator is most likely to follow.",
			deployManifest, pairsKey, accountsKey, pairsSpec, accountsSpec)
	}

	cfg := baseCfg()
	cfg.CustodyPairs = env.SplitList(pairsSpec)
	cfg.CustodyAccounts = accountsSpec

	cc, err := buildCustodyConfig(cfg)
	if err != nil {
		t.Fatalf("the manifest's own example does not start this service: %v\n\n"+
			"pairs=%q accounts=%q", err, pairsSpec, accountsSpec)
	}
	if len(cc.pairs) < 2 {
		t.Fatalf("the manifest example configures %d pair(s), want at least 2 — a single-custodian "+
			"portfolio never reaches the scoping #1006 added, so the example would demonstrate "+
			"nothing about it", len(cc.pairs))
	}

	portfolio := cc.pairs[0].PortfolioID
	if !cc.scope.Declared(portfolio) {
		t.Fatalf("portfolio %s has NO declared per-custodian accounts in the manifest's example — the "+
			"scope would have to be derived from the journal, which cannot say which of two custodians "+
			"holds what, so every run compares one custodian's statement against holdings at both. "+
			"That is #1006 itself",
			portfolio)
	}
	several := false
	for _, c := range cc.scope.Custodians(portfolio) {
		scope, _ := cc.scope.For(portfolio, c)
		if len(scope) == 0 {
			t.Errorf("custodian %s of %s has an empty account scope — its book folds to nothing and "+
				"every position it holds breaks as MISSING_IN_IBOR", c, portfolio)
		}
		if len(scope) > 1 {
			several = true
		}
	}
	if !several {
		t.Error("no custodian in the manifest's example holds more than one exchange account, so the " +
			"deployment contract still only demonstrates the single-account case #1029 was filed on")
	}
}

// TestTheRemediationARefusalPrintsActuallyLiftsIt applies the message to the
// refusal that produced it.
//
// THE OLD REMEDIATION DID NOT START THE POD. It ended with a one-entry example
// naming a single account of a single custodian — printed by a refusal that fires
// precisely because a portfolio has two custodians, neither of which the one
// entry covers. An operator who applied it literally got the same exit 2 naming
// custodian. A remediation that does not lift the failure it is printed under is
// a wrong answer with an authoritative tone, and no unit test of the parser can
// see it: the message is correct syntax and incomplete configuration.
func TestTheRemediationARefusalPrintsActuallyLiftsIt(t *testing.T) {
	pairs := []custody.Subject{
		{PortfolioID: "PF1", CustodianID: "CUST-A"},
		{PortfolioID: "PF1", CustodianID: "CUST-B"},
	}

	for _, tc := range []struct {
		name string
		// declared is what the operator had already written when the refusal
		// fired; the remediation must not silently discard it.
		declared string
	}{
		{"nothing declared", ""},
		{"one custodian declared", "PF1:CUST-A:okx-sub-1,okx-sub-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			had, err := custody.ParseCustodyAccounts(tc.declared)
			if err != nil {
				t.Fatalf("the fixture declaration does not parse: %v", err)
			}
			_, err = custody.NewBookScope(pairs, had)
			if err == nil {
				t.Fatal("a two-custodian portfolio with an incomplete declaration was ACCEPTED — " +
					"every run would compare the whole book against one custodian's statement")
			}

			remedy := remediationLine(t, err.Error())
			fixed, perr := custody.ParseCustodyAccounts(remedy)
			if perr != nil {
				t.Fatalf("the refusal tells an operator to set %s%s, which does not parse: %v",
					accountsKey, remedy, perr)
			}
			if _, err := custody.NewBookScope(pairs, fixed); err != nil {
				t.Fatalf("applying the refusal's own remediation does not lift the refusal: %v\n\n"+
					"remediation was %s%s. An operator following the message gets the same exit 2, "+
					"which is #1029.", err, accountsKey, remedy)
			}
			if got := len(fixed["PF1"]); got != len(pairs) {
				t.Errorf("the remediation covers %d of %d custodians (%v) — an incomplete one is "+
					"refused the same way, so it does not start the pod", got, len(pairs), fixed)
			}
			// The operator's existing work survives: a template that replaced a
			// real account list with a placeholder would silently un-scope the
			// custodian that WAS declared correctly.
			for custodian, ids := range had["PF1"] {
				if strings.Join(fixed["PF1"][custodian], ",") != strings.Join(ids, ",") {
					t.Errorf("the remediation rewrote %s's already-declared accounts %v as %v",
						custodian, ids, fixed["PF1"][custodian])
				}
			}
		})
	}
}

// remediationLine extracts the value the refusal tells the operator to set: the
// rest of the line after the variable name, which is the unit somebody copies.
func remediationLine(t *testing.T, msg string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\n") {
		if v := valueAfter(line, accountsKey); v != "" {
			return v
		}
	}
	t.Fatalf("the refusal prints no %s value at all, so it tells an operator nothing to do:\n%s",
		accountsKey, msg)
	return ""
}

// TestATwoAccountCustodianReachesTheBookScope is the issue's own case, at the
// composition root rather than in a unit test that builds the scope in Go.
//
// The defect lived in the ENVIRONMENT PARSE, which every in-Go test walked past:
// the packages beneath main were covered, they were handed already-split values,
// and none of them could see that the split was wrong. This one starts from the
// declaration string.
func TestATwoAccountCustodianReachesTheBookScope(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = env.SplitList("PF1:CUST-A,PF1:CUST-B")
	cfg.CustodyAccounts = "PF1:CUST-A:okx-sub-1,okx-sub-2 PF1:CUST-B:bin-main"

	cc, err := buildCustodyConfig(cfg)
	if err != nil {
		t.Fatalf("a custodian holding two exchange accounts was refused: %v\n\n"+
			"This is #1029: the declaration #1006's repair requires could not be written for the "+
			"multi-account custodian it was written for.", err)
	}
	scope, claimed := cc.scope.For("PF1", "CUST-A")
	if len(scope) != 2 || !scope.Has("okx-sub-1") || !scope.Has("okx-sub-2") {
		t.Fatalf("CUST-A's book scope is %v, want both okx-sub-1 and okx-sub-2 — a scope missing one "+
			"of them folds a partial book and breaks every position it omitted", scope)
	}
	if !claimed.Has("bin-main") {
		t.Error("bin-main is not in PF1's claimed set, so a holding at CUST-B would look like an " +
			"account no custodian claims rather than one held elsewhere")
	}
	if other, _ := cc.scope.For("PF1", "CUST-B"); other.Has("okx-sub-2") {
		t.Error("okx-sub-2 leaked into CUST-B's scope — the entry was split across custodians")
	}
}

// TestAPairEntryContainingWhitespaceRefusesToStart closes the disagreement
// between the two parsers (#1029).
//
// ACCOUNTING_CUSTODY_ACCOUNTS separates entries by whitespace, so an operator who
// has just written one naturally writes the pair list the same way.
// parseCustodyPairs cut on the FIRST colon and accepted "PF1:CUST-A PF2:CUST-B"
// as ONE pair whose custodian id is "CUST-A PF2:CUST-B" — a name no custodian
// statement will ever carry, so that pair reconciles as NO_STATEMENT forever
// while PF2 goes unreconciled entirely, and the pod starts clean. Its own doc
// says a malformed pair must be an error and not a skip; swallowing one is worse
// than skipping it, because the phantom pair exports healthy-looking series.
func TestAPairEntryContainingWhitespaceRefusesToStart(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = env.SplitList("PF1:CUST-A PF2:CUST-B")

	_, err := buildPlane(t, cfg, &capturingHandler{})
	if err == nil {
		t.Fatal("a whitespace-joined pair list was accepted — PF2 is unreconciled and PF1's pair " +
			"carries a custodian id no statement will ever name, with nothing said")
	}
	for _, want := range []string{"whitespace", "COMMAS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot see which separator is "+
				"wrong: %v", want, err)
		}
	}
}
