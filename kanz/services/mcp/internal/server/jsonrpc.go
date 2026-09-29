// Package server is the MCP read plane's HTTP surface (#743).
//
// MCP IS AN AGENT-FACING READ PLANE AND NOTHING ELSE. AGENTS.md states the
// boundary and this package is where it is kept: it may not place, cancel or
// amend orders, may not call a venue, may not hold or proxy a venue credential,
// and may not bypass OMS/risk/compliance. It reaches state through authorized
// domain reads like any other caller, and every one of them is tenant-scoped
// through internal/agentgate — the same decision services/copilot runs, not a
// second copy of it.
//
// IT IS NOT THE VENUE TRANSPORT. The OMS → venue adapter boundary is typed
// gRPC/mTLS over venue.v1, buf-breaking-gated and credential-isolating; MCP
// would replace a compile-time-checked contract with a discovery-time-negotiated
// one, and the boundary it would provide already exists.
//
// # Why the protocol is hand-rolled
//
// The surface is three methods of JSON-RPC 2.0 over one POST route. The estate's
// precedent for a narrow protocol need is to write it — the gateway's rate
// limiter and REST transcoding, the venue websocket connectors, and the AUTH-01
// authorizer were each hand-rolled "over embedding X deliberately", on the
// grounds that a full runtime is dependency bloat when the decision is
// structurally narrow.
//
// There is a second reason here, and it is the load-bearing one. MCP's discovery
// model reports capability by OMISSION: a tool absent from tools/list is
// indistinguishable from a tool the server cannot perform. That is exactly the
// two-state trap #675 removed from this estate's capability contract, where
// "empty means DID NOT SAY, never SUPPORTS NOTHING". Rendering the tool list
// ourselves is what lets absence stay explicit rather than inferred.
package server

import (
	"encoding/json"
	"fmt"
)

// jsonRPCVersion is the only version this surface speaks. A request naming
// anything else is refused rather than assumed compatible.
const jsonRPCVersion = "2.0"

// JSON-RPC 2.0 error codes used here. The first four are the spec's; the last is
// this surface's own, in the implementation-defined range.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeUnauthorized is returned when the gate refuses. It is DISTINCT from
	// codeInvalidParams so a caller cannot read a refusal as a malformed request
	// and retry it differently.
	codeUnauthorized = -32001
)

// request is one JSON-RPC call.
//
// ID IS json.RawMessage because the spec permits a string, a number or null, and
// a response MUST echo it back unchanged. Decoding it into a typed field would
// make this surface answer "1" to a caller that asked with 1.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is one JSON-RPC reply. Result and Error are mutually exclusive.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ok(id json.RawMessage, result any) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

func fail(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// validate checks the envelope before any method runs.
func (r request) validate() error {
	if r.JSONRPC != jsonRPCVersion {
		return fmt.Errorf("jsonrpc must be %q", jsonRPCVersion)
	}
	if r.Method == "" {
		return fmt.Errorf("method is required")
	}
	return nil
}
