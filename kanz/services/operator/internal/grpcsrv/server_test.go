package grpcsrv

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
)

type stubReader struct {
	nodes    []estate.Node
	clusters []estate.Cluster
	err      error
}

func (s stubReader) ListNodes(context.Context) ([]estate.Node, error) {
	return s.nodes, s.err
}
func (s stubReader) ListClusters(context.Context) ([]estate.Cluster, error) {
	return s.clusters, s.err
}

func TestListNodesMapsStatusToEnum(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	srv := New(stubReader{nodes: []estate.Node{
		{Name: "london", Status: estate.StatusReady, Roles: []string{"control-plane"}, Region: "europe", KubeletVersion: "v1.31.3", CreatedAt: created},
		{Name: "tokyo", Status: estate.StatusNotReady, Region: "asia"},
		{Name: "ghost", Status: estate.StatusUnknown},
	}})
	resp, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(resp.GetNodes()) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(resp.GetNodes()))
	}
	got := map[string]*operatorpb.Node{}
	for _, n := range resp.GetNodes() {
		got[n.GetName()] = n
	}
	if got["london"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_READY {
		t.Errorf("london status = %v", got["london"].GetStatus())
	}
	if got["tokyo"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_NOT_READY {
		t.Errorf("tokyo status = %v", got["tokyo"].GetStatus())
	}
	if got["ghost"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED {
		t.Errorf("ghost status = %v, want UNSPECIFIED (deny-by-default)", got["ghost"].GetStatus())
	}
	if got["london"].GetCreatedAt().AsTime().UTC() != created {
		t.Errorf("london created_at = %v, want %v", got["london"].GetCreatedAt().AsTime(), created)
	}
	if got["tokyo"].GetCreatedAt() != nil {
		t.Errorf("tokyo created_at = %v, want nil (zero CreatedAt must not leak the epoch)", got["tokyo"].GetCreatedAt())
	}
	if got["ghost"].GetCreatedAt() != nil {
		t.Errorf("ghost created_at = %v, want nil (zero CreatedAt must not leak the epoch)", got["ghost"].GetCreatedAt())
	}
}

func TestListClustersMapsCounts(t *testing.T) {
	srv := New(stubReader{clusters: []estate.Cluster{{Region: "usa", Online: 2, Offline: 1}}})
	resp, err := srv.ListClusters(context.Background(), &operatorpb.ListClustersRequest{})
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(resp.GetClusters()) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(resp.GetClusters()))
	}
	c := resp.GetClusters()[0]
	if c.GetRegion() != "usa" || c.GetOnline() != 2 || c.GetOffline() != 1 {
		t.Errorf("cluster = %+v", c)
	}
}

func TestReaderErrorMapsToInternal(t *testing.T) {
	srv := New(stubReader{err: errors.New("boom")})
	_, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}
