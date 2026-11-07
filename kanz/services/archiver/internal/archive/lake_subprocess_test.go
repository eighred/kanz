package archive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/archiver/internal/config"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

type lakeArchiverInit struct {
	NATSURL, KafkaBroker, Tenant, ExpectedAccount string
	Cert, Key, Bundle                             []byte
	ArchiverID, BrokerID                          string
}
type lakeArchiverSource struct {
	svid   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s lakeArchiverSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }
func (s lakeArchiverSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != s.bundle.TrustDomain() {
		return nil, errors.New("fixture trust domain refused")
	}
	return s.bundle, nil
}

// Forward to the native durable implementation; no fixture delivery/ACK logic.
type lakeArchiverSubscriber struct {
	client *bus.NATSClient
	ready  func()
}

func (s lakeArchiverSubscriber) Subscribe(ctx context.Context, subject, group string, h bus.Handler) error {
	return s.client.SubscribeReady(ctx, subject, group, h, s.ready)
}
func lakeLoopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && port != "" && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

// TestLakeSnapshotArchiverSubprocess runs only in the hash-bound parent fixture
// binary. Synthetic PEM credentials remain on private stdin, never in logs.
func TestLakeSnapshotArchiverSubprocess(t *testing.T) {
	if os.Getenv("KANZ_LAKE_ARCHIVER_CHILD") != "1" {
		t.Skip("owned archiver subprocess only")
	}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reader := bufio.NewReaderSize(os.Stdin, 64*1024+1)
	initLine := make(chan []byte, 1)
	inputDone := make(chan struct{})
	inputEOF := false
	go func() {
		defer close(inputDone)
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > 64*1024 {
			initLine <- nil
			return
		}
		initLine <- bytes.Clone(line)
		_, stopErr := reader.ReadByte()
		inputEOF = errors.Is(stopErr, io.EOF)
		cancel()
	}()
	defer func() { _ = os.Stdin.Close(); <-inputDone }()
	var raw []byte
	select {
	case raw = <-initLine:
	case <-ctx.Done():
		t.Fatal("archiver init deadline")
	}
	var in lakeArchiverInit
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF {
		t.Fatal("archiver init refused")
	}
	u, err := url.Parse(in.NATSURL)
	if err != nil || u.Scheme != "tls" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !lakeLoopback(u.Host) || !lakeLoopback(in.KafkaBroker) {
		t.Fatal("archiver endpoint refused")
	}
	if in.Tenant == "" || len(in.Tenant) > 128 || in.ExpectedAccount != "acme" || in.ArchiverID != "spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme" || in.BrokerID != "spiffe://kanz.internal/ns/kanz-messaging/sa/nats" {
		t.Fatal("archiver identity scope refused")
	}
	svid, err := x509svid.Parse(in.Cert, in.Key)
	if err != nil || svid.ID.String() != in.ArchiverID {
		t.Fatal("archiver SVID refused")
	}
	td := spiffeid.RequireTrustDomainFromString("kanz.internal")
	bundle, err := x509bundle.Parse(td, in.Bundle)
	if err != nil {
		t.Fatal("archiver bundle refused")
	}
	tlsConfig := transport.ClientTLSConfig(lakeArchiverSource{svid, bundle}, transport.AuthorizeServices(spiffeid.RequireFromString(in.BrokerID)))
	for key, value := range map[string]string{"ARCHIVER_TENANT": in.Tenant, "ARCHIVER_NATS_URL": in.NATSURL, "ARCHIVER_KAFKA_BROKERS": in.KafkaBroker, "ARCHIVER_SUBJECTS": "lake.snapshot.>", "ARCHIVER_LAKE_SNAPSHOTS": "true", "ARCHIVER_CONSUMER_GROUP": "archiver", "ARCHIVER_SOURCE": "archiver-fixture"} {
		t.Setenv(key, value)
	}
	cfg, err := config.Load()
	if err != nil || len(cfg.Subjects) != 1 || cfg.Subjects[0] != config.LakeSnapshotSubject {
		t.Fatal("archiver configuration refused")
	}
	kafka, err := bus.DialKafka(bus.KafkaConfig{Brokers: cfg.Brokers, ClientID: cfg.Source})
	if err != nil {
		t.Fatal("archiver Kafka setup refused")
	}
	nc, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: tlsConfig, ConnectTimeout: 5 * time.Second})
	if err != nil {
		_ = kafka.Close()
		t.Fatal("archiver NATS setup refused")
	}
	ready := make(chan struct{}, 1)
	a := New(Config{Tenant: cfg.Tenant, Group: cfg.Group, Subjects: cfg.Subjects, Kafka: kafka, NATS: lakeArchiverSubscriber{nc, func() {
		select {
		case ready <- struct{}{}:
		default:
		}
	}}, Logger: slog.Default()})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	emit := func(status string) {
		if json.NewEncoder(os.Stdout).Encode(struct {
			Status string `json:"status"`
		}{status}) != nil {
			cancel()
		}
	}
	var runErr error
	select {
	case <-ready:
		emit("connected")
		runErr = <-done
	case runErr = <-done:
	case <-ctx.Done():
		runErr = <-done
	}
	// Run joins native subscription work before both owned transports close.
	cancel()
	nerr := nc.Close()
	kerr := kafka.Close()
	if runErr != nil || nerr != nil || kerr != nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("archiver shutdown refused")
	}
	_ = os.Stdin.Close()
	<-inputDone
	if !inputEOF {
		t.Fatal("archiver stop input refused")
	}
	emit("closed")
}
