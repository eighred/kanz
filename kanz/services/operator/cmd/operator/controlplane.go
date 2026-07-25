package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"

	"github.com/kanz-eng/kanz/pkg/transport"
)

// THE CONTROL PLANE AUTHENTICATES ITS CALLER (OPS-M2a).
//
// This gRPC surface provisions nodes, cordons and drains them, and writes exchange
// API keys — the credentials that move a fund's money. It used to be a PLAINTEXT
// listener with no authentication of any kind, defended solely by a NetworkPolicy
// that admitted nothing but the health port. The consequence was not only that
// anything reaching the socket was trusted completely; it was that the ONLY way to
// call it at all was `kubectl port-forward`, which made kubectl a routine
// operational dependency and put a cluster credential in every operator's hands.
//
// Both halves are replaced by one mechanism: the peer presents an SVID, the server
// verifies it, and the server admits only the identities it was configured to admit.
// Authentication and authorization are separate — every workload in the trust domain
// holds a valid SVID, so verifying one proves the caller is *some* Kanz workload and
// nothing more. The allow-list is what makes it the gateway.
//
// DENY BY DEFAULT, following the api-gateway precedent (KANZ_BRAIN: "The identity
// authority has NO anonymous mode, and adding one is the wrong fix"). There is
// deliberately no plaintext escape hatch: an env var whose only purpose is to
// re-open an unauthenticated control plane is one manifest typo away from
// production, and nothing needs it.

// authorizedClients validates the control-plane access configuration and parses the
// allow-list. It is separate from source creation so the whole validation surface is
// unit-testable without a running SPIRE agent.
func authorizedClients(socket, list string) ([]spiffeid.ID, error) {
	if strings.TrimSpace(socket) == "" {
		return nil, fmt.Errorf("SPIFFE_ENDPOINT_SOCKET is empty: the operator cannot " +
			"authenticate callers without a workload-API socket, and it will not serve node " +
			"provisioning or venue-credential writes unauthenticated. Mount the SPIFFE CSI " +
			"volume and set SPIFFE_ENDPOINT_SOCKET")
	}

	var ids []spiffeid.ID
	for _, raw := range strings.Split(list, ",") {
		entry := strings.TrimSpace(raw)
		// A trailing comma is a formatting slip, not a request to admit "". Skipping
		// empties here is safe because a list that yields ZERO ids is refused below —
		// so ",," cannot become an empty allow-list by a different spelling.
		if entry == "" {
			continue
		}
		id, err := spiffeid.FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("OPERATOR_ALLOWED_CLIENTS contains %q, which is not a valid "+
				"SPIFFE ID: %w. A typo must fail startup rather than silently shrink the "+
				"allow-list — a server that refuses the caller it was configured to admit is "+
				"diagnosable only from the peer's side", entry, err)
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("OPERATOR_ALLOWED_CLIENTS is empty: mTLS with no authorized peer " +
			"list admits EVERY workload in the trust domain, because every one of them carries a " +
			"valid SVID. Authentication is not authorization. Name the identities that may call " +
			"this control plane (e.g. the api-gateway's)")
	}
	return ids, nil
}

// controlPlaneServerOption returns the gRPC server option that makes the operator's
// listener mutually authenticated and restricted to the configured callers.
func controlPlaneServerOption(ctx context.Context, socket, list string) (grpc.ServerOption, []spiffeid.ID, error) {
	ids, err := authorizedClients(socket, list)
	if err != nil {
		return nil, nil, err
	}
	src, err := transport.NewSource(ctx, socket)
	if err != nil {
		return nil, nil, fmt.Errorf("control-plane SVID source: %w", err)
	}
	return transport.ServerOption(src, transport.AuthorizeServices(ids...)), ids, nil
}

// spiffeIDStrings renders the allow-list for the startup log. Who may call the
// control plane is stated at boot, not left to be inferred from a manifest.
func spiffeIDStrings(ids []spiffeid.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}
