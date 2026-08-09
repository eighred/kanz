package arch

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// EVERY SUBJECT THE CODE NAMES MUST BE CARRIED BY A STREAM.
//
// This is the audit that follows EXEC-M9, made permanent. A subject with no stream
// bound to it fails in two directions, and both are silent from the code's point of
// view:
//
//   - PUBLISH: a JetStream publish to an unbound subject is a HARD ERROR. Every FACT
//     the producer emits is dropped on the floor of a live broker (EXEC-M7a: the
//     whole signal→order path, and the accounting FACTs).
//   - CONSUME: a consumer on an unbound subject 404s at startup and takes the
//     service's ENTIRE CONSUMER GROUP down with it. That is how one forgotten stream
//     (compliance.>) left the OMS consuming NOTHING AT ALL — no orders, ever — while
//     every test stayed green (EXEC-M9).
//
// Neither is visible without a real broker, and services are not booted against one
// in a unit test. So the check is static: collect the subjects the code declares,
// and require each to match a stream the PRODUCTION bootstrap provisions. This is
// the same file ops applies, so the test fails HERE rather than in a cluster.
//
// The live counterpart (TestEverySubjectResolvesOnARealBroker) lives in this file
// rather than with the other real-bus tests, because it must consume the SAME
// subject list — and duplicating the scanner is how the two would drift apart. It
// is the one broker-gated test in this otherwise dependency-free package.
//
// Scope, honestly: it sees subjects declared as string literals — a `Subject*` /
// `*Subject` / `eventType*` constant, or a literal `Subject:` field on a bus.Event.
// That is how every subject on this platform is written today. A subject assembled
// at runtime from parts would slip past it; if one is ever added, extend this test
// rather than routing around it.
func TestEverySubjectIsCarriedByAStream(t *testing.T) {
	root := moduleRoot(t)

	patterns := streamSubjects(t, filepath.Join(root, "infra", "nats", "bootstrap-job.yaml"))
	if len(patterns) == 0 {
		t.Fatal("no ensure_stream lines found in the bootstrap ConfigMap — has the format changed?")
	}
	subjects := declaredSubjects(t, root)
	if len(subjects) == 0 {
		t.Fatal("no subjects found in the source — this test would pass vacuously")
	}

	var unbound []string
	for _, s := range subjects {
		if !covered(s, patterns) {
			unbound = append(unbound, s)
		}
	}
	sort.Strings(unbound)
	if len(unbound) > 0 {
		t.Fatalf("these subjects are carried by NO stream in infra/nats/bootstrap-job.yaml:\n\n  %s\n\n"+
			"A publish to an unbound subject is a hard error, and a CONSUMER on one 404s and takes the whole "+
			"service's consumer group down with it. Provisioned streams: %v",
			strings.Join(unbound, "\n  "), patterns)
	}
	t.Logf("%d subjects, all carried by one of: %v", len(subjects), patterns)
}

// streamSubjects reads the subject patterns the production bootstrap provisions:
//
//	ensure_stream EXECUTION "execution.>,strategy.>,order.>" 24h
func streamSubjects(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the bootstrap ConfigMap: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*ensure_stream\s+\w+\s+"([^"]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		for _, s := range strings.Split(m[1], ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// subjectShape matches a bus subject ("order.order.submit"). It deliberately
// excludes proto type names ("order.v1.Fill"), which share the shape but are
// event types, not subjects.
//
// A `*` is allowed as a WHOLE token, for the MT-02 tenant routing subjects
// ("tenant.*.order.order.submit", #358/#360). Without it those were filtered out
// here — before any guard saw them — so every tenancy grant and stream binding
// for a tenant-routed subject was unverified. Measured: deleting
// webhook-ingest's prefixed grant left the permissions guard green.
var (
	subjectShape = regexp.MustCompile(`^[a-z][a-z0-9]*(\.([a-z0-9_]+|\*))+$`)
	protoTypeRef = regexp.MustCompile(`\.v[0-9]+\.`)
)

// declaredSubjects collects every subject literal the non-test source declares.
func declaredSubjects(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "gen", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil // not our job to police syntax; the compiler does that
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.ValueSpec:
				// const SubjectSubmit = "order.order.submit"
				for i, name := range v.Names {
					if i >= len(v.Values) || !subjectish(name.Name) {
						continue
					}
					if s, ok := stringLit(v.Values[i]); ok {
						record(seen, s)
					}
				}
			case *ast.KeyValueExpr:
				// bus.Event{Subject: "order.order.filled", ...}
				if k, ok := v.Key.(*ast.Ident); ok && k.Name == "Subject" {
					if s, ok := stringLit(v.Value); ok {
						record(seen, s)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// subjectish reports whether an identifier names a subject or an event type (on
// this platform the two are the same string wherever a subject is declared).
func subjectish(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "subject") || strings.Contains(n, "eventtype")
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func record(seen map[string]bool, s string) {
	if subjectShape.MatchString(s) && !protoTypeRef.MatchString(s) {
		seen[s] = true
	}
}

// covered reports whether a stream pattern carries the subject. NATS patterns are
// a bare `>` (everything, account-wide — audit's subscribe grant and
// archiver's publish deny both use exactly this shape), `domain.>` (everything
// under domain), or a literal subject.
//
// The bare `>` case used to fall through both branches below: it doesn't end
// in `.>` (that suffix check needs the dot), and no real subject equals the
// literal string ">", so covered(anything, []string{">"}) returned false —
// silently treating "matches everything" as "matches nothing". Nothing
// exercised it before nats_jetstream_machinery_test.go (no existing publish
// allow-list or deny-list used a bare ">"; only streamSubjects' `domain.>`
// patterns reached this function), so it shipped unnoticed. It matters now:
// audit's subscribe permission is `allow: [">"]` by design (its own comment
// in tenancy.yaml — "the one entry below where subscribe: [">"] is the
// CORRECT, code-derived answer"), and a bare-">" bug would make the JetStream
// machinery guard demand $INBOX.> be listed a second time even though ">"
// already covers it.
func covered(subject string, patterns []string) bool {
	for _, p := range patterns {
		if p == ">" {
			return true
		}
		if strings.HasSuffix(p, ".>") {
			if strings.HasPrefix(subject, strings.TrimSuffix(p, ">")) {
				return true
			}
			continue
		}
		if subject == p {
			return true
		}
	}
	return false
}

// TestEverySubjectResolvesOnARealBroker is the other half of the claim.
//
// The static test above proves the bootstrap FILE names a stream for every subject.
// It does not prove the broker AGREES: that the script ran, that the patterns bind
// what we think they bind, and that StreamNameBySubject — the exact call
// pkg/bus.Subscribe makes before it creates a consumer — actually resolves. That
// call is the one that 404s and takes a service's whole consumer group down with it.
//
// CI bootstraps the topology with the script production applies, so this runs there.
func TestEverySubjectResolvesOnARealBroker(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL (with the topology bootstrapped) to check the subjects against a real broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	subjects := declaredSubjects(t, moduleRoot(t))
	for _, s := range subjects {
		if _, err := js.StreamNameBySubject(ctx, s); err != nil {
			t.Errorf("no stream carries %q on this broker: %v\n"+
				"    A consumer on it would 404 at startup and take its service's ENTIRE consumer group down; "+
				"a publish would be a hard error.", s, err)
		}
	}
}
