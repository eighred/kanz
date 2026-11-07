package arch

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A reviewed call-site inventory, not a Rust parser or a latency measurement.
// Hash exact SQL bytes; bind method cardinality, parameter identity/type/order,
// and call order inside the named scopes. Existing guards own financial checks.
type typedSelectCall struct{ method, sqlSHA, parameters string }
type typedSelectScope struct {
	file, start, end string
	calls            []typedSelectCall
}

var recoveryTypedSelectScopes = []typedSelectScope{
	{"lib.rs", "async fn scope_existing(", "async fn enqueue(", []typedSelectCall{{"query_typed_one", "de2f86857b1ba3769a35a9df4112db920e25a7fb5e661b4280d6bc45bea232b4", ""}, {"query_typed_one", "2b6bf60ae37638fd5de13f6c62a8aa3c16d0c4b76dc5224f337729c091a81780", "(&tenant,Type::TEXT)"}, {"query_typed_one", "b59e0936a6f03c1d08eeb9e2872b63304cb34067cfda41fc7e7194caa6b926dc", ""}}},
	{"lib.rs", "pub async fn load(", "pub async fn applied_fills(", []typedSelectCall{{"query_typed_opt", "b3eee25fc5ac8b2c7050ae50a8f3c72293723e8cb52ff14f2e79cec6610c2701", "(&order_id,Type::TEXT),(&(MAX_BYTESasi64),Type::INT8)"}}},
	{"recovery_repository.rs", "async fn scope<'a>", "pub async fn observe(", []typedSelectCall{{"query_typed_one", "086ff49b1a93e523c4e4e196185043b1ff48da629dff03555b0f0dcff1a619b1", ""}, {"query_typed_opt", "55f4f895e19a3e9dadda9466334c158b4b434160cc10e7380ed8f2197750b1cc", "(&id,Type::TEXT),(&limit,Type::INT8)"}}},
	{"recovery_ack.rs", "pub async fn acknowledge_recovery(", "", []typedSelectCall{{"query_typed_one", "d95da72bb37d58a985780f3832482c9d956d3992c73cebcf3be00ca316f93d50", ""}, {"query_typed_opt", "20cdaa02339a3233df5a4426d298f2ca2ed75097d899467c126b47b6ee68a610", "(&ack.case_id,Type::TEXT),(&limit,Type::INT8)"}, {"query_typed_one", "035df3434770d5974cb8778466f2f51b982a90996118c3244e7bb16c4ece5792", "(&ack.case_id,Type::TEXT)"}, {"query_typed_one", "e3bb0ba2fb37ae1c385b6568570b59971870bbbf304e55e6656c7ad38187fce7", "(&ack.case_id,Type::TEXT),(&ack.execution_key,Type::TEXT),(&ack.payload_digest,Type::TEXT),(&ack.execution_digest,Type::TEXT)"}, {"query_typed_opt", "62e3a73b821eebf58430990acf97effd93158ae7b10e29eec8215ce6c6d0c999", "(&ack.order_id,Type::TEXT),(&limit,Type::INT8)"}, {"query_typed_one", "6655708000f09c0995ec0515621651d704c713ce1dc48507742b666c353555b4", "(&ack.case_id,Type::TEXT)"}}},
	{"outbox.rs", "async fn owned(", "fn page(", []typedSelectCall{{"query_typed_one", "3ddefd052b6234f65a904876a6c449364302db122abcd6352bc2e87aee06cd82", ""}}},
	{"outbox.rs", "pub async fn pending(", "pub async fn mark_published(", []typedSelectCall{{"query_typed", "770d194e0278607615c77246d8f4d19fca7b2f3a2577359200c3f7838193a8c3", "(&key,Type::TEXT),(&limit,Type::INT8),(&(MAX_BYTESasi64),Type::INT8)"}}},
	{"outbox.rs", "impl DrainLock {", "pub async fn lock_key(", []typedSelectCall{{"query_typed_one", "d3ddaa086a8c76c6e78a6c02e81a647382388502f328c791ea4dc167716bd3aa", "(&self.scope,Type::INT4),(&self.key,Type::INT4)"}}},
	{"outbox.rs", "async fn acquire_key(", "pub(crate) async fn lock_key_before(", []typedSelectCall{{"query_typed_one", "9ac78d0617444f80993c2e9aea60a3ee85f749d25b0cf674c6bd641e3d9deec5", "(&key,Type::TEXT),(&SUFFIX,Type::TEXT)"}, {"query_typed_one", "11823042f0e795702be8d002e45e54599869cb973270707c4dc411356c3a475d", "(&scope,Type::INT4),(&key,Type::INT4)"}, {"query_typed_one", "a9044416c01db9d90234ca4a5893a633e4f56bcb0ce67176862dbf772b9b7800", "(&scope,Type::INT4),(&key,Type::INT4)"}}},
}
var recoveryTypedSelectPattern = regexp.MustCompile(`(?s)\.(query_typed(?:_one|_opt)?)\(\s*("(?:[^"\\]|\\.)*"|LOCK_IDS)\s*,\s*&\[(.*?)\]\s*,?\s*\)`)
var recoveryStrSelectPattern = regexp.MustCompile(`\.query(?:_one|_opt|_raw)?\s*\(`)
var recoveryLockSQLPattern = regexp.MustCompile(`const LOCK_IDS: &str = ("[^\n]*");`)

