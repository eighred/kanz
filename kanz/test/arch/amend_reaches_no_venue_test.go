package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE PLATFORM HAS NO AMEND AT THE VENUE BOUNDARY, AND THE OMS NOW SAYS SO (#740).
//
// handleAmend rewrote a resting order's stored size and price and answered
// EXECUTED while the exchange went on working the original. Nothing sends an
// amend anywhere: venue.v1.VenueAdapterService has four RPCs and none is amend
// or replace, internal/execution declares no Amender/Replacer, and neither
// connector wires an amend endpoint. The ABSENCE is honest. What was not honest
// was the OMS proceeding as though it had one — and order.proto stating in
// writing that "an amend resets the order's working priority at the venue".
//
// The refusal that closes it (ReasonVenueCannotAmend) is correct only WHILE the
// absence holds. The day somebody implements a real amend — an RPC, an Amender
// interface, a connector endpoint — that refusal becomes a wrong answer: it
// would refuse an amend the platform could now actually deliver, and whoever
// added the capability would have no reason to look in handleAmend for it.
//
// SO THIS GUARD WATCHES THE ABSENCE, NOT THE REFUSAL. It fails when a venue
// amend capability appears, and its message says what to do about it. A guard
// that instead grepped handleAmend for the refusal would match its own
// explanatory comment — the checked thing could be deleted and the guard would
// stay green, which is how three guards in this tree already passed while
// asserting nothing. The refusal itself is carried by named behavioural tests
// (TestAmend_RefusedWhileAVenueIsWorkingTheOrder and its siblings), which assert
// against the STORED order rather than the handler's return.

// amendVenueProto is the venue contract that would have to grow an amend.
const amendVenueProto = "../kanz-schemas/proto/venue/v1/venue.proto"

// amendExecutionDir is where a venue capability interface would be declared.
const amendExecutionDir = "internal/execution"

var (
	// amendRPCRe finds an RPC whose name suggests amending or replacing a live
	// order. Anchored on the rpc keyword so a message or comment mentioning the
	// word does not trip it.
	amendRPCRe = regexp.MustCompile(`(?m)^\s*rpc\s+(\w*(?:Amend|Replace|Modify)\w*)\s*\(`)
	// amendIfaceRe finds a capability interface for the same.
	amendIfaceRe = regexp.MustCompile(`(?m)^type\s+(Amender|Replacer|Modifier)\s+interface\b`)
)

// amendCapabilityExempt maps a discovered capability to the reason it may exist
// while the OMS still refuses, and the issue that retires the entry.
//
// IT IS EMPTY, AND IT SHOULD STAY THAT WAY. There is no argued case for a venue
// amend the OMS refuses to use: either the platform can amend at the venue, in
// which case handleAmend must route to it, or it cannot, in which case the
// capability should not exist. An entry here would mean shipping both answers.
var amendCapabilityExempt = map[string]string{}

func TestNoVenueAmendCapabilityExistsWhileTheOMSRefusesToAmend(t *testing.T) {
	root := moduleRoot(t)

	var found []string

	// 1. The wire contract.
	protoPath := filepath.Join(root, filepath.FromSlash(amendVenueProto))
	body, err := os.ReadFile(protoPath)
	if err != nil {
		t.Fatalf("read %s: %v — this guard cannot check a contract it cannot open", amendVenueProto, err)
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	// NON-VACUITY: the file must be the venue service. If a move left this
	// pointing at something without the RPCs we know are there, the scan is
	// broken rather than the estate clean.
	for _, must := range []string{"rpc Execute(", "rpc CancelOrder("} {
		if !strings.Contains(text, must) {
			t.Fatalf("%s does not contain %q — the guard is reading the wrong file, and would "+
				"report an absence it never actually checked", amendVenueProto, must)
		}
	}
	for _, hit := range amendRPCRe.FindAllStringSubmatch(text, -1) {
		found = append(found, amendVenueProto+": rpc "+hit[1])
	}

	// 2. The in-module capability interface.
	execFiles := goFilesUnder(t, filepath.Join(root, filepath.FromSlash(amendExecutionDir)))
	if len(execFiles) == 0 {
		t.Fatalf("found no Go files under %s — the scan is broken", amendExecutionDir)
	}
	sawCloser := false
	for _, gf := range execFiles {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		// NON-VACUITY for the interface scan: Closer is the sibling capability
		// that DOES exist, and the pattern below is the shape that would find an
		// Amender. If Closer stops matching, the regex has drifted.
		if regexp.MustCompile(`(?m)^type\s+Closer\s+interface\b`).MatchString(gf.body) {
			sawCloser = true
		}
		for _, hit := range amendIfaceRe.FindAllStringSubmatch(gf.body, -1) {
			found = append(found, amendExecutionDir+"/"+gf.rel+": type "+hit[1])
		}
	}
	if !sawCloser {
		t.Fatalf("did not find `type Closer interface` under %s — the interface pattern no longer "+
			"matches this tree's declarations, so its Amender scan proves nothing", amendExecutionDir)
	}

	// Exemptions.
	var unexcused []string
	seenExempt := map[string]bool{}
	for _, f := range found {
		if reason, ok := amendCapabilityExempt[f]; ok {
			seenExempt[f] = true
			t.Logf("%s: exempt — %s", f, reason)
			continue
		}
		unexcused = append(unexcused, f)
	}

	if len(unexcused) > 0 {
		sort.Strings(unexcused)
		unexcused = uniq(unexcused)
		t.Errorf("a venue amend capability now exists: %v.\n"+
			"THIS IS NOT A FAILURE OF THE NEW CODE — it is the signal that services/oms's amend "+
			"refusal is now the wrong answer. handleAmend refuses every amend on an order a venue "+
			"is working (ReasonVenueCannotAmend, #740) precisely BECAUSE no such capability "+
			"existed; leaving that in place would refuse an amend the platform can now deliver.\n"+
			"Route handleAmend to the new capability, decide what happens when the venue rejects "+
			"the amend (the order's terms must not move unless the venue agrees), update "+
			"AmendOrder's comment in order/v1/order.proto, and delete this guard along with the "+
			"refusal it protects.", unexcused)
	}

	// DEAD-ENTRY ARM.
	for f, reason := range amendCapabilityExempt {
		if !seenExempt[f] {
			t.Errorf("exemption for %q (%s) matches no discovered capability — delete it", f, reason)
		}
	}
}

// The schema must not promise an amend the platform cannot send.
//
// The sentence this replaces — "An amend resets the order's working priority at
// the venue (venue-dependent)" — is the contract other teams read, and it
// described a venue round-trip that has never existed in this module. A comment
// justifying behaviour is dated evidence; this one had expired, and the code it
// described was the bug.
func TestAmendOrderSchemaDoesNotClaimAVenueRoundTrip(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash("../kanz-schemas/proto/order/v1/order.proto"))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read order.proto: %v", err)
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")

	i := strings.Index(text, "message AmendOrder {")
	if i < 0 {
		t.Fatal("message AmendOrder not found in order/v1/order.proto — the guard is reading the wrong file")
	}
	// The leading comment block is what a caller reads. Take the text above the
	// message declaration, back to the previous blank-line-separated block.
	head := text[:i]
	if j := strings.LastIndex(head, "\n\n"); j >= 0 {
		head = head[j:]
	}

	if strings.Contains(head, "resets the order's working priority at the venue") {
		t.Error("AmendOrder still claims an amend reaches the venue. No adapter in this module " +
			"sends one, and the OMS refuses an amend on any order a venue is working (#740) — so " +
			"this sentence promises a round-trip that cannot happen, to the people most likely to " +
			"build on it.")
	}
	// NON-VACUITY: the replacement must actually say what IS true, or a future
	// edit that simply deletes the sentence leaves the contract silent on the
	// one thing a caller needs to know.
	if !strings.Contains(head, "VENUE_CANNOT_AMEND") {
		t.Error("AmendOrder's comment does not name VENUE_CANNOT_AMEND. Deleting the false claim " +
			"is only half the repair: a caller has to learn that an order already at a venue " +
			"cannot be amended, and which code says so.")
	}
}
