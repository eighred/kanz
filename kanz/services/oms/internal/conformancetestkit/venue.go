package conformancetestkit

import (
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
	"net"
	"testing"
	"time"
)

// StartVenue owns only a loopback transport. The caller supplies the independent
// typed response behavior; this helper computes no financial or recovery state.
func StartVenue(t *testing.T, adapter venuepb.VenueAdapterServiceServer, address string, options ...grpc.ServerOption) (string, *grpc.Server) {
	t.Helper()
	if address == "" {
		address = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("venue fixture must bind literal loopback")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(options...)
	venuepb.RegisterVenueAdapterServiceServer(server, adapter)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("venue server did not close")
		}
	})
	return listener.Addr().String(), server
}
