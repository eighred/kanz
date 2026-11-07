// Command kanz-kafka-provision is the opt-in, non-destructive tenant topic/ACL
// owner. It preserves the legacy broker, data, default onboarding and offboard.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eighred/kanz/cmd/kanz-kafka-provision/internal/provision"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/segmentio/kafka-go"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type result struct {
	Version     string            `json:"version"`
	Receipt     provision.Receipt `json:"receipt"`
	Error       string            `json:"error,omitempty"`
	FailureCode string            `json:"failure_code,omitempty"`
}

// Check the presented identity on every TLS certificate callback, including
// rotations; an initial SVID check alone would not constrain later updates.
type provisionerSource struct{ transport.Source }

func (s provisionerSource) GetX509SVID() (*x509svid.SVID, error) {
	v, err := s.Source.GetX509SVID()
	if err != nil || v == nil || v.ID.String() != "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner" {
		return nil, provision.ErrInput
	}
	return v, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r, err := run(ctx, os.Args[1:])
	category := ""
	if err != nil {
		category = "broker-unavailable"
		switch {
		case errors.Is(err, provision.ErrInput):
			category = "input-refused"
		case errors.Is(err, provision.ErrConflict):
			category = "resource-conflict"
		case errors.Is(err, provision.ErrMissing):
			category = "resource-missing"
		}
	}
	if e := json.NewEncoder(os.Stdout).Encode(result{Version: version.String(), Receipt: r, Error: category, FailureCode: provision.FailureCode(err)}); e != nil {
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}

func brokerIdentity(value string) (spiffeid.ID, error) {
	if value != "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka" && value != "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda" {
		return spiffeid.ID{}, provision.ErrInput
	}
	return spiffeid.FromString(value)
}

func run(parent context.Context, args []string) (provision.Receipt, error) {
	var empty provision.Receipt
	fs := flag.NewFlagSet("kanz-kafka-provision", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", "plan", "plan, verify or apply")
	manifest := fs.String("manifest", "", "pinned read-only topic manifest")
	digest := fs.String("manifest-sha256", "", "exact manifest SHA256")
	tenant := fs.String("tenant", "", "one non-system tenant")
	principal := fs.String("principal", "", "existing tenant workload principal")
	encoding := fs.String("principal-encoding", "spiffe-uri", "explicit broker principal encoding")
	durability := fs.String("durability-profile", "kafka-min-isr", "explicit broker durability capability profile")
	broker := fs.String("broker", "", "TLS bootstrap host:port")
	clusterID := fs.String("expected-cluster-id", "", "exact expected Kafka estate identity")
	peer := fs.String("broker-spiffe-id", "", "exact broker identity")
	socket := fs.String("workload-socket", "", "Workload API socket")
	work := fs.Duration("timeout", 240*time.Second, "one total work deadline, at most 240s")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *work <= 0 || *work > 240*time.Second || (*mode != "plan" && *mode != "verify" && *mode != "apply") {
		return empty, provision.ErrInput
	}
	host, port, err := net.SplitHostPort(*broker)
	if err != nil || host == "" || port == "" || strings.ContainsAny(*broker, "/@?#\\ \t\r\n") || !strings.HasPrefix(*socket, "unix:///") {
		return empty, provision.ErrInput
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 1 || numericPort > 65535 || len(*broker) > 512 || len(*socket) > 4096 || len(*manifest) > 4096 {
		return empty, provision.ErrInput
	}
	peerID, err := brokerIdentity(*peer)
	if err != nil {
		return empty, provision.ErrInput
	}
	raw, err := provision.ReadManifest(*manifest)
	if err != nil {
		return empty, err
	}
	p, err := provision.NewPlan(raw, *digest, *tenant, *principal, *clusterID)
	if err != nil {
		return empty, err
	}
	p, err = provision.WithPrincipalEncoding(p, *encoding, *peer)
	if err != nil {
		return empty, err
	}
	p, err = provision.WithDurabilityProfile(p, *durability, *peer)
	if err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(parent, *work)
	defer cancel()
	source, err := transport.NewSource(ctx, *socket)
	if err != nil {
		return empty, provision.ErrBroker
	}
	defer source.Close()
	pinned := provisionerSource{Source: source}
	if _, err := pinned.GetX509SVID(); err != nil {
		return empty, provision.ErrInput
	}
	tr := &kafka.Transport{TLS: transport.ClientTLSConfig(pinned, transport.AuthorizeServices(peerID))}
	defer tr.CloseIdleConnections()
	client := &kafka.Client{Addr: kafka.TCP(*broker), Transport: tr}
	return provision.Run(ctx, client, p, *mode)
}
