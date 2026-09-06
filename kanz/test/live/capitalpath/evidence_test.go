package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestEvidenceIsMachineReadableAndContainsNoCredentialMaterial(t *testing.T) {
	now := testNow()
	cfg := validConfig(now)
	cfg.token = "TOP-SECRET-TOKEN"
	cfg.ledgerDSN = "postgres://reader:TOP-SECRET-PASSWORD@db/kanz"
	fills := []*orderpb.Fill{fillEvent(true, "fill-1").(*orderpb.OrderFilled).GetFill()}
	record := buildEvidence(cfg, "order-1", fills, now.Add(-time.Second), now)
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{cfg.token, cfg.ledgerDSN, "TOP-SECRET", "postgres://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("evidence leaked %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{"order-1", "fill-1", cfg.dr.EvidenceSHA256, "ledger_exact"} {
		if !strings.Contains(text, required) {
			t.Fatalf("evidence missing %q: %s", required, text)
		}
	}
}
