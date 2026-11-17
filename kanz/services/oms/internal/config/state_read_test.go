package config

import "testing"

func TestStateQueryRequiresSeparateAuthenticatedListener(t *testing.T) {
	t.Setenv("OMS_STATE_QUERY_LISTEN", "127.0.0.1:9091")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "")
	if _, err := Load(); err == nil {
		t.Fatal("explicit state listener accepted without identity")
	}
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "unix:///fixture/workload.sock")
	t.Setenv("OMS_GRPC_LISTEN", "127.0.0.1:9091")
	if _, err := Load(); err == nil {
		t.Fatal("state projection shared the broader query listener")
	}
	t.Setenv("OMS_GRPC_LISTEN", "127.0.0.1:9090")
	t.Setenv("OMS_NATS_URL", "nats://fixture.invalid:4222")
	t.Setenv("OMS_DATABASE_URL", "postgres://fixture.invalid/recorded_state")
	if _, err := Load(); err != nil {
		t.Fatalf("complete separated configuration refused: %v", err)
	}
}
