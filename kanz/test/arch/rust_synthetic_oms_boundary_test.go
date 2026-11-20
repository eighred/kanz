package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The synthetic owner is an opt-in workspace executable. Its only transport
// is a local PostgreSQL session; every economic fold remains inside OMS.
const syntheticOMSPath = "services/synthetic_oms"

var syntheticOMSFiles = []string{
	"Cargo.toml", "src/main.rs", "schema.sql", "writer.sql", "go_writer.sql", "go_admission.sql",
	"funding.sql", "cancellation.sql", "settlement.sql",
}

const syntheticOMSCargo = `[package]
name = "kanz-synthetic-oms"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-oms-store = { path = "../../crates/oms_store", features = ["synthetic-command"] }
base64 = "=0.22.1"
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
tokio = { version = "=1.54.1", features = ["rt", "time", "net"] }
tokio-postgres = "=0.7.18"`

func checkSyntheticOMS(root string) error {
	dir := filepath.Join(root, filepath.FromSlash(syntheticOMSPath))
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("synthetic OMS contains symlink")
		}
		rel, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && rel != "src" {
				return fmt.Errorf("unexpected synthetic OMS directory %s", rel)
			}
			return nil
		}
		if !slices.Contains(syntheticOMSFiles, rel) {
			return fmt.Errorf("unexpected synthetic OMS input %s", rel)
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) != len(syntheticOMSFiles) || rustMigrationCompact(files["Cargo.toml"]) != rustMigrationCompact(syntheticOMSCargo) {
		return fmt.Errorf("synthetic OMS workspace recipe changed")
	}
	source, err := rustMigrationWithoutComments(files["src/main.rs"])
	if err != nil {
		return err
	}
	src := rustMigrationCompact(source)
	for _, marker := range []string{`#![forbid(unsafe_code)]`, `#[serde(deny_unknown_fields)]`, `config.get_dbname()!=Some("kanz_synthetic_oms")`, `config.get_hosts().len()!=1`, `!config.get_hostaddrs().is_empty()`, `Host::Tcp(host)ifhost=="127.0.0.1"||host=="::1"`, `owner::submit(`, `owner::observe(`, `owner::get(`} {
		if !strings.Contains(src, marker) {
			return fmt.Errorf("synthetic OMS local ingress changed %s", marker)
		}
	}
	for _, token := range []string{"TcpListener", "TcpStream", "tokio::net", "std::net", "include!", "include_str!", "#[path=", "std::process", "tonic::", "reqwest::", "async_nats", "https://"} {
		if strings.Contains(src, token) {
			return fmt.Errorf("synthetic OMS external ingress/egress %s", token)
		}
	}
	sql := rustMigrationCompact(files["schema.sql"])
	writer := rustMigrationCompact(files["writer.sql"])
	if !strings.Contains(writer, "BEGIN;SETLOCALsearch_path=") || !strings.HasSuffix(writer, "COMMIT;") {
		return fmt.Errorf("synthetic writer migration must be atomic")
	}
	for _, marker := range []string{
		"FROMpublic.synthetic_oms_writerWHEREsingletonFORUPDATE",
		"FROMpublic.synthetic_oms_writerWHEREsingletonFORSHARE",
		"session_user::regrole::oidNOTIN(owner.runtime_oid,owner.adapter_oid)",
		"expectedISDISTINCTFROMold_epoch",
		"REVOKEALLONFUNCTIONsynthetic_oms_handoff(bigint,regrole,regrole)FROMPUBLIC",
		"NEW.writer_epoch:=public.synthetic_oms_assert_writer()",
		"NEW.writer_epoch:=OLD.writer_epoch",
		"BEFOREINSERTORUPDATEONpublic.%I",
		"BEFOREINSERTONpublic.outbox",
	} {
		if !strings.Contains(writer, marker) {
			return fmt.Errorf("synthetic OMS writer fence changed %s", marker)
		}
	}
	funding := rustMigrationCompact(files["funding.sql"])
	goAdmission := rustMigrationCompact(files["go_admission.sql"])
	for _, marker := range []string{
		"current_database()<>'kanz_synthetic_oms'",
		"NEW.writer_epochISDISTINCTFROMpublic.synthetic_oms_assert_writer()",
		"gateway_frame_sha256=sha256(NEW.go_gateway_frame)",
		"cardinality(sources)ISDISTINCTFROM5ORsourcesISDISTINCTFROMNEW.evidence_ids",
		"ANDxmin<>pg_current_xact_id()::xidFORSHARE",
		"armedISDISTINCTFROM(NEW.go_funding_amountsISNOTNULL)",
		"a.currencies=ARRAY['BNB','USD']ANDa.amounts=NEW.go_funding_amounts",
		"c.xmin=pg_current_xact_id()::xidANDm.xmin=pg_current_xact_id()::xid",
		"b.xmin=pg_current_xact_id()::xid",
		"CREATECONSTRAINTTRIGGERsynthetic_go_admissionAFTERINSERTONpublic.synthetic_oms_commandsDEFERRABLEINITIALLYDEFERREDFOREACHROWWHEN(NEW.go_gateway_frameISNOTNULL)EXECUTEFUNCTIONpublic.synthetic_oms_check_go_admission();",
	} {
		if !strings.Contains(goAdmission, marker) {
			return fmt.Errorf("synthetic Go admission changed %s", marker)
		}
	}
	goWriter := rustMigrationCompact(files["go_writer.sql"])
	for _, marker := range []string{
		"current_database()<>'kanz_synthetic_oms'",
		"CREATEROLEkanz_synthetic_oms_go_runtimeNOLOGINNOINHERITNOSUPERUSERNOBYPASSRLS",
		"GRANTkanz_synthetic_oms_go_runtimeTOkanz_synthetic_oms_go_funded_runtime",
		"pg_has_role(NEW.runtime_oid,'kanz_synthetic_oms_go_runtime','MEMBER')",
		"current_setting('transaction_isolation')<>'readcommitted'",
		"c.phase<>'working'ORo.status<>2",
		"a.operation='ack'ANDa.observation=convert_to(c.order_id,'UTF8')",
		"d.operation='dispatch'ANDd.observation=''::bytea",
		"FROMpublic.capital_commitments",
		"PERFORMpublic.synthetic_oms_check_funded_go_handback(OLD.epoch)",
		"b.currencyCOLLATE\"C\"FORUPDATEOFb",
		"receipt.prior_version+1=o.versionANDreceipt.next_state_sha256=sha256(o.state)",
		"b.causation_id=cancel_source.event_idANDb.trace_context=cancel_source.trace_context",
		"public.capital_exact_parts(cash.reserved)=public.capital_exact_parts(leg.debit)",
		"hold.booked_debit='0'ANDmember.booked_debit='0'",
		"BEFOREUPDATEOFruntime_oidONpublic.synthetic_oms_writer",
		"REVOKEALLONFUNCTIONpublic.synthetic_oms_check_go_handback()FROMPUBLIC",
	} {
		if !strings.Contains(goWriter, marker) {
			return fmt.Errorf("synthetic Go handback changed %s", marker)
		}
	}
	for _, marker := range []string{
		"current_database()<>'kanz_synthetic_oms'",
		"ALTERTABLEpublic.synthetic_oms_funding_authorizationsFORCEROWLEVELSECURITY",
		"GRANTINSERTONpublic.synthetic_oms_funding_authorizationsTOkanz_synthetic_oms_funding",
		"public.synthetic_oms_assert_writer()ISDISTINCTFROMNEW.writer_epoch",
		"NEW.command_sha256<>sha256(NEW.command)",
		"sourcesISDISTINCTFROMNEW.evidence_ids",
		"outcome:=public.capital_reserve(",
		"REVOKEALLONFUNCTIONpublic.synthetic_oms_reserve_authorized()FROMPUBLIC",
		"reserve_synthetic_fundingAFTERINSERTONpublic.synthetic_oms_commands",
		"EXECUTEFUNCTIONpublic.synthetic_oms_hold_funded_lifecycle()",
	} {
		if !strings.Contains(funding, marker) {
			return fmt.Errorf("synthetic OMS funding boundary changed %s", marker)
		}
	}
	for _, marker := range []string{`current_database()<>'kanz_synthetic_oms'`, `ALTERTABLE%IFORCEROWLEVELSECURITY`, `GRANTINSERTONsynthetic_oms_evidenceTOkanz_synthetic_oms_control`, `UNIQUE(tenant_id,idempotency_key)`, `PRIMARYKEY(tenant_id,order_id,event_id)`, `effective_secondsbigintNOTNULL`, `recorded_attimestamptzNOTNULLDEFAULTtransaction_timestamp()`} {
		if !strings.Contains(sql, marker) {
			return fmt.Errorf("synthetic OMS evidence/persistence changed %s", marker)
		}
	}
	withdrawal := rustMigrationCompact(files["cancellation.sql"])
	for _, marker := range []string{
		"current_database()<>'kanz_synthetic_oms'",
		"ALTERTABLEpublic.synthetic_oms_cancellation_authorizationsFORCEROWLEVELSECURITY",
		"ALTERTABLEpublic.synthetic_oms_funded_withdrawalsFORCEROWLEVELSECURITY",
		"GRANTINSERTONpublic.synthetic_oms_cancellation_authorizationsTOkanz_synthetic_oms_control",
		"public.synthetic_oms_assert_writer()ISDISTINCTFROMNEW.writer_epoch",
		"a.terminal_state_sha256=sha256(NEW.state)",
		"a.fact_payload_sha256=sha256(NEW.fact_payload)",
		"outcome:=public.capital_release_unexecuted(",
		"REVOKEALLONFUNCTIONpublic.synthetic_oms_withdraw_funded()FROMPUBLIC",
		"CREATETRIGGERrequire_withdrawal_fact_slotBEFOREINSERTONpublic.outbox",
		"SETfact_row_id=NEW.id",
		"ANDfact_row_idISNULL",
		"DEFERRABLEINITIALLYDEFERREDFOREACHROWEXECUTEFUNCTIONpublic.synthetic_oms_require_withdrawal_fact()",
	} {
		if !strings.Contains(withdrawal, marker) {
			return fmt.Errorf("synthetic OMS cancellation boundary changed %s", marker)
		}
	}
	settlement := rustMigrationCompact(files["settlement.sql"])
	for _, marker := range []string{
		"CREATEORREPLACEFUNCTIONpublic.synthetic_oms_apply_settlement(",
		"f.proowner<>guardORNOTf.prosecdefORf.proconfigISDISTINCTFROM",
		"a.granteeNOTIN(guard,adapter)",
		"a.relowner=writer.relowner",
		"pg_get_expr(polwithcheck,polrelid)='(tenant_id=app_current_tenant())'",
		"tgfoid=to_regprocedure(",
		"unknowninstalledsyntheticsettlementfunction",
		"CREATEROLEkanz_synthetic_settlement_guardNOLOGINNOINHERITNOSUPERUSERNOBYPASSRLS",
		"ALTERTABLEpublic.synthetic_oms_settlement_authorizationsFORCEROWLEVELSECURITY",
		"GRANTINSERTONpublic.synthetic_oms_settlement_authorizationsTOkanz_synthetic_oms_control",
		"epoch:=public.synthetic_oms_assert_writer()",
		"EXECUTEFUNCTIONpublic.synthetic_oms_require_execution_fact_owner()",
		"NEW.envelope_tenant_idISDISTINCTFROMNEW.tenant_id",
		"b.coverage_completeANDNOTb.conflictedANDb.gap_revision<=b.revisionFORSHARE",
		"auth.next_state_sha256ISDISTINCTFROMsha256(p_state)",
		"auth.fact_payload_sha256ISDISTINCTFROMsha256(p_fact)",
		"live_sourcesISDISTINCTFROMoriginal_sourcesORcardinality(live_sources)<>5",
		"sha256(j.fill)=auth.venue_execution_sha256",
		"public.capital_settle_direct(p_portfolio,p_order,funding.currencies,funding.amounts,auth.executed_debits,p_op='cancelled')",
		"REVOKEALLONFUNCTIONpublic.synthetic_oms_apply_settlement(text,text,text,text,bigint,integer,bytea,bytea,bytea)FROMPUBLIC",
	} {
		if !strings.Contains(settlement, marker) {
			return fmt.Errorf("synthetic OMS settlement boundary changed %s", marker)
		}
	}
	return nil
}

