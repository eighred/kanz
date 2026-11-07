package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These two fresh-session fixture operations use explicit PostgreSQL TEXT
// parameters to avoid a separate prepare exchange. This does not prove latency;
// the unchanged full recovery fixture remains the acceptance test.
func recoveryFixtureTypedQueries(source string) bool {
	compact := strings.Join(strings.Fields(source), "")
	connectorStart := strings.Index(compact, "implSessionsforConnector{")
	connectorEnd := strings.Index(compact, "structBroker{")
	closedStart := strings.Index(compact, "asyncfnclosed(")
	closedEnd := strings.Index(compact, "asyncfnrun()")
	if connectorStart < 0 || connectorEnd <= connectorStart || closedStart < 0 || closedEnd <= closedStart {
		return false
	}
	connector := compact[connectorStart:connectorEnd]
	closed := compact[closedStart:closedEnd]
	for _, body := range []string{connector, closed} {
		if strings.Count(body, ".query_typed_one(") != 1 || strings.Contains(body, ".query_one(") || strings.Contains(body, ".prepare(") {
			return false
		}
	}
	for _, marker := range []string{
		`self.config.connect(NoTls).await?`,
		`Session::new(`,
		`session.client().query_typed_one("SELECTset_config('search_path',$1,false),set_config('statement_timeout','30000',false)",&[(&self.schema,Type::TEXT)]).await`,
		`ifconfigured.is_err(){session.close().await;returnErr(Error::Database);}`,
	} {
		if !strings.Contains(connector, marker) {
			return false
		}
	}
	for _, marker := range []string{
		`tokio::time::timeout(Duration::from_secs(3),async{loop{`,
		`observer.query_typed_one("SELECTcount(*)FROMpg_stat_activityWHEREapplication_name=$1",&[(&application,Type::TEXT)],).await.map_err(|_|())?.get(0)`,
		`ifcount==0{returnOk(());}`,
		`tokio::time::sleep(Duration::from_millis(10)).await;`,
	} {
		if !strings.Contains(closed, marker) {
			return false
		}
	}
	return true
}

func TestRustRecoveryFixtureTypedQueries(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "kanz-rs", "crates", "oms-recovery", "examples", "recovery-coordinator-fixture.rs"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if !recoveryFixtureTypedQueries(source) {
		t.Fatal("fixture setup/absence must retain their SQL, TEXT parameters, fresh session and cleanup checks")
	}
	for _, mutation := range []struct{ old, replacement string }{
		{`session.client().query_typed_one(`, `session.client().query_one(`},
		{`.query_typed_one(` + "\n", `.query_one(` + "\n"},
		{`(&self.schema, Type::TEXT)`, `(&self.schema, Type::VARCHAR)`},
		{`(&application, Type::TEXT)`, `(&application, Type::VARCHAR)`},
		{`application_name=$1`, `application_name<>$1`},
		{`'statement_timeout','30000'`, `'statement_timeout','60000'`},
		{`self.config.connect(NoTls).await?`, `reuse_existing_session().await?`},
		{`session.close().await;`, `drop(session);`},
		{`if count == 0`, `if count >= 0`},
		{`Duration::from_secs(3)`, `Duration::from_secs(30)`},
	} {
		changed := strings.Replace(source, mutation.old, mutation.replacement, 1)
		if changed == source {
			t.Fatalf("mutation did not apply: %s", mutation.old)
		}
		if recoveryFixtureTypedQueries(changed) {
			t.Fatalf("mutation accepted: %s", mutation.old)
		}
	}
}

func recoveryFixtureDelay(source, workflow string) bool {
	compact := strings.Join(strings.Fields(source), "")
	return strings.Contains(compact, "asyncfnrequested_session_delay(delay:Duration){if!delay.is_zero(){tokio::time::sleep(delay).await;}}") &&
		strings.Contains(compact, "requested_session_delay(self.delay).await;let(client,connection)=self.config.connect(NoTls).await?;") &&
		strings.Contains(compact, "fnzero_requested_delay_is_ready_without_runtime()") &&
		strings.Contains(compact, "assert!(old.is_err());") &&
		strings.Contains(workflow, "cargo test --locked -p kanz-oms-recovery --all-targets\n")
}
func TestRustRecoveryFixtureZeroDelay(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "kanz-rs/crates/oms-recovery/examples/recovery-coordinator-fixture.rs"))
	if err != nil {
		t.Fatal(err)
	}
	flow, err := os.ReadFile(filepath.Join(root, ".github/workflows/rust-oms-fold.yml"))
	if err != nil {
		t.Fatal(err)
	}
	source, workflow := string(raw), strings.ReplaceAll(string(flow), "\r\n", "\n")
	if !recoveryFixtureDelay(source, workflow) {
		t.Fatal("zero-delay timer refusal or native qualification missing")
	}
	for _, mutation := range []struct{ old, replacement string }{
		{"if !delay.is_zero()", "if true"}, {"if !delay.is_zero()", "if false"},
		{"tokio::time::sleep(delay).await;", "tokio::time::sleep(Duration::ZERO).await;"},
		{"requested_session_delay(self.delay).await;", "tokio::time::sleep(self.delay).await;"},
		{"assert!(old.is_err());", "assert!(true);"},
	} {
		changed := strings.Replace(source, mutation.old, mutation.replacement, 1)
		if changed == source || recoveryFixtureDelay(changed, workflow) {
			t.Fatal("delay mutation escaped")
		}
	}
	if recoveryFixtureDelay(source, strings.ReplaceAll(workflow, "cargo test --locked -p kanz-oms-recovery --all-targets\n", "")) {
		t.Fatal("native unit omission escaped")
	}
}
