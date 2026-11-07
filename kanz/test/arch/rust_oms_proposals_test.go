package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkRustOMSProposals(source, contract string) error {
	code, e := rustMigrationWithoutComments(source)
	if e != nil {
		return e
	}
	code = rustMigrationCompact(code)
	for _, marker := range []string{
		`kanz_order_core::proto::order::v1::SubmitOrder`, `constMAX_PROPOSALS:usize=4096`,
		`fee_subject::normalize(value).is_empty()`, `p.id!=p.subject`, `command.order_id!=p.id`,
		`p.expires_at<=p.created_at`, `zero(p.created_at)`, `zero(p.expires_at)`,
		`scoped(client,tenant).await?`, `'order_proposals'::regclass`, `relrowsecurityANDrelforcerowsecurity`,
		`ONCONFLICT(tenant_id,order_id)DONOTHING`, `enqueue(&tx,records).await?`,
		`tx.commit().await.map_err(|_|Error::CommitUnconfirmed)`,
		`expiry_announced_atISNULLANDlower(btrim(proposer))<>lower(btrim($2))`,
		`blank(approver)||records.is_empty()`, `blank(approver)||blank(reason)||zero(at)`,
		`refusal_reason=$3,refused_by=$2,refused_at=$4WHEREorder_id=$1ANDapprover=''`,
		`expires_at<=$2`, `expires_at>$1ORDERBYcreated_at,order_idLIMIT4097`,
		`expires_at<=$1ORDERBYexpires_at,order_idLIMIT$2`,
		`ifrows.len()>MAX_PROPOSALS`, `CASEWHENfitsTHENcommandEND`,
		`sum(octet_length(`, `&p.command`,
	} {
		if !strings.Contains(code, marker) {
			return fmt.Errorf("Rust proposal boundary missing %s", marker)
		}
	}
	for _, name := range []string{"put", "get", "pending", "expired_unannounced", "claim", "record_refusal", "announce_expiry"} {
		if !strings.Contains(code, "pubasyncfn"+name+"(") {
			return fmt.Errorf("missing proposal operation %s", name)
		}
	}
	for _, bad := range []string{"clock_timestamp(", "statement_timestamp(", "SystemTime::now(", "DELETEFROMorder_proposals", "structSubmitOrder", ".publish(", ".connect("} {
		if strings.Contains(code, bad) {
			return fmt.Errorf("proposal authority/contract drift %s", bad)
		}
	}
	if !strings.Contains(contract, "proposalstoretest.Run(t,") || !strings.Contains(contract, "RefusesSelfApprovalAtTheStore: true") || !strings.Contains(contract, "return rustProposals{newFixture(t)}") {
		return fmt.Errorf("canonical actual proposal contract missing")
	}
	return nil
}
func TestRustOMSProposalsBoundary(t *testing.T) {
	root := moduleRoot(t)
	raw, e := os.ReadFile(filepath.Join(filepath.Dir(root), "kanz-rs", "crates", "oms-store", "src", "proposals.rs"))
	if e != nil {
		t.Fatal(e)
	}
	tests, e := os.ReadFile(filepath.Join(root, "services", "oms", "tools", "store-conformance", "proposals_test.go"))
	if e != nil {
		t.Fatal(e)
	}
	source, contract := string(raw), string(tests)
	if e = checkRustOMSProposals(source, contract); e != nil {
		t.Fatal(e)
	}
	for _, mutation := range []struct{ name, from, to string }{
		{"scope", "scoped(client, tenant)", "client.transaction()"},
		{"forced-rls", "relforcerowsecurity", "true"},
		{"bound-command", "command.order_id != p.id", "false"},
		{"self-approval", "lower(btrim(proposer))<>lower(btrim($2))", "true"},
		{"expiry-exclusion", "expiry_announced_at IS NULL", "true"},
		{"mandatory-fact", "records.is_empty()", "false"},
		{"atomic-enqueue", "enqueue(&tx, records).await?", "Ok::<(), Error>(())?"},
		{"commit-unknown", "Error::CommitUnconfirmed", "Error::Database"},
		{"whole-result", "rows.len() > MAX_PROPOSALS", "false"},
		{"raw-command", "&p.command", "&Vec::<u8>::new()"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := strings.ReplaceAll(source, mutation.from, mutation.to)
			if changed == source {
				t.Fatal("mutation missed")
			}
			if checkRustOMSProposals(changed, contract) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
	t.Run("shared-contract", func(t *testing.T) {
		if checkRustOMSProposals(source, strings.ReplaceAll(contract, "proposalstoretest.Run(t,", "other.Run(t,")) == nil {
			t.Fatal("private contract accepted")
		}
	})
}
