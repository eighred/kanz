package arch

import (
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// EVERY SERVICE COMPOSITION ROOT MUST DELEGATE ITS EXIT CODE.
//
// `func main()` in services/*/cmd/*/ must have exactly one statement in its
// body: `os.Exit(<call>)`. Nothing before it, nothing after it.
//
// WHY THIS SHAPE AND NOT "logs the error somewhere":
//
// os.Exit skips deferred functions. That is not a style detail — it is the
// reason the shape is forced. A composition root has two obligations that
// conflict unless the lifecycle moves out of main:
//
//   - It must exit NON-ZERO when its run loop dies, and
//   - it must let every `defer` it registered (obs.Shutdown flushing the crash
//     reason via OTel, pool.Close, nc.Close, mesh.Close) actually run.
//
// Written inline in main those are mutually exclusive, and the estate resolved
// the conflict in both wrong directions at once (#266):
//
//   - 22 of 26 services logged the terminal error and then RETURNED from main —
//     defers ran, exit code 0.
//   - api-gateway and schema-registry called os.Exit(1) for a startup failure
//     with a `defer conn.Close()` already on the stack — exit code right, defer
//     skipped.
//
// WHAT EXIT 0 ON A FATAL ERROR ACTUALLY COSTS. Not restarts: a Deployment's pod
// template is necessarily restartPolicy: Always, and the kubelet restarts and
// backs off regardless of exit code, so CrashLoopBackOff is reached either way.
// What is destroyed is the ability to tell two events apart:
//
//   - a graceful SIGTERM on a rolling deploy — kubelet signals,
//     signal.NotifyContext cancels ctx, the run function returns, exit 0; and
//   - a sink that halted because it could neither handle nor dead-letter a
//     message — logs, returns, exit 0.
//
// Byte-identical in the pod's own termination record: lastState.terminated
// reads exitCode 0 / reason "Completed" for both, so
// kube_pod_container_status_last_terminated_reason{reason="Error"} never fires.
// The benign case is the most common event in the cluster, so the failure hides
// inside it. CLAUDE.md's rule, applied to shutdown: "crashed" and "shut down
// cleanly" must never look the same.
//
// THE EXIT-CODE VOCABULARY the delegate function returns:
//
//	0  clean shutdown — SIGTERM/SIGINT observed, the run loop returned nil.
//	1  fatal runtime error — startup completed, then the run loop died.
//	2  startup failure — config, dial, migration or wiring; never became ready.
//
// The 1/2 split is what a human reading `kubectl describe pod` needs first:
// 2 means this build/config never worked, 1 means it worked and then stopped.
//
// SCOPE AND LIMITS, stated so nobody reads more into a green run:
//
//   - services/*/cmd/* only. The CLI and Job binaries under cmd/ and tools/ are
//     out of scope: nats-rebuild and kanz-halt run as Kubernetes Jobs under
//     restartPolicy: Never/OnFailure, where exit 0 means the Job SUCCEEDED, and
//     both already exit non-zero on failure.
//   - This is a STRUCTURAL assertion. It proves main delegates; it does not
//     prove the delegate returns the right code on the right path, and it
//     cannot — that needs the built binary, which is why #266 required running
//     one both ways.
//   - It does force the question at every site. A main that must write
//     `os.Exit(run())` cannot avoid deciding what run() returns, which a rule
//     phrased over log statements never achieves.

// mainsExemptFromExitDelegation is the default-deny allow-list of service
// composition roots permitted to keep a main() that does not delegate. Keyed on
// the module-relative package directory; the value must name the issue that
// retires the entry.
//
// Empty, and intended to stay that way: #266 converted all 26. A grandfathered
// list here would be the thing that lets number 27 land broken.
var mainsExemptFromExitDelegation = map[string]string{}

func TestEveryServiceMainDelegatesItsExitCode(t *testing.T) {
	root := moduleRoot(t)

	var pkgs []*mainPkg
	for _, p := range mainPackages(t, root) {
		if strings.HasPrefix(p.dir, "services/") {
			pkgs = append(pkgs, p)
		}
	}

	// NON-VACUITY: this module has 26 services, each with one composition root.
	// A scan that finds a handful means the glob or the `func main` filter broke,
	// and the check below would then pass over an unrepresentative sample.
	if len(pkgs) < 26 {
		t.Fatalf("found only %d main packages under services/*/cmd/* — this module has 26 services, "+
			"so the exit-delegation check would run over an unrepresentative sample. Fix the scanner.",
			len(pkgs))
	}

	type finding struct {
		pkg    string
		site   string
		reason string
	}
	var (
		violations []finding
		seen       = map[string]bool{}
	)

	for _, p := range pkgs {
		seen[p.dir] = true
		if _, ok := mainsExemptFromExitDelegation[p.dir]; ok {
			continue
		}
		if reason := exitDelegationDefect(p.mainDecl); reason != "" {
			violations = append(violations, finding{
				pkg:    p.dir,
				site:   p.where(p.mainDecl.Pos(), root),
				reason: reason,
			})
		}
	}

	// DEAD-ENTRY CHECK: an exemption naming a package that no longer exists reads
	// as a reviewed decision while protecting nothing.
	var dead []string
	for dir := range mainsExemptFromExitDelegation {
		if !seen[dir] {
			dead = append(dead, dir)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("mainsExemptFromExitDelegation names %d package(s) with no main() under "+
			"services/*/cmd/*: %s\n\nEither the service moved (update the key) or it was removed "+
			"(delete the entry). A stale exemption cannot outlive the thing it excused.",
			len(dead), strings.Join(dead, ", "))
	}

	if len(violations) == 0 {
		return
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].pkg < violations[j].pkg })
	var lines []string
	for _, v := range violations {
		lines = append(lines, fmt.Sprintf("%s (%s): %s", v.pkg, v.site, v.reason))
	}
	t.Fatalf("%d service main() function(s) do not delegate their exit code:\n  %s\n\n"+
		"Required shape, and the whole body:\n\n"+
		"    func main() { os.Exit(run()) }\n\n"+
		"with the lifecycle — and every defer it registers — inside run() int. os.Exit skips "+
		"deferred functions, so a main that exits non-zero inline abandons obs.Shutdown (the crash "+
		"reason is never flushed) and leaks connections, while a main that returns normally to keep "+
		"its defers exits 0 and makes a fatal halt byte-identical to a graceful SIGTERM in "+
		"lastState.terminated. Moving the lifecycle out of main is the only shape that gets both.\n\n"+
		"Return 2 for a startup failure, 1 for a run loop that died after startup, 0 for a clean "+
		"shutdown. See services/archiver/cmd/archiver/main.go.",
		len(violations), strings.Join(lines, "\n  "))
}

