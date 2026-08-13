package gateway_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// THE EDGE MUST NOT RELAY AN UPSTREAM 5xx's TEXT (#440).
//
// httpStatus returned st.Message() on every arm, including the default — and
// risk-engine's mapError puts the RAW error into exactly that code:
//
//	default:
//		return status.Error(codes.Internal, err.Error())
//
// so an internal failure's text travelled upstream → gateway → API client. A pgx
// error carries SQL and constraint names; a transport error carries internal
// host:port. Neither is readable by an API client on a multi-tenant platform.
//
// THE RULE IS BY DIRECTION, and it is the one webhook-ingest already follows:
// a 4xx describes what the CALLER sent, so it keeps detail; a 5xx describes a
// fault of OURS, so the client gets a constant and the operator gets the detail
// in a log line.
//
// leakyDetail is written to look like what actually leaks — a Postgres error
// naming a table and a constraint.
const leakyDetail = `pq: duplicate key value violates unique constraint "orders_pkey" on relation "oms.orders"`

// fiveXXCodes are the gRPC codes that mean "our fault, or our upstream's".
// Every one must answer with a constant body.
func TestAn5xxDoesNotRelayUpstreamDetail(t *testing.T) {
	for _, code := range []codes.Code{
		codes.Internal,
		codes.Unknown,
		codes.DataLoss,
		codes.Unavailable,
		codes.DeadlineExceeded,
		codes.Unimplemented,
	} {
		fc := &fakeClient{err: status.Error(code, leakyDetail)}
		ts := serve(t, fc)
		resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
		if err != nil {
			t.Fatal(err)
		}
		body := string(readAll(t, resp))
		resp.Body.Close()

		if resp.StatusCode < 500 {
			t.Errorf("gRPC %v ⇒ HTTP %d, want a 5xx", code, resp.StatusCode)
		}
		// The whole point. Any fragment of the upstream text is a leak.
		for _, fragment := range []string{"pq:", "orders_pkey", "oms.orders", "constraint"} {
			if strings.Contains(body, fragment) {
				t.Errorf("gRPC %v ⇒ body %q contains upstream detail %q.\n"+
					"An API client must not be able to read an internal error's text: it names "+
					"tables, constraints and hosts. Return a constant on 5xx and log the detail.",
					code, body, fragment)
			}
		}
	}
}

// The same rule one level up: an error that is not a gRPC status at all.
//
// That arm is reached by dial and transport failures, whose text IS the internal
// topology — which made it the worst of the three places this leaked.
func TestANonStatusErrorDoesNotRelayItsText(t *testing.T) {
	const internalTopology = "dial tcp 10.0.3.12:9090: connect: connection refused"
	fc := &fakeClient{err: errors.New(internalTopology)}
	ts := serve(t, fc)
	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	body := string(readAll(t, resp))
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("a non-status error ⇒ HTTP %d, want 500", resp.StatusCode)
	}
	for _, fragment := range []string{"10.0.3.12", "9090", "dial tcp"} {
		if strings.Contains(body, fragment) {
			t.Fatalf("body %q contains %q — the internal address of an upstream reached the client",
				body, fragment)
		}
	}
}

// A 4xx STILL CARRIES ITS REASON. Without this, the rule above is satisfied by a
// gateway that says nothing on any error — which would be a worse API than the
// leaking one, and the failure mode a "return a constant" fix walks into.
//
// InvalidArgument is the caller's own input: telling them what was wrong with it
// is the entire purpose of a 400.
func TestA4xxStillCarriesItsReason(t *testing.T) {
	const reason = "as_of must not be in the future"
	fc := &fakeClient{err: status.Error(codes.InvalidArgument, reason)}
	ts := serve(t, fc)
	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	body := string(readAll(t, resp))
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, reason) {
		t.Fatalf("body %q dropped the reason %q — a 400 that will not say what was wrong "+
			"forces every caller to guess", body, reason)
	}
}

// THE WHOLE gRPC TABLE, NOT THE THREE SOMEBODY IMPLEMENTED (#440).
//
// gateway.go says the mapping "mirrors the standard grpc-gateway table so REST
// clients see conventional statuses". It mirrored six of seventeen; the other
// eleven fell to a default answering 500. TestErrorMapping covered exactly the
// three that were implemented, which is why the gap was invisible — a table test
// only tests the rows in the table.
//
// The cost is the shape #434 and #439 documented for webhook-ingest: a 5xx is
// RETRYABLE and blames this platform, so a client-cancelled request and a
// caller-side precondition failure both read as "kanz is broken" and both invite
// a retry. It also puts faults that are not ours into the error-rate SLO.
func TestErrorMappingMirrorsTheGrpcGatewayTable(t *testing.T) {
	cases := []struct {
		code codes.Code
		want int
	}{
		{codes.Canceled, 499}, // client closed request (grpc-gateway's own code)
		{codes.Unknown, http.StatusInternalServerError},
		{codes.InvalidArgument, http.StatusBadRequest},
		{codes.DeadlineExceeded, http.StatusGatewayTimeout},
		{codes.NotFound, http.StatusNotFound},
		{codes.AlreadyExists, http.StatusConflict},
		{codes.PermissionDenied, http.StatusForbidden},
		{codes.ResourceExhausted, http.StatusTooManyRequests},
		{codes.FailedPrecondition, http.StatusBadRequest},
		{codes.Aborted, http.StatusConflict},
		{codes.OutOfRange, http.StatusBadRequest},
		{codes.Unimplemented, http.StatusNotImplemented},
		{codes.Internal, http.StatusInternalServerError},
		{codes.Unavailable, http.StatusServiceUnavailable},
		{codes.DataLoss, http.StatusInternalServerError},
		{codes.Unauthenticated, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		fc := &fakeClient{err: status.Error(tc.code, "x")}
		ts := serve(t, fc)
		resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("gRPC %v ⇒ HTTP %d, want %d", tc.code, resp.StatusCode, tc.want)
		}
	}
}
