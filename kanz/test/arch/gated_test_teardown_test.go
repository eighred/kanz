package arch

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A GATED TEST'S TEARDOWN MUST DROP EVERYTHING ITS MIGRATIONS CREATE.
//
// # The defect, which shipped twice in two days
//
// The Postgres-gated tests replay a service's migrations into a shared database
// and drop the tables first so the run is repeatable. When a migration adds a
// TABLE and the teardown's DROP list is not updated with it, the FIRST run
// passes — nothing exists yet — and every run after it fails with
//
//	ERROR: relation "<table>" already exists (SQLSTATE 42P07)
//
// which reads as a broken migration rather than an incomplete teardown, and
// takes every other gated test in the package down with it: applySchema is
// shared, so a golden-store test fails because an unrelated table was added.
//
// It happened with exception_override_proposals (#410) and then, in the change
// that FIXED that one, with outbox (#410's FACT path). Main was red across three
// merges.
//
// # Why nobody caught it locally
//
// TEST_POSTGRES_URL is unset on a developer box, so all fourteen gated files
// SKIP. A skip is not a pass, and the whole class of defect is invisible until
// CI — where it also does not appear on the run that introduces it, because the
// database is fresh. It needs a SECOND run against the same database, which is
// the one thing a developer never does and CI always does.
//
// So this checks it statically: no database, no second run, no waiting.
func TestGatedTestTeardownsDropEveryTableTheirMigrationsCreate(t *testing.T) {
	root := moduleRoot(t)

	createRe := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)
	dropRe := regexp.MustCompile(`(?is)DROP\s+TABLE\s+IF\s+EXISTS\s+([^` + "`" + `;]+?)\s+CASCADE`)
	dropSchemaRe := regexp.MustCompile(`(?i)DROP\s+SCHEMA\s+IF\s+EXISTS`)

	type teardown struct {
		file  string
		drops map[string]bool
	}

	// Find every _test.go that tears a schema down, and the migrations it means.
	var teardowns []teardown
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)

		// TWO KINDS OF TEARDOWN, and only one of them can have the missing-table
		// defect.
		//
		//   DROP TABLE IF EXISTS a, b, c   names each table, so a table added by a
		//                                  migration and not added here survives
		//                                  into the next run.
		//   DROP SCHEMA … CASCADE          takes everything, whatever was created.
		//
		// The second is strictly better and internal/outbox already uses it. It is
		// recorded here rather than silently skipped, because "this file has no
		// drop list" and "this file cannot have the defect" are different
		// statements and only the second is a reason to stop looking.
		var drops map[string]bool
		if m := dropRe.FindStringSubmatch(string(b)); m != nil {
			drops = map[string]bool{}
			for _, name := range strings.Split(m[1], ",") {
				if n := strings.Trim(strings.TrimSpace(name), `"`); n != "" {
					drops[n] = true
				}
			}
		} else if !dropSchemaRe.MatchString(string(b)) {
			return nil // replays nothing; not a schema teardown at all
		}
		if len(migrationPathLiterals(t, path)) == 0 && drops == nil {
			return nil
		}
		teardowns = append(teardowns, teardown{file: relSlash, drops: drops})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// NON-VACUITY: a walk that found no teardowns would pass on an estate where
	// every one of them was wrong.
	if len(teardowns) < 2 {
		t.Fatalf("found only %d schema teardowns in the module — the scanner is broken, not the "+
			"tests (there were 3 when this guard was written)", len(teardowns))
	}

	var problems []string
	var checkedTables int
	for _, td := range teardowns {
		// The migrations this teardown is replaying: the ones its own service
		// owns. Derived from the test's directory rather than from the literal
		// path in the source, because that literal is exactly what broke when
		// internal/outbox moved — a guard that read it would have moved with it
		// and gone on agreeing with itself.
		dir := migrationsDirFor(root, td.file)
		if dir == "" {
			continue
		}
		files, gerr := filepath.Glob(filepath.Join(dir, "*.sql"))
		if gerr != nil || len(files) == 0 {
			problems = append(problems, td.file+": replays migrations from "+
				strings.TrimPrefix(dir, root)+", which holds no .sql files — the path is wrong, and "+
				"every gated test in this package fails with `glob migrations: found 0`")
			continue
		}

		// AND THE PATH THE TEST ACTUALLY USES MUST RESOLVE TO THAT DIRECTORY.
		//
		// Everything above derives the migrations from the test's LOCATION, which
		// is deliberate — a check that simply read the literal would move with it
		// and go on agreeing with itself. But derivation alone cannot see a wrong
		// literal, and a wrong literal is exactly what shipped: promoting
		// internal/outbox left "../../migrations" pointing at kanz/migrations, and
		// every gated test in the package failed with `glob migrations: found 0`.
		//
		// So the two are compared. The derived directory says where the migrations
		// ARE; the literal says where the test will LOOK; a guard that checked
		// only one of them has been useless for one of the two failures already.
		for _, lit := range migrationPathLiterals(t, filepath.Join(root, td.file)) {
			resolved, rerr := filepath.Abs(filepath.Join(filepath.Dir(filepath.Join(root, td.file)), lit))
			if rerr != nil {
				continue
			}
			want, _ := filepath.Abs(dir)
			if resolved != want {
				problems = append(problems, td.file+": looks for migrations at "+lit+
					" (which resolves to "+strings.TrimPrefix(resolved, root)+
					"), but this package's migrations are at "+strings.TrimPrefix(want, root))
			}
		}
		sort.Strings(files)
		for _, f := range files {
			sql, rerr := os.ReadFile(f)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if td.drops == nil {
				continue // DROP SCHEMA CASCADE: cannot miss a table
			}
			for _, m := range createRe.FindAllStringSubmatch(string(sql), -1) {
				table := m[1]
				checkedTables++
				if td.drops[table] {
					continue
				}
				problems = append(problems, td.file+": does not drop \""+table+"\", created by "+
					filepath.Base(f))
			}
		}
	}

	// NON-VACUITY, half two: if the CREATE scan stopped matching, every teardown
	// would pass with nothing checked.
	if checkedTables < 5 {
		t.Fatalf("matched only %d CREATE TABLE statements across the migrations these teardowns "+
			"replay — the SQL scan is broken", checkedTables)
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d gated-test teardown gap(s):\n  %s\n\n"+
			"The FIRST run against a fresh database passes and every run after it fails with "+
			"`relation \"<table>\" already exists`, taking every other gated test in the package with "+
			"it — applySchema is shared. It cannot be reproduced locally, because TEST_POSTGRES_URL "+
			"is unset and the gated tests SKIP; and it does not appear on the CI run that introduces "+
			"it either, because that database is fresh.\n\nA missing table: add it to the DROP list. "+
			"A wrong path: the test finds no migrations at all and fails with `glob migrations: "+
			"found 0` on the very first run.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// migrationsDirFor returns the migrations directory a gated test replays.
//
// A test under services/<svc>/... replays that service's migrations. internal/
// packages own no database — the outbox is created per consumer — so the one
// under internal/outbox is mapped explicitly rather than guessed, and the map is
// the place a reader learns that the coupling exists at all.
func migrationsDirFor(root, testFile string) string {
	if strings.HasPrefix(testFile, "services/") {
		svc := strings.SplitN(strings.TrimPrefix(testFile, "services/"), "/", 2)[0]
		return filepath.Join(root, "services", svc, "migrations")
	}
	if strings.HasPrefix(testFile, "internal/outbox/") {
		// internal/outbox owns no migrations: the table is created by each
		// consumer's own, and the OMS's copy is the reference one its durable
		// tests replay.
		return filepath.Join(root, "services", "oms", "migrations")
	}
	return ""
}

// migrationPathLiterals returns the relative paths a test source names as its
// migrations directory.
//
// Matched on a literal ENDING in "migrations", which is the convention every
// service follows. A test that named its directory some other way would be
// invisible here — and would also be the first in the module to do so, which is
// a change worth making visible in this guard rather than working around.
func migrationPathLiterals(t *testing.T, testFile string) []string {
	t.Helper()
	// PARSED WITHOUT COMMENTS, then re-printed — so what is scanned is string
	// literals, not prose.
	//
	// The reason for this rule necessarily QUOTES the wrong path ("the literal
	// was ../../migrations before the package moved"), and a raw-source scan
	// matched that sentence and reported the file as broken while it was correct.
	// That is the second time in one day a guard of mine checked a comment: the
	// SPA guard matched a workflow STEP NAME containing `npm run build`. A guard
	// that a comment can trip is a guard people reword their way around.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, testFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", testFile, err)
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		t.Fatalf("print %s: %v", testFile, err)
	}
	b := buf.Bytes()
	re := regexp.MustCompile(`"((?:\.\./)+[^"]*migrations)"`)
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}
