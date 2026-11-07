package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These source/ordering checks bind the reviewed owners; actual PG differential
// tests separately qualify SQL rollback, maker/checker and published FACTs.
var rustFeeCommandMarkers = map[string][]string{
	"crates/oms-recovery/src/fee_commands.rs": {
		`env.value().tenant_id!=tenant`, `env.value().event_class!=2`, `payload.len()>65536`,
		`m.target_id!=order`, `m.issuer.strip_prefix("user:")`, `!m.principal_portfolios.contains(&state.value().portfolio_id)`,
		`c.order_id!=order`, `old.proposer!=actor||old.reason!=reason||old.order_id!=c.order_id`,
		`preparation::posted_with(`, `previous.journal_bytes()?`, `identity::same_except_fee(previous.value(),revised.value())`,
		`used>repository::PROPOSAL_BYTES`, `fee_control::digest(tenant,&unsigned)?`,
		`control.approve(&old.approver,&old.digest,signed)`, `c.status=="investigating"&&c.checkpoint==0`,
		`Error::FeeRevision|Error::EvidenceConflict|Error::PostedHistoryUnknown`, `returnErr(Error::RecoveryBlocked)`,
	},
	"crates/oms-recovery/src/fee_control.rs": {
		`DEFAULT_TTL_SECONDS:i64=24*60*60`, `actor==fee_subject::normalize(self.proposer)`,
		`digest!=self.digest`, `now>=self.expires`, `(expiry-created).min(i128::from(i64::MAX))`,
		`if!(n==number&&w==2)`, `h.update((part.len()asu64).to_be_bytes())`,
	},
	"crates/oms-store/src/fee_commands.rs": {
		`PROPOSAL_BYTES:usize=512*1024`, `octet_length(proposal)::bigint<=$2`, `octet_length(approval)::bigint<=$2`,
		`fact.tenant!=tenant`, `fact.partition_key!=order`, `fact.payload!=payload`,
		`cas(&tx,&st,state,version).await?`, `old.value().digest!=a.digest`,
		`fee_subject::normalize(&old.value().approver)!=fee_subject::normalize(&a.approver)`,
		`version=$3ANDcheckpoint=0ANDstatusIN('blocked','investigating')`, `ifcount!=1`,
	},
}

func checkRustFeeCommands(root string) error {
	for name, markers := range rustFeeCommandMarkers {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		code, err := rustMigrationWithoutComments(string(raw))
		if err != nil {
			return err
		}
		code = rustMigrationCompact(code)
		for _, marker := range markers {
			if !strings.Contains(code, marker) {
				return fmt.Errorf("fee command %s missing %s", name, marker)
			}
		}
		if strings.HasPrefix(name, "crates/oms-store/") {
			if strings.Count(code, `repository::scope(client,tenant).await?`) != 4 || strings.Count(code, `enqueue(&tx,std::slice::from_ref(fact)).await?`) != 2 {
				return fmt.Errorf("fee scope/enqueue owners changed")
			}
			for _, segment := range []struct{ start, end string }{{`pubasyncfnsave_proposal(`, `pubasyncfnsave_approval(`}, {`pubasyncfnsave_approval(`, ""}} {
				start := strings.Index(code, segment.start)
				if start < 0 {
					return fmt.Errorf("missing fee transaction")
				}
				part := code[start:]
				if segment.end != "" {
					end := strings.Index(part, segment.end)
					if end < 0 {
						return fmt.Errorf("missing next transaction")
					}
					part = part[:end]
				}
				insert, queue, commit := strings.Index(part, "INSERTINTOexecution_fee_"), strings.Index(part, "enqueue(&tx,"), strings.LastIndex(part, "commit(tx).await")
				if insert < 0 || queue < insert || commit < queue {
					return fmt.Errorf("fee FACT must share decision transaction")
				}
			}
		}
	}
	return nil
}
func TestRustFeeCommandBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkRustFeeCommands(root); err != nil {
		t.Fatal(err)
	}
}
func TestRustFeeCommandBoundaryMutations(t *testing.T) {
	source := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for name, markers := range rustFeeCommandMarkers {
		for _, marker := range markers {
			t.Run(name+"/"+marker, func(t *testing.T) {
				root := t.TempDir()
				for file := range rustFeeCommandMarkers {
					raw, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(file)))
					if err != nil {
						t.Fatal(err)
					}
					code, err := rustMigrationWithoutComments(string(raw))
					if err != nil {
						t.Fatal(err)
					}
					code = rustMigrationCompact(code)
					if file == name {
						changed := strings.Replace(code, marker, "removed", 1)
						if changed == code {
							t.Fatal("vacuous fee mutation")
						}
						code = changed
					}
					target := filepath.Join(root, filepath.FromSlash(file))
					if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte(code), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if checkRustFeeCommands(root) == nil {
					t.Fatal("weakened fee ownership accepted")
				}
			})
		}
	}
}
