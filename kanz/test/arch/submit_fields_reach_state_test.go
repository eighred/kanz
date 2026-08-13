package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A FIELD THE PERIMETER ACCEPTS MUST BE ONE THE WIRE CARRIES (#405, #240).
//
// order.v1.SubmitOrder is what a caller sends. order.v1.OrderState is what the
// OMS stores and what Venue.Execute RECEIVES — so a term that exists on the
// command and not on the state is a term the venue can never be told.
//
// THREE INSTANCES, WHICH IS WHY THIS IS A GUARD AND NOT THREE FIXES:
//
//	leverage    (#240, closed)  parsed, bounds-checked, written to the immutable
//	                            FACT, then dropped — the audit root asserted 10x
//	                            on a position the venue held as spot.
//	stop_price  (#405)          REQUIRED at admission ("stop_price required for a
//	                            stop order") and then discarded, so a connector
//	                            implementing STOP_LOSS_LIMIT had nothing to send
//	                            as the trigger.
//	expire_at   (#405)          REQUIRED for GTD and then discarded. The adapters
//	                            forward time_in_force, so a good-till-date order
//	                            reached the venue with no date. Found by writing
//	                            THIS GUARD, not by a bug report.
//
// Every one passed its own tests. Admission validated the field, so the
// validation tests were green; the state had no field, so no test could miss it.
// The defect lives in the GAP BETWEEN two messages, which is exactly what a
// per-message test cannot see.
//
// WHAT IT CHECKS: every field of SubmitOrder has a counterpart on OrderState,
// by name or by a declared rename.
//
// WHAT IT CANNOT CHECK: that the counterpart is POPULATED. `StopPrice:` could be
// omitted from the struct literal and this guard would still pass. That half is
// carried by services/oms/internal/order/stopprice_test.go, and by
// TestEveryAuthenticatorPopulatesTheWholePrincipal's argument applied here: a
// field that exists is a field a reviewer can see is unset, whereas one that
// does not exist cannot be assigned at all and the omission is invisible.

const (
	// Relative to the SCHEMAS root, which is a sibling of the Go module root —
	// resolved via moduleRoot rather than a path relative to the test's CWD, the
	// same anchor decimal_grpc_domain_test.go uses.
	submitFieldsCommandProto = "order/v1/order.proto"
	submitFieldsStateProto   = "order/v1/order_events.proto"
	submitFieldsCommandMsg   = "SubmitOrder"
	submitFieldsStateMsg     = "OrderState"
)

// submitFieldRenames maps a SubmitOrder field to the OrderState field that
// carries it under a different name. A rename is fine; a DISAPPEARANCE is not.
var submitFieldRenames = map[string]string{
	// The command says how big the order is; the state says how big it is NOW,
	// after amends, beside filled_quantity and leaves_quantity. Same term.
	"quantity": "ordered_quantity",
}

// submitFieldsExempt maps a SubmitOrder field to the reason OrderState carries
// no counterpart, and the issue that retires the entry.
//
// ONE ENTRY, AND IT IS NOT AN ORDER TERM. Everything a caller can say about the
// order itself must reach the venue; if it need not, it should not be on the
// command.
var submitFieldsExempt = map[string]string{
	"metadata": "command envelope (issuer, correlation, principal entitlement), not a term of the order — " +
		"it authorises the command and is recorded on the FACT, and the venue has no use for it",
}

func TestEverySubmitOrderFieldReachesOrderState(t *testing.T) {
	protoRoot := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-schemas", "proto")
	cmdFields := protoFieldNames(t, filepath.Join(protoRoot, filepath.FromSlash(submitFieldsCommandProto)), submitFieldsCommandMsg)
	stateFields := protoFieldNames(t, filepath.Join(protoRoot, filepath.FromSlash(submitFieldsStateProto)), submitFieldsStateMsg)

	// NON-VACUITY, BOTH SIDES. A path typo or a message rename would otherwise
	// make this pass by comparing two empty sets.
	if len(cmdFields) < 10 {
		t.Fatalf("%s has %d fields (%v) — expected at least 10. The message moved or was "+
			"renamed and this guard is asserting nothing", submitFieldsCommandMsg, len(cmdFields), cmdFields)
	}
	if len(stateFields) < 15 {
		t.Fatalf("%s has %d fields (%v) — expected at least 15. The message moved or was "+
			"renamed and this guard is asserting nothing", submitFieldsStateMsg, len(stateFields), stateFields)
	}

	have := map[string]bool{}
	for _, f := range stateFields {
		have[f] = true
	}

	var missing []string
	for _, f := range cmdFields {
		if reason, ok := submitFieldsExempt[f]; ok {
			t.Logf("%s: exempt — %s", f, reason)
			continue
		}
		want := f
		if renamed, ok := submitFieldRenames[f]; ok {
			want = renamed
		}
		if !have[want] {
			if want != f {
				missing = append(missing, f+" (declared rename: "+want+")")
			} else {
				missing = append(missing, f)
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s carries %d field(s) %s cannot: %v.\n"+
			"OrderState is what Venue.Execute receives, so a term on the command with no "+
			"counterpart on the state is a term the venue can NEVER be told — the caller sends "+
			"it, admission may even validate it, and it is dropped before the order. That is "+
			"#240 (leverage) and #405 (stop_price, expire_at), three times over.\n"+
			"Add the field to %s, or declare a rename in submitFieldRenames, or add an argued "+
			"entry to submitFieldsExempt.",
			submitFieldsCommandMsg, len(missing), submitFieldsStateMsg, missing, submitFieldsStateMsg)
	}

	// DEAD-ENTRY ARMS: an exemption or rename naming a field that no longer
	// exists has outlived its repair and would wave through a future field that
	// reused the name.
	cmdHas := map[string]bool{}
	for _, f := range cmdFields {
		cmdHas[f] = true
	}
	for f, reason := range submitFieldsExempt {
		if !cmdHas[f] {
			t.Errorf("exemption for %q (%s) matches no %s field — delete it", f, reason, submitFieldsCommandMsg)
		}
	}
	for f, to := range submitFieldRenames {
		if !cmdHas[f] {
			t.Errorf("rename %q→%q matches no %s field — delete it", f, to, submitFieldsCommandMsg)
		}
		if !have[to] {
			t.Errorf("rename %q→%q names no %s field — %s does not exist", f, to, submitFieldsStateMsg, to)
		}
	}
}

// submitFieldDeclRe matches a scalar/message field line inside a message body:
// an optional `repeated`/`optional`, a (possibly dotted) type, a NAME, `=`, a
// number.
//
// Not decimal_grpc_domain_test.go's protoFieldRe, which is the same line for a
// different question: it captures the TYPE (to chase common.v1.Decimal through
// the message graph) and discards the name. This one needs the name and does not
// care about the type. Sharing one regex would mean one of the two callers
// reading a capture group that is not the thing it wants.
var submitFieldDeclRe = regexp.MustCompile(`^\s*(?:repeated\s+|optional\s+)?[A-Za-z_][\w.]*\s+([a-z_][a-z0-9_]*)\s*=\s*\d+\s*;`)

// protoFieldNames returns the field names declared in message msg of the .proto
// at path, in declaration order.
//
// Text, not descriptors: test/arch is deliberately dependency-free (see the
// package comment on risk_boundary_test.go), and the generated Go SDK is not
// committed — so a guard that reflected over it would be checking whatever
// happened to be generated last rather than what the schema says.
func protoFieldNames(t *testing.T, path, msg string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var (
		out    []string
		inMsg  bool
		depth  int
		header = "message " + msg + " {"
	)
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inMsg {
			if strings.HasPrefix(trimmed, header) {
				inMsg, depth = true, 1
			}
			continue
		}
		// Nested types (oneof, nested message, enum) would otherwise leak their
		// fields into the parent's list.
		depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
		if depth <= 0 {
			break
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := submitFieldDeclRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	if !inMsg {
		t.Fatalf("message %s not found in %s — it was renamed or moved, and this guard is "+
			"asserting nothing", msg, path)
	}
	return out
}
