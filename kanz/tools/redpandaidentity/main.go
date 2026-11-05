// Command redpandaidentity is a disposable development probe, never a service.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/eighred/kanz/internal/version"

	"github.com/segmentio/kafka-go"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Only fixed local stage labels may appear in failed probe diagnostics.
var stage = "configuration"

func subject(cert *x509.Certificate, id string) (string, error) {
	if len(cert.URIs) != 1 || cert.URIs[0].String() != id || len(cert.Subject.Names) != 1 {
		return "", errors.New("unexpected SVID identity attributes")
	}
	h := sha256.Sum256([]byte(id))
	n := cert.Subject.Names[0]
	if n.Type.String() != "2.5.4.45" || n.Value != hex.EncodeToString(h[:16]) {
		return "", errors.New("uniqueid is not bound to the complete SPIFFE ID")
	}
	return cert.Subject.String(), nil
}

func produce(ctx context.Context, c *kafka.Client, topic, value string) error {
	r, err := c.Produce(ctx, &kafka.ProduceRequest{Topic: topic, RequiredAcks: kafka.RequireAll,
		Records: kafka.NewRecordReader(kafka.Record{Value: kafka.NewBytes([]byte(value))})})
	if err != nil {
		return err
	}
	return r.Error
}

func fetch(ctx context.Context, c *kafka.Client, topic string) ([]string, error) {
	r, err := c.Fetch(ctx, &kafka.FetchRequest{Topic: topic, Offset: 0, MinBytes: 1, MaxBytes: 4096, MaxWait: time.Second})
	if err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, r.Error
	}
	if closer, ok := r.Records.(io.Closer); ok {
		defer closer.Close()
	}
	var values []string
	for {
		record, err := r.Records.ReadRecord()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(values) >= 8 {
			return nil, errors.New("unexpected record count")
		}
		value, err := kafka.ReadAll(record.Value)
		if err != nil {
			return nil, err
		}
		values = append(values, string(value))
	}
	return values, nil
}

