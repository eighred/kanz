package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ackInsertSQL = "INSERT INTO execution_recovery_acks (case_id,execution_key,book) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING"

func typedAckInsert(source, protocol, workflow string) bool {
	compact := strings.Join(strings.Fields(source), "")
	expected := `tx.execute_typed("` + strings.ReplaceAll(ackInsertSQL, " ", "") + `",&[(&ack.case_id,Type::TEXT),(&ack.execution_key,Type::TEXT),(&book.as_str(),Type::TEXT)]).await?;`
	return strings.Count(source, ackInsertSQL) == 1 && strings.Contains(compact, expected) &&
		strings.Contains(protocol, ackInsertSQL) && strings.Contains(protocol, `witness(false, affected).await;`) &&
		strings.Contains(protocol, `witness(true, affected).await;`) && strings.Contains(protocol, `assert_eq!(first, b"PBDES");`) &&
		strings.Contains(protocol, `assert_eq!(first, b"PDS");`) &&
		strings.Contains(workflow, "cargo test --locked -p kanz-oms-store --all-targets\n")
}

func TestRustRecoveryAckTypedInsert(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(path string) string {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(root, path))
		if e != nil {
			t.Fatal(e)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	source := read("kanz-rs/crates/oms-store/src/recovery_ack.rs")
	protocol := read("kanz-rs/crates/oms-store/tests/typed_ack_protocol.rs")
	workflow := read(".github/workflows/rust-oms-fold.yml")
	if !typedAckInsert(source, protocol, workflow) {
		t.Fatal("typed ACK SQL/bindings or independent protocol witness missing")
	}
	for _, m := range []struct{ old, new string }{
		{"tx.execute_typed(", "tx.execute("},
		{"(&ack.case_id,Type::TEXT)", "(&ack.case_id,Type::VARCHAR)"},
		{"(&ack.execution_key,Type::TEXT)", "(&ack.case_id,Type::TEXT)"},
		{"(&book.as_str(),Type::TEXT)", "(&book.as_str(),Type::VARCHAR)"},
		{"ON CONFLICT DO NOTHING", "ON CONFLICT DO UPDATE"},
	} {
		changed := strings.Replace(source, m.old, m.new, 1)
		if changed == source || typedAckInsert(changed, protocol, workflow) {
			t.Fatalf("mutation not refused: %s", m.old)
		}
	}
	if typedAckInsert(source, strings.Replace(protocol, "witness(false, affected).await;", "", 1), workflow) || typedAckInsert(source, protocol, strings.Replace(workflow, "cargo test --locked -p kanz-oms-store --all-targets\n", "", 1)) {
		t.Fatal("missing negative/native execution accepted")
	}
}
