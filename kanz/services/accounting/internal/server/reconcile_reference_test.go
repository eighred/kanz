package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAdHocReconcileKeepsBalancesOnlyWhenRawReferencesRepeat(t *testing.T) {
	s, store := newServer(t)
	for _, id := range []string{"execution-a", "execution-b"} {
		entry := accountTrade(id, "PF", "account", "BTC-USD", 1, -100)
		entry.SourceRef = "same-raw-ref"
		if err := store.Append(context.Background(), entry, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.Journal(t.Context(), "PF")
	if err != nil || len(before) != 2 {
		t.Fatal("fixture did not preserve distinct journal identities", err)
	}
	response := reconcileAs(t, s, "PF", `{"custodian_id":"CUST-A","positions":{"BTC-USD":"2"},"cash":{"USD":"-200"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("ad-hoc balances read acquired a transaction policy: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Count           int    `json:"count"`
		Grain           string `json:"grain"`
		TransactionPass string `json:"transaction_pass"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 || result.Grain != "balances_only" || result.TransactionPass != "grain_unknown" {
		t.Fatalf("date-free HTTP contract changed: %+v", result)
	}
	after, err := store.Journal(t.Context(), "PF")
	if err != nil || len(after) != len(before) {
		t.Fatal("read-only comparison changed journal history", err)
	}
}
