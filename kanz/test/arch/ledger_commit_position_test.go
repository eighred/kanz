package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Financial time cannot be a persistence cursor. A fresh envelope timestamp is
// still allocated before commit and therefore cannot repair #1309.
func TestLedgerCheckpointUsesCommitProgress(t *testing.T) {
	for path, required := range map[string][]string{
		"services/accounting/internal/ledger/store.go": {
			"JournalSince(ctx context.Context, portfolioID string, after int64)",
			"st.JournalSince(ctx, portfolioID, snap.JournalPosition)",
			"return ErrStaleSnapshot",
		},
		"services/accounting/internal/ledger/postgres.go": {
			"AND position > $4", "JOIN ledger_append_positions",
			"snap.JournalPosition != head", "pgx.RepeatableRead",
		},
		"services/accounting/migrations/0019_journal_commit_position.sql": {
			"AFTER INSERT ON ledger_entries", "pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext(NEW.portfolio_id))",
			"UPDATE ledger_heads SET position=position+1", "BEFORE UPDATE OR DELETE ON ledger_append_positions",
			"NEW.journal_position <> head_position", "NEW.corporate_action_version IS DISTINCT FROM 2",
		},
	} {
		data, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range required {
			if !strings.Contains(string(data), term) {
				t.Errorf("%s lost commit-progress boundary %q (#1309)", path, term)
			}
		}
	}
}