func run() error {
	if version.String() == version.Unstamped {
		return errors.New("fixture executable must identify its source")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	sa, mode := os.Getenv("PROOF_SA"), os.Getenv("PROOF_MODE")
	if sa != "proof-a" && sa != "proof-b" && sa != "proof-bootstrap" && sa != "proof-ungranted" {
		return errors.New("invalid fixture identity")
	}
	id := "spiffe://kanz.internal/ns/kanz-services/sa/" + sa
	stage = "svid"
	src, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr("unix:///run/spiffe/spire-agent.sock")))
	if err != nil {
		return errors.New("SVID source unavailable")
	}
	defer src.Close()
	get := func() (*x509svid.SVID, string, error) {
		s, err := src.GetX509SVID()
		if err != nil {
			return nil, "", err
		}
		if s.ID.String() != id || len(s.Certificates) == 0 {
			return nil, "", errors.New("unexpected workload identity")
		}
		dn, err := subject(s.Certificates[0], id)
		return s, dn, err
	}
	initial, dn, err := get()
	if err != nil {
		return err
	}
	stage = "kafka"
	transport := &kafka.Transport{TLS: tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeID(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda")))}
	defer transport.CloseIdleConnections()
	c := &kafka.Client{Addr: kafka.TCP("redpanda.kanz-messaging.svc:9094"), Transport: transport, Timeout: 10 * time.Second}
	var acls []kafka.ACLEntry
	for _, name := range []string{"proof-a", "proof-b"} {
		for _, op := range []kafka.ACLOperationType{kafka.ACLOperationTypeRead, kafka.ACLOperationTypeWrite, kafka.ACLOperationTypeDescribe} {
			acls = append(acls, kafka.ACLEntry{ResourceType: kafka.ResourceTypeTopic, ResourceName: name, ResourcePatternType: kafka.PatternTypeLiteral, Principal: "User:" + name, Host: "*", Operation: op, PermissionType: kafka.ACLPermissionTypeAllow})
		}
	}
	switch mode {
	case "bootstrap":
		stage = "bootstrap"
		if sa != "proof-bootstrap" {
			return errors.New("bootstrap identity required")
		}
		r, err := c.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{Topic: "proof-a", NumPartitions: 1, ReplicationFactor: 1}, {Topic: "proof-b", NumPartitions: 1, ReplicationFactor: 1}}})
		if err != nil {
			return err
		}
		for _, err := range r.Errors {
			if err != nil {
				return err
			}
		}
		ar, err := c.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: acls})
		if err != nil {
			return err
		}
		for _, err := range ar.Errors {
			if err != nil {
				return err
			}
		}
	case "publish":
		stage = "own-topic"
		if sa != "proof-a" && sa != "proof-b" {
			return errors.New("client identity required")
		}
		if err := produce(ctx, c, sa, "before-renewal"); err != nil {
			return err
		}
		values, err := fetch(ctx, c, sa)
		if err != nil || len(values) != 1 || values[0] != "before-renewal" {
			return errors.New("own-topic readback failed")
		}
		other := "proof-a"
		if sa == other {
			other = "proof-b"
		}
		stage = "cross-topic-denial"
		if !errors.Is(produce(ctx, c, other, "FORBIDDEN"), kafka.TopicAuthorizationFailed) {
			return errors.New("cross-topic write not explicitly denied")
		}
		if _, err := fetch(ctx, c, other); !errors.Is(err, kafka.TopicAuthorizationFailed) {
			return errors.New("cross-topic read not explicitly denied")
		}
		stage = "acl-admin-denial"
		ar, err := c.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: acls})
		if err != nil {
			if !errors.Is(err, kafka.ClusterAuthorizationFailed) {
				return errors.New("ACL denial was not authorization failure")
			}
		} else {
			for _, err := range ar.Errors {
				if !errors.Is(err, kafka.ClusterAuthorizationFailed) {
					return errors.New("client could administer ACLs")
				}
			}
			if len(ar.Errors) != len(acls) {
				return errors.New("missing ACL denial results")
			}
		}
		// Observe actual leaf replacement, discard pooled TLS connections, then
		// prove the renewed identity retains only its original topic authority.
		stage = "renewal"
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return errors.New("SVID did not renew within bound")
			case <-ticker.C:
			}
			next, nextDN, err := get()
			if err != nil {
				return err
			}
			if next.Certificates[0].SerialNumber.Cmp(initial.Certificates[0].SerialNumber) == 0 {
				continue
			}
			if nextDN != dn {
				return errors.New("principal changed on renewal")
			}
			break
		}
		stage = "renewed-topic"
		transport.CloseIdleConnections()
		// A distinct pool guarantees the next request cannot reuse any old
		// authenticated connection, including background transport activity.
		renewedTransport := &kafka.Transport{TLS: tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeID(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda")))}
		defer renewedTransport.CloseIdleConnections()
		c = &kafka.Client{Addr: c.Addr, Transport: renewedTransport, Timeout: 10 * time.Second}
		if err := produce(ctx, c, sa, "after-renewal"); err != nil {
			return err
		}
		if !errors.Is(produce(ctx, c, other, "FORBIDDEN"), kafka.TopicAuthorizationFailed) {
			return errors.New("renewed client escaped its ACL")
		}
	case "ungranted":
		stage = "ungranted-denial"
		if sa != "proof-ungranted" {
			return errors.New("ungranted identity required")
		}
		// A valid SVID maps to its own unprivileged alias. TLS success alone
		// grants no topic permission; require explicit Kafka authorization codes.
		for _, topic := range []string{"proof-a", "proof-b"} {
			if !errors.Is(produce(ctx, c, topic, "FORBIDDEN"), kafka.TopicAuthorizationFailed) {
				return errors.New("ungranted write not explicitly denied")
			}
			if _, err := fetch(ctx, c, topic); !errors.Is(err, kafka.TopicAuthorizationFailed) {
				return errors.New("ungranted read not explicitly denied")
			}
		}
	case "verify":
		stage = "exact-records"
		if sa != "proof-bootstrap" {
			return errors.New("verification identity required")
		}
		for _, topic := range []string{"proof-a", "proof-b"} {
			values, err := fetch(ctx, c, topic)
			if err != nil || len(values) != 2 || values[0] != "before-renewal" || values[1] != "after-renewal" {
				return errors.New("unexpected accepted records")
			}
		}
	default:
		return errors.New("unknown fixture mode")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"passed": true, "mode": mode, "identity": id, "subjectDN": dn, "renewed": mode == "publish", "sourceVersion": version.String()})
}

func main() {
	if err := run(); err != nil {
		// Broker errors can contain arbitrary diagnostic text. Never forward
		// raw errors, certificates, keys or workload API payloads to CI logs.
		fmt.Fprintf(os.Stderr, "PROBE_FAILED_STAGE:%s\n", stage)
		os.Exit(1)
	}
}
