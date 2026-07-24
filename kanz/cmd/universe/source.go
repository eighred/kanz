package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"
)

// nodeSource fetches one snapshot of the estate. The gRPC-backed impl dials the
// operator service; tests use a stub. fetch does the clock read (Age), so
// render stays a pure function of model.
type nodeSource interface {
	fetch(ctx context.Context) (fetchMsg, error)
}

// fetchMsg carries one poll cycle's result into Update.
type fetchMsg struct {
	nodes    []nodeRow
	clusters []clusterRow
	err      error
}

// grpcSource dials the operator.v1 service over a plaintext connection — the
// laptop has no SVID; the security boundary is the kubeconfig-gated
// port-forward the connection runs inside.
type grpcSource struct {
	client operatorpb.OperatorServiceClient
}

func dialOperator(addr string) (*grpcSource, func() error, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return &grpcSource{client: operatorpb.NewOperatorServiceClient(conn)}, conn.Close, nil
}

func (g *grpcSource) fetch(ctx context.Context) (fetchMsg, error) {
	nodesResp, err := g.client.ListNodes(ctx, &operatorpb.ListNodesRequest{})
	if err != nil {
		return fetchMsg{}, err
	}
	clustersResp, err := g.client.ListClusters(ctx, &operatorpb.ListClustersRequest{})
	if err != nil {
		return fetchMsg{}, err
	}
	return fetchMsg{
		nodes:    toNodeRows(nodesResp.GetNodes()),
		clusters: toClusterRows(clustersResp.GetClusters()),
	}, nil
}

func toNodeRows(nodes []*operatorpb.Node) []nodeRow {
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeRow{
			Name:    n.GetName(),
			Status:  statusLabel(n.GetStatus()),
			Roles:   joinRoles(n.GetRoles()),
			Region:  n.GetRegion(),
			Version: n.GetKubeletVersion(),
			Age:     age(n.GetCreatedAt().AsTime()),
		})
	}
	return out
}

func toClusterRows(clusters []*operatorpb.Cluster) []clusterRow {
	out := make([]clusterRow, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, clusterRow{Region: c.GetRegion(), Online: int(c.GetOnline()), Offline: int(c.GetOffline())})
	}
	return out
}

func statusLabel(s operatorpb.NodeStatus) string {
	switch s {
	case operatorpb.NodeStatus_NODE_STATUS_READY:
		return "Ready"
	case operatorpb.NodeStatus_NODE_STATUS_NOT_READY:
		return "NotReady"
	default:
		return "Unknown"
	}
}

func joinRoles(roles []string) string {
	if len(roles) == 0 {
		return "-"
	}
	out := roles[0]
	for _, r := range roles[1:] {
		out += "," + r
	}
	return out
}

// age renders a coarse duration since t. A zero time (no created_at) ⇒ "-".
func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
