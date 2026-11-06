package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustOMSStorePath = "crates/oms-store"

var rustOMSStoreSources = []string{"src/lib.rs", "src/identity.rs", "examples/fixture.rs"}

const rustOMSStoreCargo = `[package]
name = "kanz-oms-store"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false
[dependencies]
kanz-oms-fold-shadow = { path = "../../services/oms-fold-shadow" }
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.1"
prost-types = "=0.14.1"
sha2 = "=0.10.9"
tokio-postgres = "=0.7.18"
[dev-dependencies]
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "time"] }
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
base64 = "=0.22.1"`

// #1413: source/layout/token guard, not Rust AST/data-flow or SQL correctness.
// Actual Go-read/Rust-write PostgreSQL qualification supplies separate evidence.
func checkRustOMSStoreBoundary(root string) error {
	root = filepath.Join(root, filepath.FromSlash(rustOMSStorePath))
	data, e := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if e != nil {
		return e
	}
	code, e := rustMigrationWithoutComments(string(data))
	if e != nil {
		return e
	}
	if rustMigrationCompact(code) != rustMigrationCompact(rustOMSStoreCargo) {
		return fmt.Errorf("unreviewed OMS store dependency/recipe")
	}
	found := map[string]string{}
	allowed := map[string]bool{}
	for _, p := range rustOMSStoreSources {
		allowed[p] = true
	}
	e = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("store symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if strings.EqualFold(filepath.Ext(rel), ".proto") || strings.EqualFold(d.Name(), "config.toml") || strings.EqualFold(d.Name(), "config") {
			return fmt.Errorf("parallel store contract/config")
		}
		if strings.EqualFold(d.Name(), "Cargo.toml") && rel != "Cargo.toml" {
			return fmt.Errorf("nested store crate")
		}
		if filepath.Ext(rel) != ".rs" {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("unreviewed store source %s", rel)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		s, e := rustMigrationWithoutComments(string(b))
		if e != nil {
			return e
		}
		s = rustMigrationCompact(s)
		found[rel] = s
		if regexp.MustCompile(`\bf(?:32|64)\b|\bunsafe\b|(?:struct|enum|type)(?:Decimal|Money|Fill|OrderState)\b`).MatchString(s) || strings.Contains(s, "include!") || strings.Contains(s, "#[path=") {
			return fmt.Errorf("store financial/unsafe/alternate-source boundary")
		}
		if rel != "examples/fixture.rs" {
			for _, bad := range []string{"std::env", "std::process", "std::net", ".connect(", "NoTls", ".publish("} {
				if strings.Contains(s, bad) {
					return fmt.Errorf("repository acquires runtime authority")
				}
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	if len(found) != len(allowed) {
		return fmt.Errorf("missing store source")
	}
	if strings.Count(found["src/lib.rs"], `scoped(client,tenant).await?`) != 4 || strings.Count(found["src/lib.rs"], `enqueue(&tx,records).await?`) != 2 || strings.Count(found["src/lib.rs"], `commit(tx).await`) != 2 {
		return fmt.Errorf("repository owners must reuse scope/outbox/commit")
	}
	for _, marker := range []string{`ONCONFLICT(tenant_id,order_id)DONOTHING`, `returnErr(Error::Exists)`, `SELECTCASEWHENoctet_length(state)::bigint<=$2THENstateEND,versionFROMordersWHEREorder_id=$1`, `ok_or(Error::NotFound)?`, `COALESCE(NULLIF(raw_fill_id,''),fill_id)`, `LIMIT100001`, `sum(octet_length(id))OVER()<=$2THENidEND`, `&(MAX_BYTESasi64)`, `rows.len()>100000`, `total>MAX_BYTES`, `row.try_get::<_,Option<String>>(0)?.ok_or(Error::Capacity)?`, `tx.rollback().await?;Ok(claims)`} {
		if !strings.Contains(found["src/lib.rs"], marker) {
			return fmt.Errorf("repository lifecycle missing %s", marker)
		}
	}
	for file, markers := range map[string][]string{
		"src/lib.rs":          {`iftenant.is_empty()||tenant.len()>256`, `client.transaction().await?`, `set_config('app.tenant_id',$1,true)`, `rolsuper,rolbypassrls`, `relrowsecurityANDrelforcerowsecurity`, `tables.get::<_,i64>(0)!=5`, `WHEREorders.version=$4`, `ONCONFLICT(tenant_id,fill_id)DONOTHING`, `duplicate(&tx,fill.as_ref().map(|f|&f.0)).await?`, `same_except_fee(&original,&revised)`, `ORDERBYrevision_sequenceDESCLIMIT1`, `producer_serviceISNULL`, `tx.commit().await.map_err(|_|Error::CommitUnconfirmed)?`, `&state`, `&r.payload`, `fill_wire(&r.payload)?`},
		"examples/fixture.rs": {`config.get_hostaddrs().is_empty()`, `s=="127.0.0.1"||s=="localhost"||s=="::1"`, `if!local_config(&config)`, `Duration::from_secs(20)`, `option_env!("KANZ_BUILD_VERSION")`, `std::io::stdin().take(2*1024*1024+1)`, `input.schema.starts_with("oms_store_")`},
	} {
		for _, marker := range markers {
			if !strings.Contains(found[file], marker) {
				return fmt.Errorf("store missing boundary %s: %s", file, marker)
			}
		}
	}
	return nil
}
func TestRustOMSStoreBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if e := checkRustOMSStoreBoundary(root); e != nil {
		t.Fatal(e)
	}
}

func checkRustOMSStoreEvidence(workflow, checker string) error {
	for _, marker := range []string{"TestRustRepositoryLifecycle/", "concurrent-create-one-fact", "create-restart-load-go-save", "create-invalid-fact-rollback", "create-deferred-commit-rollback", "create-tenant-same-id", "applied-fill-raw-aliases", "read-notfound-unavailable-corruption", "claim-capacity-no-prefix", "claim-read-failure-no-prefix", "load-concurrent-version", "read-oversized-bytes"} {
		if !strings.Contains(checker, marker) {
			return fmt.Errorf("missing lifecycle proof %s", marker)
		}
	}
	for _, marker := range []string{"cargo test --locked -p kanz-oms-store --all-targets", "cargo clippy --locked -p kanz-oms-store --all-targets -- -D warnings", "cargo build --locked -p kanz-oms-store --example fixture", "KANZ_OMS_STORE_BIN:", "KANZ_OMS_STORE_VERSION:", "./services/oms/tools/store-conformance", "go test -race -p 1", "check_oms_fold_evidence.py"} {
		if !strings.Contains(workflow, marker) {
			return fmt.Errorf("missing Rust SQL execution gate: %s", marker)
		}
	}
	for _, marker := range []string{"services/oms/tools/store-conformance", "TestExecutionKeyIndependentOracle", "TestRustExecutionIdentity", "TestRustExecutionEconomics", "TestRustRepositoryTransactions/", "durable-restart-exact-bytes", "duplicate-conflict-and-alias", "concurrent-duplicate", "fill-wins-cancel", "cancel-wins-fill", "commit-failure-rollback-reclaim", "tenant-and-missing-scope", "legacy-alias", "fee-head-duplicate-and-corruption", "unowned-outbox-refusal", "unsafe-rls-and-invalid-record"} {
		if !strings.Contains(checker, marker) {
			return fmt.Errorf("missing Rust SQL case: %s", marker)
		}
	}
	return nil
}
func TestRustOMSStoreEvidenceAndMutations(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	w, e := os.ReadFile(filepath.Join(root, ".github/workflows/rust-oms-fold.yml"))
	if e != nil {
		t.Fatal(e)
	}
	c, e := os.ReadFile(filepath.Join(root, ".github/scripts/check_oms_fold_evidence.py"))
	if e != nil {
		t.Fatal(e)
	}
	if e = checkRustOMSStoreEvidence(string(w), string(c)); e != nil {
		t.Fatal(e)
	}
	for _, marker := range []string{"--all-targets", "./services/oms/tools/store-conformance", "KANZ_OMS_STORE_BIN:"} {
		mut := strings.ReplaceAll(string(w), marker, "")
		if checkRustOMSStoreEvidence(mut, string(c)) == nil {
			t.Fatalf("missing workflow gate %s accepted", marker)
		}
	}
	for _, marker := range []string{"legacy-alias", "fee-head-duplicate-and-corruption", "commit-failure-rollback-reclaim"} {
		mut := strings.ReplaceAll(string(c), marker, "")
		if checkRustOMSStoreEvidence(string(w), mut) == nil {
			t.Fatalf("missing case %s accepted", marker)
		}
	}
}
func TestRustOMSStoreBoundaryMutations(t *testing.T) {
	source := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ file, from, to string }{
		{"src/lib.rs", "ON CONFLICT (tenant_id,order_id) DO NOTHING", "ON CONFLICT (tenant_id,order_id) DO UPDATE SET state=EXCLUDED.state"},
		{"src/lib.rs", "END,version FROM orders WHERE order_id=$1", "END,0 FROM orders WHERE order_id=$1"},
		{"src/lib.rs", "COALESCE(NULLIF(raw_fill_id,''),fill_id)", "fill_id"},
		{"src/lib.rs", "rows.len() > 100000", "false"},
		{"src/lib.rs", "octet_length(state)::bigint<=$2", "true"},
		{"src/lib.rs", "sum(octet_length(id)) OVER ()<=$2", "true"},
		{"src/lib.rs", ".try_get::<_, Option<String>>(0)?", ".try_get::<_, Option<String>>(1)?"},
		{"src/lib.rs", "tenant.is_empty()", "false"}, {"src/lib.rs", "set_config('app.tenant_id',$1,true)", "set_config('app.tenant_id',$1,false)"},
		{"src/lib.rs", "relrowsecurity AND relforcerowsecurity", "relrowsecurity"}, {"src/lib.rs", "WHERE orders.version=$4", "WHERE true"},
		{"src/lib.rs", "Error::CommitUnconfirmed", "Error::Database"}, {"src/lib.rs", "same_except_fee(&original, &revised)", "true"},
		{"src/lib.rs", "producer_service IS NULL", "false"}, {"src/lib.rs", "fill_wire(&r.payload)?", "Vec::new()"},
		{"examples/fixture.rs", "config.get_hostaddrs().is_empty()", "true"}, {"examples/fixture.rs", "if !local_config(&config)", "if false"},
	} {
		t.Run(tc.from, func(t *testing.T) {
			dst := t.TempDir()
			for _, file := range append([]string{"Cargo.toml"}, rustOMSStoreSources...) {
				b, e := os.ReadFile(filepath.Join(source, rustOMSStorePath, file))
				if e != nil {
					t.Fatal(e)
				}
				p := filepath.Join(dst, rustOMSStorePath, file)
				if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e := checkRustOMSStoreBoundary(dst); e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(dst, rustOMSStorePath, tc.file)
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			s := string(b)
			if !strings.Contains(s, tc.from) {
				t.Fatal("vacuous mutation")
			}
			if e = os.WriteFile(p, []byte(strings.Replace(s, tc.from, tc.to, 1)), 0600); e != nil {
				t.Fatal(e)
			}
			if checkRustOMSStoreBoundary(dst) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
