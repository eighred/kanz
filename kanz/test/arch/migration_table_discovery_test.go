package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A migration may not DISCOVER the tables it rewrites.
//
// Eight services shipped a byte-identical DO block that selected its targets from
// pg_class — every table in current_schema() with relrowsecurity — then dropped and
// recreated every policy it found. Each copy was correct in a database holding only
// its own service's tables, and wrong in one holding more: whichever copy ran LAST
// rewrote all of them.
//
// What that cost, reproduced against Postgres 16 with the real migration files (#227):
// accounting/0003 installs
//
//	WITH CHECK (tenant_id = app_current_tenant()
//	        AND venue_account_id = app_current_venue_account())
//
// and after wealth's copy of the loop ran, it read `WITH CHECK (tenant_id =
// app_current_tenant())`. Both wealth migrations reported success — no error, no
// warning, no notice. The collateral-segregation guard accounting/0003 exists to
// enforce was gone, and nothing said so. A migration cannot know that a policy it did
// not write is stronger than its own, so it does not get to decide: the table list is
// explicit or the build fails.
//
// SCOPE. This bans DISCOVERY, not catalog access. Reading pg_policies to drop every
// policy on a table the migration NAMED is still required — a leftover permissive
// policy would be OR'd with the new one and would restore the silent empty read that
// app_current_tenant() exists to prevent. The line is whether the catalog chose the
// table or the author did.
//
// Comments are stripped before matching, deliberately: the repaired migrations explain
// the defect in prose and quote the offending query, and a guard that fired on its own
// post-mortem would be deleted rather than fixed.
var migrationDiscoveryTokens = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bpg_class\b`),
	regexp.MustCompile(`(?i)\brelrowsecurity\b`),
	regexp.MustCompile(`(?i)information_schema\.tables\b`),
}

// migrationDiscoveryExempt is the default-deny allow-list of migrations permitted to
// discover their targets, keyed "service/file". It is EMPTY, and the dead-entry arm
// below fails the build if an entry stops matching — so an exemption cannot outlive
// the repair it was granted for. An entry here needs the issue that retires it.
var migrationDiscoveryExempt = map[string]string{}

// stripSQLComments removes `--` line comments. Block comments and `--` inside a string
// literal are not handled: no migration in this tree uses either, and a half-correct
// SQL lexer here would be a second thing to get wrong. If one appears, this is where
// the parser goes.
func stripSQLComments(body string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func migrationFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	base := filepath.Join(root, "services")
	svcs, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	for _, svc := range svcs {
		if !svc.IsDir() {
			continue
		}
		dir := filepath.Join(base, svc.Name(), "migrations")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue // a service with no migrations is normal
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("read %s/%s: %v", svc.Name(), e.Name(), rerr)
			}
			out[svc.Name()+"/"+e.Name()] = string(body)
		}
	}
	return out
}

func discoversTables(body string) string {
	sql := stripSQLComments(body)
	for _, re := range migrationDiscoveryTokens {
		if m := re.FindString(sql); m != "" {
			return m
		}
	}
	return ""
}

func TestNoMigrationDiscoversTheTablesItRewrites(t *testing.T) {
	files := migrationFiles(t, moduleRoot(t))

	// NON-VACUITY. This tree definitely has migrations; a walk that finds none means
	// the layout moved and this guard is asserting nothing.
	if len(files) < 8 {
		t.Fatalf("scanned only %d migrations — expected at least 8. The guard is not "+
			"looking where the migrations are, so it would pass no matter what they contain", len(files))
	}

	var offenders []string
	for name, body := range files {
		tok := discoversTables(body)
		if tok == "" {
			continue
		}
		if _, ok := migrationDiscoveryExempt[name]; ok {
			continue
		}
		offenders = append(offenders, name+" (matched "+tok+")")
	}
	sort.Strings(offenders)

	if len(offenders) > 0 {
		t.Errorf("these migrations choose their targets from the catalog rather than naming them:\n  %s\n\n"+
			"In a schema holding more than one service's tables this rewrites tables the migration "+
			"does not own. It already cost accounting its venue_account_id WITH CHECK — silently, "+
			"with every migration reporting success (#227).\n\n"+
			"Name the tables instead:\n"+
			"    FOREACH t IN ARRAY ARRAY['your_table', 'your_other_table'] LOOP\n\n"+
			"Reading pg_policies for a table you NAMED is still fine — that is not discovery.",
			strings.Join(offenders, "\n  "))
	}

	// DEAD ENTRIES. An exemption for a migration that no longer discovers anything is
	// stale permission: it would silently re-authorise the pattern if the file changed
	// back. Same stance as every other exemption map in this package.
	var dead []string
	for name := range migrationDiscoveryExempt {
		body, ok := files[name]
		if !ok {
			dead = append(dead, name+" (no such migration)")
			continue
		}
		if discoversTables(body) == "" {
			dead = append(dead, name+" (no longer discovers tables — repaired)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("migrationDiscoveryExempt has %d stale entr(y/ies) — remove them so the "+
			"exemption cannot outlive the repair:\n  %s", len(dead), strings.Join(dead, "\n  "))
	}
}
