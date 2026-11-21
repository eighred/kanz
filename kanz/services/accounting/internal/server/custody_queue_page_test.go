package server

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestCustodyQueuePageBoundAndScope(t *testing.T) {
	st := scopeBreaks(t)
	h := httptest.NewServer(breakServer(t, st))
	defer h.Close()
	sub := custody.Subject{PortfolioID: "PF-A", CustodianID: "C", BusinessDate: breakT0}
	detected := make([]recon.Break, 0, custody.MaxQueuePage+1)
	fill := func(n int) {
		t.Helper()
		for len(detected) < n {
			detected = append(detected, recon.Break{Kind: recon.BreakQuantity, Key: fmt.Sprintf("item-%05d", len(detected)), IBOR: big.NewRat(2, 1), Custodian: big.NewRat(1, 1), Diff: big.NewRat(1, 1)})
		}
		if _, err := st.UpsertBreaks(t.Context(), sub, custody.FromRecon(sub, detected, breakT0), breakT0); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Breaks []breakView
		Count  int
		Next   string `json:"next_cursor"`
	}
	read := func(query, scope string) page {
		t.Helper()
		status, body := scopedHTTP(t, h.Client(), h.URL, "GET", "/v1/custody/breaks"+query, testTenant, "", scope)
		var p page
		if status != 200 {
			t.Fatalf("status %d %s", status, body)
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil || p.Count != len(p.Breaks) {
			t.Fatalf("invalid page count %d, rows %d, err %v", p.Count, len(p.Breaks), err)
		}
		return p
	}
	fill(custody.MaxQueuePage)
	complete := read("", `["PF-A"]`)
	if complete.Count != custody.MaxQueuePage || complete.Next != "" {
		t.Fatalf("complete authorized queue: %d continuation %q", complete.Count, complete.Next)
	}
	fill(custody.MaxQueuePage + 1)
	status, body := scopedHTTP(t, h.Client(), h.URL, "GET", "/v1/custody/breaks", testTenant, "", `["PF-A"]`)
	if status != 422 || !strings.Contains(body, "custody_queue_pagination_required") || strings.Contains(body, `"breaks":`) || strings.Contains(body, "item-") || strings.Contains(body, `"count":`) {
		t.Fatalf("overflow exposed incomplete queue: status %d", status)
	}
	first := read("?limit=4096", `["PF-A"]`)
	if first.Count != 4096 || first.Next == "" {
		t.Fatalf("first page size %d continuation %q", first.Count, first.Next)
	}
	last := read("?limit=4096&after="+url.QueryEscape(first.Next), `["PF-A"]`)
	if last.Count != 1 || last.Next != "" {
		t.Fatalf("last page: %+v", last)
	}
	ids := append(first.Breaks, last.Breaks...)
	for i, b := range ids {
		if b.Key != fmt.Sprintf("item-%05d", i) || b.PortfolioID != "PF-A" || b.IBOR != "2" {
			t.Fatalf("wrong row %d %+v", i, b)
		}
	}
	// The same cursor carries no authority into the next request.
	excluded := read("?limit=1&after="+url.QueryEscape(first.Next), `["absent"]`)
	if excluded.Count != 0 || excluded.Next != "" {
		t.Fatalf("reused cursor exposed unauthorized rows: %+v", excluded)
	}
}

func TestCustodyQueueRejectsMalformedPage(t *testing.T) {
	for _, raw := range []string{"after=YQ", "limit=", "limit=0", "limit=-1", "limit=01", "limit=4097", "limit=1&limit=2", "limit=1&after=", "limit=1&after=YA==", "limit=1&after=YQ&after=YQ", "limit=1&after=AA", "limit=1&after=_w", "limit=1&after=%ZZ", "limit=1&after=" + strings.Repeat("A", 8193)} {
		if _, _, _, err := custodyQueueWindow(raw); err == nil {
			t.Errorf("accepted invalid query of length %d: %.45s", len(raw), raw)
		}
	}
}
