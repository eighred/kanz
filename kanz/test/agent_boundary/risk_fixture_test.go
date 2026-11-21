package agent_boundary_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This is a wire oracle, not a financial engine: production MCP/gateway clients
// receive adversarial canonical responses through an actual TCP gRPC server.
type riskFixture struct {
	querypb.UnimplementedRiskQueryServiceServer
	calls atomic.Int64
	stamp *timestamppb.Timestamp
}

func newRiskFixture(t *testing.T) (*riskFixture, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &riskFixture{stamp: timestamppb.New(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))}
	server := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(server, fixture)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return fixture, lis.Addr().String()
}

// The gateway correctly requires a workload identity to register its operator
// surface. Reuse the established synthetic Workload API for this isolated HTTP
// proof. It does not stand in for the separate genuine SPIRE/broker proof.
func gatewayMeshFixture(t *testing.T, risk *riskFixture) (socket, riskAddress, operatorAddress string) {
	t.Helper()
	domain, err := spiffeid.TrustDomainFromString(transport.TrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(namespace, serviceAccount string) *testidentity.Source {
		t.Helper()
		id, err := transport.ServiceID(namespace, serviceAccount)
		if err != nil {
			t.Fatal(err)
		}
		source, err := authority.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	gateway := issue("kanz-services", "api-gateway")
	gatewayID, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	_, socket = testidentity.StartWorkload(t, gateway)
	serve := func(source *testidentity.Source, register func(*grpc.Server)) string {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(transport.ServerOption(source, transport.AuthorizeServices(gatewayID)))
		register(server)
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		return listener.Addr().String()
	}
	riskAddress = serve(issue("kanz-services", "risk-engine"), func(server *grpc.Server) {
		querypb.RegisterRiskQueryServiceServer(server, risk)
	})
	operatorAddress = serve(issue("kanz-operator", "operator"), func(server *grpc.Server) {
		operatorpb.RegisterOperatorServiceServer(server, &operatorpb.UnimplementedOperatorServiceServer{})
	})
	return socket, riskAddress, operatorAddress
}

func (f *riskFixture) Exposure(_ context.Context, r *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	f.calls.Add(1)
	owner := "tenant_a"
	switch r.GetPortfolioId() {
	case "MISSING":
		return nil, status.Error(codes.NotFound, "private missing-resource detail")
	case "OWNER_DOWN":
		return nil, status.Error(codes.Unavailable, "private upstream diagnostic")
	case "OWNER_UNKNOWN":
		owner = ""
	case "PF_B":
		owner = "tenant_b"
	}
	return &querypb.ExposureResponse{PortfolioId: r.GetPortfolioId(), OwnerTenant: owner, AsOf: f.stamp}, nil
}

func (f *riskFixture) Measures(_ context.Context, r *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	f.calls.Add(1)
	m := &domainpb.RiskMeasure{Name: "VaR99", Value: &commonpb.Decimal{Coefficient: 42},
		Coverage: &domainpb.InputCoverage{Contributed: 2}, SourceEventIds: []string{"PRIVATE-SOURCE-IDENTITY"}}
	owner := "tenant_a"
	var flags []querypb.QualityFlag
	switch r.GetPortfolioId() {
	case "PF_B":
		owner = "tenant_b"
		m.Name = "PRIVATE-TENANT-MEASURE"
	case "PF_A_UNKNOWN":
		m.Coverage = &domainpb.InputCoverage{ExcludedCount: 1,
			Exclusions: []*domainpb.InputExclusion{{InstrumentId: "PRIVATE-INSTRUMENT", Reason: "no_terms"}}}
		flags = []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED}
	case "PF_A_STALE":
		flags = []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_STALE}
	}
	return &querypb.MeasuresResponse{PortfolioId: r.GetPortfolioId(), AsOf: f.stamp, OwnerTenant: owner,
		SourcePosition: &commonpb.LogPosition{Topic: "PRIVATE-LOG", Partition: 1, Offset: 23},
		QualityFlags:   flags, Set: &domainpb.RiskMeasureSet{PortfolioId: r.GetPortfolioId(), Measures: []*domainpb.RiskMeasure{m}}}, nil
}
