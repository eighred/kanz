package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A FIELD OUTSIDE THE DUAL-CONTROL DIGEST CAN BE EDITED AFTER APPROVAL (#410).
//
// services/oms/internal/approval hashes an order's terms; dualcontrol.Approve
// takes that digest and Approval.Covers refuses a mismatch. That pair is the only
// thing that binds a second signature to a VALUE rather than to a request — so a
// field the digest does not cover is a field somebody can change between the
// approval and the submission. The audit trail then shows two names, and the
// order that trades is not the order that was shown to the approver.
//
// THIS IS THE GAP-BETWEEN-TWO-THINGS DEFECT AGAIN, and the sibling guard next
// door exists for the same shape one layer out: submit_fields_reach_state_test.go
// caught leverage (#240), stop_price and expire_at (#405) — three fields the
// perimeter accepted and the wire dropped, each of which passed every one of its
// own tests, because a per-message test cannot see a gap between two messages.
// Here the gap is between order.v1.SubmitOrder and the part of it a signature
// covers, and it is INVISIBLE in exactly the same way: the digest computes, the
// approval verifies, and the uncovered field simply is not in it.
//
// # What it checks
//
// Every field of SubmitOrder, and every field of the ExecutionSchedule it
// carries, is either hashed by Terms.Digest or carries an argued exemption below.
// The chain is followed in three links, because breaking any one of them would
// silently narrow the signature:
//
//	proto field  →  TermsOfSubmit reads it  →  Digest hashes the Terms field
//
// A field read into Terms and never hashed is the failure this guard is most
// likely to catch, because it is the one an author would call done.
//
// # Why it strips comments first, and this is not paranoia
//
// approval/digest.go's own doc comment NAMES EVERY COVERED FIELD and every
// excluded one — it has to, it is the argument for the decision. A guard that
// grepped the raw source would therefore match that prose and pass with the
// covered set deleted, which this repository has already shipped three times:
// three arch guards passed with the checked thing removed because they matched
// their own comments. So the two function bodies are comment-stripped before
// anything is matched, and TestTheDigestGuardWouldSeeAnUncoveredField mutates the
// stripped text to prove the matcher is looking at code.
//
// # What it cannot check
//
// That the hashed value is CORRECT — that limit_price is rendered by value and
// not by its wire encoding, that unset and zero differ. Those are properties of
// arithmetic and live in approval's own tests. This guard answers only "is the
// field in there at all", which is the half no per-field test can notice missing.

const (
	digestSourceFile   = "services/oms/internal/approval/digest.go"
	digestCommandProto = "order/v1/order.proto"
	digestCommandMsg   = "SubmitOrder"
	digestScheduleMsg  = "ExecutionSchedule"
)

// orderDigestExempt maps a SubmitOrder field to the argument for NOT covering it.
//
// AN ENTRY HERE IS A STATEMENT THAT THE FIELD CAN CHANGE BETWEEN PROPOSE AND
// APPLY WITHOUT INVALIDATING THE SIGNATURE. That is sometimes correct and never
// free, so each one says what protects the field instead, or states the residual
// risk outright. The dead-entry arm deletes the entry for you if the field goes.
var orderDigestExempt = map[string]string{
	"metadata": "THE COMMAND ENVELOPE, EXCLUDED DELIBERATELY AND THE EXCLUSION IS LOAD-BEARING. The " +
		"approve step is a DIFFERENT command with its own metadata — necessarily a different issuer, " +
		"because the approver is by definition not the proposer — so hashing it would make EVERY " +
		"approval fail as a payload change, for a reason that has nothing to do with the payload. " +
		"That is #511's defect generalised: effective_at defaulting to now() made propose and approve " +
		"hash different payloads and every approval failed, and a control that fails for a reason " +
		"unrelated to the control is worse than no control. Field by field: target_id MUST equal " +
		"order_id (order.proto) and order_id IS covered; issuer is covered by something STRONGER than " +
		"a hash, since dualcontrol.Proposal.Proposer is the authenticated proposer and Approve refuses " +
		"an approver equal to it — a digest can prove a name did not change, it cannot enforce that " +
		"two names differ; principal_portfolios is the gateway's entitlement snapshot for THIS " +
		"delivery, and freezing the proposer's copy into the signature would preserve an entitlement " +
		"that may since have been revoked; valid_until is a staleness bound the command path already " +
		"enforces. THE ONE RESIDUAL RISK, STATED RATHER THAN HIDDEN: `reason` is free text with no " +
		"economic effect, and an approver who was shown a reason cannot prove from the digest that it " +
		"was not edited afterwards. Covering it would tie the signature to a human note the approve " +
		"step has no reason to resend verbatim.",
}

// orderDigestScheduleExempt is the same thing for ExecutionSchedule's fields.
//
// EVERY FIELD A CALLER CAN SET MUST BE HASHED, because every one of them changes
// what reaches a venue and when. The single entry below is not a caller field at
// all.
var orderDigestScheduleExempt = map[string]string{
	"volume_profile": "THE PLATFORM STAMPS IT AND THE CALLER CANNOT (#943), on the same line and " +
		"for the same reason as volume_profile_version below — the curve is the version's own " +
		"content, so the two must be exempt together or the pair is half-protected. " +
		"IT IS THE MORE DANGEROUS HALF, AND THAT ARGUES FOR THE EXEMPTION RATHER THAN AGAINST IT. " +
		"A version names a curve somebody measured; a curve IS one, so a caller able to send this " +
		"would supply a market nobody published — and since the OMS checks the two against each " +
		"other rather than against the feed, a self-consistent pair a client sent would verify. " +
		"That is precisely why services/oms/internal/order.validateSchedule clears BOTH before " +
		"reading either, and why a digest is the wrong control here: hashing a field the platform " +
		"overwrites cannot stop a client setting it, it can only make every approval of a VWAP or " +
		"POV order fail, in #511's shape. What stops it is that nothing a proposer sends survives " +
		"into the admitted order, and services/oms/internal/order." +
		"TestValidateSchedule_DiscardsAClientSuppliedProfileVersion sends a genuine forged pair " +
		"and asserts neither field reaches the store. " +
		"WHAT AN APPROVER IS PROMISED IS UNCHANGED by this field existing: the schedule's SHAPE " +
		"- algo, window, slice_count and both caps - is hashed, and what this carries is the " +
		"measured market data neither party chooses.",
	"volume_profile_version": "THE PLATFORM STAMPS IT AND THE CALLER CANNOT (#897). It records " +
		"which published market.v1.VolumeProfile a volume-driven schedule was derived against, and " +
		"the OMS RESOLVES it at admission: services/oms/internal/order.validateSchedule clears " +
		"whatever arrived on the command before reading anything, then sets it only if the " +
		"algorithm actually consulted the curve. So nothing a proposer sends survives into the " +
		"admitted order, and there is nothing for an approver's signature to protect — the field " +
		"is in the same position as venue_account_id, which SubmitOrder deliberately has no field " +
		"for at all. " +
		"HASHING IT WOULD BREAK THE CONTROL RATHER THAN STRENGTHEN IT, in exactly #511's shape: " +
		"the proposed command carries no version and the applied one carries whichever profile was " +
		"current at admission, so propose and apply would hash different payloads and EVERY " +
		"approval of a VWAP or POV order would fail for a reason unrelated to the control. " +
		"WHAT AN APPROVER IS ACTUALLY PROMISED, stated rather than implied: the fields that decide " +
		"the schedule's SHAPE - algo, window, slice_count and both caps - are all hashed, so the " +
		"order they signed is worked by the algorithm they saw over the window they saw. What the " +
		"version pins is the measured curve, which is market data neither party chooses and which " +
		"moves between propose and apply by construction.",
}

func TestTheOrderDigestCoversEverySubmitOrderField(t *testing.T) {
	root := moduleRoot(t)
	protoRoot := filepath.Join(filepath.Dir(root), "kanz-schemas", "proto")
	cmdFields := protoFieldNames(t, filepath.Join(protoRoot, filepath.FromSlash(digestCommandProto)), digestCommandMsg)
	scheduleFields := protoFieldNames(t, filepath.Join(protoRoot, filepath.FromSlash(digestCommandProto)), digestScheduleMsg)

	// NON-VACUITY. A renamed message or a moved proto would otherwise make this
	// pass by iterating an empty set — the exact silent emptiness it exists to
	// report, committed by the guard itself.
	if len(cmdFields) < 10 {
		t.Fatalf("%s has %d fields (%v), expected at least 10 — the message moved or was renamed and "+
			"this guard is asserting nothing", digestCommandMsg, len(cmdFields), cmdFields)
	}
	if len(scheduleFields) < 5 {
		t.Fatalf("%s has %d fields (%v), expected at least 5 — same", digestScheduleMsg, len(scheduleFields), scheduleFields)
	}

	src := readDigestSource(t, root)
	readsInto := digestTermsReaders(t, src, "TermsOfSubmit")
	hashed := digestHashedTermsFields(t, src)

	if len(readsInto) < 10 {
		t.Fatalf("TermsOfSubmit was parsed as reading only %d fields (%v) — the function was "+
			"restructured and this guard now checks nothing", len(readsInto), readsInto)
	}
	if len(hashed) < 10 {
		t.Fatalf("Digest was parsed as hashing only %d Terms fields (%v) — the function was "+
			"restructured and this guard now checks nothing", len(hashed), digestSortedKeys(hashed))
	}

	var uncovered []string
	seenExempt := map[string]bool{}
	for _, f := range cmdFields {
		if reason, ok := orderDigestExempt[f]; ok {
			seenExempt[f] = true
			t.Logf("%s: excluded from the digest — %s", f, reason)
			continue
		}
		getter := digestGoGetter(f)
		termsField, ok := readsInto[getter]
		if !ok {
			uncovered = append(uncovered, f+" (TermsOfSubmit never reads cmd."+getter+"())")
			continue
		}
		if !hashed[termsField] {
			uncovered = append(uncovered, f+" (read into Terms."+termsField+" and never hashed)")
		}
	}
	for _, f := range scheduleFields {
		if reason, ok := orderDigestScheduleExempt[f]; ok {
			seenExempt[digestScheduleMsg+"."+f] = true
			t.Logf("execution_schedule.%s: excluded — %s", f, reason)
			continue
		}
		if !strings.Contains(src.digest, "t.Schedule.Get"+digestGoGetterBare(f)+"()") {
			uncovered = append(uncovered, "execution_schedule."+f)
		}
	}

	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Errorf("%d order field(s) are NOT covered by the dual-control digest: %v.\n"+
			"Each one can be changed between the approval and the submission and still apply under the "+
			"approver's signature — so the audit trail names two people while the order that trades is "+
			"not the order either of them saw. A limit price outside the digest is a price the approver "+
			"did not sign for; a venue outside it moves the order onto a different exchange ACCOUNT, "+
			"and an exchange liquidates per account.\n"+
			"Hash the field in Terms.Digest (and bump digestParts, which is what makes the change "+
			"deliberate — it invalidates every digest already signed), or add an argued entry to "+
			"orderDigestExempt saying what protects the field instead.", len(uncovered), uncovered)
	}

	// DEAD-ENTRY ARM. An exemption for a field that is now hashed, or for one
	// that no longer exists, asserts something about the message that is no
	// longer true — and would wave through a future field that reused the name.
	for f, reason := range orderDigestExempt {
		if !seenExempt[f] {
			t.Errorf("exemption for %q is stale — the field is now covered, renamed or removed. "+
				"Delete the entry (%s)", f, reason)
		}
	}
	for f, reason := range orderDigestScheduleExempt {
		if !seenExempt[digestScheduleMsg+"."+f] {
			t.Errorf("exemption for execution_schedule.%q is stale — delete it (%s)", f, reason)
		}
	}
}

