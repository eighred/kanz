package filing

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rational literal %q in the fixture", s)
	}
	return r
}

// TestFiledAndSignedAreTheSameString is the invariant this package exists to hold
// (#633): for every value, the string in the SERVED body and the string in the
// SIGNED bytes are the same string.
//
// It is checked over the shapes that break a rendering rather than over a
// convenient fixture — a repeating fraction, a value below the platform's scale,
// a tie at the eighth decimal, a negative, a value far past int64 — because the
// bug it replaces was invisible on the one value where every rendering agrees
// (zero), which is exactly what the service's climate fixture used.
//
// The expected strings are derived from the rationals outside Go, at dec.Str's
// fixed scale of 8 with halves away from zero. They are not read off this
// implementation.
func TestFiledAndSignedAreTheSameString(t *testing.T) {
	cases := []struct {
		code string
		lit  string
		want string
	}{
		{"A_ZERO", "0", "0"},
		{"B_INTEGER", "5", "5"},
		{"C_NEG_INTEGER", "-5", "-5"},
		// 0.1 is the value that is not itself in IEEE-754; as a rational it is exact.
		{"D_TENTH", "0.1", "0.1"},
		// The issue's headline: a WACI whose *big.Rat marshals as "6172839/5000".
		{"E_WACI", "1234.5678", "1234.5678"},
		// A repeating fraction has no finite decimal — the scale decides, and the
		// scale must decide identically on both sides.
		{"F_THIRD", "1/3", "0.33333333"},
		{"G_NEG_THIRD", "-1/3", "-0.33333333"},
		{"H_MILLIONTH", "1/1000000", "0.000001"},
		// A tie at the eighth decimal, negative: halves round AWAY from zero.
		{"I_TIE_NEG", "-1/200000000", "-0.00000001"},
		// A value BELOW the platform's scale is no longer here: it is REFUSED rather
		// than rendered, because "0" is a different claim and not a smaller number
		// (#672). See TestABelowScaleValueIsRefusedRatherThanFiledAsZero. The case
		// above (I_TIE_NEG) is the smallest magnitude this filing can still express.
		// Far past int64, so nothing here may go through a fixed-width coefficient.
		{"K_HUGE", "123456789012345678901234567890.123456789", "123456789012345678901234567890.12345679"},
	}

	tmpl := make([]Field, 0, len(cases))
	values := map[string]*big.Rat{}
	want := map[string]string{}
	for _, c := range cases {
		tmpl = append(tmpl, Field{Code: c.code, Label: c.code + " label"})
		values[c.code] = rat(t, c.lit)
		want[c.code] = c.want
	}

	at := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	rep, err := Build("TEST", tmpl, at, values, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// What the recipient is served.
	body, err := json.Marshal(rep.Body())
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var out struct {
		LineItems []struct {
			Code  string `json:"code"`
			Value string `json:"value"`
		} `json:"line_items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the served body does not decode as decimal strings: %v\nbody: %s", err, body)
	}
	if len(out.LineItems) != len(cases) {
		t.Fatalf("served %d line items, want %d", len(out.LineItems), len(cases))
	}

	// What the signature covers.
	canonical := string(rep.Canonical())

	for _, li := range out.LineItems {
		if got := want[li.Code]; li.Value != got {
			t.Errorf("%s served as %q, want %q", li.Code, li.Value, got)
			continue
		}
		if line := li.Code + "=" + li.Value; !strings.Contains(canonical, line+"\n") {
			t.Errorf("%s: the signature does not cover the string that was served — %q is absent "+
				"from the canonical bytes:\n%s", li.Code, line, canonical)
		}
	}

	// And the whole point: the body alone re-derives the signature.
	rebuilt := Report{Framework: "TEST", AsOf: at}
	for _, li := range out.LineItems {
		rebuilt.LineItems = append(rebuilt.LineItems, LineItem{Code: li.Code, Value: rat(t, li.Value)})
	}
	sig, err := HashSigner{}.Sign(rebuilt.Canonical())
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if sig != rep.Signature {
		t.Fatalf("a filing rebuilt from its own served body does not reproduce its signature:\n"+
			"re-derived %s\ncarried    %s\nrebuilt canonical:\n%s", sig, rep.Signature, rebuilt.Canonical())
	}
}

// TestBuildRefusesRatherThanSigns pins both refusals, because a signature over a
// wrong number is worse than no filing: the signature is what makes it
// authoritative.
func TestBuildRefusesRatherThanSigns(t *testing.T) {
	tmpl := []Field{{Code: "X", Label: "x"}, {Code: "Y", Label: "y"}}
	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	t.Run("missing line item", func(t *testing.T) {
		rep, err := Build("TEST", tmpl, at, map[string]*big.Rat{"X": big.NewRat(1, 1)}, nil)
		if err == nil {
			t.Fatalf("an incomplete filing was signed as %q", rep.Signature)
		}
		if !strings.Contains(err.Error(), `"Y"`) {
			t.Errorf("the refusal must name the missing line item; got %q", err)
		}
	})

	t.Run("non-finite line item", func(t *testing.T) {
		// nil is what big.Rat.SetFloat64 returns for NaN/±Inf — how a non-finite
		// model output arrives at this boundary.
		if v := new(big.Rat).SetFloat64(nan()); v != nil {
			t.Fatalf("premise broken: SetFloat64(NaN) = %v, want nil", v)
		}
		rep, err := Build("TEST", tmpl, at, map[string]*big.Rat{"X": big.NewRat(1, 1), "Y": nil}, nil)
		if err == nil {
			t.Fatalf("a filing carrying a non-finite value was signed as %q — it would have been "+
				"filed as %q, indistinguishable from a measured zero", rep.Signature, "0")
		}
		if !strings.Contains(err.Error(), `"Y"`) {
			t.Errorf("the refusal must name the offending line item; got %q", err)
		}
	})

	t.Run("signer failure", func(t *testing.T) {
		rep, err := Build("TEST", tmpl, at,
			map[string]*big.Rat{"X": big.NewRat(1, 1), "Y": big.NewRat(2, 1)}, refusingSigner{})
		if err == nil {
			t.Fatalf("a filing was issued with signature %q although the audit chain link did not "+
				"land — it claims a chain position that exists in no chain", rep.Signature)
		}
		if rep.Signature != "" {
			t.Errorf("the refused report still carries a signature %q", rep.Signature)
		}
	})
}

// TestCanonicalIsOrderIndependent: the signature must not depend on the order the
// template happened to list the codes in, because a re-derivation by the
// recipient reads them back in whatever order the JSON gave them.
func TestCanonicalIsOrderIndependent(t *testing.T) {
	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	vals := map[string]*big.Rat{"A": big.NewRat(1, 3), "B": big.NewRat(2, 7)}

	fwd, err := Build("TEST", []Field{{Code: "A"}, {Code: "B"}}, at, vals, nil)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := Build("TEST", []Field{{Code: "B"}, {Code: "A"}}, at, vals, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fwd.Signature != rev.Signature {
		t.Fatalf("the same filing signed two different ways depending on template order:\n%s\n%s",
			fwd.Canonical(), rev.Canonical())
	}
	// Tamper-evidence, the other half: a changed value MUST change the signature.
	vals["B"] = big.NewRat(3, 7)
	tampered, err := Build("TEST", []Field{{Code: "A"}, {Code: "B"}}, at, vals, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tampered.Signature == fwd.Signature {
		t.Fatal("changing a line item did not change the signature — the filing is not tamper-evident")
	}
}

func TestLookup(t *testing.T) {
	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	rep, err := Build("TEST", []Field{{Code: "A"}}, at, map[string]*big.Rat{"A": big.NewRat(1, 4)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := rep.Lookup("A"); !ok || v.Cmp(big.NewRat(1, 4)) != 0 {
		t.Fatalf("Lookup(A) = %v %v", v, ok)
	}
	if _, ok := rep.Lookup("NOPE"); ok {
		t.Fatal("Lookup of an absent code reported found")
	}
}

// refusingSigner stands in for an audit chain whose link did not land.
type refusingSigner struct{}

func (refusingSigner) Sign([]byte) (string, error) { return "", errSignerDown }

var errSignerDown = &signerDownError{}

type signerDownError struct{}

func (*signerDownError) Error() string { return "audit chain unavailable" }

// nan returns a NaN without importing math into the test's expectations.
func nan() float64 {
	var zero float64
	return zero / zero
}

// A VALUE TOO SMALL TO RENDER IS REFUSED, NOT FILED AS ZERO (#672).
//
// dec.Str renders at dec.Scale decimal places, so 1e-9 comes out as "0" — the
// string Body serves and Canonical signs. The filing would then assert a measured
// zero, and the signature would commit to it. A regulator receiving that cannot
// tell it from a genuine zero, and nothing downstream can either.
//
// This is the nil/non-finite refusal one line up in Build, on a different cause:
// there the value was not a number, here it is a number this filing cannot
// express. Both would be signed as "0" without a refusal.
func TestABelowScaleValueIsRefusedRatherThanFiledAsZero(t *testing.T) {
	tmpl := []Field{{Code: "TINY", Label: "Sub-scale metric"}}

	for _, c := range []struct {
		name string
		rat  string
	}{
		{"positive", "1/1000000000"},
		// The negative arm matters on its own: Str trims "-0" to "0", so a negative
		// sub-scale value files as a POSITIVE-looking zero.
		{"negative", "-1/1000000000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, ok := new(big.Rat).SetString(c.rat)
			if !ok {
				t.Fatalf("bad fixture %q", c.rat)
			}
			_, err := Build("TEST", tmpl, time.Unix(0, 0).UTC(), map[string]*big.Rat{"TINY": v}, nil)
			if err == nil {
				t.Fatalf("Build accepted %s, which renders as %q — the filing would assert a measured "+
					"zero and sign it", c.rat, dec.Str(v))
			}
			if !strings.Contains(err.Error(), "TINY") {
				t.Fatalf("the refusal must name the line item so an operator can go and look at it, got: %v", err)
			}
		})
	}
}

// AND AN EXACT ZERO IS STILL FILEABLE. The refusal above keys on a value that is
// non-zero yet renders as zero; a genuine measured zero is a legitimate figure and
// must not be caught by it, or every clean report becomes an outage.
func TestAnExactZeroIsStillFiled(t *testing.T) {
	tmpl := []Field{{Code: "ZERO", Label: "A measured zero"}}
	r, err := Build("TEST", tmpl, time.Unix(0, 0).UTC(),
		map[string]*big.Rat{"ZERO": new(big.Rat)}, nil)
	if err != nil {
		t.Fatalf("a genuine zero must still file: %v", err)
	}
	v, ok := r.Lookup("ZERO")
	if !ok {
		t.Fatal("the filed report does not carry ZERO at all")
	}
	if got := dec.Str(v); got != "0" {
		t.Fatalf("ZERO filed as %q, want \"0\"", got)
	}
}
