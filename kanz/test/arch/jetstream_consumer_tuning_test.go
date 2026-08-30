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

// #237: every JetStream consumer this module creates must set its DELIVERY
// CONTRACT explicitly — AckWait, MaxDeliver and MaxAckPending.
//
// WHAT THIS CATCHES, AND WHY IT IS A BUILD GUARD RATHER THAN A COMMENT. Before
// #237 pkg/bus created every durable in the estate with exactly three fields —
// Durable, AckPolicy, FilterSubject. The three fields below were unset
// everywhere, and a repo-wide grep for them returned only comments. That is not
// a visible mistake: the consumer is created, it binds, it delivers, nothing
// errors and no log line appears. It simply runs on three JetStream server
// defaults nobody chose:
//
//   - MaxDeliver -1 — a message the handler can neither process nor park (the
//     archiver NAKs when its DLQ produce fails) redelivers every AckWait
//     FOREVER. The only symptom is load and a consumer lag that never clears,
//     and nothing alerts on lag yet (#230).
//   - AckWait 30s — shorter than the OMS submit path's worst case (up to 5s
//     waiting for the goroutine working the order, then a venue REST
//     round-trip, then a contended write), so the broker redelivered a command
//     whose first copy was still executing.
//   - MaxAckPending 1000 — JetStream starts the AckWait clock at FETCH time, and
//     one bus subject is dispatched by one goroutine, so 1000 prefetched
//     messages are a queue in front of a single handler with the clock already
//     running. A backlog manufactured its own duplicate deliveries.
//
// This guard is DEFAULT-DENY over the whole module: any new
// jetstream.ConsumerConfig literal anywhere must carry all three, so a future
// consumer created outside pkg/bus cannot quietly inherit the same defaults.
//
// WHAT IT DELIBERATELY DOES NOT CHECK: the VALUES, and the ordering between
// AckWait and the dedup claim lease. Those are asserted where the real values
// are in scope — pkg/bus/tuning_internal_test.go for every class, plus two
// compile-time constant assertions (`const _ = uint(...)`) in pkg/bus/dedup.go
// and pkg/bus/dedup_redis.go that fail the BUILD if the two leases are ever
// ordered wrongly against maxTunedAckWait / minTunedAckWait. An AST scanner
// re-deriving durations from source text would be a worse copy of a check that
// already runs against the values themselves.
func TestJetStreamConsumersAreExplicitlyTuned(t *testing.T) {
	root := moduleRoot(t)
	sites := jetStreamConsumerConfigLiterals(t, root)

	// Non-vacuity FIRST: a scanner that matched nothing would pass this test no
	// matter how untuned every consumer in the module was. pkg/bus creates
	// exactly two — the queue-group durable in Subscribe and the ephemeral
	// consumer in subscribeEphemeral, which SubscribeBroadcastReady and
	// SubscribeReplay share (one literal, two delivery policies) — and both must
	// be found.
	const wantMin = 2
	if len(sites) < wantMin {
		t.Fatalf("found %d jetstream.ConsumerConfig literal(s) in non-test code, expected at least %d "+
			"(pkg/bus/nats.go creates one in Subscribe and one in subscribeEphemeral) — the scanner "+
			"is broken or the consumer-creation path moved, and this guard is checking nothing",
			len(sites), wantMin)
	}
	var fromBus int
	for _, s := range sites {
		if s.file == "pkg/bus/nats.go" {
			fromBus++
		}
	}
	if fromBus < wantMin {
		t.Fatalf("only %d of %d jetstream.ConsumerConfig literals are in pkg/bus/nats.go, expected at "+
			"least %d — consumer creation is supposed to be centralised there", fromBus, len(sites), wantMin)
	}

	required := []string{"AckWait", "MaxDeliver", "MaxAckPending"}
	var problems []string
	for _, s := range sites {
		var missing []string
		for _, f := range required {
			if !s.fields[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) == 0 {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s:%d — jetstream.ConsumerConfig does not set %s. Unset means the JetStream SERVER default, "+
				"which for MaxDeliver is -1 (a poison message redelivers every AckWait forever, symptom: load "+
				"and permanent lag), for AckWait is 30s (shorter than a venue round-trip, so the broker "+
				"redelivers a command whose first copy is still executing), and for MaxAckPending is 1000 "+
				"(prefetched messages queue in front of one dispatch goroutine with their ack clock already "+
				"running, so a backlog manufactures its own duplicates). Resolve a contract from "+
				"pkg/bus.tuningForSubject instead of leaving these to the server",
			s.file, s.line, strings.Join(missing, ", ")))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d JetStream consumer(s) are created without an explicit delivery contract:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

type consumerConfigSite struct {
	file   string
	line   int
	fields map[string]bool
}

// jetStreamConsumerConfigLiterals finds every non-test `jetstream.ConsumerConfig{...}`
// composite literal in the module and records which field names it sets.
//
// Composite literals only: a config assembled field-by-field on a variable
// (`var c jetstream.ConsumerConfig; c.AckWait = ...`) would not be matched. That
// shape does not exist in this module and inventing a second matcher for it
// would be speculative — but it is the known blind spot, and the non-vacuity
// floor above is what would catch the day someone rewrote pkg/bus into it (the
// literal count would drop below 2 and the test fails loudly rather than
// silently passing).
func jetStreamConsumerConfigLiterals(t *testing.T, root string) []consumerConfigSite {
	t.Helper()
	var out []consumerConfigSite
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", ".claude", "gen", "testdata":
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
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ConsumerConfig" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "jetstream" {
				return true
			}
			fields := map[string]bool{}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					fields[key.Name] = true
				}
			}
			out = append(out, consumerConfigSite{
				file:   filepath.ToSlash(rel),
				line:   fset.Position(lit.Pos()).Line,
				fields: fields,
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