// TestBothDerivationsOfTheDigestCoverTheSameFields.
//
// Terms is built two ways — from the SubmitOrder a proposal holds, and from the
// OrderState the approved order becomes — and re-deriving the digest from the
// stored order is how anybody later proves the order that traded is the one that
// was signed for. If the two derivations set different Terms fields, one of them
// hashes a zero value where the other hashes a term, and the proof is silently
// unavailable: the digests simply differ, and the approve step fails on an order
// nobody changed.
func TestBothDerivationsOfTheDigestCoverTheSameFields(t *testing.T) {
	src := readDigestSource(t, moduleRoot(t))
	fromSubmit := digestTermsReaders(t, src, "TermsOfSubmit")
	fromState := digestTermsReaders(t, src, "TermsOfState")

	if len(fromSubmit) < 10 || len(fromState) < 10 {
		t.Fatalf("parsed %d/%d assignments — one of the constructors was restructured and this guard "+
			"now checks nothing", len(fromSubmit), len(fromState))
	}

	submitSets, stateSets := map[string]bool{}, map[string]bool{}
	for _, field := range fromSubmit {
		submitSets[field] = true
	}
	for _, field := range fromState {
		stateSets[field] = true
	}
	var diff []string
	for f := range submitSets {
		if !stateSets[f] {
			diff = append(diff, "TermsOfState never sets Terms."+f)
		}
	}
	for f := range stateSets {
		if !submitSets[f] {
			diff = append(diff, "TermsOfSubmit never sets Terms."+f)
		}
	}
	if len(diff) > 0 {
		sort.Strings(diff)
		t.Errorf("the two derivations of Terms disagree: %v.\n"+
			"One of them hashes a zero value where the other hashes a term, so an approval collected "+
			"against a proposal could never be shown to cover the stored order — every approval would "+
			"fail as a payload change, for a reason that has nothing to do with the payload (#511).", diff)
	}
}

