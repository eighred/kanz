//go:build operator_placement

package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
	"github.com/eighred/kanz/pkg/transport"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The disposable target speaks no SSH and holds no credentials. Kubernetes
// readiness checks and the probe both establish a TCP connection on port 22.
func TestPlacementTarget(t *testing.T) {
	listener, err := net.Listen("tcp", ":22")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The Python fixture starts this test in the production Deployment, with its
// RBAC, node selector, disruption budget, and network policies. The fixture
// cordons the operator's own node only AFTER draining the other server.
func TestWorkerProbeDuringServerDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	target, err := netip.ParseAddr(os.Getenv("FIXTURE_TARGET"))
	if err != nil || !target.IsPrivate() {
		t.Fatal("expected the private address of the disposable target pod")
	}
	operator, err := transport.ServiceID("kanz-operator", "operator")
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := testidentity.NewAuthority(operator.TrustDomain())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	serverID, err := authority.Issue(operator, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := authority.Issue(gateway, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, socket := testidentity.StartWorkload(t, serverID)
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", socket)
	server := exec.CommandContext(ctx, "/operator")
	server.Stdout, server.Stderr = os.Stdout, os.Stderr
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = server.Wait()
	}()

	config, err := rest.InClusterConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	worker := os.Getenv("FIXTURE_WORKER")
	err = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		var servers, eligible int
		for _, node := range nodes.Items {
			_, control := node.Labels["node-role.kubernetes.io/control-plane"]
			if control && node.Spec.Unschedulable {
				servers++
			}
			if !control && node.Name == worker && !node.Spec.Unschedulable {
				for _, state := range node.Status.Conditions {
					if state.Type == corev1.NodeReady && state.Status == corev1.ConditionTrue {
						eligible++
					}
				}
			}
		}
		return len(nodes.Items) == 3 && servers == 2 && eligible == 1, nil
	})
	if err != nil {
		t.Fatalf("waiting for both cordoned servers and one Ready worker: %v", err)
	}
	channel, err := grpc.NewClient("127.0.0.1:9090", transport.ClientDialOption(clientID, transport.AuthorizeServices(operator)))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	client := operatorpb.NewOperatorServiceClient(channel)
	type result struct {
		response *operatorpb.TestConnectionResponse
		err      error
	}
	rpc := make(chan result, 1)
	go func() {
		response, err := client.TestConnection(ctx, &operatorpb.TestConnectionRequest{Ip: target.String(), SshPort: 22}, grpc.WaitForReady(true))
		rpc <- result{response, err}
	}()

	// The provisioner deletes the Job on return. Observe the admitted Pod
	// while the operator awaits its termination message, before garbage collection.
	var probe string
	err = wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		pods, err := kube.CoreV1().Pods("kanz-operator").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=node-provisioner",
		})
		if err != nil {
			return false, err
		}
		if len(pods.Items) == 0 {
			select {
			case result := <-rpc:
				return false, fmt.Errorf("RPC ended before a scheduled probe was observed: %v, %v", result.response, result.err)
			default:
				return false, nil
			}
		}
		if len(pods.Items) != 1 {
			return false, fmt.Errorf("expected exactly one probe, found %d", len(pods.Items))
		}
		p := pods.Items[0]
		if p.Spec.NodeName == "" {
			return false, nil
		}
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.Kind != "Job" || !strings.HasPrefix(owner.Name, "probe-") ||
			p.Spec.NodeName != worker || p.Spec.ServiceAccountName != "kanz-node-provisioner" ||
			(p.Spec.AutomountServiceAccountToken != nil && *p.Spec.AutomountServiceAccountToken) ||
			len(p.Spec.Volumes) != 0 || len(p.Spec.InitContainers) != 0 || len(p.Spec.Containers) != 1 {
			return false, fmt.Errorf("unexpected probe %s: owner=%v node=%s account=%s volumes=%d init=%d containers=%d",
				p.Name, owner, p.Spec.NodeName, p.Spec.ServiceAccountName, len(p.Spec.Volumes),
				len(p.Spec.InitContainers), len(p.Spec.Containers))
		}
		container := p.Spec.Containers[0]
		if container.Name != "probe" || len(container.VolumeMounts) != 0 || len(container.EnvFrom) != 0 || len(container.Env) != 2 {
			return false, fmt.Errorf("unexpected probe container inputs for pod %s", p.Name)
		}
		expected := map[string]string{"PROVISION_MODE": "probe", "PROVISION_TARGET_ADDR": net.JoinHostPort(target.String(), "22")}
		for _, env := range container.Env {
			value, ok := expected[env.Name]
			if !ok || env.ValueFrom != nil || env.Value != value {
				return false, fmt.Errorf("unexpected probe environment for pod %s", p.Name)
			}
			delete(expected, env.Name)
		}
		probe = p.Name
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-rpc:
		if result.err != nil || !result.response.GetReachable() {
			t.Fatalf("TestConnection failed: %v, %v", result.response, result.err)
		}
		t.Logf("TestConnection reached the disposable target from pod %s on worker %s", probe, worker)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
