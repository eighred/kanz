package universe

import (
	"context"
	"fmt"
	"time"

	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
)

// nodeSource fetches estate reads and drives provisioning. The real implementation
// (gatewaySource) goes through the api-gateway's /v1/control routes; tests use a
// stub. fetch does the clock read (Age), so render stays a pure function of Model.
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
// through to the RPC and never retained on a Model field (the S2a key-bytes
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
// UI thread by submitAddForm's file read and never touches a Model field.
type addNodeInput struct {
	hostname, ip, sshUser string
	sshPort               int32
	sshKey                []byte
	// sshHostKey is the target's PUBLIC host key (authorized_keys format), which
	// authenticates the host to us. sshKey above authenticates us to the host —
	// the two are not interchangeable and both are required.
	sshHostKey string
}

// provisionRow is one row of the provisioning status strip.
type provisionRow struct {
	id, hostname, status, message string
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
