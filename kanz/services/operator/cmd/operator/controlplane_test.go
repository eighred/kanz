package main

import (
	"strings"
	"testing"
)

// The operator's gRPC surface provisions nodes and writes exchange API keys. It
// used to be plaintext, defended only by a NetworkPolicy that admitted nothing but
// the health port — so the ONLY way to call it was a kubeconfig-gated
// `kubectl port-forward`. That made kubectl a routine operational dependency and
// left the RPC itself unauthenticated: anything that reached the socket was
// trusted completely.
//
// These tests pin the replacement. The rule they encode is the one the api-gateway
// already follows — the identity authority has no anonymous mode — and its point
// is that absence of configuration must never quietly become trust.
//
// THESE TESTS ARE THE ENFORCEMENT. The design document that first recorded the
// rule was deleted on 2026-07-29, so a comment citing it would point at nothing;
// what makes the rule real here is that removing the socket check turns these
// red.

func TestAuthorizedClientsRequiresASocket(t *testing.T) {
	// Without the workload-API socket there is no SVID, so the server cannot
	// authenticate anyone. Starting anyway would serve node provisioning and
	// credential writes to any caller that reached the port.
	_, err := authorizedClients("", "spiffe://kanz.internal/ns/kanz-services/sa/api-gateway")
	if err == nil {
		t.Fatal("authorizedClients accepted an empty SPIFFE socket. The operator must REFUSE " +
			"to start without one — a plaintext control plane that provisions nodes and writes " +
			"exchange credentials is the defect this replaces, not a degraded mode of it.")
	}
	if !strings.Contains(err.Error(), "SPIFFE_ENDPOINT_SOCKET") {
		t.Errorf("error must name the variable an operator has to set; got: %v", err)
	}
}

func TestAuthorizedClientsRequiresAtLeastOneClient(t *testing.T) {
	// mTLS with no authorized peer list would admit every workload in the trust
	// domain — every service in the estate holds a valid SVID. Authentication is
	// not authorization.
	_, err := authorizedClients("unix:///run/spiffe/spire-agent.sock", "")
	if err == nil {
		t.Fatal("authorizedClients accepted an empty client list. An empty allow-list is not " +
			"'allow nobody' by accident of configuration — it must be refused, because every " +
			"workload in the trust domain carries a valid SVID and would otherwise be admitted.")
	}
	if !strings.Contains(err.Error(), "OPERATOR_ALLOWED_CLIENTS") {
		t.Errorf("error must name the variable an operator has to set; got: %v", err)
	}
}

func TestAuthorizedClientsRejectsAMalformedID(t *testing.T) {
	// A typo must fail startup, not silently shrink the allow-list. Dropping an
	// unparseable entry and continuing would produce a server that refuses the
	// caller it was configured to admit, diagnosable only from the peer's side.
	_, err := authorizedClients("unix:///run/spiffe/spire-agent.sock",
		"spiffe://kanz.internal/ns/kanz-services/sa/api-gateway,not-a-spiffe-id")
	if err == nil {
		t.Fatal("authorizedClients accepted a malformed SPIFFE ID. A typo in the allow-list " +
			"must fail loudly at startup rather than silently narrowing who may call.")
	}
	if !strings.Contains(err.Error(), "not-a-spiffe-id") {
		t.Errorf("error must quote the offending entry so it can be found; got: %v", err)
	}
}

func TestAuthorizedClientsParsesAndTrims(t *testing.T) {
	ids, err := authorizedClients("unix:///run/spiffe/spire-agent.sock",
		" spiffe://kanz.internal/ns/kanz-services/sa/api-gateway , spiffe://kanz.internal/ns/kanz-operator/sa/operator ")
	if err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 authorized clients, got %d: %v", len(ids), ids)
	}
	if got := ids[0].String(); got != "spiffe://kanz.internal/ns/kanz-services/sa/api-gateway" {
		t.Errorf("surrounding whitespace must be trimmed; got %q", got)
	}
}

func TestAuthorizedClientsIgnoresEmptyEntries(t *testing.T) {
	// A trailing comma is a formatting slip, not a request to admit "".
	ids, err := authorizedClients("unix:///run/spiffe/spire-agent.sock",
		"spiffe://kanz.internal/ns/kanz-services/sa/api-gateway,")
	if err != nil {
		t.Fatalf("a trailing comma must not be an error: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 authorized client, got %d: %v", len(ids), ids)
	}
}

func TestAuthorizedClientsRefusesAListOfOnlySeparators(t *testing.T) {
	// ",," trims to nothing. It must land on the empty-list refusal above rather
	// than producing a server with an empty authorizer.
	_, err := authorizedClients("unix:///run/spiffe/spire-agent.sock", " , , ")
	if err == nil {
		t.Fatal("a list containing only separators yields zero clients and must be refused " +
			"exactly like an empty one — otherwise it is an empty allow-list reached by a " +
			"different spelling.")
	}
	if !strings.Contains(err.Error(), "OPERATOR_ALLOWED_CLIENTS") {
		t.Errorf("error must name the variable; got: %v", err)
	}
}
