package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every bus consumer must wire a DLQ, and none may wire in-handler retry.
//
// A Consumer built without WithDLQ has dlq == nil, so consumer.go's terminal
// path releases the dedup claim and RETURNS THE ERROR — it asks the broker to
// redeliver. Nothing parks the event and nothing records that it failed. If the
// redelivery is then acked by a handler that mistakes it for a duplicate, the
// event is GONE: no DLQ entry, no metric that outlives the process, no log line
// tying the two deliveries together.
//
// That is exactly what the OMS did on the capital path. A SubmitOrder whose
// venue call failed after admission returned an error, the broker redelivered
// it, and handleSubmit's fast path found the order already in the store and
// acked it. The order sat at ROUTED forever — indistinguishable from a limit
// order resting normally — while nothing was working it and nothing said so.
//
// The DLQ subsystem to prevent this was already built, documented and tested
// (publishDLQ, routeToDLQ, the Kanz-DLQ-* headers, dlq.<subject>). It was wired
// by ZERO of the estate's consumers. This guard is what makes it true rather
// than available.

// busConsumerCall is one bus.NewConsumer(...) call site and the option
// constructors passed to it.
type busConsumerCall struct {
	file    string
	line    int
	options map[string]bool
}

func (c busConsumerCall) where() string { return fmt.Sprintf("%s:%d", c.file, c.line) }

// busConsumerCalls finds every non-test bus.NewConsumer call site in the module.
//
// Parsed with go/ast rather than matched with a regex: the calls span several
// lines, carry comments between arguments, and a textual scan would have to
// re-implement paren balancing and string-literal skipping to find where each
// call ends. The option list is the thing under test, so reading it wrongly
// would make this guard lie in whichever direction the parser drifted.
func busConsumerCalls(t *testing.T, root string) []busConsumerCall {
	t.Helper()
	var out []busConsumerCall
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "gen", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSelector(call.Fun, "bus", "NewConsumer") {
				return true
			}
			opts := map[string]bool{}
			for _, arg := range call.Args {
				optCall, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				if sel, ok := optCall.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "bus" {
						opts[sel.Sel.Name] = true
					}
				}
			}
			out = append(out, busConsumerCall{
				file:    filepath.ToSlash(rel),
				line:    fset.Position(call.Pos()).Line,
				options: opts,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return out
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func TestEveryBusConsumerWiresADLQ(t *testing.T) {
	calls := busConsumerCalls(t, moduleRoot(t))

	// NON-VACUITY: this estate definitely constructs consumers. A scan that finds
	// none would pass this guard no matter how many were wired without a DLQ,
	// which is the failure mode the guard exists to prevent.
	if len(calls) == 0 {
		t.Fatal("found zero bus.NewConsumer call sites — the scanner is broken, not the services")
	}

	var unwired []string
	for _, c := range calls {
		if !c.options["WithDLQ"] {
			unwired = append(unwired, c.where())
		}
	}
	if len(unwired) > 0 {
		sort.Strings(unwired)
		t.Fatalf("%d of %d bus.NewConsumer call sites are wired WITHOUT bus.WithDLQ:\n  %s\n\n"+
			"Without a DLQ the consumer has nowhere to park a terminal failure, so it releases the "+
			"dedup claim and returns the error — asking the broker for a redelivery that any "+
			"already-handled check will silently ack. The event is then gone with no record. "+
			"Pass bus.WithDLQ(<publisher>) at the composition root.",
			len(unwired), len(calls), strings.Join(unwired, "\n  "))
	}
}

func TestNoBusConsumerWiresRetryWhileHandlersResumeByAcking(t *testing.T) {
	calls := busConsumerCalls(t, moduleRoot(t))

	if len(calls) == 0 {
		t.Fatal("found zero bus.NewConsumer call sites — the scanner is broken, not the services")
	}

	var retrying []string
	for _, c := range calls {
		if c.options["WithRetry"] {
			retrying = append(retrying, c.where())
		}
	}
	if len(retrying) > 0 {
		sort.Strings(retrying)
		t.Fatalf("%s wire bus.WithRetry, and the handlers in this estate are not all "+
			"safely re-enterable yet.\n\n"+
			"WithRetry re-runs the SAME handler in-process. A handler that begins by "+
			"treating an event it can already load as a duplicate and returning nil turns "+
			"attempt 2 into an instant silent ack: the consumer marks the command HANDLED "+
			"and the failure never reaches the DLQ at all. Retry there is strictly worse "+
			"than no retry.\n\n"+
			"THE OMS's handleSubmit IS NOW AN EXCEPTION — it resumes against venue truth "+
			"rather than acking on sight, using OrderState.venue_ack_at to tell an order "+
			"the venue never received from one it acknowledged (see "+
			"services/oms/internal/order/reconcile.go). The other twelve consumers have had "+
			"no such change, so this ban stays estate-wide: it is now over-broad rather than "+
			"load-bearing for the OMS specifically. Narrowing it to the handlers that still "+
			"ack-on-sight is a real task; deleting it because one handler was fixed is not.",
			strings.Join(retrying, ", "))
	}
}
