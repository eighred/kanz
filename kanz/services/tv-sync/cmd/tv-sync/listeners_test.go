package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/platform/httpserver"
)

func TestBindHTTPListenersClosesOperationsWhenAPIBindFails(t *testing.T) {
	// Reserve then release the first address so it can be checked for leakage
	// after the second bind fails against a real occupied socket.
	lc := net.ListenConfig{}
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	first, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	operationsAddr := first.Addr().String()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	servers := []*http.Server{
		httpserver.New(operationsAddr, http.NotFoundHandler(), httpserver.Standard()),
		httpserver.New(occupied.Addr().String(), http.NotFoundHandler(), httpserver.Standard()),
	}
	listeners, err := bindHTTPListeners(context.Background(), servers)
	if err == nil {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		t.Fatal("occupied API address did not refuse startup")
	}
	if !strings.Contains(err.Error(), occupied.Addr().String()) {
		t.Fatalf("startup did not reach the API bind: %v", err)
	}
	if listeners != nil {
		t.Fatal("failed startup returned listeners")
	}
	rebound, err := lc.Listen(context.Background(), "tcp", operationsAddr)
	if err != nil {
		t.Fatalf("operations socket leaked after failed startup: %v", err)
	}
	_ = rebound.Close()
}
