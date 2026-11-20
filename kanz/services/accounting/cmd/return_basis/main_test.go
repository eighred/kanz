package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
)

func TestSignedReturnBasisAdmissionAndReplay(t *testing.T) {
	owner, source, reader := os.Getenv("TEST_RETURN_BASIS_OWNER"), os.Getenv("TEST_RETURN_BASIS_SOURCE"), os.Getenv("TEST_RETURN_BASIS_READER")
	if owner == "" || source == "" || reader == "" {
		t.Skip("requires dedicated migrated PostgreSQL and distinct restricted source and reader roles")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	const tenant, portfolio, ccy = "basis-test", "PF1", "USD"
	key, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	authorities, err := pg.NewTenantPool(ctx, owner, tenant)
	if err != nil {
		t.Fatal("open migration owner")
	}
	defer authorities.Close()
	if _, err := authorities.Exec(ctx, `INSERT INTO return_basis_authorities (portfolio_id,currency,issuer_id,public_key,flow_scope)
		VALUES ($1,$2,'synthetic-close',$3,'fund-cash')`, portfolio, ccy, []byte(key)); err != nil {
		t.Fatal("enroll scoped synthetic issuer", err)
	}
	t.Setenv("ACCOUNTING_DATABASE_URL", source)
	t.Setenv("ACCOUNTING_TENANT", tenant)
	dir := t.TempDir()
	inputPath, sigPath := filepath.Join(dir, "close.json"), filepath.Join(dir, "close.sig")
	put := func(e returnbasis.Evidence, signer ed25519.PrivateKey) (string, error) {
		t.Helper()
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(inputPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sigPath, ed25519.Sign(signer, raw), 0600); err != nil {
			t.Fatal(err)
		}
		return run(ctx, inputPath, sigPath)
	}
	zero := uint64(0)
	original := returnbasis.Evidence{
		Format: returnbasis.Format, Tenant: tenant, Portfolio: portfolio, Currency: ccy, CloseDate: "2020-02-04",
		AsOf: time.Date(2020, 2, 4, 1, 0, 0, 0, time.UTC), Issuer: "synthetic-close",
		Opening: &returnbasis.Close{Reference: "nav-open-r1", Total: "100"},
		Closing: &returnbasis.Close{Reference: "nav-close-r1", Total: "102"},
		Flows:   &returnbasis.FlowCompletion{Scope: "fund-cash", Reference: "cash-close-r1", EventCount: &zero},
	}
	oldDigest, err := put(original, priv)
	if err != nil {
		t.Fatal(err)
	}
	if repeat, err := put(original, priv); err != nil || repeat != oldDigest {
		t.Fatal("exact replay changed receipt", err)
	}
	correction := original
	correction.Closing = &returnbasis.Close{Reference: "nav-close-r2", Total: "100"}
	if _, err := put(correction, priv); err == nil {
		t.Fatal("conflicting edition accepted")
	}
	correction.AsOf = correction.AsOf.Add(time.Hour)
	correction.PreviousDigest = oldDigest
	newDigest, err := put(correction, priv)
	if err != nil || newDigest == oldDigest {
		t.Fatal("corrected edition not retained", err)
	}
	incomplete := correction
	incomplete.AsOf = incomplete.AsOf.Add(time.Hour)
	incomplete.PreviousDigest = newDigest
	incomplete.Flows = nil
	if _, err := put(incomplete, priv); err != nil {
		t.Fatal("signed unknown must be retained without asserting zero", err)
	}
	_, unauthorizedKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put(original, unauthorizedKey); err == nil {
		t.Fatal("untrusted signing key accepted")
	}
	outside := original
	outside.Flows = &returnbasis.FlowCompletion{Scope: "partial-cash", Reference: "cash-close-r1", EventCount: &zero}
	if _, err := put(outside, priv); err == nil {
		t.Fatal("partial source scope accepted as fund completeness")
	}
	foreign := original
	foreign.Portfolio = "PF2"
	if _, err := put(foreign, priv); err == nil {
		t.Fatal("key reused for a different portfolio")
	}
	foreign = original
	foreign.Tenant = "other"
	if _, err := put(foreign, priv); err == nil {
		t.Fatal("foreign tenant accepted")
	}

	read, err := pg.NewTenantPool(ctx, reader, tenant)
	if err != nil {
		t.Fatal("open independent reader")
	}
	defer read.Close()
	var restricted bool
	if err := read.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("require restricted reader")
	}
	scope := bus.WithTenantID(ctx, tenant)
	store := returnbasis.NewStore(read)
	for _, edition := range []returnbasis.Evidence{original, correction, incomplete} {
		got, err := store.Read(scope, portfolio, ccy, edition.CloseDate, edition.AsOf)
		if err != nil || got.Evidence.Closing.Total != edition.Closing.Total || (got.Evidence.Flows == nil) != (edition.Flows == nil) {
			t.Fatalf("historical edition differs: %v", err)
		}
	}
	if _, err := store.Read(bus.WithTenantID(ctx, "other"), portfolio, ccy, original.CloseDate, original.AsOf); !errors.Is(err, returnbasis.ErrNotFound) {
		t.Fatal("reader accepted a mismatched tenant")
	}
	if _, err := store.Read(scope, "PF2", ccy, original.CloseDate, original.AsOf); !errors.Is(err, returnbasis.ErrNotFound) {
		t.Fatal("foreign portfolio appeared")
	}
}
