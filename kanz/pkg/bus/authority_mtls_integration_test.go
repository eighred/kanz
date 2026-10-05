package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

// Exercise production SVID mappings and PubAck permissions. A successful
// connection alone does not establish permission to persist evidence.
func TestNATSMTLS_AuthorityPublishersAreRestrictedToEvidence(t *testing.T) {
	url, dir := mtlsEnv(t)
	for _, name := range []string{"identity", "webbff"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name, PublishTimeout: time.Second,
				TLSConfig: transport.ClientTLSConfig(sourceFromDir(t, dir, name), transport.AuthorizeMesh())})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			for _, subject := range []string{"audit.authority.decision", "order.test", "audit.authority.decision"} {
				err = client.Publish(ctx, bus.Message{Subject: subject, Body: []byte("authority permission probe")})
				if subject == "order.test" {
					if err == nil {
						t.Fatal("authority publisher could inject capital-path traffic")
					}
				} else if err != nil {
					t.Fatalf("authority evidence was not acknowledged: %v", err)
				}
			}
		})
	}
}
