package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exact source ownership/marker checks, not a Rust data-flow or financial proof.
const rustRecoveryCargo = `[package]
name = "kanz-oms-recovery"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[features]
default = []
qualification-diagnostics = ["kanz-oms-store/qualification-diagnostics"]

[dependencies]
kanz-order-core = { path = "../order-core" }
kanz-oms-store = { path = "../oms-store" }
kanz-oms-venue = { path = "../oms-venue" }
kanz-proto-wire = { path = "../proto-wire" }
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.4"
prost-types = "=0.14.4"
prost-reflect = "=0.16.5"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"
sha2 = "=0.10.9"
tokio = { version = "=1.54.1", features = ["macros", "rt", "time", "sync"] }
tokio-postgres = "=0.7.18"

[dev-dependencies]
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "time", "io-std", "io-util"] }
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
base64 = "=0.22.1"
async-nats = { version = "=0.51.0", default-features = false, features = ["ring", "jetstream", "nuid"] }
rustls = { version = "=0.23.46", default-features = false, features = ["ring", "std", "tls12"] }
http = "=1.5.0"
`

var recoveryMarkers = map[string][]string{
	"src/coordinator.rs":  {`Client::connect_until(config.venue,receiver)`, `verified_account:true`, `declared_order_types:true`, `measure(Stage::HandlerClose,session.close()).await`, `letexpires=tokio::time::Instant::now()+deadline`, `tokio::time::timeout_at(expires,measure(Stage::HandlerConnect,self.sessions.connect()),)`, `tokio::time::timeout_at(expires,async`, `tokio::time::Instant::now()>=expires`, `self.relay.flush_before(&key,expires)`, `outcome.stalled.is_none()`},
	"src/observation.rs":  {`repository::observe(`, `repository::mapping_matches(`, `latest.version!=loaded.version`, `venue.describe(deadline)`, `venue.query_recovery_history(`, `history::complete(`, `repository::freeze(`, `preparation::posted(`, `approval::approved(`, `recovery::commit_streamed_recovery(`, `id.account_verified`, `Error::Conflict`},
	"src/preparation.rs":  {`recovery_repository::posted_reader(`, `cursor.finish().await?`, `identity::same_execution(row.current.value(),previous.value())`, `aggregate::apply_recovered_fill(`, `aggregate::recovery_average(`},
	"src/approval.rs":     {`actual!=evidence.stored_digest`, `fee_control::Control::new(p)?`, `control.approve(&a.approver,&a.digest,approved)`, `p.history_digest!=history_digest`, `p.mapping_version!=case.mapping_version`},
	"src/fee_control.rs":  {`letttl=(expiry-created).min(i128::from(i64::MAX))`, `actor==fee_subject::normalize(self.proposer)`, `digest!=self.digest`, `now>=self.expires`, `if!(n==number&&w==2)`, `h.update((part.len()asu64).to_be_bytes())`},
	"src/fee_commands.rs": {`env.value().tenant_id!=tenant`, `env.value().event_class!=2`, `payload.len()>65536`, `m.target_id!=order`, `m.issuer.strip_prefix("user:")`, `!m.principal_portfolios.contains(&state.value().portfolio_id)`, `preparation::posted_with(`, `previous.journal_bytes()?`, `used>repository::PROPOSAL_BYTES`, `control.approve(&old.approver,&old.digest,signed)`, `observation::prepared(`},
	"src/handlers.rs":     {`env.value().tenant_id!=tenant`, `env.value().event_class!=1`, `recovery_ack::acknowledge_recovery(`, `repository::target_matches(`},
	"src/history.rs":      {`fills.len()>MAX_FILLS`, `sum!=total`, `f.recovery.is_some()`, `decimal_domain(&dynamic,0)?`},
}

func checkRustRecoveryCoordinator(root string) error {
	dir := filepath.Join(root, "crates", "oms-recovery")
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(raw)) != rustMigrationCompact(rustRecoveryCargo) {
		return fmt.Errorf("unreviewed recovery manifest")
	}
	allowed := map[string]bool{"src/lib.rs": true, "src/coordinator.rs": true, "src/observation.rs": true, "src/preparation.rs": true, "src/approval.rs": true, "src/handlers.rs": true, "src/history.rs": true, "src/fee_commands.rs": true, "src/fee_control.rs": true, "examples/recovery-coordinator-fixture.rs": true}
	if err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("recovery source symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if filepath.Ext(rel) == ".rs" && !allowed[rel] {
			return fmt.Errorf("unreviewed recovery source %s", rel)
		}
		if filepath.Ext(rel) == ".proto" || d.Name() == "build.rs" || d.Name() == "config.toml" {
			return fmt.Errorf("parallel recovery model/config")
		}
		return nil
	}); err != nil {
		return err
	}
	for name := range allowed {
		raw, e := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if e != nil {
			return e
		}
		code, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			return e
		}
		code = rustMigrationCompact(code)
		for _, marker := range recoveryMarkers[name] {
			if !strings.Contains(code, marker) {
				return fmt.Errorf("recovery %s missing %s", name, marker)
			}
		}
		if strings.HasPrefix(name, "src/") {
			for _, forbidden := range []string{".execute_response(", ".cancel_order(", "AllowAll", "unsafe{", "f64", "f32", "std::env::"} {
				if strings.Contains(code, forbidden) {
					return fmt.Errorf("recovery authority/parallel owner %s", forbidden)
				}
			}
		}
	}
	return nil
}
func TestRustRecoveryCoordinatorBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if e := checkRustRecoveryCoordinator(root); e != nil {
		t.Fatal(e)
	}
}
func TestRustRecoveryCoordinatorMarkerMutations(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-recovery")
	for path, markers := range recoveryMarkers {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if e != nil {
			t.Fatal(e)
		}
		code, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			t.Fatal(e)
		}
		code = rustMigrationCompact(code)
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				changed := strings.ReplaceAll(code, marker, "removed")
				if changed == code || strings.Contains(changed, marker) {
					t.Fatal("required boundary mutation was not discriminating")
				}
			})
		}
	}
}

