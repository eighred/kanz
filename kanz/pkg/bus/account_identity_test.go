package bus

import (
	"context"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestNATSDisableReconnectAppliesNativeConnectionOption(t *testing.T) {
	endpoint := os.Getenv("TEST_NATS_URL")
	if endpoint == "" {
		t.Skip("TEST_NATS_URL required for real connection option proof")
	}
	u, err := url.Parse(endpoint)
	if err != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("fixture broker must be literal loopback")
	}
	for _, disabled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client, err := DialNATS(ctx, NATSConfig{URL: endpoint, Name: "snapshot-option-fixture", DisableReconnect: disabled})
		cancel()
		if err != nil {
			t.Fatal("fixture connection unavailable")
		}
		allow := client.conn.Opts.AllowReconnect
		_ = client.Close()
		if allow == disabled {
			t.Fatal("actual native reconnect policy differs")
		}
	}
}

func TestAccountIdentityRequiresActualAccountAndUser(t *testing.T) {
	const user = "spiffe://fixture/ns/kanz-services/sa/lake-sink"
	for _, tc := range []struct {
		raw     string
		allowed bool
	}{
		{`{"data":{"account":"tenant-a","user":"` + user + `"},"server":{"id":"server"}}`, true},
		{`{"data":{"account":"tenant-b","account_name":"tenant-a","user":"` + user + `"}}`, false},
		{`{"data":{"account":"tenant-a","user":"other"}}`, false},
		{`{"data":{"account_name":"tenant-a","user":"` + user + `"}}`, false},
		{`{"data":{"account":"tenant-a","user":"` + user + `"},"error":{"code":400}}`, false},
		{`{"data":null}`, false}, {`{}`, false}, {`{"data":`, false},
		{`{"data":{"account":"tenant-a","user":"` + user + `"}} {}`, false},
	} {
		if (checkAccountIdentity([]byte(tc.raw), "tenant-a", user) == nil) != tc.allowed {
			t.Fatal("account identity verdict differs")
		}
	}
}
