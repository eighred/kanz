package config

import "testing"

func TestLakeSnapshotGatewayRequiresEveryExplicitBoundary(t *testing.T) {
	settings := map[string]string{
		"API_GATEWAY_LAKE_SNAPSHOT_ADDR":        "lake-sink:8086",
		"API_GATEWAY_LAKE_SNAPSHOT_TENANT":      "tenant-a",
		"API_GATEWAY_LAKE_SNAPSHOT_PEER_ID":     "spiffe://fixture/ns/kanz-services/sa/lake-sink",
		"API_GATEWAY_LAKE_SNAPSHOT_CREATE_ROLE": "snapshot-create",
		"API_GATEWAY_LAKE_SNAPSHOT_READ_ROLE":   "snapshot-read",
	}
	for key := range settings {
		t.Setenv(key, "")
	}
	if cfg, err := loadLakeSnapshot(""); cfg != nil || err != nil {
		t.Fatal("default gateway changed")
	}
	for key, value := range settings {
		t.Setenv(key, value)
	}
	if _, err := loadLakeSnapshot("unix:///fixture"); err != nil {
		t.Fatal("explicit config refused")
	}
	if _, err := loadLakeSnapshot(""); err == nil {
		t.Fatal("plaintext lake accepted")
	}
	for key := range settings {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if _, err := loadLakeSnapshot("unix:///fixture"); err == nil {
				t.Fatal("partial boundary accepted")
			}
		})
	}
	t.Setenv("API_GATEWAY_LAKE_SNAPSHOT_PEER_ID", "spiffe://fixture/ns/kanz-services/sa/other")
	if _, err := loadLakeSnapshot("unix:///fixture"); err == nil {
		t.Fatal("wrong peer accepted")
	}
}

func TestDedicatedCashHistoryCannotAcquireLakeSnapshotCapabilities(t *testing.T) {
	devAuthEnv(t)
	for key, value := range map[string]string{
		"API_GATEWAY_SPIFFE_SOCKET": "unix:///synthetic-spire.sock", "API_GATEWAY_HISTORY_LISTEN": ":8443", "API_GATEWAY_BFF_TLS_LISTEN": ":9443",
		"API_GATEWAY_CASH_HISTORY_SIGNING_SECRET": "test-only-history",
		"API_GATEWAY_ACCOUNTING_ADDR":             "http://accounting:8101", "API_GATEWAY_CASH_HISTORY_ROLE": "oms-history",
		"API_GATEWAY_LAKE_SNAPSHOT_ADDR": "lake-sink:8086", "API_GATEWAY_LAKE_SNAPSHOT_TENANT": "tenant-a",
		"API_GATEWAY_LAKE_SNAPSHOT_PEER_ID":     "spiffe://fixture/ns/kanz-services/sa/lake-sink",
		"API_GATEWAY_LAKE_SNAPSHOT_CREATE_ROLE": "snapshot-create", "API_GATEWAY_LAKE_SNAPSHOT_READ_ROLE": "snapshot-read",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil || cfg.HistoryListen != ":8443" || cfg.BFFTLSListen != ":9443" || cfg.LakeSnapshot == nil {
		t.Fatalf("independent workload listeners and roles: %v", err)
	}
	for _, name := range []string{"API_GATEWAY_LAKE_SNAPSHOT_CREATE_ROLE", "API_GATEWAY_LAKE_SNAPSHOT_READ_ROLE"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "oms-history")
			if _, err := Load(); err == nil {
				t.Fatal("cash-history role acquired independent lake authority")
			}
		})
	}
}

func TestOMSHistoryListenerRequiresIndependentSigningAuthority(t *testing.T) {
	devAuthEnv(t)
	t.Setenv("API_GATEWAY_SPIFFE_SOCKET", "unix:///synthetic-spire.sock")
	t.Setenv("API_GATEWAY_HISTORY_LISTEN", ":8443")
	t.Setenv("API_GATEWAY_SIGNING_SECRET", "fixture-public")
	t.Setenv("API_GATEWAY_ACCOUNTING_ADDR", "http://accounting:8101")
	t.Setenv("API_GATEWAY_CASH_HISTORY_ROLE", "oms-history")
	t.Setenv("API_GATEWAY_CASH_HISTORY_SIGNING_SECRET", "fixture-history")
	for _, tc := range []struct{ name, setting, value string }{
		{"no key", "API_GATEWAY_CASH_HISTORY_SIGNING_SECRET", ""},
		{"empty key", "API_GATEWAY_CASH_HISTORY_SIGNING_SECRET", "   "},
		{"shared public key", "API_GATEWAY_CASH_HISTORY_SIGNING_SECRET", "fixture-public"},
		{"missing declared mount", "API_GATEWAY_CASH_HISTORY_SIGNING_SECRET_FILE", t.TempDir() + "/absent"},
		{"no workload role", "API_GATEWAY_CASH_HISTORY_ROLE", ""},
		{"no listener", "API_GATEWAY_HISTORY_LISTEN", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.setting, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("incomplete or shared OMS signing authority accepted")
			}
		})
	}
}
