package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ackReadBoundary(lib, repository, handlers string) bool {
	for _, marker := range []string{"letloaded=load_in(&tx,order_id).await?;tx.rollback().await?;Ok(loaded)", "asyncfnload_in(tx:&Transaction<'_>,order_id:&str)", "ifrole.get::<_,bool>(0)||role.get::<_,bool>(1){returnErr(Error::UnsafeRole);}", "iftables.get::<_,i64>(0)!=5{returnErr(Error::UnsafeRole);}"} {
		if !strings.Contains(rustMigrationCompact(lib), marker) {
			return false
		}
	}
	role := strings.Index(lib, "SELECT rolsuper,rolbypassrls")
	catalog := strings.Index(lib, "SELECT (SELECT count(*) FROM pg_class")
	if role < 0 || catalog <= role {
		return false
	}
	start := strings.Index(repository, "pub async fn load_ack_context(")
	end := strings.Index(repository, "pub async fn observe(")
	if start < 0 || end <= start {
		return false
	}
	body := rustMigrationCompact(repository[start:end])
	previous := -1
	for _, marker := range []string{"if!text(case_id,256)", ".isolation_level(tokio_postgres::IsolationLevel::ReadCommitted)", ".read_only(true)", "super::scope_existing(&tx,tenant).await?", "require_tables(&tx).await?", "load_case_in(&tx,case_id)", "super::load_in(&tx,order_id)", "tx.rollback().await?", "Ok((case,order))"} {
		at := strings.Index(body, marker)
		if at <= previous {
			return false
		}
		previous = at
	}
	if strings.Contains(body, ".execute(") || strings.Contains(body, ".query_") || strings.Contains(body, "commit(") {
		return false
	}
	return strings.Contains(rustMigrationCompact(handlers), "repository::load_ack_context(client,tenant,&ack.case_id,&ack.order_id).await?") && strings.Contains(repository, "let case = load_case_in(&tx, id).await?;") && strings.Contains(repository, "commit(tx).await?;")
}
func TestRustRecoveryPairedACKRead(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates")
	var sources []string
	for _, p := range []string{"oms-store/src/lib.rs", "oms-store/src/recovery_repository.rs", "oms-recovery/src/handlers.rs"} {
		b, e := os.ReadFile(filepath.Join(root, p))
		if e != nil {
			t.Fatal(e)
		}
		sources = append(sources, string(b))
	}
	if !ackReadBoundary(sources[0], sources[1], sources[2]) {
		t.Fatal("ACK read ownership changed")
	}
	for _, m := range []struct {
		index            int
		old, replacement string
	}{
		{1, "ReadCommitted", "RepeatableRead"}, {1, ".read_only(true)", ".read_only(false)"},
		{1, "super::scope_existing(&tx, tenant).await?;", "Ok::<(), Error>(())?;"},
		{1, "require_tables(&tx).await?;", "Ok::<(), Error>(())?;"},
		{1, "load_case_in(&tx, case_id)", "load_case_in(&tx, order_id)"},
		{1, "super::load_in(&tx, order_id)", "super::load_in(&tx, case_id)"},
		{2, "repository::load_ack_context(", "repository::load_case("},
	} {
		copySources := append([]string(nil), sources...)
		copySources[m.index] = strings.ReplaceAll(copySources[m.index], m.old, m.replacement)
		if copySources[m.index] == sources[m.index] {
			t.Fatal("mutation absent")
		}
		if ackReadBoundary(copySources[0], copySources[1], copySources[2]) {
			t.Fatal("mutation accepted")
		}
	}
}
