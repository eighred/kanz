package arch

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// NO NATS USER IS UNRESTRICTED, IN ANY ACCOUNT (#630).
//
// # The rule, and why it needed a third guard
//
// tenancy.yaml states it twice in its own commentary: a user with NO
// `permissions` block is unrestricted within its account, and — the trap found
// while proving that — an explicitly present but EMPTY `allow: []` does NOT
// deny, it behaves identically to omitting the block.
//
// Two guards already enforce it and BOTH ARE STRUCTURALLY BLIND to most of the
// file:
//
//   - TestServicePublishesOnlySubjectsItsTenancyPermissionsAllow builds its list
//     from servicesWithEntrypoints() mapped through systemAccountSVID(), so it
//     only ever asks about Go services under services/ in __system__. The
//     stream-provisioning Job is a shell script in kanz-messaging and was never
//     in that list.
//   - servicePublishPermissions / serviceSubscribeAllow parse systemUserEntries,
//     which isolates the __system__ account by brace depth. No tenant account is
//     read by either, so no tenant user was ever considered.
//
// So three of twenty-nine users carried no permissions block at all — the
// provisioning Job in __system__, and BOTH users of the acme tenant account —
// and the check written to find exactly that shape could not see any of them.
//
// # Why it reads every account rather than extending the existing parser
//
// The existing helpers are correct for their question ("what may this SERVICE
// publish") and answering it requires knowing which account a service lands in.
// This one asks a question that has no per-service part: does any user, anywhere
// in this file, hold an identity with no boundary on it. Widening
// systemUserEntries to serve both would make the __system__ isolation that the
// permission guards depend on into a parameter, which is how a working guard
// acquires a way to be wrong.
//
// It also matters that this reads the TENANT accounts, because acme is the
// template every future tenant is provisioned from: a hole there is duplicated
// once per onboarding, which is how two of the three arose.
func TestEveryNATSUserCarriesPermissions(t *testing.T) {
	root := moduleRoot(t)
	// Comments stripped FIRST. This file argues about `permissions` and
	// `allow: []` in prose at length — three guards in this package have already
	// passed while the checked thing was deleted because a regex matched their
	// own commentary.
	body := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "nats", "tenancy.yaml")))

	users := natsUserEntries(t, body)
	// NON-VACUITY: there are 29 of these. A parser that found none would pass
	// however unrestricted the estate had become.
	if len(users) < 25 {
		t.Fatalf("parsed only %d user entries from tenancy.yaml, want at least 25 — the parser is "+
			"broken and this guard proves nothing", len(users))
	}

	var problems []string
	seen := map[string]bool{}
	for _, u := range users {
		seen[u.svid] = true
		reason, exempt := unrestrictedNATSUserExempt[u.svid]

		if !u.hasPermissions {
			if exempt {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s (account %q) has NO permissions block, so it is "+
				"UNRESTRICTED inside that account — it may publish and subscribe on every subject the "+
				"account carries, including order.order.submit and platform.mode.changed where those "+
				"are reachable. tenancy.yaml states this rule itself; nothing was enforcing it here.",
				u.svid, u.account))
			continue
		}
		if exempt {
			problems = append(problems, fmt.Sprintf("the exemption for %s is DEAD: it now carries a "+
				"permissions block, so the entry only hides it from this guard. Delete it.\n"+
				"      reason on file: %s", u.svid, reason))
		}
		// THE EMPTY-ALLOW TRAP, which is worse than the missing block because it
		// LOOKS like a restriction. tenancy.yaml records it as proven empirically:
		// `allow: []` behaves as unrestricted. A reviewer reading a diff that adds
		// one would see a boundary being drawn.
		for _, dir := range u.emptyAllows {
			problems = append(problems, fmt.Sprintf("%s (account %q) declares an EMPTY %s allow list. "+
				"tenancy.yaml records this as proven to behave as UNRESTRICTED, not as a deny — so this "+
				"reads as a boundary in review and is none. Remove the user's access another way, or "+
				"list the subjects it genuinely needs.", u.svid, u.account, dir))
		}
	}

	for svid := range unrestrictedNATSUserExempt {
		if !seen[svid] {
			problems = append(problems, "the exemption for "+svid+" is DEAD: no such user exists in "+
				"tenancy.yaml any more")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d NATS identity/identities are unrestricted in their account:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// unrestrictedNATSUserExempt names users that deliberately carry no permissions
// block, with the reason. DEFAULT-DENY: an entry is permission, and the
// dead-entry arms above remove it the moment it stops being needed.
var unrestrictedNATSUserExempt = map[string]string{
	"spiffe://kanz.internal/ns/kanz-messaging/sa/nats": "THE BROKER'S OWN IDENTITY, in the SYS account. " +
		"SYS is NATS's system account: its whole function is $SYS.> — monitoring, JetStream cluster RPC " +
		"and the server-to-server traffic the cluster is built out of. Constraining it is constraining " +
		"the broker's ability to be a broker, and the subject set is the server's, not this platform's. " +
		"It is also the only user in SYS, so 'unrestricted within its account' grants nothing this " +
		"identity does not already have by being the thing that runs the account.",
}

type natsUserEntry struct {
	svid           string
	account        string
	hasPermissions bool
	emptyAllows    []string // "publish" / "subscribe" declared as allow: []
}

var (
	natsAccountRe    = regexp.MustCompile(`^\s{6}([A-Za-z_][\w-]*)\s*\{`)
	natsUserRe       = regexp.MustCompile(`user:\s*"([^"]+)"`)
	natsEmptyAllowRe = regexp.MustCompile(`(publish|subscribe):\s*\{\s*allow:\s*\[\s*\]`)
)

// natsUserEntries walks the comment-stripped tenancy.yaml and returns every
// `user:` entry with the account it sits in and whether it carries permissions.
//
// Brace-depth scanning rather than a YAML unmarshal: this is a NATS
// configuration embedded in a ConfigMap's string block, not YAML — `accounts {`
// and `permissions: { publish: { allow: [...] } }` are NATS config syntax, and a
// YAML parser sees the whole thing as one opaque string.
func natsUserEntries(t *testing.T, body string) []natsUserEntry {
	t.Helper()

	var out []natsUserEntry
	account := ""
	accountDepth := -1
	depth := 0

	var cur *natsUserEntry
	curDepth := 0

	for _, line := range strings.Split(body, "\n") {
		if m := natsAccountRe.FindStringSubmatch(line); m != nil && strings.Contains(line, "{") {
			account = m[1]
			accountDepth = depth
		}
		if m := natsUserRe.FindStringSubmatch(line); m != nil {
			// close any entry still open (a one-line `{ user: "x" }` form)
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &natsUserEntry{svid: m[1], account: account}
			curDepth = depth
			if strings.Contains(line, "}") && strings.Count(line, "}") >= strings.Count(line, "{") {
				// `{ user: "x" }` on one line: no permissions, entry ends here.
				out = append(out, *cur)
				cur = nil
			}
		}
		if cur != nil {
			if strings.Contains(line, "permissions:") {
				cur.hasPermissions = true
			}
			if m := natsEmptyAllowRe.FindStringSubmatch(line); m != nil {
				cur.emptyAllows = append(cur.emptyAllows, m[1])
			}
		}

		depth += strings.Count(line, "{") - strings.Count(line, "}")

		if cur != nil && depth <= curDepth && !strings.Contains(line, natsUserRe.FindString(line)) {
			out = append(out, *cur)
			cur = nil
		}
		if account != "" && depth <= accountDepth {
			account = ""
			accountDepth = -1
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}
