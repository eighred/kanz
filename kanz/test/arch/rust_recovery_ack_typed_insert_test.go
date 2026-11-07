package arch

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ackInsertSQL = "INSERT INTO execution_recovery_acks (case_id,execution_key,book) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING"

// Full LF-normalized test source pin, not an allowance for arbitrary tests/.
// #1413: synthetic protocol encoding evidence only; real financial proof remains mandatory.
func reviewedAckProtocol(source string) bool {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	if fmt.Sprintf("%x", sha256.Sum256([]byte(source))) != "1a2774fad4f8800d046ed608c2ca84e74db6e18080e153be0b0a6e8b9cc91e69" {
		return false
	}
	for _, marker := range []string{
		`TcpListener::bind("127.0.0.1:0")`, `.host("127.0.0.1")`,
		`assert!((4..=4096).contains(&length));`, `for _ in 0..8`,
		`set_read_timeout(Some(Duration::from_secs(5)))`,
		`set_write_timeout(Some(Duration::from_secs(5)))`,
		`.connect_timeout(Duration::from_secs(5));`,
		`tokio::time::timeout(Duration::from_secs(5), async {`,
		`driver.abort();`, `let _closed = driver.await;`, `peer.join().unwrap();`,
	} {
		if !strings.Contains(source, marker) {
			return false
		}
	}
	return true
}

func typedAckInsert(source, protocol, workflow string) bool {
	compact := strings.Join(strings.Fields(source), "")
	expected := `tx.execute_typed("` + strings.ReplaceAll(ackInsertSQL, " ", "") + `",&[(&ack.case_id,Type::TEXT),(&ack.execution_key,Type::TEXT),(&book.as_str(),Type::TEXT)]).await?;`
	return reviewedAckProtocol(protocol) && strings.Count(source, ackInsertSQL) == 1 && strings.Contains(compact, expected) &&
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

func TestRustRecoveryAckProtocolExceptionIsExact(t *testing.T) {
	source := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, target := range []string{"src/queries.rs", "tests/unreviewed_protocol.rs"} {
		t.Run(target, func(t *testing.T) {
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
			p := filepath.Join(dst, rustOMSStorePath, target)
			if target == "src/queries.rs" {
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, append(b, []byte(`fn unauthorized() { std::net::TcpListener::bind("127.0.0.1:0"); }`)...), 0600); e != nil {
					t.Fatal(e)
				}
			} else if e := os.Rename(filepath.Join(dst, rustOMSStorePath, "tests/typed_ack_protocol.rs"), p); e != nil {
				t.Fatal(e)
			}
			if checkRustOMSStoreBoundary(dst) == nil {
				t.Fatal("transport escaped exact test path")
			}
		})
	}
	raw, e := os.ReadFile(filepath.Join(source, rustOMSStorePath, "tests/typed_ack_protocol.rs"))
	if e != nil {
		t.Fatal(e)
	}
	for _, pair := range [][2]string{{"127.0.0.1", "0.0.0.0"}, {"4..=4096", "4..=8192"}, {"driver.abort();", ""}, {"peer.join().unwrap();", ""}, {"Duration::from_secs(5)", "Duration::from_secs(50)"}} {
		mutated := strings.Replace(string(raw), pair[0], pair[1], 1)
		if mutated == string(raw) || reviewedAckProtocol(mutated) {
			t.Fatal("protocol pin mutation accepted")
		}
	}
}