func checkRecoveryTypedSelects(files map[string]string) error {
	for _, scope := range recoveryTypedSelectScopes {
		source, err := rustMigrationWithoutComments(files[scope.file])
		if err != nil {
			return err
		}
		if strings.Count(source, scope.start) != 1 {
			return fmt.Errorf("typed SELECT owner missing/ambiguous: %s", scope.start)
		}
		start := strings.Index(source, scope.start)
		body := source[start:]
		if scope.end != "" {
			end := strings.Index(body, scope.end)
			if end < 0 {
				return fmt.Errorf("typed SELECT scope end missing")
			}
			body = body[:end]
		}
		if recoveryStrSelectPattern.MatchString(body) {
			return fmt.Errorf("str SELECT fallback in %s", scope.start)
		}
		found := recoveryTypedSelectPattern.FindAllStringSubmatch(body, -1)
		if len(found) != len(scope.calls) {
			return fmt.Errorf("typed SELECT inventory changed in %s", scope.start)
		}
		for i, call := range found {
			literal := call[2]
			if literal == "LOCK_IDS" {
				m := recoveryLockSQLPattern.FindStringSubmatch(source)
				if len(m) != 2 {
					return fmt.Errorf("lock SQL missing")
				}
				literal = m[1]
			}
			sql, err := strconv.Unquote(literal)
			if err != nil {
				return err
			}
			actual := typedSelectCall{call[1], fmt.Sprintf("%x", sha256.Sum256([]byte(sql))), rustMigrationCompact(call[3])}
			if actual != scope.calls[i] {
				return fmt.Errorf("typed SELECT SQL/type/order/cardinality changed: %s call %d", scope.start, i)
			}
		}
	}
	return nil
}
func TestRustRecoveryTypedSelectProtocol(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store", "src")
	files := map[string]string{}
	for _, scope := range recoveryTypedSelectScopes {
		raw, err := os.ReadFile(filepath.Join(root, scope.file))
		if err != nil {
			t.Fatal(err)
		}
		files[scope.file] = string(raw)
	}
	if err := checkRecoveryTypedSelects(files); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ name, file, old, replacement string }{
		{"str-fallback", "lib.rs", ".query_typed_one(", ".query_one("},
		{"wrong-cardinality", "lib.rs", ".query_typed_opt(", ".query_typed_one("},
		{"missing-call", "recovery_ack.rs", ".query_typed_one(", ".removed("},
		{"wrong-byte-type", "recovery_repository.rs", "Type::INT8", "Type::INT4"},
		{"wrong-lock-type", "outbox.rs", "Type::INT4", "Type::INT8"},
		{"wrong-parameter", "outbox.rs", "(&key, Type::TEXT)", "(&SUFFIX, Type::TEXT)"},
		{"changed-predicate", "recovery_ack.rs", "case_id=$1 FOR UPDATE", "case_id=$1"},
		{"changed-sql-bytes", "lib.rs", "SELECT rolsuper,rolbypassrls", "SELECT  rolsuper,rolbypassrls"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			original := files[mutation.file]
			changed := strings.Replace(original, mutation.old, mutation.replacement, 1)
			if changed == original {
				t.Fatal("mutation not applied")
			}
			files[mutation.file] = changed
			defer func() { files[mutation.file] = original }()
			if checkRecoveryTypedSelects(files) == nil {
				t.Fatal("mutation escaped typed SELECT guard")
			}
		})
	}
}
