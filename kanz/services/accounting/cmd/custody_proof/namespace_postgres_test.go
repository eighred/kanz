package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Use the dedicated migration test database and the disposable issuer login
// from the custody pilot. No accounting grants or ledger read access are needed.
func TestProofIssuerPinsOnlyAnUnambiguousRestrictedAccountingSchema(t *testing.T) {
	dsn := os.Getenv("TEST_ACCOUNTING_MIGRATION_POSTGRES_URL")
	password := os.Getenv("TEST_CUSTODY_VENUE_PROOF_PASSWORD")
	if dsn == "" || password == "" {
		t.Skip("requires the dedicated accounting migration database and the custody pilot issuer")
	}
	ctx := t.Context()
	owner, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("cannot connect as the test schema owner")
	}
	defer func() { _ = owner.Close(ctx) }()
	var restricted bool
	if err := owner.QueryRow(ctx, "SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=session_user").Scan(&restricted); err != nil || !restricted {
		t.Fatal("restricted test schema owner required")
	}
	cfg := owner.Config().Copy()
	cfg.User, cfg.Password = "pilot_venue_proof", password
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	issuer, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot connect as the test issuer")
	}
	defer func() { _ = issuer.Close(ctx) }()
	exec := func(sql string) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	schema := pgx.Identifier{fmt.Sprintf("Books %d", time.Now().UnixNano())}.Sanitize()
	peer := pgx.Identifier{fmt.Sprintf("Peer %d", time.Now().UnixNano())}.Sanitize()
	for _, name := range []string{schema, peer} {
		exec("CREATE SCHEMA " + name)
		defer exec("DROP SCHEMA " + name + " CASCADE")
	}
	check := func(allowed bool) {
		t.Helper()
		tx, err := issuer.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		oid, quoted, err := pinAccountingNamespace(ctx, tx)
		if !allowed {
			if err == nil {
				t.Fatal("accepted an absent, ambiguous or writable accounting namespace")
			}
			return
		}
		if err != nil || quoted != schema {
			t.Fatalf("wrong accounting namespace: %q, %v", quoted, err)
		}
		var matches bool
		err = tx.QueryRow(ctx, `SELECT pg_catalog.to_regnamespace($1)::oid=$2::oid
			AND pg_catalog.current_setting('search_path')=$3`, quoted, oid, "pg_catalog,"+schema+",pg_temp").Scan(&matches)
		if err != nil || !matches {
			t.Fatal("issuer transaction scope does not match the anchored schema", err)
		}
	}
	check(false)
	exec("CREATE TABLE " + schema + ".ledger_entries (id int) PARTITION BY RANGE(id)")
	check(false) // A ledger exists but this login has no schema USAGE.
	exec("GRANT USAGE ON SCHEMA " + schema + " TO pilot_venue_proof")
	check(true)
	exec("GRANT CREATE ON SCHEMA " + schema + " TO pilot_venue_proof")
	check(false)
	exec("REVOKE CREATE ON SCHEMA " + schema + " FROM pilot_venue_proof")
	exec("CREATE TABLE " + peer + ".ledger_entries (id int)")
	check(false) // The second anchor counts even without USAGE on its schema.
	exec("DROP TABLE " + peer + ".ledger_entries")
	check(true)
}
