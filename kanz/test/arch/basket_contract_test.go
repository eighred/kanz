package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE PORTFOLIO-TO-EXCHANGE-ACCOUNT BINDING IS A DEPLOY-TIME CONTRACT, AND A
// RUNTIME API WOULD RE-OPEN THE ONE REFUSAL THAT MAKES IT MEAN ANYTHING (#68).
//
// Owner ruling, 2026-07-27: baskets stay a deploy-time concern. No runtime CRUD.
//
// The reason is in internal/execution/account.go and is worth restating because
// it is what this guard protects: an exchange margins, nets and LIQUIDATES per
// ACCOUNT. Two portfolios bound to one account share one collateral pool, and no
// ledger entry can undo that — when a drawdown in the first triggers a
// liquidation, the exchange sells whatever is in the account and the second
// portfolio's margin is gone while its books still show the cash.
//
// So ParseBindings refuses an account with two owners (ErrAccountShared) and the
// OMS exits 2 rather than starting. That refusal is only worth something while
// the binding set is FIXED AT STARTUP. A runtime endpoint that rebinds a
// portfolio moves the check to a moment nobody is watching: the process is
// already up, already trading, and the refusal it passed at boot describes a
// configuration that no longer exists.
//
// Nothing enforced that. The ruling lived in an issue, and an issue does not
// fail a build — so this is the ruling as a guard. It is deliberately about the
// SCHEMA rather than any one service: the gateway is grpc-gateway, so a runtime
// CRUD surface arrives as a proto RPC with an HTTP annotation, and catching it
// there catches it before an implementation exists to argue about.
func TestNoRuntimeAPIMutatesAPortfolioAccountBinding(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	protoRoot := filepath.Join(repoRoot, "kanz-schemas", "proto")

	rpcs := protoRPCs(t, protoRoot)
	// NON-VACUITY. A scan that finds no RPCs would pass however many binding
	// mutators the schema grew — the scanner would be broken, not the estate, and
	// it would read exactly like a clean result.
	if len(rpcs) == 0 {
		t.Fatalf("found no `rpc` declarations under %s — the scanner is broken, not the schema", protoRoot)
	}

	// A binding mutator pairs a mutation VERB with a binding NOUN.
	//
	// The nouns are specific on purpose. "Venue" alone would flag SetVenueKeys,
	// which rotates an exchange CREDENTIAL and is a legitimate operator action —
	// it changes which key reaches an account, never which portfolio owns one. A
	// guard that had to be exempted on its first run would teach the next reader
	// that its exemption list is where you go to make it quiet.
	verb := `(?:Create|Update|Set|Delete|Add|Remove|Bind|Rebind|Assign|Move|Attach|Detach)`
	noun := `(?:Portfolio|Basket|Binding|Bindings|VenueAccount|VenueAccounts|CollateralAccount|AccountOwner)`
	mutator := regexp.MustCompile(`^` + verb + noun)

	var problems []string
	for _, r := range rpcs {
		if !mutator.MatchString(r.name) {
			continue
		}
		if reason, ok := bindingMutationExempt[r.name]; ok {
			t.Logf("exempt: %s (%s)", r.name, reason)
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s declares rpc %s\n\n"+
				"      That is a RUNTIME mutation of the portfolio-to-exchange-account binding, which\n"+
				"      the deploy-time contract forbids (#68, owner ruling 2026-07-27). The binding set\n"+
				"      is fixed at startup so ParseBindings can refuse an account with two owners\n"+
				"      (ErrAccountShared) and the OMS can exit rather than trade a shared collateral\n"+
				"      pool it reports as segregated. Rebinding at runtime moves that check to a moment\n"+
				"      nobody is watching — the process is already up and already trading.\n\n"+
				"      Change OMS_VENUE_ACCOUNTS and redeploy. If this RPC genuinely does not touch a\n"+
				"      binding, name it in bindingMutationExempt with the reason.",
			r.file, r.name))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("runtime mutation of a deploy-time binding contract:\n\n  %s", strings.Join(problems, "\n\n  "))
	}
	t.Logf("%d rpc(s) scanned; none mutate a portfolio-to-account binding", len(rpcs))
}

// bindingMutationExempt names RPCs that match the mutator shape but do not
// change which portfolio owns an exchange account.
//
// Empty, and that is the point: the ruling is that no such API exists. An entry
// here is a decision to re-open the refusal for one path, so it carries the
// issue that closes it again.
var bindingMutationExempt = map[string]string{}

type protoRPC struct {
	file string // repo-relative, forward slashes
	name string
}

var rpcDeclRe = regexp.MustCompile(`^\s*rpc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// protoRPCs returns every rpc declared under root.
//
// Comment lines are skipped: a .proto explains its RPCs constantly, and a guard
// that fires on a sentence describing an API rather than an API is one that gets
// deleted instead of fixed — the lesson serviceMentions had to learn twice
// (#147).
func protoRPCs(t *testing.T, root string) []protoRPC {
	t.Helper()
	var out []protoRPC

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		body, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if m := rpcDeclRe.FindStringSubmatch(line); m != nil {
				out = append(out, protoRPC{file: filepath.ToSlash(rel), name: m[1]})
			}
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("%s does not exist — the schema tree moved and this guard is scanning nothing", root)
		}
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
