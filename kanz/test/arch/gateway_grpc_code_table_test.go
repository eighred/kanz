package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// THE EDGE'S gRPC→HTTP TABLE MUST BE COMPLETE (#440).
//
// api-gateway's httpStatus is the ONE place a gRPC code becomes an HTTP status
// for every REST route. Its own doc comment says the mapping "mirrors the
// standard grpc-gateway table so REST clients see conventional statuses".
//
// IT MIRRORED SIX OF SIXTEEN. The other ten fell to a default answering 500,
// including four the caller causes:
//
//	Canceled            the client hung up            → 500, not 499
//	AlreadyExists       a conflict                    → 500, not 409
//	ResourceExhausted   rate limited                  → 500, not 429
//	FailedPrecondition  caller-side precondition      → 500, not 400
//
// The cost is the class #434 and #439 documented for webhook-ingest: a 5xx is
// RETRYABLE and blames this platform, so a client's own cancellation reads as
// "kanz is broken" and invites a retry that cannot help. It also puts faults that
// are not ours into the error-rate SLO.
//
// WHY A GUARD AND NOT THE TABLE TEST. There WAS a table test — TestErrorMapping
// — and it passed throughout. It listed exactly the three codes somebody had
// implemented, because a table test can only test the rows in its table. That is
// the same asymmetry as #439: the thing nobody thought about is precisely the
// thing an enumerated-by-hand test cannot cover.
//
// This enumerates from the codes package itself, so a code cannot be omitted
// from the check by being omitted from a list.
//
// WHAT IT CHECKS: every code gRPC defines except OK is NAMED in httpStatus.
// WHAT IT CANNOT CHECK: that the status chosen for it is the conventional one.
// TestErrorMappingMirrorsTheGrpcGatewayTable carries that half, row by row. What
// this forces is that no code can be reached by the default arm unnoticed.

// gatewayCodeTableFile is where the mapping lives, module-relative.
const gatewayCodeTableFile = "services/api-gateway/internal/gateway/gateway.go"

// gatewayCodeTableFunc is the function that must name them.
const gatewayCodeTableFunc = "httpStatus"

// gatewayCodeTableExempt maps a code to the reason it needs no arm, and the
// issue that retires the entry.
//
// OK IS NOT AN ERROR, so it never reaches an error-mapping function. Everything
// else is deliberately absent: a code with no arm is a 500 nobody chose.
var gatewayCodeTableExempt = map[string]string{
	"OK": "not an error — status.FromError returns ok for a nil error, which never reaches httpStatus",
}

func TestGatewayMapsEveryGrpcCode(t *testing.T) {
	root := moduleRoot(t)

	named := identsNamedIn(t, filepath.Join(root, filepath.FromSlash(gatewayCodeTableFile)), gatewayCodeTableFunc)
	if len(named) == 0 {
		t.Fatalf("%s: found no identifiers inside %s — the function was renamed or moved, and "+
			"this guard can no longer see the table", gatewayCodeTableFile, gatewayCodeTableFunc)
	}

	// WHO RETURNS WHAT, for the failure message. A code some kanz server actually
	// returns is a live defect; one nothing returns yet is a latent one. Both fail,
	// but an author deserves to know which they are looking at.
	returned := grpcCodesReturnedBy(t, root)

	var missing []string
	for c := codes.Code(0); c <= codes.Code(16); c++ {
		name := c.String()
		// A Code outside the defined range stringifies as `Code(N)`; stop rather
		// than assert against a name that is not an identifier.
		if strings.HasPrefix(name, "Code(") {
			continue
		}
		if reason, ok := gatewayCodeTableExempt[name]; ok {
			t.Logf("codes.%s: exempt — %s", name, reason)
			continue
		}
		if named[name] {
			continue
		}
		if who := returned[name]; len(who) > 0 {
			missing = append(missing, name+" (RETURNED TODAY by "+strings.Join(who, ", ")+")")
			continue
		}
		missing = append(missing, name)
	}

	// NON-VACUITY: sixteen codes minus OK. If the loop stopped matching names the
	// guard would pass while checking nothing.
	if got := len(named); got < 10 {
		t.Fatalf("%s names only %d identifiers — too few to be the code table. It moved and "+
			"this guard is asserting nothing", gatewayCodeTableFunc, got)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s does not name %d gRPC code(s): %v.\n"+
			"Each falls to the default arm, which answers 500 — the status reserved for a fault "+
			"of ours. For a code the CALLER causes that is wrong twice: it blames this platform, "+
			"and 5xx is RETRYABLE so it invites a retry that cannot succeed. Add an arm, or add "+
			"an argued entry to gatewayCodeTableExempt.",
			gatewayCodeTableFunc, len(missing), missing)
	}

	// DEAD-ENTRY ARM: an exemption for a code that is now handled has outlived its
	// repair and would wave the next omission through.
	for name, reason := range gatewayCodeTableExempt {
		if named[name] {
			t.Errorf("exemption for codes.%s (%s) is dead — %s names it now. Delete the entry",
				name, reason, gatewayCodeTableFunc)
		}
	}
}

// grpcCodesReturnedBy maps a code name to the module-relative files where a kanz
// server hands it to status.Error/status.Errorf. Best-effort and used only to
// sharpen the failure message: a false negative costs a less specific error, not
// a missed violation.
func grpcCodesReturnedBy(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, dir := range []string{"services", "internal"} {
		for _, gf := range goFilesUnder(t, filepath.Join(root, dir)) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				continue
			}
			rel := dir + "/" + gf.rel
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, rel, gf.body, 0)
			if err != nil {
				continue // not this guard's job to police parseability
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := fn.X.(*ast.Ident)
				if !ok || pkg.Name != "status" ||
					(fn.Sel.Name != "Error" && fn.Sel.Name != "Errorf") {
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Args[0].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "codes" {
					name := sel.Sel.Name
					if !slices.Contains(out[name], rel) {
						out[name] = append(out[name], rel)
					}
				}
				return true
			})
		}
	}
	return out
}
