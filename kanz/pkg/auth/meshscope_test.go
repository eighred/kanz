package auth

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestPortfolioScopeRoundTripAndRefusals(t *testing.T) {
	h := http.Header{}
	SetPrincipalHeaders(h, "actor", "tenant", nil)
	want := []string{"PF,1", "PF/2", "İstanbul"}
	if err := SetPrincipalPortfolios(h, want); err != nil {
		t.Fatal(err)
	}
	p, ok := PrincipalFromHeaders(h)
	if !ok || !reflect.DeepEqual(p.Portfolios, want) || !PortfolioEntitled(p.Portfolios, "PF,1") || PortfolioEntitled(p.Portfolios, "PF") {
		t.Fatalf("scope changed: %+v", p)
	}
	for _, value := range []string{"null", `[null]`, `[""]`, `[1]`, `{"PF":true}`, `["PF"] []`, strings.Repeat(" ", 65537)} {
		h.Set(HeaderPrincipalPortfolios, value)
		if _, ok := PrincipalFromHeaders(h); ok {
			t.Fatalf("malformed scope accepted: %.30s", value)
		}
	}
	h.Set(HeaderPrincipalPortfolios, `["PF"]`)
	h.Add(HeaderPrincipalPortfolios, `["OTHER"]`)
	if _, ok := PrincipalFromHeaders(h); ok {
		t.Fatal("ambiguous repeated header accepted")
	}
	SetPrincipalHeaders(h, "next", "tenant", nil)
	p, ok = PrincipalFromHeaders(h)
	if !ok || len(p.Portfolios) != 0 || PortfolioEntitled(p.Portfolios, "PF") {
		t.Fatal("prior identity scope leaked")
	}
	if err := SetPrincipalPortfolios(h, []string{""}); err == nil {
		t.Fatal("empty ID accepted")
	}
	if err := SetPrincipalPortfolios(h, []string{string([]byte{255})}); err == nil {
		t.Fatal("lossy Unicode identity accepted")
	}
}