func TestSyntheticOMSGuardsLocalIngressAndEvidence(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "rust")
	for _, tc := range []struct{ file, old, new string }{
		{"src/main.rs", `host == "127.0.0.1"`, `host != "127.0.0.1"`},
		{"schema.sql", "FORCE ROW LEVEL SECURITY", "NO FORCE ROW LEVEL SECURITY"},
		{"writer.sql", "WHERE singleton FOR SHARE", "WHERE singleton FOR KEY SHARE"},
		{"writer.sql", "session_user::regrole::oid", "current_user::regrole::oid"},
		{"writer.sql", "expected IS DISTINCT FROM old_epoch", "FALSE"},
		{"go_admission.sql", "gateway_frame_sha256=sha256(NEW.go_gateway_frame)", "TRUE"},
		{"go_admission.sql", "b.xmin=pg_current_xact_id()::xid", "TRUE"},
		{"go_admission.sql", "    ON public.synthetic_oms_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW", "    ON public.synthetic_oms_commands NOT DEFERRABLE FOR EACH ROW"},
		{"go_writer.sql", "current_setting('transaction_isolation')<>'read committed'", "FALSE"},
		{"go_writer.sql", "c.phase<>'working' OR o.status<>2", "FALSE"},
		{"go_writer.sql", "a.observation=convert_to(c.order_id,'UTF8')", "TRUE"},
		{"go_writer.sql", "receipt.next_state_sha256=sha256(o.state)", "TRUE"},
		{"go_writer.sql", "b.causation_id=cancel_source.event_id", "TRUE"},
		{"go_writer.sql", "public.capital_exact_parts(cash.reserved)=public.capital_exact_parts(leg.debit)", "TRUE"},
		{"funding.sql", "TO kanz_synthetic_oms_funding;", "TO kanz_synthetic_oms_runtime;"},
		{"funding.sql", "NEW.command_sha256 <> sha256(NEW.command)", "FALSE"},
		{"funding.sql", "outcome := public.capital_reserve(", "outcome := public.other_reserve("},
		{"cancellation.sql", "a.terminal_state_sha256=sha256(NEW.state)", "TRUE"},
		{"cancellation.sql", "outcome := public.capital_release_unexecuted(", "outcome := public.other_release("},
		{"cancellation.sql", "AND fact_row_id IS NULL", "AND TRUE"},
		{"cancellation.sql", "DEFERRABLE INITIALLY DEFERRED", "NOT DEFERRABLE"},
		{"settlement.sql", "f.proowner<>guard", "FALSE"},
		{"settlement.sql", "a.relowner=writer.relowner", "TRUE"},
		{"settlement.sql", "pg_get_expr(polwithcheck,polrelid)", "pg_get_expr(polqual,polrelid)"},
		{"settlement.sql", "a.grantee NOT IN (guard,adapter)", "FALSE"},
		{"settlement.sql", "tgfoid=to_regprocedure(", "tgfoid<>to_regprocedure("},
		{"settlement.sql", "b.coverage_complete AND NOT b.conflicted", "TRUE"},
		{"settlement.sql", "NEW.envelope_tenant_id IS DISTINCT FROM NEW.tenant_id", "FALSE"},
		{"settlement.sql", "NOLOGIN NOINHERIT", "LOGIN NOINHERIT"},
		{"settlement.sql", "TO kanz_synthetic_oms_control;", "TO kanz_synthetic_oms_runtime;"},
		{"settlement.sql", "auth.next_state_sha256 IS DISTINCT FROM sha256(p_state)", "FALSE"},
		{"settlement.sql", "sha256(j.fill)=auth.venue_execution_sha256", "TRUE"},
		{"settlement.sql", "auth.executed_debits,p_op='cancelled'", "auth.executed_debits,TRUE"},
		{"Cargo.toml", `["synthetic-command"]`, `["qualification-diagnostics"]`},
	} {
		t.Run(tc.file, func(t *testing.T) {
			dst := t.TempDir()
			dir := filepath.Join(dst, syntheticOMSPath)
			if err := os.MkdirAll(filepath.Join(dir, "src"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range syntheticOMSFiles {
				raw, err := os.ReadFile(filepath.Join(root, syntheticOMSPath, name))
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkSyntheticOMS(dst); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, tc.file)
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tc.old) {
				t.Fatal("mutation input missing")
			}
			if err = os.WriteFile(name, []byte(strings.Replace(string(raw), tc.old, tc.new, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			if err = checkSyntheticOMS(dst); err == nil {
				t.Fatal("unsafe mutation accepted")
			}
		})
	}
}
