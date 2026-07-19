package arch

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dev rig's Postgres was created by hand: somebody ran CREATE ROLE and
// CREATE DATABASE against a pod, wrote the DSN into a Secret, and nothing in the
// repository recorded any of it. The pod's storage is ephemeral, so when it was
// recreated the role and the database went with it and the OMS's migrate init
// container has been failing "password authentication failed for user kanzapp"
// ever since. The rig could not be rebuilt from source because the source never
// described it.
//
// infra/deploy/postgres-dev.yaml now declares it. This guard pins the one
// property that makes a second Postgres convention acceptable: it is not a
// second convention. CI already declares role/database/password in
// .github/workflows/kanz-ci.yml, the board documents WHY the role is
// NOSUPERUSER, and a rig that used different values would mean a test proving
// tenant isolation in CI proves nothing about the rig, and vice versa.
//
// This test reads CI as the source of truth and fails if the manifest disagrees,
// in EITHER direction — so changing CI's convention without changing the rig is
// also a failure, which is the drift that actually happens.
func TestDevPostgresMatchesTheCIConvention(t *testing.T) {
	root := moduleRoot(t)
	// The workflow lives above the Go module root.
	ci := readFile(t, filepath.Join(root, "..", ".github", "workflows", "kanz-ci.yml"))
	manifest := readFile(t, filepath.Join(root, "infra", "deploy", "postgres-dev.yaml"))

	role := mustFindOne(t, ci, `CREATE ROLE (\w+) LOGIN PASSWORD '([^']+)' NOSUPERUSER`,
		"CI no longer declares the app role with a literal CREATE ROLE ... NOSUPERUSER")
	db := mustFindOne(t, ci, `CREATE DATABASE (\w+) OWNER (\w+)`,
		"CI no longer declares the app database with a literal CREATE DATABASE ... OWNER")

	roleName, rolePassword := role[1], role[2]
	dbName, dbOwner := db[1], db[2]

	if dbOwner != roleName {
		t.Fatalf("CI declares database %q owned by %q but the app role is %q. This guard assumes "+
			"the single-role shape (the role owns its own database), which is safe ONLY because every "+
			"tenant table runs FORCE ROW LEVEL SECURITY and therefore binds its owner. If CI has split "+
			"into a migrate role and an app role, this test needs to learn about both before the dev "+
			"manifest copies one of them.", dbName, dbOwner, roleName)
	}

	// --- the manifest must declare the SAME role and database ---------------
	for _, want := range []struct{ needle, why string }{
		{"CREATE ROLE " + roleName + " LOGIN PASSWORD '" + rolePassword + "' NOSUPERUSER;",
			"the role, its password and its NOSUPERUSER posture must match CI exactly"},
		{"CREATE DATABASE " + dbName + " OWNER " + roleName + ";",
			"the database name and owner must match CI exactly"},
	} {
		if !strings.Contains(manifest, want.needle) {
			t.Errorf("infra/deploy/postgres-dev.yaml does not contain:\n  %s\n%s.\n\n"+
				"CI (.github/workflows/kanz-ci.yml) is the source of truth for this convention. If you "+
				"changed CI, change the manifest to match; if you changed the manifest, you have created "+
				"a second convention and a tenant-isolation test that passes in one place proves nothing "+
				"about the other.", want.needle, want.why)
		}
	}

	// --- NOSUPERUSER is the whole point ------------------------------------
	// NOSUPERUSER contains SUPERUSER as a substring, so a naive Contains check for
	// "SUPERUSER" is true on a CORRECT manifest and a plain
	// `Contains(SUPERUSER) && !Contains(NOSUPERUSER)` is dead in every case —
	// including the one that matters, where somebody appends an
	// `ALTER ROLE kanzapp SUPERUSER;` and leaves the original declaration in place.
	// Removing the negated form first is what makes the remaining match mean
	// "a genuine grant of superuser".
	if strings.Contains(strings.ReplaceAll(manifest, "NOSUPERUSER", ""), "SUPERUSER") {
		t.Error("infra/deploy/postgres-dev.yaml grants the app role SUPERUSER. A superuser BYPASSES " +
			"row-level security even where the table sets FORCE, so every tenant-isolation test would " +
			"pass without isolation existing. If migrations are failing on permissions, grant the " +
			"specific privilege — do not widen the role. (This check previously could not fire: " +
			"NOSUPERUSER contains SUPERUSER as a substring, so the old Contains/!Contains pair was " +
			"dead even in this exact scenario. If you are reading this message, the check is now live.)")
	}

	// --- the DSN the services read must address that same database ---------
	wantDSN := "postgres://" + roleName + ":" + rolePassword + "@postgres.kanz-services.svc:5432/" + dbName + "?sslmode=disable"
	if !strings.Contains(manifest, wantDSN) {
		t.Errorf("infra/deploy/postgres-dev.yaml does not carry the DSN %q. The Secret's DSN and the "+
			"initdb script are the two halves of one fact: a manifest that creates database %q and hands "+
			"out a DSN for a different one fails at runtime with an authentication error that looks like "+
			"a password problem and is not.", wantDSN, dbName)
	}

	// --- it must not become a production manifest ---------------------------
	if !strings.Contains(manifest, "DEV-ONLY") {
		t.Error("infra/deploy/postgres-dev.yaml must state DEV-ONLY in its header. It commits a known " +
			"password and runs a single ephemeral replica with no backups; the reason that is acceptable " +
			"is that it is unmistakably a dev rig, and the header is what makes it unmistakable.")
	}
	if strings.Contains(manifest, "persistentVolumeClaim") {
		t.Error("infra/deploy/postgres-dev.yaml declares a PVC. The fix for the rig is that it REBUILDS " +
			"from the initdb script on a fresh data directory — that is what makes it reproducible from " +
			"source. A PVC preserves state instead, which means the next divergence survives too and is " +
			"invisible again. If this rig needs durable storage, that is a different decision than this one.")
	}
}

// mustFindOne applies pattern to src and requires exactly one match, so a
// workflow that grew a second CREATE ROLE fails loudly instead of silently
// pinning whichever one happened to come first.
func mustFindOne(t *testing.T, src, pattern, why string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1)
	if len(m) == 0 {
		t.Fatalf("%s (pattern %q matched nothing). This guard reads CI as the source of truth for the "+
			"Postgres convention; if CI now provisions its database another way, teach this test the new "+
			"shape rather than deleting it — the rig and CI silently disagreeing is the failure it exists "+
			"to prevent.", why, pattern)
	}
	if len(m) > 1 {
		t.Fatalf("pattern %q matched %d times in the CI workflow. This guard pins ONE app role/database; "+
			"with several it cannot tell which one the dev rig should mirror. Narrow the pattern or split "+
			"the guard deliberately.", pattern, len(m))
	}
	return m[0]
}