// osExitOutsideMainExemptions is the default-deny allow-list of os.Exit calls in
// a service main package that may sit outside `func main`. Keyed on "file:line"
// so a moved call site un-certifies itself and must be re-reviewed.
//
// The bar for an entry is a structural argument that no defer can be on the
// stack at that point — which in practice only holds before the lifecycle
// starts, and the lifecycle starts on main's first line. Empty today.
var osExitOutsideMainExemptions = map[string]string{}

// TestNoServiceMainPackageOsExitsOutsideMain is the other half of the shape.
// Delegation is worthless if run() — or a helper it calls — still reaches for
// os.Exit: that is the INVERSE defect #266 found in api-gateway and
// schema-registry, where os.Exit(1) fired for a startup failure while
// `defer conn.Close()` and the obs.Shutdown defer were already on the stack. The
// exit code was right and the crash reason was never flushed.
//
// So: exactly one os.Exit per service main package, and it is the one in main().
// Anything deeper must return an error or a code up to run().
func TestNoServiceMainPackageOsExitsOutsideMain(t *testing.T) {
	root := moduleRoot(t)

	type finding struct {
		pkg  string
		site string
		fn   string
	}
	var (
		strays []finding
		total  int
		live   = map[string]bool{}
	)

	for _, p := range mainPackages(t, root) {
		if !strings.HasPrefix(p.dir, "services/") {
			continue
		}
		for _, f := range p.files {
			// Walk top-level func decls so every os.Exit can be attributed to the
			// function that owns it — including one nested in a closure or a
			// goroutine, where `return` would not have been a fix anyway.
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !isSelector(call.Fun, "os", "Exit") {
						return true
					}
					total++
					site := p.where(call.Pos(), root)
					live[site] = true
					if fd.Name.Name == "main" && fd.Recv == nil {
						return true
					}
					if _, ok := osExitOutsideMainExemptions[site]; ok {
						return true
					}
					strays = append(strays, finding{pkg: p.dir, site: site, fn: fd.Name.Name})
					return true
				})
			}
		}
	}

	// NON-VACUITY: 26 services, each with exactly one os.Exit — the one in main.
	// A count far below that means the AST match for os.Exit stopped working, and
	// with it broken this guard reports a clean estate no matter what is there.
	if total < 26 {
		t.Fatalf("found only %d os.Exit call(s) across all services/*/cmd/* main packages; there are "+
			"26 services and each must have exactly one (in main). The AST match for os.Exit has "+
			"stopped working — fix the scanner, because with it broken this guard passes vacuously.",
			total)
	}

	// DEAD-ENTRY CHECK: an exemption naming a call site that no longer exists
	// reads as a reviewed decision while protecting nothing.
	var dead []string
	for site := range osExitOutsideMainExemptions {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("osExitOutsideMainExemptions names %d os.Exit call site(s) that no longer exist: %s\n\n"+
			"Either the call moved (update the file:line key) or it was removed (delete the entry). "+
			"A stale exemption cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}

	if len(strays) == 0 {
		return
	}
	sort.Slice(strays, func(i, j int) bool { return strays[i].site < strays[j].site })
	var lines []string
	for _, s := range strays {
		lines = append(lines, fmt.Sprintf("%s in %s() (%s)", s.site, s.fn, s.pkg))
	}
	t.Fatalf("%d os.Exit call(s) in a service main package sit outside func main():\n  %s\n\n"+
		"os.Exit skips every deferred function on the stack. Below main that stack is never "+
		"empty — obs.Shutdown (which flushes the crash reason via OTel), conn.Close, pool.Close "+
		"and mesh.Close are all registered before the first thing that can fail this way. Exiting "+
		"here gets the code right and abandons the evidence, which is the inverse of the exit-0 "+
		"defect and just as expensive to debug.\n\n"+
		"Return an error, or an exit code, up to run() and let main's single os.Exit fire once the "+
		"defer chain has unwound. See services/archiver/cmd/archiver/main.go.",
		len(strays), strings.Join(lines, "\n  "))
}

// mainsExemptFromFatalRecorder is the default-deny allow-list of service
// composition roots permitted not to wire a lifecycle.Fatal. Keyed on the
// module-relative package directory; the value must name the issue retiring it.
//
// The bar for an entry is that the service has NO way to fail after startup —
// which no service in this estate meets, because every one of them serves HTTP
// and a failed ListenAndServe is a fatal it already brings the process down for.
// Empty today.
var mainsExemptFromFatalRecorder = map[string]string{}

// TestEveryServiceMainWiresTheFatalRecorder is the third leg, and the one that
// stops #266 coming back under a passing first two.
//
// Delegation alone does not fix the bug. A composition root can satisfy
// `os.Exit(run())`, hold no stray os.Exit at all, and STILL return 0 from every
// runtime failure — which is precisely the estate this issue found. The way that
// happened is structural and repeatable: a goroutine hits a terminal error, logs
// it, and calls the signal.NotifyContext stop() to bring the service down. That
// cancellation is indistinguishable from the SIGTERM of a rolling deploy, so
// run() takes its clean-shutdown path and reports success.
//
// internal/lifecycle.Fatal is the one place that remembers the reason across
// that cancellation. Requiring every service to construct one forces the
// question "what makes this service exit non-zero after it is up?" at the site
// that has to answer it — which a rule phrased over log statements cannot.
//
// LIMITS: this proves the recorder is WIRED, not that every fatal path Raises
// into it. A goroutine that logs and returns without Raising still exits 0, and
// no static rule can tell that from a deliberate degradation (accounting's FX
// feed and market-data's runFeed are exactly that, on purpose). Deciding which
// is which is a code review; having the mechanism present is what this enforces.
func TestEveryServiceMainWiresTheFatalRecorder(t *testing.T) {
	root := moduleRoot(t)

	var (
		missing []string
		seen    = map[string]bool{}
		total   int
	)
	for _, p := range mainPackages(t, root) {
		if !strings.HasPrefix(p.dir, "services/") {
			continue
		}
		seen[p.dir] = true
		total++
		if _, ok := mainsExemptFromFatalRecorder[p.dir]; ok {
			continue
		}
		wired := false
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isSelector(call.Fun, "lifecycle", "NewFatal") {
					wired = true
				}
				return !wired
			})
			if wired {
				break
			}
		}
		if !wired {
			missing = append(missing, p.dir)
		}
	}

	// NON-VACUITY: 26 services. A scan finding a handful means the glob or the
	// `func main` filter broke and every service silently became "not checked".
	if total < 26 {
		t.Fatalf("found only %d main packages under services/*/cmd/* — this module has 26 services. "+
			"Fix the scanner; with it broken this guard passes vacuously.", total)
	}

	// DEAD-ENTRY CHECK.
	var dead []string
	for dir := range mainsExemptFromFatalRecorder {
		if !seen[dir] {
			dead = append(dead, dir)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("mainsExemptFromFatalRecorder names %d package(s) with no main() under "+
			"services/*/cmd/*: %s\n\nEither the service moved (update the key) or it was removed "+
			"(delete the entry). A stale exemption cannot outlive the thing it excused.",
			len(dead), strings.Join(dead, ", "))
	}

	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Fatalf("%d service composition root(s) never construct a lifecycle.Fatal:\n  %s\n\n"+
		"Wire it next to the signal context and Raise into it wherever the code already calls "+
		"stop() on an error:\n\n"+
		"    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)\n"+
		"    defer stop()\n"+
		"    fatal := lifecycle.NewFatal(stop)\n"+
		"    ...\n"+
		"    return fatal.Code()\n\n"+
		"Without it a terminal error that cancels ctx is byte-identical to the SIGTERM of a "+
		"rolling deploy: same shutdown, same defers, exit 0, reason \"Completed\" in the pod's "+
		"termination record. The benign case is the most common event in the cluster, so the "+
		"failure hides inside it (#266). See services/archiver/cmd/archiver/main.go.",
		len(missing), strings.Join(missing, "\n  "))
}

