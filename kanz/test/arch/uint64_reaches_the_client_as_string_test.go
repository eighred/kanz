package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A uint64 THE SERVER SENDS AS A STRING MUST BE TYPED AS A STRING IN THE CLIENT
// (#606).
//
// # The half this closes
//
// uint64_json_domain_test.go holds the SERVER half: no hand-built JSON body may
// carry a bare uint64 getter, because JSON.parse destroys anything above 2^53
// before a browser sees it. That guard is default-deny with an empty exemption
// list and it works.
//
// Nothing held the CLIENT half, and the two moved separately — twice.
//
// compliance made the repair: proposalJSON emits `jsonUint64(...)`, so
// `version` became a JSON string on all four mandate bodies.
// kanz-web was never updated. It still declared `version?: number`, and
// renderVersion opened with `if (typeof v !== 'number') return null`. Against a
// string that test is ALWAYS true, so the signatory's queue rendered "VERSION NOT
// RENDERABLE" on every row — not just the ones above 2^53 the check existed to
// protect. A signatory approving a change to what governs a portfolio could not
// see which version they were signing.
//
// The client's own comment recorded the split as it happened: "(The repair is
// server-side and is not this PR's: compliance would have to emit the version as
// a string...)". The server-side repair landed. Nobody came back.
//
// # Why a guard and not a comment on each side
//
// Both sides already had a comment naming the other. That is precisely the
// arrangement that failed: a comment is dated evidence, and neither file's tests
// could see the other's contract. The client's suite was GREEN throughout,
// because its fixtures encoded the old number contract — a test that asserts the
// wrong wire shape passes forever and is worse than no test.
//
// # What this checks
//
// Every uint64/fixed64 field name in the schema tree is a JSON key that reaches a
// browser as a STRING — from protojson, which has always rendered 64-bit integers
// that way, or from the jsonUint64 wrapper the server-half guard requires. So if
// kanz-web declares a field of that name in its API types, the declared type must
// be `string`, never `number`.
//
// DERIVED FROM THE PROTOS, not listed, for uint64_json_domain_test.go's reason: a
// hand-copied list is correct the day it is written and silently narrows every
// time a field is added.
//
// # What it CANNOT check, stated plainly
//
//   - It matches on FIELD NAME, not on the message the field belongs to. A
//     kanz-web interface with a `version` that is genuinely some other message's
//     string field would be checked too — and would PASS, since the requirement
//     is `string`. The failure direction is a TS field named after a proto uint64
//     and typed `number`, which is the defect itself.
//   - It reads TypeScript with a regexp. There is no TS parser here and adding a
//     Node toolchain to the Go arch suite to get one is a cost this repository has
//     declined before (see decimal_domain_test.go, which reads kanz-web the same
//     way for the same reason). A field declared through a mapped type or a
//     generic would be invisible; none exists in src/api today.
//   - It says nothing about fields kanz-web does NOT declare. That is the correct
//     scope: a key the client never reads cannot be misread by it.

// uint64ClientExempt maps "<file>:<field>" to the reason a client-side field
// named after a proto uint64 may be typed as something other than a string.
//
// DEFAULT-DENY, and empty: the #606 sweep found no legitimate case. An entry here
// is somebody deciding on purpose that a browser may hold a 64-bit integer in a
// float64, which is a decision about silent data loss and should read like one.
var uint64ClientExempt = map[string]string{}

// tsFieldRe matches an interface property declaration: `  name?: type` — the
// shape every kanz-web API type uses. The type runs to end of line so a union is
// captured whole.
var tsFieldRe = regexp.MustCompile(`^\s*([a-z][a-zA-Z0-9_]*)\??:\s*(.+?)\s*$`)

func TestAUint64FieldReachesTheClientAsAString(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	// The proto-derived uint64 field names, snake_case — which is also the JSON
	// key, because both protojson and the hand-built maps use the proto name.
	getters := uint64GetterNames(t, filepath.Join(repoRoot, "kanz-schemas", "proto"))
	fields := map[string]string{} // field name -> "field (proto file)"
	for _, origin := range getters {
		name := origin
		if i := strings.Index(origin, " ("); i > 0 {
			name = origin[:i]
		}
		fields[name] = origin
	}
	if len(fields) == 0 {
		t.Fatal("derived zero uint64/fixed64 proto fields — the schema walk is broken, not the " +
			"client. A guard that checks an empty set passes no matter what kanz-web declares")
	}

	apiDir := filepath.Join(repoRoot, "kanz-web", "src", "api")
	entries, err := os.ReadDir(apiDir)
	if err != nil {
		t.Fatalf("read %s: %v — kanz-web is the browser client for these bodies, and this guard "+
			"cannot hold the client half if it cannot find it", apiDir, err)
	}

	var problems []string
	checked := 0
	used := map[string]bool{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") || strings.HasSuffix(e.Name(), ".test.ts") {
			continue
		}
		rel := "kanz-web/src/api/" + e.Name()
		b, err := os.ReadFile(filepath.Join(apiDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comment bodies quote field names constantly, and a guard that matches
			// its own prose checks nothing.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "/*") {
				continue
			}
			m := tsFieldRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name, declared := m[1], strings.TrimSuffix(m[2], ",")
			origin, isUint64 := fields[name]
			if !isUint64 {
				continue
			}
			checked++
			key := rel + ":" + name
			if _, ok := uint64ClientExempt[key]; ok {
				used[key] = true
				continue
			}
			if tsTypeIsString(declared) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s:%d declares %q as %s — the wire carries "+
				"%s as a STRING", rel, i+1, name, declared, origin))
		}
	}

	// NON-VACUITY. kanz-web declares at least one proto uint64 field today
	// (PendingMandateChange.version, on the mandate dual-control queue). A scan
	// finding none has lost sight of the client types — through a moved directory,
	// a renamed extension, or a declaration shape this regexp no longer matches —
	// and would stay green while every one of them was a number.
	if checked == 0 {
		t.Fatalf("found zero uint64-named fields in %s — expected at least one (version). "+
			"The client type shape moved and this guard is asserting nothing", apiDir)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d client field(s) declare a 64-bit integer as something a float64 cannot "+
			"hold:\n\n  %s\n\nJSON.parse turns every JSON number into a float64, so a value above "+
			"2^53 is already destroyed by the time the first line of client code runs — "+
			"18446744073709551615 arrives as 18446744073709552000, with no error and no way to "+
			"recover it. The server sends these as strings; the client must read them as strings "+
			"and must never Number() one (#606).",
			len(problems), strings.Join(problems, "\n  "))
	}

	// A DEAD EXEMPTION IS A REPAIR NOBODY NOTICED.
	for key := range uint64ClientExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no client field — the declaration it excuses is gone, "+
				"so remove the entry", key)
		}
	}
}

// tsTypeIsString reports whether a declared TypeScript type carries the value as
// a string. A union is accepted only if every non-null member is a string: one
// `number` arm is enough to put a float64 back on the path.
func tsTypeIsString(declared string) bool {
	parts := strings.Split(declared, "|")
	sawString := false
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "string":
			sawString = true
		case "null", "undefined":
			// Absence is not a number; it is the "nothing was sent" case every
			// renderer here already distinguishes.
		default:
			return false
		}
	}
	return sawString
}
