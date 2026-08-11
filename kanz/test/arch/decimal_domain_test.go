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
	// THE THIRD COPY IS IN THE BROWSER (#399). kanz-web renders common.v1.Decimal
	// and applies the exponent by moving the decimal point; an unbounded one turns
	// `'0'.repeat(exponent)` into a hung TAB, which is #95's incident with a worse
	// audience. It is a fourth ingress to the same question, so it is read here
	// too — it is outside the Go module, hence the walk up from moduleRoot.
	webBoundPattern = regexp.MustCompile(`(?m)^export const MAX_SAFE_EXPONENT\s*=\s*(\d+)`)
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

	// The web app is a sibling of the Go module, not inside it.
	webPath := filepath.Join(filepath.Dir(root), "kanz-web", "src", "api", "decimal.ts")
	webRaw, err := os.ReadFile(webPath)
	if err != nil {
		t.Fatalf("read kanz-web/src/api/decimal.ts: %v — the browser renders Decimal too, and its "+
			"bound has to agree with these. If that module moved, point this at its new home "+
			"rather than dropping it: an unbounded exponent hangs the tab.", err)
	}
	m := webBoundPattern.FindSubmatch(webRaw)
	if m == nil {
		t.Fatal("could not find `export const MAX_SAFE_EXPONENT = <n>` in kanz-web/src/api/decimal.ts. " +
			"The browser applies the exponent by building a digit string, so it needs the same bound " +
			"the Go ingresses have — a renderer without one is a denial of service on the operator.")
	}
	webBound := string(m[1])

	if webBound != decBound {
		t.Fatalf("the browser's Decimal bound has diverged from the platform's:\n"+
			"  internal/dec              maxSafeExponent   = %s\n"+
			"  kanz-web src/api/decimal  MAX_SAFE_EXPONENT = %s\n\n"+
			"They answer the same question — how far can 10^abs(exponent) be materialised before "+
			"the computation IS the incident — at two ingresses. A browser that accepts what the "+
			"fold refuses renders a figure the platform would not compute.",
			decBound, webBound)
	}

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
