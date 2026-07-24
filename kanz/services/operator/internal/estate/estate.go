// Package estate reads the Kubernetes node inventory into a clean, proto-free
// read model. It is the operator service's view of the estate: what nodes
// exist, whether they are ready, and how they group into regions. The gRPC
// adapter (internal/grpcsrv) converts these types to operator.v1; nothing here
// imports the generated schema, so the read model is testable against a fake
// clientset with no gRPC in the loop.
package estate

import (
	"context"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeStatus is a node's readiness. The zero value is the deny-by-default
// state — an absent/Unknown Ready condition is StatusUnknown, never ready.
type NodeStatus int

const (
	StatusUnknown NodeStatus = iota
	StatusReady
	StatusNotReady
)

const (
	regionLabel = "topology.kubernetes.io/region"
	rolePrefix  = "node-role.kubernetes.io/"
)

// Node is the operator's projection of a Kubernetes Node.
type Node struct {
	Name           string
	Status         NodeStatus
	Roles          []string
	Region         string
	KubeletVersion string
	CreatedAt      time.Time
}

// Cluster is a region grouping with online/offline counts.
type Cluster struct {
	Region  string
	Online  int
	Offline int
}

// Reader is the read surface the gRPC adapter depends on. A fake clientset in
// tests and the live cluster in production both satisfy it via *K8s.
type Reader interface {
	ListNodes(ctx context.Context) ([]Node, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
}

// K8s reads nodes from the Kubernetes API.
type K8s struct {
	cs kubernetes.Interface
}

// NewK8s returns a Reader over the given clientset.
func NewK8s(cs kubernetes.Interface) *K8s { return &K8s{cs: cs} }

func (k *K8s) ListNodes(ctx context.Context) ([]Node, error) {
	list, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mapNode(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (k *K8s) ListClusters(ctx context.Context) ([]Cluster, error) {
	nodes, err := k.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	idx := map[string]*Cluster{}
	var order []string
	for _, n := range nodes {
		c, ok := idx[n.Region]
		if !ok {
			c = &Cluster{Region: n.Region}
			idx[n.Region] = c
			order = append(order, n.Region)
		}
		if n.Status == StatusReady {
			c.Online++
		} else {
			c.Offline++
		}
	}
	sort.Strings(order)
	out := make([]Cluster, 0, len(order))
	for _, r := range order {
		out = append(out, *idx[r])
	}
	return out, nil
}

func mapNode(n *corev1.Node) Node {
	return Node{
		Name:           n.Name,
		Status:         readyStatus(n),
		Roles:          roles(n.Labels),
		Region:         n.Labels[regionLabel],
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CreatedAt:      n.CreationTimestamp.Time,
	}
}

// readyStatus maps the NodeReady condition to a NodeStatus. Absent or Unknown
// ⇒ StatusUnknown (deny-by-default): an offline node must read as offline.
func readyStatus(n *corev1.Node) NodeStatus {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			return StatusReady
		case corev1.ConditionFalse:
			return StatusNotReady
		default:
			return StatusUnknown
		}
	}
	return StatusUnknown
}

// roles extracts node-role.kubernetes.io/<role> label suffixes, sorted.
func roles(labels map[string]string) []string {
	var out []string
	for k := range labels {
		if strings.HasPrefix(k, rolePrefix) {
			if r := strings.TrimPrefix(k, rolePrefix); r != "" {
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}