// TestTheDigestGuardWouldSeeAnUncoveredField proves the matchers read CODE and
// not the surrounding argument.
//
// approval/digest.go's doc comment names every covered field by name, so a
// matcher that ran over the raw file would find "limit_price" in the prose and
// pass with the hash deleted. Three guards in this directory have already shipped
// that way. This deletes the assignment and the hash from the STRIPPED text and
// requires both matchers to notice.
func TestTheDigestGuardWouldSeeAnUncoveredField(t *testing.T) {
	src := readDigestSource(t, moduleRoot(t))

	if _, ok := digestTermsReaders(t, src, "TermsOfSubmit")["GetLimitPrice"]; !ok {
		t.Fatal("the fixture is wrong: TermsOfSubmit does not read cmd.GetLimitPrice() today")
	}
	if !digestHashedTermsFields(t, src)["LimitPrice"] {
		t.Fatal("the fixture is wrong: Digest does not hash Terms.LimitPrice today")
	}

	// Remove the READ. The comment prose still mentions limit_price everywhere.
	mutated := *src
	mutated.termsOfSubmit = strings.ReplaceAll(src.termsOfSubmit, "cmd.GetLimitPrice()", `""`)
	if _, ok := digestTermsReaders(t, &mutated, "TermsOfSubmit")["GetLimitPrice"]; ok {
		t.Error("the reader matcher still reports cmd.GetLimitPrice() after it was deleted — it is " +
			"matching prose, not code, and this guard would pass with the field uncovered")
	}

	// Remove the HASH. This is the subtler half: the field is still read into
	// Terms, so a guard that only checked the constructor would be green.
	mutated = *src
	mutated.digest = strings.ReplaceAll(src.digest, "t.LimitPrice", "nil")
	if digestHashedTermsFields(t, &mutated)["LimitPrice"] {
		t.Error("the hash matcher still reports Terms.LimitPrice after it was removed from Digest — " +
			"a field read into Terms and never hashed is the failure most likely to be called done")
	}
}

