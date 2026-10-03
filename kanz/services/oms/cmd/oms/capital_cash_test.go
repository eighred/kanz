package main

import (
	"path/filepath"
	"testing"

	"github.com/eighred/kanz/services/oms/internal/config"
)

func TestCapitalHistoryDeclaredConfigurationFailsLoudly(t *testing.T) {
	t.Setenv("OMS_CASH_HISTORY_TOKEN", "")
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", "")
	if source, err := capitalCashHistory(config.Config{}); err != nil || source != nil {
		t.Fatalf("unconfigured history: %v %v", source, err)
	}
	cfg := config.Config{CashHistoryGateway: "https://gateway"}
	if _, err := capitalCashHistory(cfg); err == nil {
		t.Fatal("declared history accepted absent authentication")
	}
	t.Setenv("OMS_CASH_HISTORY_TOKEN", "test-only")
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := capitalCashHistory(cfg); err == nil {
		t.Fatal("unreadable secret mount fell back to environment")
	}
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", "")
	if source, err := capitalCashHistory(cfg); err != nil || source == nil {
		t.Fatalf("declared history could not initialize: %v", err)
	}
	cfg.CashHistoryGateway = "http://gateway"
	if _, err := capitalCashHistory(cfg); err == nil {
		t.Fatal("credential-bearing plaintext history accepted")
	}
}
