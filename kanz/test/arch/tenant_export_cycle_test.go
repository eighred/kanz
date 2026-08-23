package arch

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AN ACCOUNT THAT EXPORTS WHAT IT IMPORTS MAKES THE BROKER REFUSE TO START —
// SOMETIMES.
//
// # What went wrong
//
// The #668 return path had each tenant account export its FACT subject spaces
// so __system__ could import them under a `tfact.<tenant>.` prefix. One of those
// exports was `order.>`.
//
// The same account IMPORTS its order COMMANDS from __system__ and remaps them
// onto the logical names — `to: "order.order.submit"`, and its cancel and
// approve siblings. Those land INSIDE `order.>`. So the account re-offered to
// __system__ the very subjects __system__ had just sent it, and nats-server
// answered:
//
//	Error adding stream import "order.>": import forms a cycle
//
// # Why the broker's own check is not enough
//
// IT IS NON-DETERMINISTIC. The check runs while imports are added and its
// outcome depends on iteration order, so the same committed file was accepted
// most of the time and rejected the rest. Measured before the repair:
//
//	nats-server -t on the committed config      1 rejection in 8 runs
//	internal/platform/halt tenancy integration  3 failures in 6 runs
//
// A config that is valid seven times out of eight is worse than one that is
// never valid: it merges, it deploys, it works — and then a broker restart
// during an incident does not come back, on a file nobody changed. This guard
// makes the property deterministic, which the broker cannot.
//
// # The rule
//
// For every account, no subject it EXPORTS may overlap a subject its own
// IMPORTS land on. Where an import carries `to:`, the landing subject is that
// remap; otherwise it is the imported subject itself.
//
// The reverse direction — an import WIDER than the export behind it — fails the
// same way with a different message ("stream import not authorized"), and is
// caught by the same comparison from the other side.
func TestNoTenantExportsWhatItImports(t *testing.T) {
	text := tenancyText(t)
	blocks := accountBlocks(t, text)
	if len(blocks) < 2 {
		t.Fatalf("parsed %d account block(s) from tenancy.yaml — this guard is blind", len(blocks))
	}

	checked := 0
	for _, account := range sortedKeys(blocks) {
		block := blocks[account]
		exports := exportedSubjects(block)
		lands := importLandingSubjects(block)
		if len(exports) == 0 || len(lands) == 0 {
			continue
		}
		checked++
		for _, e := range exports {
			for _, l := range lands {
				if !subjectsOverlap(e, l) {
					continue
				}
				t.Errorf("account %q EXPORTS %q, which overlaps %q — a subject its own imports land "+
					"on.\n\n"+
					"The account re-offers to the exporting account the very subjects it was just "+
					"sent, and nats-server rejects the whole file with \"import forms a cycle\". It "+
					"does so NON-DETERMINISTICALLY: the check runs while imports are added, so the "+
					"same file is accepted most of the time. A broker that starts seven times out "+
					"of eight will fail on a restart nobody associates with this change.\n\n"+
					"Narrow the export to the subjects this account actually PRODUCES. A command it "+
					"receives is not one of them.", account, e, l)
			}
		}
	}

	// NON-VACUITY. acme both exports and imports today; a run that examined no
	// account has checked nothing and would pass over any cycle at all.
	if checked == 0 {
		t.Fatal("no account in tenancy.yaml both exports and imports — either the MT-02 bridge has " +
			"been withdrawn, or the export/import parse is broken and this guard is watching nothing")
	}
}

var (
	// exportEntry matches `{ stream: "subject" }` inside an exports: [ ... ] list.
	exportEntry = regexp.MustCompile(`\{\s*stream:\s*"([^"]+)"\s*\}`)
	// importTo matches an import's `to:` remap — where the subject LANDS.
	importTo = regexp.MustCompile(`\bto:\s*"([^"]+)"`)
)

// exportedSubjects returns the subjects an account block exports.
//
// Only the `{ stream: "…" }` form is matched, which is the shape this file uses
// for exports; an import's nested `stream: { account: …, subject: … }` is a
// different shape and is deliberately not caught here.
func exportedSubjects(block string) []string {
	region := sectionAfter(block, "exports:")
	if region == "" {
		return nil
	}
	var out []string
	for _, m := range exportEntry.FindAllStringSubmatch(region, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// importLandingSubjects returns the subjects this account's imports deliver ON.
//
// A `to:` remap is the landing subject — that is the whole point of the remap,
// and it is what an export must not overlap. An import without one lands on the
// subject it names, optionally under a prefix; the prefixed case cannot collide
// with a bare export and the unprefixed case can, so both are included and the
// overlap test decides.
func importLandingSubjects(block string) []string {
	region := sectionAfter(block, "imports:")
	if region == "" {
		return nil
	}
	var out []string
	for _, entry := range importEntry.FindAllString(region, -1) {
		if to := importTo.FindStringSubmatch(entry); to != nil {
			out = append(out, to[1])
			continue
		}
		if p := importPrefix.FindStringSubmatch(entry); p != nil {
			// Lands under a prefix, so it cannot collide with a bare export.
			continue
		}
		if s := importSubject.FindStringSubmatch(entry); s != nil {
			out = append(out, s[1])
		}
	}
	sort.Strings(out)
	return out
}

// sectionAfter returns the text following key up to the closing bracket of its
// list, which is where that section's entries live.
func sectionAfter(block, key string) string {
	i := strings.Index(block, key)
	if i < 0 {
		return ""
	}
	rest := block[i+len(key):]
	depth := 0
	for j, r := range rest {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return rest[:j]
			}
		}
	}
	return rest
}
