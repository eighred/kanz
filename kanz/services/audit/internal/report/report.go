// Package report generates regulatory reports from the audit log (AUDIT-01d):
// configurable templates select and render a slice of the log, and every report
// carries a tamper-evidence attestation — the AUDIT-01b chain is verified over
// the whole log at generation time, so a report states not just "here are the
// records" but "and the log they came from is intact". Retention and legal-hold
// policy lives alongside in retention.go.
package report

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/services/audit/internal/audit"
)

// Format is a report rendering.
type Format string

const (
	FormatJSON Format = "json"
	FormatCSV  Format = "csv"
)

// Template is a report definition — configurable as data (load from JSON via a
// ConfigMap, same policy-as-data shape as the AUTH-01b authz config). It names a
// selection over the audit log and how to render it.
type Template struct {
	Name   string       `json:"name"`
	Title  string       `json:"title"`
	Filter audit.Filter `json:"filter"`
	Format Format       `json:"format"`
}

// Attestation is the integrity statement attached to every report: the result of
// verifying the AUDIT-01b hash chain over the entire log at generation time.
type Attestation struct {
	HeadSeq  int64  `json:"head_seq,string"`
	State    string `json:"state"`
	Verified bool   `json:"verified"`
	Records  int    `json:"records"`
	Head     string `json:"head_hash"`
	Detail   string `json:"detail,omitempty"`
}

// Page sizing for report templates (#304).
//
// DefaultPageSize is what a template carries when it does not choose; MaxPageSize
// is the ceiling a caller may raise it to. The numbers are not load-derived and
// do not need to be: the defect being fixed is UNBOUNDED, so any finite cap is a
// different class of thing from no cap. What matters is that exceeding the cap is
// visible, which Report.Complete makes it.
const (
	DefaultPageSize = 1000
	MaxPageSize     = 10000
)

// ErrUnboundedTemplate refuses a template whose filter has no Limit.
//
// THIS IS A REFUSAL, NOT A DEFAULT, and that is the whole point. Quietly
// substituting DefaultPageSize here would make a misconfigured template — one
// that asks for the entire WORM log — indistinguishable from a correctly capped
// one, which is exactly the "nothing configured and checked-and-fine look the
// same" failure the standard forbids. A template is data; data can be wrong; it
// must say so on the first request rather than on the request that runs the pod
// out of memory.
var ErrUnboundedTemplate = errors.New("report template has no Limit: it would read the tenant's entire audit log into memory")

// Report is a generated report: the selected records plus provenance + integrity.
//
// Complete/NextCursor are the half that makes paging honest. A truncated audit
// export that looks whole is worse than a refused one, because it will be read
// as evidence — so a partial report says so IN THE ARTIFACT, and carries the
// cursor to resume from rather than leaving the caller to infer it.
type Report struct {
	Template    string      `json:"template"`
	Title       string      `json:"title"`
	GeneratedAt time.Time   `json:"generated_at"`
	Integrity   Attestation `json:"integrity"`
	Count       int         `json:"count"`
	// Complete reports whether this page is the END of the selection. False
	// means more records match and NextCursor resumes after the last one here.
	Complete   bool            `json:"complete"`
	NextCursor int64           `json:"next_cursor,omitempty"`
	Records    []*audit.Record `json:"records"`
}

// Generate runs a template against the store: it verifies the full chain for the
// attestation, then queries the template's filter for the report body. now is
// injectable for deterministic tests.
//
// HOW "IS THERE MORE?" IS ANSWERED: by asking for Limit+1 rows and returning at
// most Limit. A COUNT(*) would be a second scan of the same predicate — the
// expensive half — and it would race the appends still arriving at the head of a
// WORM log. One extra row is exact, costs one row, and cannot disagree with the
// page it was read alongside.
func Generate(ctx context.Context, store audit.Store, tmpl Template, now func() time.Time) (*Report, error) {
	return generate(ctx, store, tmpl, now, true)
}

// GeneratePage selects bounded tenant evidence without claiming chain verification.
func GeneratePage(ctx context.Context, store audit.Store, tmpl Template, now func() time.Time) (*Report, error) {
	if tmpl.Filter.Tenant == "" {
		return nil, errors.New("tenant is required for evidence selection")
	}
	return generate(ctx, store, tmpl, now, false)
}
func generate(ctx context.Context, store audit.Store, tmpl Template, now func() time.Time, attest bool) (*Report, error) {
	if now == nil {
		now = time.Now
	}
	if tmpl.Filter.Limit <= 0 || tmpl.Filter.Limit > MaxPageSize {
		return nil, fmt.Errorf("%w: template %q", ErrUnboundedTemplate, tmpl.Name)
	}
	att := Attestation{State: "not_requested"}
	if attest {
		var err error
		att, err = Verify(ctx, store)
		if err != nil {
			return nil, err
		}
	}
	limit := tmpl.Filter.Limit
	probe := tmpl.Filter
	probe.Limit = limit + 1
	if attest && (probe.ThroughSeq == nil || *probe.ThroughSeq > att.HeadSeq) {
		// Query can run after new appends. Its rows must still belong to the
		// prefix just scanned, including when that prefix was empty.
		probe.ThroughSeq = &att.HeadSeq
	}
	records, err := store.Query(ctx, probe)
	if err != nil {
		return nil, err
	}
	complete := true
	var next int64
	if len(records) > limit {
		records = records[:limit]
		complete = false
		next = records[len(records)-1].Seq
	}
	title := tmpl.Title
	if title == "" {
		title = tmpl.Name
	}
	return &Report{
		Template:    tmpl.Name,
		Title:       title,
		GeneratedAt: now().UTC(),
		Integrity:   att,
		Count:       len(records),
		Complete:    complete,
		NextCursor:  next,
		Records:     records,
	}, nil
}

