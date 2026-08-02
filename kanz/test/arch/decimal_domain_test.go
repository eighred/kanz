package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The platform's Decimal domain bound is written down TWICE, and nothing in the
// compiler ties the two together.
//
//	internal/dec         maxSafeExponent    — guards FromProtoChecked, which the
//	                                          market-data fold calls on untrusted
//	                                          prices off market.>
//	internal/compliance  maxDecimalExponent — guards PreTradeGate.Evaluate, which
//	                                          the OMS calls on wire-derived orders
//
// They are duplicated, and each doc comment names its sibling. But a comment is
// not a check, and this repository has spent a lot of effort on exactly that
// distinction — a claim that was true when written and quietly false later.
//
// This comment was itself an example (#216). It used to say the duplication was
// deliberate because "the two packages must not import each other". They can:
// internal/compliance imports internal/dec as decutil, and since #216 it gets
// its Decimal arithmetic from there. Only dec→compliance is impossible, and the
// cycle forbids that without needing a rule. So the duplication is now a
// LEFTOVER, not a design: exporting maxSafeExponent and deleting
// maxDecimalExponent is a real option, and doing it means rewriting this guard
// (it reads both literals out of the source) in the same change.
//
// Both bounds exist to stop the same thing: an unbounded 10^abs(exponent) on a
// path reachable from outside. Both were verified to hang before they were
// added. If a future change tightens or loosens one, the other must move with
// it, or one ingress silently accepts what the other refuses.
//
// This reads the literals out of the source rather than importing them, because
// both are unexported and exporting them purely to satisfy a test would widen
// two package APIs to work around a missing dependency edge.
var (
	decBoundPattern        = regexp.MustCompile(`(?m)^const maxSafeExponent\s*=\s*(\d+)`)
	complianceBoundPattern = regexp.MustCompile(`(?m)^const maxDecimalExponent\s*=\s*(\d+)`)
)

func TestDecimalDomainBoundsAgree(t *testing.T) {
	root := moduleRoot(t)

	read := func(rel string, pat *regexp.Regexp, name string) string {
		path := filepath.Join(root, filepath.FromSlash(rel))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		m := pat.FindSubmatch(b)
		if m == nil {
			t.Fatalf("could not find `const %s = <n>` in %s — either it was renamed, or "+
				"the bound moved somewhere this guard cannot see. Point the pattern at its "+
				"new home rather than deleting this test: the two bounds still have to agree.",
				name, rel)
		}
		return string(m[1])
	}

	decBound := read("internal/dec/dec.go", decBoundPattern, "maxSafeExponent")
	complianceBound := read("internal/compliance/gate.go", complianceBoundPattern, "maxDecimalExponent")

	if decBound != complianceBound {
		t.Fatalf("the two Decimal domain bounds have diverged:\n"+
			"  internal/dec        maxSafeExponent    = %s\n"+
			"  internal/compliance maxDecimalExponent = %s\n\n"+
			"They guard the same thing at two different ingresses — the market-data fold and "+
			"the pre-trade gate — and both exist because an unbounded 10^abs(exponent) was "+
			"shown to hang on a crafted input. Diverging means one entry point accepts what "+
			"the other refuses, which is worse than either bound alone: it looks covered. "+
			"Change both, or write down here why they are allowed to differ.",
			decBound, complianceBound)
	}
}
