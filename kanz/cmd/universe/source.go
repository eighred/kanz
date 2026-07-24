package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"
)

// nodeSource fetches estate reads and drives provisioning. The gRPC-backed impl
// dials the operator service; tests use a stub. fetch does the clock read
// (Age), so render stays a pure function of model.
type nodeSource interface {
	fetch(ctx context.Context) (fetchMsg, error)
	addNode(ctx context.Context, req addNodeInput) (string, error)
	listProvisions(ctx context.Context) ([]provisionRow, error)
	testConnection(ctx context.Context, ip string, port int32) (testConnResult, error)
	cordon(ctx context.Context, name string) error
	uncordon(ctx context.Context, name string) error
	drain(ctx context.Context, name string) error
	setRegion(ctx context.Context, name, region string) error
	setVenueKeys(ctx context.Context, venue string, keys venueKeys) (string, error)
	listVenueKeys(ctx context.Context) ([]venueRow, error)
}

// venueKeys is the write payload for SetVenueKeys — the typed secret is passed
// through to the RPC and never retained on a model field (the S2a key-bytes
// discipline).
type venueKeys struct{ apiKey, apiSecret, passphrase string }

// testConnResult is the outcome of a pre-flight reachability probe of a
// candidate host's SSH port — reachability only, never authentication.
type testConnResult struct {
	reachable bool
	latencyMs int64
	message   string
}

// fetchMsg carries one poll cycle's result into Update.
type fetchMsg struct {
	nodes      []nodeRow
	clusters   []clusterRow
	provisions []provisionRow
	venues     []venueRow
	err        error
}

// addNodeInput is the write payload for AddNode — sshKey is populated off the
// UI thread by submitAddForm's file read and never touches a model field.
type addNodeInput struct {
	hostname, ip, sshUser string
	sshPort               int32
	sshKey                []byte
}

// provisionRow is one row of the provisioning status strip.
type provisionRow struct {
	id, hostname, status, message string
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
	// Provisioning is a secondary, best-effort read: a transient failure here
	// degrades only the strip (it goes quiet for one tick), never the whole
	// poll — nodes/clusters are the read this TUI exists for.
	provisions, _ := g.listProvisions(ctx)
	// Venue-key presence is likewise best-effort: a transient failure here
	// degrades only the API Manager pane (it goes empty for one tick), never
	// the whole poll.
	venues, _ := g.listVenueKeys(ctx)
	return fetchMsg{
		nodes:      toNodeRows(nodesResp.GetNodes()),
		clusters:   toClusterRows(clustersResp.GetClusters()),
		provisions: provisions,
		venues:     venues,
	}, nil
}

func (g *grpcSource) addNode(ctx context.Context, in addNodeInput) (string, error) {
	resp, err := g.client.AddNode(ctx, &operatorpb.AddNodeRequest{
		Hostname: in.hostname, Ip: in.ip, SshPort: in.sshPort, SshUser: in.sshUser, SshPrivateKey: in.sshKey,
	})
	if err != nil {
		return "", err
	}
	return resp.GetProvisionId(), nil
}

func (g *grpcSource) listProvisions(ctx context.Context) ([]provisionRow, error) {
	resp, err := g.client.ListProvisions(ctx, &operatorpb.ListProvisionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]provisionRow, 0, len(resp.GetProvisions()))
	for _, p := range resp.GetProvisions() {
		out = append(out, provisionRow{id: p.GetId(), hostname: p.GetHostname(), status: provLabel(p.GetStatus()), message: p.GetMessage()})
	}
	return out, nil
}

func (g *grpcSource) testConnection(ctx context.Context, ip string, port int32) (testConnResult, error) {
	resp, err := g.client.TestConnection(ctx, &operatorpb.TestConnectionRequest{Ip: ip, SshPort: port})
	if err != nil {
		return testConnResult{}, err
	}
	return testConnResult{reachable: resp.GetReachable(), latencyMs: resp.GetLatencyMs(), message: resp.GetMessage()}, nil
}

func (g *grpcSource) cordon(ctx context.Context, name string) error {
	_, err := g.client.Cordon(ctx, &operatorpb.CordonRequest{Name: name})
	return err
}

func (g *grpcSource) uncordon(ctx context.Context, name string) error {
	_, err := g.client.Uncordon(ctx, &operatorpb.UncordonRequest{Name: name})
	return err
}

func (g *grpcSource) drain(ctx context.Context, name string) error {
	_, err := g.client.Drain(ctx, &operatorpb.DrainRequest{Name: name})
	return err
}

func (g *grpcSource) setRegion(ctx context.Context, name, region string) error {
	_, err := g.client.SetNodeRegion(ctx, &operatorpb.SetNodeRegionRequest{Name: name, Region: region})
	return err
}

// setVenueKeys returns the exchange's own account id for the credentials, as
// proved server-side before the write (S4b) — empty when the deployment has
// no proof configured. A FailedPrecondition error means the exchange
// rejected the credentials; its message is already sanitized server-side.
func (g *grpcSource) setVenueKeys(ctx context.Context, venue string, keys venueKeys) (string, error) {
	resp, err := g.client.SetVenueKeys(ctx, &operatorpb.SetVenueKeysRequest{
		Venue: venue, ApiKey: keys.apiKey, ApiSecret: keys.apiSecret, Passphrase: keys.passphrase,
	})
	if err != nil {
		return "", err
	}
	return resp.GetExchangeAccountId(), nil
}

func (g *grpcSource) listVenueKeys(ctx context.Context) ([]venueRow, error) {
	resp, err := g.client.ListVenueKeys(ctx, &operatorpb.ListVenueKeysRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]venueRow, 0, len(resp.GetVenues()))
	for _, v := range resp.GetVenues() {
		out = append(out, venueRow{venue: v.GetVenue(), configured: v.GetConfigured()})
	}
	return out, nil
}

func provLabel(s operatorpb.ProvisionStatus) string {
	switch s {
	case operatorpb.ProvisionStatus_PROVISION_STATUS_INSTALLING:
		return "Installing"
	case operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED:
		return "Joined"
	case operatorpb.ProvisionStatus_PROVISION_STATUS_FAILED:
		return "Failed"
	default:
		return "Pending"
	}
}

func toNodeRows(nodes []*operatorpb.Node) []nodeRow {
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		var created time.Time
		if ts := n.GetCreatedAt(); ts != nil {
			created = ts.AsTime()
		}
		out = append(out, nodeRow{
			Name:          n.GetName(),
			Status:        statusLabel(n.GetStatus()),
			Roles:         joinRoles(n.GetRoles()),
			Region:        n.GetRegion(),
			Version:       n.GetKubeletVersion(),
			Age:           age(created),
			schedulable:   n.GetSchedulable(),
			evictablePods: int(n.GetEvictablePods()),
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
