package liquidity

import "testing"

// WRAPPING MUST NOT DROP THE SPREAD CLAIM (#509).
//
// compute registers LVaR99 only when the provider says it can serve a spread,
// and treats a provider that does not answer as saying yes. A stressed provider
// is a different type, so before this it answered "no claim" no matter what the
// provider underneath had said — and LVaR99 came back on the stress path, where
// stressing a zero spread still yields zero.
func TestStressWrapForwardsTheSpreadClaim(t *testing.T) {
	type spreadServing interface{ ServesSpread() bool }

	for _, tc := range []struct {
		name  string
		inner Provider
		want  bool
	}{
		{"a provider that serves no spread", noSpreadProvider{}, false},
		{"a provider that serves one", spreadProvider{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Stress{}.Wrap(tc.inner)
			s, ok := w.(spreadServing)
			if !ok {
				t.Fatal("the stressed provider does not answer ServesSpread at all — compute reads " +
					"that as 'no claim, assume yes' and puts LVaR99 back on the wire")
			}
			if got := s.ServesSpread(); got != tc.want {
				t.Errorf("ServesSpread = %v, want %v — a wrapper must not change the answer, and "+
					"stressing a zero spread still gives zero", got, tc.want)
			}
		})
	}
}

// A PROVIDER THAT MAKES NO CLAIM STAYS SILENT THROUGH THE WRAPPER, rather than
// having one manufactured for it.
func TestStressWrapDoesNotManufactureAClaim(t *testing.T) {
	type spreadServing interface{ ServesSpread() bool }
	w := Stress{}.Wrap(staticLiquidity{})
	s, ok := w.(spreadServing)
	if !ok {
		t.Fatal("no ServesSpread on the wrapper")
	}
	if !s.ServesSpread() {
		t.Error("a wrapper over a provider that makes no claim answered false — that would " +
			"silently drop LVaR99 for every provider written before the claim existed")
	}
}

type noSpreadProvider struct{ staticLiquidity }

func (noSpreadProvider) ServesSpread() bool { return false }

type spreadProvider struct{ staticLiquidity }

func (spreadProvider) ServesSpread() bool { return true }
