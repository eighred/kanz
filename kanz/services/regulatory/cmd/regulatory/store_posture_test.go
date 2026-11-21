package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/services/regulatory/internal/config"
)

// Original signature semantics and artifact durability are independent. The
// former DSN-free hash / ephemeral-chain production paths could not commit the
// required filing+context+FACT transaction and are deliberately retired.
func TestOpenRecordingPoolRequiresDurabilityInBothSignatureModes(t *testing.T) {
	for _, kind := range []string{"chain", "hash"} {
		for _, ephemeral := range []bool{false, true} {
			t.Run(kind+"/ephemeral="+map[bool]string{false: "false", true: "true"}[ephemeral], func(t *testing.T) {
				pool, err := openRecordingPool(context.Background(), config.Config{Signer: kind, Tenant: "__system__", NATSURL: "nats://unused.invalid:4222", AllowEphemeralChain: ephemeral})
				if pool != nil {
					pool.Close()
					t.Fatal("missing DSN returned a pool")
				}
				if err == nil || !strings.Contains(err.Error(), "REGULATORY_DATABASE_URL") || !strings.Contains(err.Error(), "both signature modes") {
					t.Fatalf("missing recording durability refusal: %v", err)
				}
			})
		}
	}
}

func TestOpenRecordingPoolRefusesInvalidScopeBeforeDial(t *testing.T) {
	for _, tc := range []struct{ name, tenant, broker, kind string }{
		{"unscoped", "", "nats://unused.invalid:4222", "chain"},
		{"no-broker", "__system__", "", "chain"},
		{"unknown-signature", "__system__", "nats://unused.invalid:4222", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := openRecordingPool(context.Background(), config.Config{DatabaseURL: "postgres://unused.invalid/fixture", Tenant: tc.tenant, NATSURL: tc.broker, Signer: tc.kind})
			if pool != nil {
				pool.Close()
				t.Fatal("invalid configuration returned a pool")
			}
			if err == nil || !strings.Contains(err.Error(), "explicit tenant, broker and supported signature mode") {
				t.Fatalf("expected pre-dial scope refusal: %v", err)
			}
		})
	}
}

func TestHashSignerRetainsPlainContentHashMeaning(t *testing.T) {
	body := []byte("FORM_PF\n2026-06-30T00:00:00Z\nfixture=1\n")
	got, err := (filing.HashSigner{}).Sign(body)
	want := sha256.Sum256(body)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("hash signer changed: %v", err)
	}
}
