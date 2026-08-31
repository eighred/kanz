package grpcsrv_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/services/risk-engine/internal/grpcsrv"
)

// THE REFUSAL MUST REACH THE CALLER AS INVALID_ARGUMENT (#859).
//
// #859's "Verified when" asks for INVALID_ARGUMENT naming the field, and that is
// a claim about the WIRE, not about a Go sentinel. Three things have to line up
// for it: the engine must refuse, the refusal must stay inside the
// ErrInvalidRequest family, and mapError must not have an earlier case that
// catches it first. Each is individually plausible and the combination is what
// the caller actually experiences.
//
// THE REAL ENGINE, NOT THE fakeEngine THIS PACKAGE USES ELSEWHERE. A fake
// returning a hand-made error would prove the mapping and assume the refusal —
// and the refusal living in a different package is exactly the seam where "it
// returns the right sentinel" and "the right code comes out" drift apart. The
// only thing stubbed here is the state store, which starts empty because the
// refusal precedes any read of it.
func realEngineServer() *grpcsrv.Server {
	s := state.NewStore()
	return grpcsrv.New(engine.New(s, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector()), "acme")
}

func TestExposureWithAsOfIsInvalidArgumentOnTheWire(t *testing.T) {
	srv := realEngineServer()

	_, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{
		PortfolioId: "PORT-1",
		AsOf:        timestamppb.New(time.Now().Add(-7 * 24 * time.Hour)),
	})
	if err == nil {
		t.Fatal("a pinned exposure query succeeded over gRPC — the caller is being handed live " +
			"state under a historical label (#859)")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err %v is not a gRPC status — the gateway renders a non-status error as a 500 "+
			"with an opaque body, so the caller cannot tell a bad request from an outage", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument.\n\nNotFound would send the caller looking for "+
			"a portfolio; Internal reads as a bug and trains a retry. The gateway maps "+
			"InvalidArgument to 400 and passes the message through, which is the only code that "+
			"tells the caller they can fix this themselves.\n\nmessage: %s", st.Code(), st.Message())
	}
	if !strings.Contains(st.Message(), "as_of") {
		t.Errorf("message does not name the field: %s", st.Message())
	}
}

func TestMeasuresWithAsOfIsInvalidArgumentOnTheWire(t *testing.T) {
	srv := realEngineServer()

	_, err := srv.Measures(context.Background(), &querypb.MeasuresRequest{
		PortfolioId: "PORT-1",
		AsOf:        timestamppb.New(time.Now().Add(-7 * 24 * time.Hour)),
	})
	st, ok := status.FromError(err)
	if err == nil || !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("Measures with as_of returned (%v) — want an InvalidArgument status. A "+
			"MeasureSet carries VaR and the sensitivities a desk hedges on", err)
	}
	if !strings.Contains(st.Message(), "as_of") {
		t.Errorf("message does not name the field: %s", st.Message())
	}
}

// AN UNPINNED QUERY MUST STILL REACH THE ENGINE. With an empty store that is
// NotFound — which is the point: it proves the request travelled the whole way
// and was answered on its merits rather than refused at the door. A refusal
// firing on the zero timestamp would take every real caller down, since
// services/mcp and services/copilot both send requests without as_of.
func TestAnUnpinnedQueryIsNotRefusedByTheAsOfCheck(t *testing.T) {
	srv := realEngineServer()

	_, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{PortfolioId: "PORT-1"})
	st, ok := status.FromError(err)
	if err == nil || !ok {
		t.Fatalf("want a status error for an unknown portfolio, got %v", err)
	}
	if st.Code() == codes.InvalidArgument {
		t.Fatalf("an unpinned query was refused as InvalidArgument: %s\n\nThe as_of check is "+
			"firing on the zero value, so every first-party caller — none of which sets as_of — "+
			"is now refused and the risk read plane is down", st.Message())
	}
	if st.Code() != codes.NotFound {
		t.Fatalf("code = %s, want NotFound for a portfolio with no state", st.Code())
	}
}
