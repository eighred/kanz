package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkRustOMSRelay(files map[string]string) error {
	required := map[string][]string{
		"relay.rs":      {`r.event_class!=EventClass::Factasi32`, `r.schema_version<=0`, `r.tenant.is_empty()`, `DescriptorPool::decode(FILE_DESCRIPTOR_SET)`, `crate::relay_wire::normalize(desc,&r.payload)`, `idempotency_key:id.clone()`, `tenant_id:r.tenant.clone()`, `self.under_lock(lock.client(),key).await`, `lock.release().await`, `session.close().await`, `self.drain_key(&key,false).await`, `self.drain_key(key,true).await`, `stalled:full.then_some(Stall::FullBatch)`, `Stall::Publication(failure)`, `self.publisher.publish(&frame)`, `outbox::mark_published(client,&self.tenant,p.id)`, `outbox::mark_failed(client,&self.tenant,p.id,Some(failure.cause()))`, `Duration::from_secs(10)`, `ifself.stopped()`, `while!self.stopped()`},
		"relay_wire.rs": {`kanz_proto_wire::normalize(desc,raw).map_err(Into::into)`},
		"shared_wire":   {`DynamicMessage::decode(desc.clone(),known.as_slice())`, `unknown.raw.extend_from_slice(&raw[value..pos])`, `result.extend_from_slice(&unknown.raw)`, `entries[0].merge(sidecar,&child)?`, `field.is_list()||entries.is_empty()`, `ifdepth>32`, `raw.len()>MAX_BYTES`},
		"fixture":       {`strip_prefix("nats://127.0.0.1:")`, `"Nats-Msg-Id"`, `"Kanz-Partition-Key"`, `stopper.await`, `ConnectingDriver(Some(tokio::spawn`, `driver.0.take().ok_or(Error::Database)?`, `config.application_name(&application)`, `SELECTcount(*)FROMpg_stat_activityWHEREapplication_name=$1`, `result["sessions_closed"]=json!(true)`, `nc.drain()`, `publish_with_headers(frame.subject.clone(),headers,frame.body.clone().into())`},
	}
	for file, markers := range required {
		code, e := rustMigrationWithoutComments(files[file])
		if e != nil {
			return e
		}
		code = rustMigrationCompact(code)
		for _, marker := range markers {
			if !strings.Contains(code, marker) {
				return fmt.Errorf("relay %s missing %s", file, marker)
			}
		}
		if file == "relay.rs" && strings.Index(code, "self.publisher.publish(&frame)") > strings.LastIndex(code, "outbox::mark_published(client,&self.tenant,p.id)") {
			return fmt.Errorf("mark precedes publish")
		}
	}
	return nil
}
func TestRustOMSRelayBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store")
	files := map[string]string{}
	for name, path := range map[string]string{"relay.rs": "src/relay.rs", "relay_wire.rs": "src/relay_wire.rs", "shared_wire": "../proto-wire/src/lib.rs", "fixture": "examples/relay_support/mod.rs"} {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if e != nil {
			t.Fatal(e)
		}
		files[name] = string(raw)
	}
	if e := checkRustOMSRelay(files); e != nil {
		t.Fatal(e)
	}
	for _, m := range []struct{ name, file, from, to string }{
		{"fact-class", "relay.rs", "r.event_class != EventClass::Fact as i32", "false"},
		{"tenant", "relay.rs", "r.tenant.is_empty()", "false"},
		{"canonical-payload", "relay.rs", "crate::relay_wire::normalize(desc, &r.payload)", "Ok(r.payload.clone())"},
		{"locked-read", "relay.rs", "self.under_lock(lock.client(), key).await", "Ok(Outcome { sent: 0, stalled: None })"},
		{"join-session", "relay.rs", "session.close().await", "drop(session)"},
		{"full-batch", "relay.rs", "full.then_some(Stall::FullBatch)", "None"},
		{"published-mark", "relay.rs", "outbox::mark_published(client, &self.tenant, p.id)", "Ok::<(),Error>(())"},
		{"failed-mark", "relay.rs", "outbox::mark_failed(client, &self.tenant, p.id, Some(failure.cause()))", "Ok::<(),Error>(())"},
		{"raw-unknown", "shared_wire", "unknown.raw.extend_from_slice(&raw[value..pos])", "unknown.raw.clear()"},
		{"nested-unknown", "shared_wire", "entries[0].merge(sidecar, &child)?", "drop(sidecar)"},
		{"local-broker", "fixture", "nats://127.0.0.1:", "nats://"},
		{"partition-header", "fixture", "Kanz-Partition-Key", "Wrong-Partition"},
	} {
		t.Run(m.name, func(t *testing.T) {
			before := files[m.file]
			after := strings.ReplaceAll(before, m.from, m.to)
			if after == before {
				t.Fatal("mutation missed")
			}
			files[m.file] = after
			defer func() { files[m.file] = before }()
			if checkRustOMSRelay(files) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRustOMSRelayActualEvidenceRequired(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	workflow, e := os.ReadFile(filepath.Join(root, ".github", "workflows", "rust-oms-fold.yml"))
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := os.ReadFile(filepath.Join(root, ".github", "scripts", "check_oms_fold_evidence.py"))
	if e != nil {
		t.Fatal(e)
	}
	check := func(w, c string) bool {
		for _, m := range []string{"nats:2.12.6-alpine@sha256:1cfc36e2e5e638243d8c722f72c954cd0ec4b15ee82fadbc718ce12e2b3c1652", "TEST_NATS_URL: nats://127.0.0.1:4222", "set -o pipefail", "./services/oms/tools/store-conformance", "check_oms_fold_evidence.py", "docker rm -f oms-relay-nats"} {
			if !strings.Contains(w, m) {
				return false
			}
		}
		for _, m := range []string{"TestRustRelayFrameCompatibility", "TestRustRelayPostgresNATS", "publish-mark-rollback-redelivery", "stop-joins-inflight", "competing-go-rust"} {
			if !strings.Contains(c, m) {
				return false
			}
		}
		return true
	}
	if !check(string(workflow), string(evidence)) {
		t.Fatal("actual relay proof is not mandatory")
	}
	for _, m := range []string{"TEST_NATS_URL: nats://127.0.0.1:4222", "set -o pipefail", "docker rm -f oms-relay-nats"} {
		if check(strings.ReplaceAll(string(workflow), m, "removed"), string(evidence)) {
			t.Fatal("workflow mutation accepted")
		}
	}
	for _, m := range []string{"TestRustRelayFrameCompatibility", "publish-mark-rollback-redelivery", "stop-joins-inflight"} {
		if check(string(workflow), strings.ReplaceAll(string(evidence), m, "removed")) {
			t.Fatal("missing actual outcome accepted")
		}
	}
}
