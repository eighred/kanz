package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// WHERE THE TRUSTED-HEADER BOUNDARY IS JUSTIFIED, THE ACTUAL CONTROL MUST BE
// NAMED (#626).
//
// # What went wrong
//
// Every service behind the gateway reads X-Kanz-Principal-Subject/-Tenant/-Roles
// and serves whatever tenant they name. Nothing authenticates the caller. What
// makes that sound is stated once, correctly, in pkg/auth/meshheader.go:
//
//	An upstream may trust them ONLY because a NetworkPolicy makes the gateway
//	its only reachable caller (#232 tracks where that is not yet enforced).
//
// Three files justified the same trust with a DIFFERENT control, one the binary
// does not implement:
//
//	proxy/backend.go     "the connection is mutually authenticated to the
//	                      gateway's SVID (a non-mesh caller cannot reach it)"
//	audit/server.go      "reachable only in-cluster over the SVID-authorized mesh"
//	tv-sync/brokerapi.go "the mesh (mTLS, SVID-authorized) is what stops anyone
//	                      else reaching this port"
//
// None was true. transport.ServerTLSConfig / ServerCredentials / ServerOption
// have five production call sites and all five feed grpc.NewServer; no HTTP
// listener in the estate sets TLSConfig, nothing calls ServeTLS, and the gateway
// dials every upstream over http://. There is no sidecar mesh under infra/.
//
// audit/server.go contradicted ITSELF: line 31 claimed the SVID-authorized mesh
// and line 74 named the NetworkPolicy. Two controls, one file, one of them
// imaginary.
//
// # Why this is a security defect and not a documentation defect
//
// Those sentences are the reason a reviewer concludes the boundary has defence in
// depth. It has one layer, and that layer's own manifest says of
// allow-gateway-to-read-upstreams that "kindnetd does not enforce NetworkPolicy,
// so the policy set in this file has never actually been executed". A CNI without
// policy support, a policy typo, or any pod scheduled into kanz-services is a
// full authentication bypass: the caller sets the three headers to whatever it
// likes and auth.PrincipalFromHeaders believes it. A reader who has been told
// mTLS also holds will not weigh that risk correctly.
//
// # What this checks
//
// For every non-test file that reads or writes the principal headers — the set
// that actually takes this trust — any comment block mentioning mutual
// authentication or an SVID-authorized mesh must ALSO name the NetworkPolicy.
//
// REQUIRING THE ANCHOR RATHER THAN BANNING THE PHRASE is deliberate. A ban would
// make the honest correction unwritable: the clearest thing these comments can
// say is that the hop is NOT mutually authenticated and NetworkPolicy is the only
// control, and a phrase-ban flags that sentence exactly as hard as the false one
// it replaced. Negation is not something a grep can be trusted to read. Requiring
// both terms in the same block lets a comment discuss mTLS as much as it likes,
// so long as it does not leave the reader believing mTLS is what holds the line.
//
// The granularity is the COMMENT BLOCK, not the file, because audit/server.go
// passed a file-level check while still carrying both stories.
func TestEveryHeaderTrustJustificationNamesTheRealControl(t *testing.T) {
	root := moduleRoot(t)

	// The helpers that mark a file as taking, or minting, the header trust.
	trustMarkers := []string{
		"RequireCallerTenant", "PrincipalFromHeaders", "SetPrincipalHeaders", "HeaderPrincipal",
	}
	// Claims that a cryptographic peer identity is what protects the hop.
	claimPhrases := []string{
		"mutually authenticated", "mutual tls", "svid-authorized", "svid authorized",
		"peer svid", "mtls",
	}

	var problems []string
	trustFiles, anchored := 0, 0

	fset := token.NewFileSet()
	walkGoFilesWithComments(t, root, fset, func(rel string, f *ast.File) {
		// Does this file take the trust? Read the CODE, not the comments — a file
		// merely discussing the headers is not one that acts on them.
		takes := false
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			for _, m := range trustMarkers {
				if strings.Contains(id.Name, m) {
					takes = true
					return false
				}
			}
			return true
		})
		if !takes {
			return
		}
		trustFiles++

		for _, group := range f.Comments {
			text := strings.ToLower(group.Text())
			claim := ""
			for _, p := range claimPhrases {
				if strings.Contains(text, p) {
					claim = p
					break
				}
			}
			namesPolicy := strings.Contains(text, "networkpolicy") || strings.Contains(text, "network policy")
			if namesPolicy {
				anchored++
			}
			if claim == "" || namesPolicy {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s:%d justifies the header trust with %q and "+
				"never names the NetworkPolicy", rel, fset.Position(group.Pos()).Line, claim))
		}
	})

	// NON-VACUITY, BOTH HALVES.
	//
	// The trust set is fourteen files today. A scan finding a handful has lost
	// sight of the surfaces that take the trust, and the ones it lost are exactly
	// the ones nobody is checking.
	if trustFiles < 10 {
		t.Fatalf("found %d file(s) taking the principal-header trust — expected at least 10. "+
			"The marker set or the walk is broken, and this guard is asserting nothing", trustFiles)
	}
	// And the anchor detector must actually fire somewhere, or "names the
	// NetworkPolicy" is a condition no comment could ever satisfy and every
	// passing file is passing for the wrong reason.
	if anchored == 0 {
		t.Fatal("no comment in the trust set names the NetworkPolicy — the anchor detector matches " +
			"nothing, so this guard would pass a tree in which every justification was wrong")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d justification(s) of the trusted-header boundary name a control this estate "+
			"does not implement:\n\n  %s\n\nNo HTTP listener here serves TLS and the gateway dials "+
			"every upstream over http://, so a peer SVID protects none of these hops. The only "+
			"control is the NetworkPolicy, and pkg/auth/meshheader.go says so. A comment promising "+
			"a second layer is how a reviewer comes to accept a single one (#626).",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// THE PREMISE IS ASSERTED, NOT ASSUMED (#626).
//
// The guard above forbids a claim of mutual authentication because none exists.
// The day an HTTP listener does serve TLS, that claim becomes TRUE and the rule
// above becomes wrong — it would then force an accurate comment to keep citing a
// NetworkPolicy as though it were still the only control.
//
// So the premise is a check of its own. Turning on TLS for an HTTP hop fails this
// test, with instructions: the two are meant to be revisited together, and a guard
// whose premise silently rotted would be worse than no guard. This is the same
// discipline the exemption maps use — a decision that cannot outlive its reason.
func TestNoHTTPListenerServesTLSWhichIsWhatTheClaimGuardAssumes(t *testing.T) {
	root := moduleRoot(t)

	var serving []string
	fset := token.NewFileSet()
	walkGoFilesWithComments(t, root, fset, func(rel string, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "ServeTLS" || sel.Sel.Name == "ListenAndServeTLS" {
				serving = append(serving, fmt.Sprintf("%s:%d calls %s",
					rel, fset.Position(call.Pos()).Line, sel.Sel.Name))
			}
			return true
		})
	})

	if len(serving) > 0 {
		sort.Strings(serving)
		t.Errorf("an HTTP listener now serves TLS:\n\n  %s\n\nThis is good, and it invalidates the "+
			"premise of TestEveryHeaderTrustJustificationNamesTheRealControl, which forbids a "+
			"comment from claiming mutual authentication BECAUSE none existed. Revisit both: the "+
			"claim may now be accurate for the hops that present and verify a peer SVID, and the "+
			"comments in pkg/auth/meshheader.go and the trust set should say which hops those are "+
			"(#626, #90).", strings.Join(serving, "\n  "))
	}
}

// walkGoFilesWithComments is walkGoFiles with parser.ParseComments.
//
// IT EXISTS BECAUSE THE SHARED WALKER DROPS COMMENTS. walkGoFiles parses with
// SkipObjectResolution and nothing else, so ast.File.Comments comes back EMPTY —
// and a guard about what comments say, reading that, finds nothing and passes.
// This one did, on the first run: it reported "the anchor detector matches
// nothing" from its own non-vacuity arm rather than reporting green, which is the
// only reason the mistake was visible at all.
//
// Kept local rather than changing walkGoFiles, so the eight guards built on that
// walker keep the parse mode they were written against.
func walkGoFilesWithComments(t *testing.T, root string, fset *token.FileSet, fn func(rel string, f *ast.File)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "vendor" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
