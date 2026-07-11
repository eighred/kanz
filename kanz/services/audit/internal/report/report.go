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
	"fmt"
	"time"

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/services/audit/internal/audit"
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
	Verified bool   `json:"verified"`
	Records  int    `json:"records"`
	Head     string `json:"head_hash"`
	Detail   string `json:"detail,omitempty"`
}

// Report is a generated report: the selected records plus provenance + integrity.
type Report struct {
	Template    string          `json:"template"`
	Title       string          `json:"title"`
	GeneratedAt time.Time       `json:"generated_at"`
	Integrity   Attestation     `json:"integrity"`
	Records     []*audit.Record `json:"records"`
}

// Generate runs a template against the store: it verifies the full chain for the
// attestation, then queries the template's filter for the report body. now is
// injectable for deterministic tests.
func Generate(ctx context.Context, store audit.Store, tmpl Template, now func() time.Time) (*Report, error) {
	if now == nil {
		now = time.Now
	}
	att, err := Verify(ctx, store)
	if err != nil {
		return nil, err
	}
	records, err := store.Query(ctx, tmpl.Filter)
	if err != nil {
		return nil, err
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
		Records:     records,
	}, nil
}

// Verify verifies the AUDIT-01b hash chain over the entire log and summarizes
// it. Exported because it is both the report's integrity attestation and the
// standalone tamper-check the /verify API and AUDIT-01e tests run.
func Verify(ctx context.Context, store audit.Store) (Attestation, error) {
	all, err := store.All(ctx)
	if err != nil {
		return Attestation{}, err
	}
	head, err := store.Head(ctx)
	if err != nil {
		return Attestation{}, err
	}
	links := make([]chain.Link, len(all))
	for i := range all {
		links[i] = all[i]
	}
	att := Attestation{Records: len(all), Head: head.Hash}
	if idx, verr := chain.Verify(links); verr != nil {
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
func (r *Report) RenderCSV() ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# report: %s\n# generated_at: %s\n# integrity_verified: %t (records=%d head=%s)\n",
		r.Title, r.GeneratedAt.Format(time.RFC3339), r.Integrity.Verified, r.Integrity.Records, r.Integrity.Head)
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"seq", "event_id", "occurred_at", "kind", "event_type", "tenant", "correlation_id", "summary"}); err != nil {
		return nil, err
	}
	for _, rec := range r.Records {
		row := []string{
			fmt.Sprint(rec.Seq), rec.EventID, rec.OccurredAt.UTC().Format(time.RFC3339),
			string(rec.Kind), rec.EventType, rec.TenantID, rec.CorrelationID, rec.Summary,
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