// Verify verifies the AUDIT-01b hash chain over the entire log and summarizes
// it. Exported because it is both the report's integrity attestation and the
// standalone tamper-check the /verify API and AUDIT-01e tests run.
// It STREAMS the log rather than loading it (#229). It used to call
// store.All(ctx) — every record in the compliance log into a slice, plus a
// parallel slice of chain.Link over them — on a GET request with no bound and
// no LIMIT, which is the endpoint a regulator's request hits. The chain walk is
// a left fold, so the working set is now one record however long the log is.
//
// WHAT THIS DOES NOT FIX, deliberately: the SCAN is still whole-log, and one
// pool connection is held for its duration. That is not a refactor away — a
// hash chain cannot be verified from a suffix without a trusted anchor for
// everything before it, so bounding this read means periodically signed
// checkpoints, which is a security design and not a query change.
func Verify(ctx context.Context, store audit.Store) (Attestation, error) {
	head := audit.Head{Hash: chain.Genesis}
	v := chain.NewVerifier()
	// The verifier records the FIRST break and tolerates everything after it, so
	// yield never returns an error and the scan runs to completion. That is on
	// purpose: Attestation.Records is the log's length, and stopping early would
	// report a broken chain as a SHORT one — understating how much of the log
	// exists is the wrong way to fail a tamper check.
	if err := store.Scan(ctx, func(r *audit.Record) error {
		_ = v.Push(r)
		head = audit.Head{Seq: r.Seq, Hash: r.Hash()}
		return nil
	}); err != nil {
		return Attestation{}, err
	}
	att := Attestation{State: "verified", Records: v.Count(), Head: head.Hash, HeadSeq: head.Seq}
	if idx, verr := v.Result(); verr != nil {
		att.State = "failed"
		att.Verified = false
		att.Detail = fmt.Sprintf("chain broken at index %d: %v", idx, verr)
	} else {
		att.Verified = true
	}
	return att, nil
}

// RenderJSON renders the full report (records + attestation) as indented JSON.
func (r *Report) RenderJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// RenderCSV renders the report records as CSV. The integrity attestation is
// emitted as leading comment-style header rows so a single artifact carries both
// the data and its tamper-evidence statement.
//
// THE COMPLETENESS ROW IS NOT COSMETIC AND CSV IS WHERE IT MATTERS MOST. JSON
// carries "complete" as a field a parser sees; a CSV is opened in a spreadsheet,
// where a page of 1000 rows looks exactly like a log of 1000 rows. Whoever reads
// it as the tenant's audit history has no other signal that it stops short, so
// the artifact states it and names the cursor that continues it (#304).
func (r *Report) RenderCSV() ([]byte, error) {
	var buf bytes.Buffer
	// Encode the title as one CSV field. Embedded line breaks stay inside that
	// field instead of creating spreadsheet rows; no lossy sanitizer is needed.
	header := csv.NewWriter(&buf)
	if err := header.Write([]string{"# report: " + r.Title}); err != nil {
		return nil, err
	}
	header.Flush()
	if err := header.Error(); err != nil {
		return nil, err
	}
	fmt.Fprintf(&buf, "# generated_at: %s\n", r.GeneratedAt.Format(time.RFC3339))
	if r.Integrity.State == "not_requested" {
		fmt.Fprintln(&buf, "# integrity: not_requested — no chain attestation was requested or performed")
	} else {
		fmt.Fprintf(&buf, "# integrity_verified: %t (records=%d head=%s)\n", r.Integrity.Verified, r.Integrity.Records, r.Integrity.Head)
		fmt.Fprintf(&buf, "# scanned_through_seq: %d (completeness is relative to this scanned prefix)\n", r.Integrity.HeadSeq)
	}
	if r.Complete {
		fmt.Fprintf(&buf, "# complete: true (%d record(s), end of selection)\n", r.Count)
	} else {
		fmt.Fprintf(&buf, "# complete: false — THIS IS A PARTIAL EXPORT: %d record(s), more remain. "+
			"Resume with ?after=%d\n", r.Count, r.NextCursor)
	}
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"seq", "event_id", "occurred_at", "kind", "event_type", "tenant", "correlation_id", "summary"}); err != nil {
		return nil, err
	}
	for _, rec := range r.Records {
		row := []string{
			fmt.Sprint(rec.Seq), rec.EventID, rec.OccurredAt.UTC().Format(time.RFC3339),
			string(rec.Kind), rec.EventType, rec.TenantID, rec.CorrelationID, rec.Summary,
		}
		for i, cell := range row {
			row[i] = spreadsheetLiteral(cell)
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// spreadsheetLiteral keeps data cells inert when an export is opened in Excel.
func spreadsheetLiteral(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n\ufeff")
	if trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@") {
		return "'" + value
	}
	if strings.ContainsAny(value, "\t\r\n") {
		return "'" + value
	}
	return value
}