// digestSource holds the comment-stripped bodies this guard reads.
type digestSource struct {
	termsOfSubmit string
	termsOfState  string
	digest        string
}

func readDigestSource(t *testing.T, root string) *digestSource {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(digestSourceFile)))
	if err != nil {
		t.Fatalf("read %s: %v", digestSourceFile, err)
	}
	src := stripGoComments(string(body))
	return &digestSource{
		termsOfSubmit: goFuncBody(t, src, "func TermsOfSubmit("),
		termsOfState:  goFuncBody(t, src, "func TermsOfState("),
		digest:        goFuncBody(t, src, "func (t Terms) Digest("),
	}
}

func (s *digestSource) body(name string) string {
	switch name {
	case "TermsOfSubmit":
		return s.termsOfSubmit
	case "TermsOfState":
		return s.termsOfState
	}
	return ""
}

// digestTermsReadRe matches `TermsField: src.GetProtoField(),` inside a Terms
// composite literal.
var digestTermsReadRe = regexp.MustCompile(`(\w+):\s*\w+\.(Get\w+)\(\)`)

// digestTermsReaders maps the proto getter to the Terms field it lands in.
func digestTermsReaders(t *testing.T, src *digestSource, fn string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range digestTermsReadRe.FindAllStringSubmatch(src.body(fn), -1) {
		out[m[2]] = m[1]
	}
	return out
}

