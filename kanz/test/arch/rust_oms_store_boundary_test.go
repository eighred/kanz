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

var rustOMSStoreSources = []string{"src/lib.rs", "src/identity.rs", "src/queries.rs", "src/outbox.rs", "src/proposals.rs", "src/position.rs", "src/position_math.rs", "src/recovery.rs", "src/recovery_ack.rs", "src/fee_wire.rs", "src/fee_revision.rs", "src/fee_journal.rs", "src/fee_subject.rs", "src/go_unicode.rs", "examples/fixture.rs", "examples/relay_support/mod.rs", "src/relay.rs", "src/relay_wire.rs"}

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
prost-reflect = "=0.16.2"
uuid = { version = "=1.29.0", features = ["v7"] }
sha2 = "=0.10.9"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"
tokio-postgres = "=0.7.18"
tokio = { version = "=1.54.1", features = ["rt", "time"] }
[dev-dependencies]
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "time"] }
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
base64 = "=0.22.1"
async-nats = { version = "=0.51.0", default-features = false, features = ["ring", "jetstream", "nuid"] }`

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
		if rel != "examples/fixture.rs" && rel != "examples/relay_support/mod.rs" {
			if rel == "src/relay.rs" {
				s = strings.ReplaceAll(s, "self.sessions.connect(", "connector(")
				s = strings.ReplaceAll(s, "self.publisher.publish(", "acknowledge(")
			}

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
	if strings.Count(found["src/queries.rs"], `scoped(client,tenant).await?`) != 4 {
		return fmt.Errorf("query owners must reuse scope")
	}
	for _, marker := range []string{`rows.len()>MAX_ROWS`, `constMAX_ROWS:usize=100000`, `total>MAX_BYTES`, `row.try_get::<_,Option<Vec<u8>>>(0)?.ok_or(Error::Capacity)?`, `sum(octet_length(state))OVER()<=$1THENstateEND`, `sum(octet_length(state))OVER()<=$2THENstateEND`, `sum(octet_length(state))OVER()<=$3THENstateEND`, `LIMIT100001`, `status=ANY($1)`, `parent_order_id=$1ANDparent_order_id<>''`, `ORDERBYorder_id`, `ORDERBYcreated_atDESCLIMIT$2`, `SELECTcount(*)FROMordersWHEREportfolio_id=''`, `OrderState::decode(raw.as_slice())`, `result.push(raw)`} {
		if !strings.Contains(found["src/queries.rs"], marker) {
			return fmt.Errorf("query boundary missing %s", marker)
		}
	}
	if strings.Count(found["src/fee_wire.rs"], `raw.len()>MAX_BYTES`) != 2 {
		return fmt.Errorf("fee input/output bounds required")
	}
	for file, markers := range map[string][]string{
		"src/fee_wire.rs":    {`FileDescriptorSet::decode(FILE_DESCRIPTOR_SET)`, `raw.len()>MAX_BYTES`, `self.value==other.value&&self.unknown.equivalent(&other.unknown)`, `grouped(&self.raw)!=grouped(&other.raw)`, `self.raw.extend(other.raw)`, `Type::Sint64|Type::Sint32`, `f.label()!=prost_types::field_descriptor_proto::Label::Optional`, `!d.oneof_decl.is_empty()`, `unknown.children.entry(n).or_default().merge(sidecar)`, `stored.extend(&raw[value_start..pos])`, `original:raw.to_vec()`},
		"src/fee_subject.rs": {`SPACE.binary_search`, `LOWER.binary_search_by_key`, `for scalar in 0..=0x10ffff`},
	} {
		for _, marker := range markers {
			if !strings.Contains(found[file], rustMigrationCompact(marker)) {
				return fmt.Errorf("fee compatibility missing %s", marker)
			}
		}
	}
	if strings.Count(found["src/fee_journal.rs"], `tx:&'txmutTransaction<'connection>`) != 2 || strings.Count(found["src/fee_journal.rs"], `identity::same_except_fee(original.value(),revised.value())`) != 3 {
		return fmt.Errorf("fee token must exclusively borrow caller transaction and validate both head/append economics")
	}
	for _, marker := range []string{`validate_batch(state,records,64,MAX_BYTES)`, `message_field(wire,2)`, `claim(&tx,&st.order_id,&key,fill_id).await?`, `journal(&tx,&st.order_id,fill_id,&fill,&raw).await?`} {
		if !strings.Contains(found["src/lib.rs"], marker) {
			return fmt.Errorf("shared repository primitive missing %s", marker)
		}
	}
	for _, marker := range []string{`fnmetadata_and_posted_share_one_byte_budget()`, `constMAX_FILLS:usize=8000`, `constMAX_BATCH:usize=4*MAX_BYTES`, `MAX_BATCH.checked_sub(extra).ok_or(Error::Capacity)?`, `validate_batch(&batch.state,&batch.records,MAX_FILLS+1,budget)`, `r.tenant!=tenant`, `r.event_class!=1`, `r.payload_schema_ref!=schema`, `r.event_time!=at`, `status.checkpointasusize!=count`, `message_field(&r.payload,2)?!=batch.state`, `FillWire::decode(&message_field(&r.payload,1)?)?`, `fill.without_recovery()?.journal_bytes()?`, `p.payload_digest!=batch.history_digest`, `missing.len()+posted.len()!=fills.len()`, `tables.get::<_,i64>(0)!=3`, `relrowsecurityANDrelforcerowsecurity`, `claims.sort_by(|a,b|a.value().fill_id.cmp(&b.value().fill_id))`, `claim(&tx,&st.order_id,&identity::execution_key(f),&f.fill_id).await?`, `duplicate(&tx,Some(f)).await?`, `returnErr(Error::Conflict)`, `journal(&tx,&st.order_id,&f.fill_id,f,&fill.journal_bytes()?).await?`, `fee_journal::cas_order(&muttx,tenant,&batch.state,batch.order_version).await?`, `locked.record_fee_revision(posted.get(key).ok_or(Error::FeeRevision)?,fill).await?`, `version=$7ANDstatus='investigating'ANDcheckpoint=0`, `ifchanged!=1`, `INSERTINTOexecution_recovery_targets`, `enqueue(&tx,&batch.records).await?`, `commit(tx).await`} {
		if !strings.Contains(found["src/recovery.rs"], marker) {
			return fmt.Errorf("recovery durable boundary missing %s", marker)
		}
	}
	for _, marker := range []string{`pubenumBook{Ledger,Position,}`, `scoped(client,tenant).await?`, `recovery::fact(`, `status.case_id!=ack.case_id`, `status.status!="corrected"`, `tables.get::<_,i64>(0)!=3`, `FORUPDATE`, `octet_length(state)::bigint<=$2`, `execution_digest=$4`, `ONCONFLICTDONOTHING`, `received!=i64::from(expected)`, `expected!=checkpoint*2`, `targets!=i64::from(checkpoint)`, `status.portfolio_id!=state.portfolio_id`, `enqueue(&tx,std::slice::from_ref(corrected)).await?`, `commit(tx).await`} {
		if !strings.Contains(found["src/recovery_ack.rs"], marker) {
			return fmt.Errorf("recovery acknowledgement boundary missing %s", marker)
		}
	}
	for _, forbidden := range []string{".transaction(", ".commit(", ".rollback("} {
		if strings.Contains(found["src/fee_journal.rs"], forbidden) {
			return fmt.Errorf("fee primitive owns caller transaction lifecycle")
		}
	}
	for file, markers := range map[string][]string{
		"src/fee_journal.rs":  {`record_for(self.tx,Book::Oms,original,revised).await`, `head_for(tx,Book::Oms,original).await`, `WHEREbook=$4ANDexecution_key=$1ANDcase_id=$2`, `WHEREbook=$3ANDexecution_key=$1ORDERBY`, `tx:&'txmutTransaction<'connection>`, `scope_existing(tx,tenant).await?`, `cas(tx,&st,state,expected_version).await?`, `self.scoped().await?`, `current.as_deref()!=Some(self.tenant.as_str())`, `if!recorded.equivalent(revised)`, `delta:BigRational::zero()`, `fresh:false`, `fee_revision::approved_delta(&previous,revised)?`, `previous.journal_bytes()?`, `revised.journal_bytes()?`, `ORDERBYrevision_sequenceDESCLIMIT1`, `identity::same_except_fee(original.value(),revised.value())`, `octet_length(revised_fill)::bigint<=$2`, `octet_length(revised_fill)::bigint<=$3`, `validate_write(&self.state,records)?`, `enqueue(self.tx,records).await`},
		"src/fee_revision.rs": {`!(-64..=64).contains(&d.exponent)`, `hash.update((part.len()asu64).to_be_bytes())`, `normalize(&a.proposer)==normalize(&a.approver)`, `economic_digest(previous.value()).map_err(|_|Error::FeeRevision)?!=a.previous_execution_digest`, `identity::same_execution(previous.value(),approved.value())`, `amount(previous.value())?-amount(revised.value())?`},
	} {
		for _, marker := range markers {
			if !strings.Contains(found[file], marker) {
				return fmt.Errorf("fee transaction missing %s: %s", file, marker)
			}
		}
	}
	for file, markers := range map[string][]string{
		"src/lib.rs":          {`iftenant.is_empty()||tenant.len()>256`, `client.transaction().await?`, `set_config('app.tenant_id',$1,true)`, `rolsuper,rolbypassrls`, `relrowsecurityANDrelforcerowsecurity`, `tables.get::<_,i64>(0)!=5`, `WHEREorders.version=$4`, `ONCONFLICT(tenant_id,fill_id)DONOTHING`, `duplicate(&tx,fill.as_ref().map(|f|&f.0)).await?`, `fee_journal::head(tx,&original).await?`, `producer_serviceISNULL`, `tx.commit().await.map_err(|_|Error::CommitUnconfirmed)?`, `&state`, `&r.payload`, `fill_wire(&r.payload)?`},
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
	for _, marker := range []string{"TestRustRecoveryAcknowledgements/", "go-differential-both-books-restart", "concurrent-last-ack-one-fact", "target-and-record-binding", "outbox-and-commit-rollback", "tenant-same-case-positive", "missing-corrupt-and-unsafe-storage", "corrupt-counts-refuse-before-claim", "TestRustRecoveryTransactions/", "go-differential-terminal-restart", "zero-history-corrected", "approved-fee-go-differential", "mandatory-facts-and-provenance", "case-cas-rollback-all-owners", "fee-and-claims-rollback", "order-cas-rollback", "unsafe-recovery-rls", "order-cas-and-push-race", "concurrent-recovery-one-checkpoint", "target-outbox-commit-rollback", "tenant-same-case-and-execution", "TestFeeEconomicIndependentOracle", "TestRustFeeEconomicCompatibility", "TestRustFeeTermsCompatibility", "TestRustFeeJournalTransactions/", "commit-restart-aba-replay", "case-conflict-stale-head", "concurrent-cas-one-revision", "explicit-rollback", "outbox-failure-atomic", "deferred-commit-atomic", "tenant-same-execution-case", "other-book-not-oms-head", "corrupt-bounded-head", "scope-and-order-refusal", "TestPinnedGoUnicodeTable", "TestRustFeeUnicodeCompatibility", "TestRustFeeWireCompatibility/", "TestRustFeeWireRefusals/", "unknown-value-encoding", "duplicate-embedded-unknown", "known-wrong-wire-is-unknown", "TestRustRepositoryQueries/", "list-status-parent-exact", "portfolio-page-count-backfill", "tenant-same-ids", "corruption-capacity-no-prefix", "row-cap-no-prefix", "canonical-index-plans", "sql-failure-no-prefix", "scope-selector-refusal", "unavailable-no-result", "TestRustRepositoryLifecycle/", "concurrent-create-one-fact", "create-restart-load-go-save", "create-invalid-fact-rollback", "create-deferred-commit-rollback", "create-tenant-same-id", "applied-fill-raw-aliases", "read-notfound-unavailable-corruption", "claim-capacity-no-prefix", "claim-read-failure-no-prefix", "load-concurrent-version", "read-oversized-bytes"} {
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
	for _, marker := range []string{"legacy-alias", "fee-head-duplicate-and-corruption", "commit-failure-rollback-reclaim", "concurrent-cas-one-revision", "deferred-commit-atomic", "TestRustFeeTermsCompatibility"} {
		mut := strings.ReplaceAll(string(c), marker, "")
		if checkRustOMSStoreEvidence(string(w), mut) == nil {
			t.Fatalf("missing case %s accepted", marker)
		}
	}
}
func TestRustOMSStoreBoundaryMutations(t *testing.T) {
	source := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ file, from, to string }{
		{"src/recovery_ack.rs", "status.case_id != ack.case_id", "false"},
		{"src/recovery_ack.rs", "status.status != \"corrected\"", "false"},
		{"src/recovery_ack.rs", "tables.get::<_, i64>(0) != 3", "false"},
		{"src/recovery_ack.rs", "received != i64::from(expected)", "false"},
		{"src/recovery_ack.rs", "targets != i64::from(checkpoint)", "false"},
		{"src/recovery_ack.rs", "status.portfolio_id != state.portfolio_id", "false"},
		{"src/recovery_ack.rs", "enqueue(&tx, std::slice::from_ref(corrected)).await?", "Ok::<(), Error>(())?"},
		{"src/recovery.rs", "MAX_BATCH.checked_sub(extra)", "MAX_BATCH.checked_sub(0)"},
		{"src/recovery.rs", "r.tenant != tenant", "false"},
		{"src/recovery.rs", "r.event_class != 1", "false"},
		{"src/recovery.rs", "status.checkpoint as usize != count", "false"},
		{"src/recovery.rs", "message_field(&r.payload, 2)? != batch.state", "false"},
		{"src/recovery.rs", "p.payload_digest != batch.history_digest", "false"},
		{"src/recovery.rs", "missing.len() + posted.len() != fills.len()", "false"},
		{"src/recovery.rs", "tables.get::<_, i64>(0) != 3", "false"},
		{"src/recovery.rs", "claims.sort_by(|a, b| a.value().fill_id.cmp(&b.value().fill_id))", "claims.reverse()"},
		{"src/recovery.rs", "version=$7 AND status='investigating' AND checkpoint=0", "version=$7"},
		{"src/recovery.rs", "enqueue(&tx, &batch.records).await?", "Ok::<(), Error>(())?"},
		{"src/fee_journal.rs", "tx: &'tx mut Transaction<'connection>", "tx: &'tx Transaction<'connection>"},
		{"src/fee_journal.rs", "scope_existing(tx, tenant).await?", "Ok::<(), Error>(())?"},
		{"src/fee_journal.rs", "cas(tx, &st, state, expected_version).await?", "Ok::<(), Error>(())?"},
		{"src/fee_journal.rs", "if !recorded.equivalent(revised)", "if false"},
		{"src/fee_journal.rs", "octet_length(revised_fill)::bigint<=$3", "true"},
		{"src/fee_journal.rs", "octet_length(revised_fill)::bigint<=$2", "true"},
		{"src/fee_revision.rs", "normalize(&a.proposer) == normalize(&a.approver)", "false"},
		{"src/fee_revision.rs", "identity::same_execution(previous.value(), approved.value())", "true"},
		{"src/queries.rs", "parent_order_id=$1 AND parent_order_id<>''", "parent_order_id=$1"},
		{"src/queries.rs", "rows.len() > MAX_ROWS", "false"},
		{"src/fee_wire.rs", "self.unknown.equivalent(&other.unknown)", "true"},
		{"src/fee_wire.rs", "stored.extend(&raw[value_start..pos])", "stored.clear()"},
		{"src/fee_wire.rs", "raw.len() > MAX_BYTES", "false"},
		{"src/fee_subject.rs", "LOWER.binary_search_by_key", "SPACE.binary_search_by_key"},
		{"src/queries.rs", "sum(octet_length(state)) OVER ()<=$1", "true"},
		{"src/queries.rs", "total > MAX_BYTES", "false"},
		{"src/queries.rs", "status=ANY($1)", "true"},
		{"src/queries.rs", "result.push(raw)", "result.push(Vec::new())"},
		{"src/lib.rs", "ON CONFLICT (tenant_id,order_id) DO NOTHING", "ON CONFLICT (tenant_id,order_id) DO UPDATE SET state=EXCLUDED.state"},
		{"src/lib.rs", "END,version FROM orders WHERE order_id=$1", "END,0 FROM orders WHERE order_id=$1"},
		{"src/lib.rs", "COALESCE(NULLIF(raw_fill_id,''),fill_id)", "fill_id"},
		{"src/lib.rs", "rows.len() > 100000", "false"},
		{"src/lib.rs", "octet_length(state)::bigint<=$2", "true"},
		{"src/lib.rs", "sum(octet_length(id)) OVER ()<=$2", "true"},
		{"src/lib.rs", ".try_get::<_, Option<String>>(0)?", ".try_get::<_, Option<String>>(1)?"},
		{"src/lib.rs", "tenant.is_empty()", "false"}, {"src/lib.rs", "set_config('app.tenant_id',$1,true)", "set_config('app.tenant_id',$1,false)"},
		{"src/lib.rs", "relrowsecurity AND relforcerowsecurity", "relrowsecurity"}, {"src/lib.rs", "WHERE orders.version=$4", "WHERE true"},
		{"src/lib.rs", "Error::CommitUnconfirmed", "Error::Database"}, {"src/fee_journal.rs", "identity::same_except_fee(original.value(), revised.value())", "true"},
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
