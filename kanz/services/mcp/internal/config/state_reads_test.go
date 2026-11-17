package config

import "testing"

func TestStateReadsRequireCompleteExplicitConfiguration(t *testing.T) {
	for _, key := range []string{"MCP_STATE_READS_ENABLED", "MCP_TENANT", "MCP_ORDER_STATE_QUERY_ADDR", "MCP_RISK_QUERY_ADDR", "SPIFFE_ENDPOINT_SOCKET"} {
		t.Setenv(key, "")
	}
	if cfg, err := Load(); err != nil || cfg.StateReads {
		t.Fatalf("default must retain the original tool set: %v %v", cfg, err)
	}
	t.Setenv("MCP_STATE_READS_ENABLED", "true")
	for _, key := range []string{"MCP_TENANT", "MCP_ORDER_STATE_QUERY_ADDR", "MCP_RISK_QUERY_ADDR", "SPIFFE_ENDPOINT_SOCKET"} {
		if _, err := Load(); err == nil {
			t.Fatalf("incomplete state-read configuration accepted before %s", key)
		}
		t.Setenv(key, "fixture")
	}
	if cfg, err := Load(); err != nil || !cfg.StateReads {
		t.Fatalf("complete selected configuration refused: %v %v", cfg, err)
	}
	t.Setenv("MCP_STATE_READS_ENABLED", "false")
	if _, err := Load(); err == nil {
		t.Fatal("explicit state-read settings silently ignored")
	}
	t.Setenv("MCP_STATE_READS_ENABLED", "perhaps")
	if _, err := Load(); err == nil {
		t.Fatal("malformed selection silently ignored")
	}
}