// exitDelegationDefect returns "" when main's body is exactly `os.Exit(<call>)`,
// and otherwise describes what it is instead. The argument must itself be a
// call: `os.Exit(2)` or `os.Exit(code)` would satisfy a shape check while
// leaving the lifecycle in main, which is the thing being outlawed.
func exitDelegationDefect(main *ast.FuncDecl) string {
	if main == nil || main.Body == nil {
		return "has no body"
	}
	body := main.Body.List
	if len(body) != 1 {
		return fmt.Sprintf("body has %d statements; it must have exactly one, os.Exit(<call>), "+
			"with the lifecycle moved into the called function", len(body))
	}
	expr, ok := body[0].(*ast.ExprStmt)
	if !ok {
		return fmt.Sprintf("its single statement is a %T, not an os.Exit(<call>) expression", body[0])
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return "its single statement is not a call"
	}
	if !isSelector(call.Fun, "os", "Exit") {
		return "its single statement is not an os.Exit call"
	}
	if len(call.Args) != 1 {
		return fmt.Sprintf("os.Exit takes 1 argument, found %d", len(call.Args))
	}
	if _, ok := call.Args[0].(*ast.CallExpr); !ok {
		return "os.Exit's argument must be a call to the function owning the lifecycle " +
			"(e.g. os.Exit(run())), not a literal or a variable — otherwise the lifecycle is " +
			"still in main and its defers still cannot run before the exit"
	}
	return ""
}
