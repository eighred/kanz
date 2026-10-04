package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type historySourceFixture struct{}

func (historySourceFixture) GetX509SVID() (*x509svid.SVID, error) { return nil, nil }
func (historySourceFixture) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, nil
}

func TestCapitalHistoryDeclaredConfigurationFailsLoudly(t *testing.T) {
	t.Setenv("OMS_CASH_HISTORY_TOKEN", "")
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", "")
	if source, err := capitalCashHistory(config.Config{}, nil); err != nil || source != nil {
		t.Fatalf("unconfigured history: %v %v", source, err)
	}
	cfg := config.Config{CashHistoryGateway: "https://gateway"}
	if _, err := capitalCashHistory(cfg, historySourceFixture{}); err == nil {
		t.Fatal("declared history accepted absent authentication")
	}
	t.Setenv("OMS_CASH_HISTORY_TOKEN", "test-only")
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := capitalCashHistory(cfg, historySourceFixture{}); err == nil {
		t.Fatal("unreadable secret mount fell back to environment")
	}
	t.Setenv("OMS_CASH_HISTORY_TOKEN_FILE", "")
	if source, err := capitalCashHistory(cfg, historySourceFixture{}); err != nil || source == nil {
		t.Fatalf("declared authenticated history could not initialize: %v", err)
	}
	if _, err := capitalCashHistory(cfg, nil); err == nil {
		t.Fatal("declared history accepted absent mTLS source")
	}
	cfg.CashHistoryGateway = "http://gateway"
	if _, err := capital.NewHistoryClient(cfg.CashHistoryGateway, nil, func(context.Context) (string, error) { return "test-only", nil }); err == nil {
		t.Fatal("credential-bearing plaintext history accepted")
	}
}