// The closed budget remains shared; these markers bind each approved caller
// and keep the ordinary decoder/RPC path at its original one-MiB default.
var recoverySharedMarkers = map[string][]string{
	"crates/oms-recovery/examples/recovery-coordinator-fixture.rs": {`ifsubject!="order.order.execution_recovered"{returnfalse;}`, `count_recovered_ack(&frame.subject,&mutself.acknowledgements,self.hold_after)`, `*acknowledgements+=1`, `status_ack_does_not_consume_recovered_hold`},
	"crates/oms-store/src/relay.rs":                                {`outbox::lock_key_before(session,&self.tenant,key,expires)`, `self.under_lock(lock.client(),key,&mutconfirmed,Some(expires))`, `measure(Stage::RelayClose,lock.close()).await`, `*confirmed=sent`, `work_remaining(expires,sent)?`, `Ok(frame)=>{work_remaining(expires,sent)?;tokio::time::timeout`, `sent:confirmed,error:Error::PublicationUnconfirmed`},
	"crates/oms-store/src/outbox.rs":                               {`acquire_key(&mutsession,tenant,key,true)`, `tokio::time::Instant::now()>=expires`, `session.close().await`, `self.session.close().await`},
	"crates/proto-wire/src/lib.rs":                                 {`pubenumBudget{Standard,RecoveryHistory,}`, `Self::Standard=>MAX_BYTES`, `Self::RecoveryHistory=>4*MAX_BYTES`, `normalize_with_budget(desc,raw,Budget::Standard)`, `raw.len()>limit`, `value.encoded_len()>limit`, `out.len().checked_add(raw.len()).is_none_or(|n|n>limit)`, `ifdepth>32`},
	"crates/proto-wire/src/canonical.rs":                           {`Self::decode_with_budget(raw,Budget::Standard)`, `value.encoded_len()>MAX_BYTES`},
	"crates/oms-venue/src/client.rs":                               {`self.call_budget(method,value,deadline,kanz_proto_wire::Budget::Standard)`, `method!="/venue.v1.VenueAdapterService/QueryOrder"`, `B::NAME!="venue.v1.QueryOrderResponse"`, `max_encoding_message_size(kanz_proto_wire::MAX_BYTES)`},
	"crates/oms-venue/src/wire.rs":                                 {`A::NAME!="venue.v1.QueryOrderResponse"`, `B::NAME!="order.v1.Fill"`, `name!="fills"`, `src.remaining()>self.1.bytes()`},
	"crates/oms-store/src/recovery_repository.rs":                  {`IsolationLevel::RepeatableRead`, `.read_only(true)`, `ORDERBYfill_idLIMIT1`, `octet_length(fill)::bigint<=$3`, `f.fill_id.len()>1024`, `!self.seen.insert(key)`, `self.remaining!=0`, `!self.seen.contains(&key)`},
	"crates/oms-store/src/recovery.rs":                             {`validate_resident(&batch.executions)?`, `validate_missing_keys(&batch.missing)?`, `key.len()>256`, `Batch::Streamed(_)=>locked.execution_head(&fill).await?`, `letrecord=batch.record(index)?`, `enqueue(&tx,std::slice::from_ref(record.as_ref())).await?`, `commit(tx).await`},
	"crates/oms-store/src/fee_journal.rs":                          {`octet_length(fill)::bigint<=$6`, `head(self.tx,&original).await`, `self.scoped().await?`},
}

func checkRecoveryShared(files map[string]string) error {
	for path, markers := range recoverySharedMarkers {
		code, e := rustMigrationWithoutComments(files[path])
		if e != nil {
			return e
		}
		code = rustMigrationCompact(code)
		for _, marker := range markers {
			if !strings.Contains(code, marker) {
				return fmt.Errorf("recovery shared %s missing %s", path, marker)
			}
		}
	}
	return nil
}
func TestRustRecoveryClosedBudgetAndStreamingBoundaries(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	files := map[string]string{}
	for path := range recoverySharedMarkers {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if e != nil {
			t.Fatal(e)
		}
		files[path] = string(raw)
	}
	if e := checkRecoveryShared(files); e != nil {
		t.Fatal(e)
	}
	for path, markers := range recoverySharedMarkers {
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				original := files[path]
				code, e := rustMigrationWithoutComments(original)
				if e != nil {
					t.Fatal(e)
				}
				code = rustMigrationCompact(code)
				files[path] = strings.ReplaceAll(code, marker, "removed")
				defer func() { files[path] = original }()
				if e := checkRecoveryShared(files); e == nil {
					t.Fatal("shared boundary mutation accepted")
				}
			})
		}
	}
}