// digestTermsFieldRe matches a `t.Field` reference inside Digest's body.
var digestTermsFieldRe = regexp.MustCompile(`\bt\.([A-Z]\w*)`)

// digestHashedTermsFields is the set of Terms fields Digest actually reads.
func digestHashedTermsFields(t *testing.T, src *digestSource) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range digestTermsFieldRe.FindAllStringSubmatch(src.digest, -1) {
		out[m[1]] = true
	}
	return out
}

// goFuncBody returns the body of the function whose declaration starts with
// prefix, from the top-level `{` to the matching `}` at column 0.
func goFuncBody(t *testing.T, src, prefix string) string {
	t.Helper()
	i := strings.Index(src, prefix)
	if i < 0 {
		t.Fatalf("%q not found in %s — it was renamed and this guard is asserting nothing",
			prefix, digestSourceFile)
	}
	rest := src[i:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("no closing brace for %q in %s", prefix, digestSourceFile)
	}
	return rest[:end]
}

// stripGoComments removes // line comments and /* */ blocks.
//
// IT IS THE FIRST THING THAT RUNS, and the reason is in this file's header: the
// digest's own doc comment names every field it covers and every field it does
// not, so any matcher run over the raw source finds every name whether or not the
// code still uses it.
//
// WHICHEVER MARKER COMES FIRST ON THE LINE WINS, and getting that backwards cost
// a guard its coverage (#781). This used to look for "/*" before "//", so a
// perfectly ordinary line comment mentioning a path —
//
//	// handleProxy forwards /api/* to the gateway /v1/* as the session's caller.
//
// opened a block comment that never closed, and EVERY REMAINING LINE OF THE FILE
// was swallowed. services/web-bff/internal/server/server.go carries exactly that
// comment on line 381, so the file's last hundred-odd lines were invisible to
// every guard built on this helper. It was found by mutating a caller and
// watching the guard pass.
//
// IT STILL DOES NOT UNDERSTAND STRING LITERALS: a "//" inside a string truncates
// the line early. That direction strips too MUCH of one line and is a false
// negative — never a false positive that fails the build on correct code — and
// no caller of this helper searches for a marker-bearing literal.
func stripGoComments(src string) string {
	var b strings.Builder
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		if inBlock {
			if j := strings.Index(line, "*/"); j >= 0 {
				line, inBlock = line[j+2:], false
			} else {
				continue
			}
		}
		block := strings.Index(line, "/*")
		lineC := strings.Index(line, "//")
		switch {
		case block >= 0 && (lineC < 0 || block < lineC):
			line, inBlock = line[:block], true
		case lineC >= 0:
			line = line[:lineC]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// digestGoGetter turns a proto field name into the generated getter's name:
// order_id → GetOrderId.
func digestGoGetter(field string) string { return "Get" + digestGoGetterBare(field) }

func digestGoGetterBare(field string) string {
	parts := strings.Split(field, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func digestSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
