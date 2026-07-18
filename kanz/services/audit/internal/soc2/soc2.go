// Package soc2 turns the AUDIT-01 observation stream into SOC 2 Type II
// continuous evidence (PARITY-06d). A Type II audit asks not "does a control
// exist" but "did it OPERATE throughout the period" — which is exactly what the
// append-only, hash-chained audit log already records. This package maps the
// relevant Trust Services Criteria to the record `Kind`s that evidence them, then
// collects, over an audit window, the count + samples per control and flags any
// control with insufficient evidence as an exception the auditor must see.
//
// Only controls the observation stream can HONESTLY evidence are mapped here
// (logical-access authorization, change/decision integrity, monitoring); controls
// that need out-of-band artifacts (HR onboarding, vendor reviews) are the ops
// runbook's job, not fabricated from the stream. The mapping is DATA
// (DefaultControls) so a deployment can extend it for its own control matrix.
package soc2

import (
	"context"
	"time"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
)

// Control is one SOC 2 Trust Services Criteria point of focus evidenced from the
// audit stream: the criterion id, its category, and the record Kinds whose
// presence in the audit window demonstrates the control operated.
type Control struct {
	ID          string       `json:"id"`
	Category    string       `json:"category"`
	Description string       `json:"description"`
	Kinds       []audit.Kind `json:"kinds"`
	// MinPerWindow is the minimum supporting-record count expected across the
	// audit window; a control below it is an evidence GAP (a Type II exception —
	// the control did not demonstrably operate throughout the period).
	MinPerWindow int `json:"min_per_window"`
}

// DefaultControls is the shipped TSC → audit-Kind mapping. Each control is
// evidenced by a class of records the platform already emits to the AUDIT-01
// stream, so a Type II window over a live system produces real operating
// evidence.
func DefaultControls() []Control {
	return []Control{
		{
			ID: "CC6.1", Category: "Security",
			Description:  "Logical access is authorized before granting — every access decision is recorded.",
			Kinds:        []audit.Kind{audit.KindAuthzDecision},
			MinPerWindow: 1,
		},
		{
			ID: "CC7.2", Category: "Security",
			Description:  "The system is monitored for anomalies — data-quality events are detected and recorded.",
			Kinds:        []audit.Kind{audit.KindDataQuality},
			MinPerWindow: 1,
		},
		{
			ID: "CC8.1", Category: "Security",
			Description:  "Changes are authorized and recorded — commands and their outcomes are logged.",
			Kinds:        []audit.Kind{audit.KindCommand, audit.KindCommandOutcome},
			MinPerWindow: 1,
		},
		{
			ID: "PI1.1", Category: "Processing Integrity",
			Description:  "Processing is complete and accurate — model/strategy decisions and command outcomes are recorded.",
			Kinds:        []audit.Kind{audit.KindDecision, audit.KindCommandOutcome},
			MinPerWindow: 1,
		},
	}
}

// ControlEvidence is the collected evidence for one control over the window.
type ControlEvidence struct {
	Control   Control  `json:"control"`
	Count     int      `json:"count"`
	Samples   []string `json:"samples"` // up to maxSamples supporting event ids
	Satisfied bool     `json:"satisfied"`
}

// EvidenceReport is the SOC 2 evidence bundle for an audit window.
type EvidenceReport struct {
	From       time.Time         `json:"from"`
	To         time.Time         `json:"to"`
	Controls   []ControlEvidence `json:"controls"`
	Gaps       []string          `json:"gaps"` // control ids below MinPerWindow
	Satisfied  bool              `json:"satisfied"`
	TotalCount int               `json:"total_count"`
}

const maxSamples = 5

// Collect maps records to the given controls over [from, to] and reports per
// control the supporting count + samples, plus the set of controls whose evidence
// is insufficient (gaps). Records outside the window are ignored. A record
// supporting multiple controls (its Kind maps to several) counts toward each.
func Collect(controls []Control, records []*audit.Record, from, to time.Time) EvidenceReport {
	kindToControls := map[audit.Kind][]int{}
	for i, c := range controls {
		for _, k := range c.Kinds {
			kindToControls[k] = append(kindToControls[k], i)
		}
	}

	counts := make([]int, len(controls))
	samples := make([][]string, len(controls))
	total := 0
	for _, r := range records {
		if r == nil {
			continue
		}
		if !inWindow(r.OccurredAt, from, to) {
			continue
		}
		idxs, ok := kindToControls[r.Kind]
		if !ok {
			continue
		}
		total++
		for _, i := range idxs {
			counts[i]++
			if len(samples[i]) < maxSamples {
				samples[i] = append(samples[i], r.EventID)
			}
		}
	}

	out := EvidenceReport{From: from, To: to, Satisfied: true, TotalCount: total}
	for i, c := range controls {
		satisfied := counts[i] >= c.MinPerWindow
		if !satisfied {
			out.Satisfied = false
			out.Gaps = append(out.Gaps, c.ID)
		}
		out.Controls = append(out.Controls, ControlEvidence{
			Control: c, Count: counts[i], Samples: samples[i], Satisfied: satisfied,
		})
	}
	return out
}

// CollectFromStore is the continuous-evidence pipeline: it pulls the window's
// records from the audit store and collects evidence against the default control
// matrix. This is what a nightly job (or the /v1/soc2/evidence endpoint) calls to
// produce the running Type II evidence file. An empty/zero `to` means "now".
func CollectFromStore(ctx context.Context, store audit.Store, tenant string, from, to time.Time) (EvidenceReport, error) {
	if to.IsZero() {
		to = time.Now()
	}
	recs, err := store.Query(ctx, audit.Filter{Tenant: tenant, Since: from, Until: to, Limit: 0})
	if err != nil {
		return EvidenceReport{}, err
	}
	return Collect(DefaultControls(), recs, from, to), nil
}

// inWindow reports whether t (an event's OccurredAt — when the controlled action
// happened, the Type II operating-evidence axis, consistent with the store's
// Since/Until filter) is within [from, to], treating a zero bound as open.
func inWindow(t, from, to time.Time) bool {
	if !from.IsZero() && t.Before(from) {
		return false
	}
	if !to.IsZero() && t.After(to) {
		return false
	}
	return true
}
