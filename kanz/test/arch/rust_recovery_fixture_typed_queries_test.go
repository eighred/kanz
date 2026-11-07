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
