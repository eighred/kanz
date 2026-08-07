// THE REPORT EXPORT IS BOUNDED, AND SAYS WHEN IT STOPS SHORT (#304).
//
// The defect: `full-log` carried a zero-value filter over an append-only table,
// so one GET read a tenant's entire history into the pod and MarshalIndent held
// a second inflated copy. The fix is a page plus a Seq cursor — which is only
// half an answer on its own. A page that does not ANNOUNCE itself as a page is
// worse than the unbounded read it replaces: the unbounded one fails loudly at
// 120s (#235's WriteTimeout), whereas a silent truncation returns 200 OK with a
// short log that will be read as the tenant's complete audit history. These
// tests are mostly about that second half.
package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
)

// seedN appends n records for one tenant, so a page boundary is unambiguous.
func seedN(t *testing.T, n int) *audit.Memory {
	t.Helper()
	st := audit.NewMemory()
	ctx := context.Background()
	for i := range n {
		if _, err := st.Append(ctx, &audit.Record{
			EventID:    fmt.Sprintf("e%04d", i),
			Kind:       audit.KindCommandOutcome,
			TenantID:   "t1",
			Summary:    fmt.Sprintf("record %d", i),
			RecordedAt: time.Unix(int64(1000+i), 0),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	return st
}

func pagedTemplate(limit int, after int64) Template {
	return Template{
		Name: "full-log", Title: "Full Audit Log", Format: FormatJSON,
		Filter: audit.Filter{Tenant: "t1", Limit: limit, AfterSeq: after},
	}
}

// AN UNBOUNDED TEMPLATE IS REFUSED, NOT QUIETLY CAPPED.
//
// Substituting DefaultPageSize here would be the friendlier behaviour and the
// wrong one: it makes a template that asks for the entire WORM log
// indistinguishable from one that asked for a page, so the misconfiguration
// never surfaces. This is the runtime half of the guard — it covers templates
// loaded as data, which test/arch cannot see.
func TestGenerateRefusesAnUnboundedTemplate(t *testing.T) {
	st := seedN(t, 5)
	tmpl := Template{Name: "byo", Filter: audit.Filter{Tenant: "t1"}, Format: FormatJSON}

	_, err := Generate(context.Background(), st, tmpl, time.Now)
	if !errors.Is(err, ErrUnboundedTemplate) {
		t.Fatalf("Generate returned %v, want ErrUnboundedTemplate — a template with no Limit reads the "+
			"tenant's entire append-only log, and serving it with a substituted default is how that "+
			"misconfiguration stays invisible", err)
	}
}

// A PARTIAL PAGE SAYS SO AND CARRIES ITS CURSOR.
func TestAPartialPageIsMarkedIncompleteAndCarriesACursor(t *testing.T) {
	st := seedN(t, 10)

	rep, err := Generate(context.Background(), st, pagedTemplate(4, 0), time.Now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rep.Records) != 4 || rep.Count != 4 {
		t.Fatalf("returned %d records (Count=%d), want 4 — the Limit is not being applied", len(rep.Records), rep.Count)
	}
	if rep.Complete {
		t.Fatal("a 4-record page of a 10-record log reports Complete=true. This is the failure this " +
			"work exists to prevent: 200 OK with a short log that reads as the tenant's whole history")
	}
	if want := rep.Records[3].Seq; rep.NextCursor != want {
		t.Fatalf("NextCursor=%d, want %d (the last returned record's Seq)", rep.NextCursor, want)
	}
}

// THE EXACT-BOUNDARY CASE, which is where an off-by-one hides: a selection whose
// size equals the page size exactly is COMPLETE. Getting this wrong hands the
// caller a cursor to an empty page forever, and — worse — stamps a complete
// export as partial, which invites a reader to treat whole evidence as a
// fragment. The Limit+1 probe is what makes this exact rather than a guess.
func TestAFullFinalPageIsStillComplete(t *testing.T) {
	st := seedN(t, 4)

	rep, err := Generate(context.Background(), st, pagedTemplate(4, 0), time.Now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rep.Records) != 4 {
		t.Fatalf("returned %d records, want all 4", len(rep.Records))
	}
	if !rep.Complete {
		t.Fatal("a 4-record page over a 4-record log reports Complete=false — the probe is counting the " +
			"page itself as evidence of more")
	}
	if rep.NextCursor != 0 {
		t.Fatalf("NextCursor=%d on a complete report, want 0 — a cursor here points at an empty page", rep.NextCursor)
	}
}

// THE PROPERTY THAT MAKES PAGING USABLE AS EVIDENCE: following the cursor to
// exhaustion yields every record EXACTLY ONCE, in Seq order.
//
// A cursor bug does not look like an error, it looks like a shorter or longer
// audit log. `seq > after` is inclusive-exclusive by design; the two ways to get
// it wrong (>= duplicates the boundary record, and skipping past it drops one)
// both produce an export that renders perfectly and is wrong. 10 records at a
// page size of 3 puts a boundary in the middle four times, including a final
// partial page.
func TestPagingReconstructsTheSelectionExactlyOnce(t *testing.T) {
	const total, page = 10, 3
	st := seedN(t, total)

	var seen []string
	var cursor int64
	for i := 0; ; i++ {
		if i > total {
			t.Fatal("paging did not terminate — the cursor is not advancing")
		}
		rep, err := Generate(context.Background(), st, pagedTemplate(page, cursor), time.Now)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, r := range rep.Records {
			seen = append(seen, r.EventID)
		}
		if rep.Complete {
			break
		}
		cursor = rep.NextCursor
	}

	if len(seen) != total {
		t.Fatalf("paging yielded %d records over a %d-record log: %v\n\n"+
			"Fewer means the cursor skipped a record — missing evidence in an audit export. More means "+
			"it repeated one at a page boundary — a false duplicate in the same.", len(seen), total, seen)
	}
	for i, id := range seen {
		if want := fmt.Sprintf("e%04d", i); id != want {
			t.Fatalf("record %d of the reassembled log is %q, want %q — paging did not preserve Seq order",
				i, id, want)
		}
	}
}

// THE CURSOR IS EXCLUSIVE. Asserted directly, because the reassembly test above
// would also pass if BOTH the page size and the cursor were off in compensating
// directions.
func TestTheCursorExcludesTheRecordItNames(t *testing.T) {
	st := seedN(t, 5)

	first, err := Generate(context.Background(), st, pagedTemplate(2, 0), time.Now)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := Generate(context.Background(), st, pagedTemplate(2, first.NextCursor), time.Now)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Records) == 0 {
		t.Fatal("the second page is empty")
	}
	if got := second.Records[0].Seq; got <= first.NextCursor {
		t.Fatalf("the second page starts at Seq %d but the cursor was %d — the boundary record is "+
			"repeated, which is a false duplicate in an audit export", got, first.NextCursor)
	}
}

// CSV IS WHERE A SILENT TRUNCATION DOES THE MOST DAMAGE, because it is opened in
// a spreadsheet where 1000 rows of a page look exactly like 1000 rows of a log.
func TestCSVDeclaresAPartialExport(t *testing.T) {
	st := seedN(t, 10)

	rep, err := Generate(context.Background(), st, pagedTemplate(4, 0), time.Now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := rep.RenderCSV()
	if err != nil {
		t.Fatalf("RenderCSV: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, "# complete: false") {
		t.Fatalf("a partial CSV export does not declare itself partial. Header was:\n%s",
			strings.SplitN(out, "\n", 5)[0:4])
	}
	if !strings.Contains(out, fmt.Sprintf("?after=%d", rep.NextCursor)) {
		t.Errorf("the partial CSV does not name the cursor to resume from (%d)", rep.NextCursor)
	}

	full, err := Generate(context.Background(), st, pagedTemplate(50, 0), time.Now)
	if err != nil {
		t.Fatalf("Generate (full): %v", err)
	}
	fb, err := full.RenderCSV()
	if err != nil {
		t.Fatalf("RenderCSV (full): %v", err)
	}
	if !strings.Contains(string(fb), "# complete: true") {
		t.Error("a complete CSV export does not declare itself complete — a reader cannot tell the " +
			"difference between this and a truncated one without it")
	}
}

// EVERY SHIPPED TEMPLATE IS BOUNDED. test/arch/report_templates_bounded_test.go
// enforces this over the source; this asserts the values the service actually
// serves, so the table cannot be bounded in source and unbounded at runtime.
func TestEveryBuiltInTemplateCarriesABound(t *testing.T) {
	for name, tmpl := range BuiltIns() {
		f := tmpl.Filter
		if f.Limit <= 0 && f.Since.IsZero() && f.Until.IsZero() {
			t.Errorf("built-in template %q has no Limit and no window — it selects the tenant's entire "+
				"append-only log", name)
		}
		if f.Limit > MaxPageSize {
			t.Errorf("built-in template %q ships Limit=%d, above MaxPageSize=%d, so the route would "+
				"refuse its own default", name, f.Limit, MaxPageSize)
		}
	}
}
