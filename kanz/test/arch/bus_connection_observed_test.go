package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY COMPOSITION ROOT THAT DIALS THE SPINE CAN SAY WHETHER IT STILL HAS ONE (#636).
//
// # What went wrong without it
//
// pkg/bus's own doc has always stated the requirement: "MaxReconnects defaults
// to -1 (retry forever), which means a disconnect is otherwise SILENT: the
// client buffers and heals and nothing upstream ever learns the bus went away.
// A trading process must learn."
//
// One service learned. Of 24 bus.NATSConfig literals under services/, exactly
// ONE set OnDisconnect — and until #636 that field was also what decided whether
// DisconnectErrHandler and ClosedHandler were installed AT ALL. So in twenty
// services the nats.Conn could close for good with the process still running,
// /readyz still answering 200, and nothing written anywhere: the Go NATS client
// logs nothing of its own, and with infinite reconnects there is no crash to
// notice.
//
// The handlers are unconditional now, so LOGGING can no longer be forgotten.
// This guard covers the half that still can: the METRIC. Losing the spine is
// only alertable where DialNATS was given a *BusMetrics, and
// BusConsumerStalled — the one partial signal that existed — reads
// kanz_bus_consume_total, so a service that merely PUBLISHES (the gateway,
// market-ingest, the venue adapters emitting fills) contributes nothing to it
// and sits outside that rule entirely. For those, kanz_bus_connected is the only
// signal there is.
//
// # Why a guard rather than trusting the twenty-fourth call site
//
// The same argument TestNoCompositionRootSilentlyFallsBackToAnInMemoryStore
// makes: composition roots are the code no unit test constructs, so a field
// omitted here is invisible until production. Twenty-three of twenty-four sites
// having omitted OnDisconnect is the measured base rate for "remembered by
// habit" in this file set.
//
// Read from the AST, never a grep: this file names the field it looks for a
// dozen times in prose, and three guards in this package have already passed
// while the checked thing was deleted because a regex matched their own
// comments.
func TestEveryCompositionRootObservesItsBusConnection(t *testing.T) {
	root := moduleRoot(t)
	sites := natsConfigLiterals(t, filepath.Join(root, "services"))

	// NON-VACUITY. There are two dozen of these. A scan that found none would
	// pass however silent the estate had become — the exact shape of the defect.
	if len(sites) < 20 {
		t.Fatalf("found only %d bus.NATSConfig literal(s) under services/, want at least 20 — "+
			"the AST scan is broken and this guard proves nothing", len(sites))
	}

	var problems []string
	seen := map[string]bool{}
	for _, s := range sites {
		svc := s.service
		seen[svc] = true
		if s.hasMetrics {
			if reason, exempt := busMetricsExempt[svc]; exempt {
				problems = append(problems, svc+" is EXEMPT but now sets Metrics — the exemption is "+
					"dead and only hides this composition root from the guard. Delete it.\n      reason on file: "+reason)
			}
			continue
		}
		if _, exempt := busMetricsExempt[svc]; exempt {
			continue
		}
		problems = append(problems, s.file+":"+s.pos+": bus.NATSConfig sets no Metrics, so this process "+
			"exports no kanz_bus_connected series. Losing the spine is visible only to somebody reading "+
			"its logs — and if it publishes without consuming, BusConsumerStalled cannot see it either, "+
			"because that rule reads kanz_bus_consume_total and this process emits none.")
	}

	// DEAD ENTRIES: an exemption for a service that no longer dials the bus is
	// stale permission, and the next reader takes it for a decision.
	for svc := range busMetricsExempt {
		if !seen[svc] {
			problems = append(problems, "exemption for "+svc+" is DEAD: it declares no bus.NATSConfig "+
				"literal any more (or no longer exists), so it needs no exemption")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d composition root(s) cannot report losing the spine:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// busMetricsExempt names composition roots that dial NATS and deliberately
// export no connection metric, with the reason.
//
// DEFAULT-DENY: an entry is permission, and the dead-entry arm removes it the
// moment the service stops dialling or starts setting Metrics.
var busMetricsExempt = map[string]string{
	"archiver-drain": "A ONE-SHOT OPERATOR COMMAND, NOT A SERVICE. cmd/archiver-drain re-archives events " +
		"parked on dlq.archiver after a human has fixed the routing table; it runs to completion under " +
		"that operator's eye and exits. It has no observability.Provider and therefore no registry — " +
		"and a connection gauge that is scraped by nobody, on a process whose whole lifetime is one " +
		"operator's terminal session, would be configuration that exists to satisfy a guard. The " +
		"operator IS the observer here, and the unconditional disconnect logging added by #636 already " +
		"puts a spine loss in front of them. RETIRING THIS ENTRY means archiver-drain becoming " +
		"long-running or gaining a metrics endpoint; the dead-entry arm above catches the second.",
}

type natsConfigSite struct {
	service    string
	file       string
	pos        string
	hasMetrics bool
}

// natsConfigLiterals finds every composite literal whose type is
// bus.NATSConfig (or NATSConfig under a different import alias) in non-test Go
// files under dir, and reports whether each sets the Metrics field.
//
// It walks the whole services/ tree rather than only cmd/**: a dial can be made
// from a service's internal package just as silently, and restricting the scan
// to composition roots would be a rule about where the defect is allowed to
// live rather than about the defect.
func natsConfigLiterals(t *testing.T, dir string) []natsConfigSite {
	t.Helper()
	fset := token.NewFileSet()

	var out []natsConfigSite
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, _ := filepath.Rel(filepath.Dir(dir), path)
		rel = filepath.ToSlash(rel)
		service := strings.SplitN(strings.TrimPrefix(rel, "services/"), "/", 2)[0]
		// The binary's name, not the directory's: services/archiver holds both
		// archiver and archiver-drain, and they are different processes with
		// different lifetimes. Take it from cmd/<name>/ where present.
		if i := strings.Index(rel, "/cmd/"); i >= 0 {
			rest := rel[i+len("/cmd/"):]
			service = strings.SplitN(rest, "/", 2)[0]
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isNATSConfigType(lit.Type) {
				return true
			}
			site := natsConfigSite{
				service: service,
				file:    rel,
				pos:     fset.Position(lit.Pos()).String()[len(path)+1:],
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Metrics" {
					site.hasMetrics = true
				}
			}
			out = append(out, site)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// isNATSConfigType matches `bus.NATSConfig` and a bare `NATSConfig`, so a file
// that aliases the import is judged on the type it actually names.
func isNATSConfigType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "NATSConfig"
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "NATSConfig"
	}
	return false
}

// A COMPOSITION ROOT BUILDS BusMetrics ONCE, OR IT DOES NOT START (#636).
//
// NewBusMetrics ends in reg.MustRegister, and prometheus PANICS on a duplicate
// collector: "duplicate metrics collector registration attempted". Two calls
// against one registry is therefore not a tidiness problem, it is a process that
// dies at startup.
//
// TWO SERVICES SHIPPED IN THAT STATE and both were found while wiring #636:
//
//   - accounting built one in runConsumer and another in runFXFeed, which
//     receive the same *observability.Provider and run CONCURRENTLY whenever
//     ACCOUNTING_NATS_URL is set and a live FX feed is configured.
//   - market-data built one in runIngest and another in runFeed, which both run
//     whenever MARKET_DATA_FEED and MARKET_DATA_NATS_URL are set.
//
// Both are valid configurations. Neither service could start in them, and
// nothing caught it because no unit test constructs a composition root —
// TestNoCompositionRootSilentlyFallsBackToAnInMemoryStore's header makes the
// same argument about the same blind spot.
//
// The check is per FILE rather than per registry because a composition root is
// one file here and resolving which *prometheus.Registerer each call receives
// would mean type-checking the whole program to catch a mistake that a count
// already catches. A service that genuinely needs two — separate registries for
// separate processes in one binary — will trip this and can say so; none does.
func TestNoCompositionRootRegistersBusMetricsTwice(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	counts := map[string]int{}
	scanned := 0
	err := filepath.Walk(filepath.Join(root, "services"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, _ := filepath.Rel(filepath.Dir(filepath.Join(root, "services")), path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "NewBusMetrics" {
				return true
			}
			counts[rel]++
			scanned++
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk services/: %v", err)
	}

	// NON-VACUITY: most services build one. A scan finding none is a broken scan.
	if scanned < 10 {
		t.Fatalf("found only %d NewBusMetrics call(s) under services/, want at least 10 — "+
			"the AST scan is broken and this guard proves nothing", scanned)
	}

	var problems []string
	for file, n := range counts {
		if n > 1 {
			problems = append(problems, file+" calls bus.NewBusMetrics "+strconv.Itoa(n)+" times. Each call ends "+
				"in Registry.MustRegister, which PANICS on a duplicate collector — so if two of these "+
				"run against the same registry this process dies at startup, in whatever configuration "+
				"reaches both. Build it once and pass it down.")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d composition root(s) register the bus collectors more than once:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}
