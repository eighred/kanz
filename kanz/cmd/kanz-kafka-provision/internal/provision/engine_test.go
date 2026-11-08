package provision

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol"
)

// This fake proves ordering and refusal, never actual broker acceptance.
type fakeAdmin struct {
	configs           map[string]map[string]string
	filteredPrincipal string
	clusterID         string
	mutateMetadata    func(*kafka.MetadataResponse)
	metadataCalls     int
	changeAt          int
	topics            map[string]Topic
	acls              []kafka.ACLEntry
	creates, grants   int
	lostCreate        bool
}

func (a *fakeAdmin) Metadata(_ context.Context, r *kafka.MetadataRequest) (*kafka.MetadataResponse, error) {
	a.metadataCalls++
	if a.changeAt > 0 && a.metadataCalls >= a.changeAt {
		a.clusterID = "cluster-b"
	}
	brokers := []kafka.Broker{{ID: 0, Host: "broker-0", Port: 9094}, {ID: 1, Host: "broker-1", Port: 9094}, {ID: 2, Host: "broker-2", Port: 9094}}
	t, ok := a.topics[r.Topics[0]]
	x := kafka.Topic{Name: r.Topics[0]}
	if !ok {
		x.Error = kafka.UnknownTopicOrPartition
	} else {
		for i := 0; i < t.Partitions; i++ {
			x.Partitions = append(x.Partitions, kafka.Partition{ID: i, Leader: brokers[0], Replicas: append([]kafka.Broker(nil), brokers...), Isr: append([]kafka.Broker(nil), brokers...)})
		}
	}
	result := &kafka.MetadataResponse{ClusterID: a.clusterID, Brokers: brokers, Topics: []kafka.Topic{x}}
	if a.mutateMetadata != nil {
		a.mutateMetadata(result)
	}
	return result, nil
}
func (a *fakeAdmin) DescribeConfigs(_ context.Context, r *kafka.DescribeConfigsRequest) (*kafka.DescribeConfigsResponse, error) {
	t := a.topics[r.Resources[0].ResourceName]
	if config, ok := a.configs[t.Name]; ok {
		var entries []kafka.DescribeConfigResponseConfigEntry
		for _, name := range r.Resources[0].ConfigNames {
			if value, exists := config[name]; exists {
				entries = append(entries, kafka.DescribeConfigResponseConfigEntry{ConfigName: name, ConfigValue: value})
			}
		}
		return &kafka.DescribeConfigsResponse{Resources: []kafka.DescribeConfigResponseResource{{ResourceType: int8(kafka.ResourceTypeTopic), ResourceName: t.Name, ConfigEntries: entries}}}, nil
	}
	return &kafka.DescribeConfigsResponse{Resources: []kafka.DescribeConfigResponseResource{{ResourceType: int8(kafka.ResourceTypeTopic), ResourceName: t.Name, ConfigEntries: []kafka.DescribeConfigResponseConfigEntry{{ConfigName: "cleanup.policy", ConfigValue: t.Cleanup}, {ConfigName: "retention.ms", ConfigValue: strconv.FormatInt(t.Retention, 10)}, {ConfigName: "min.insync.replicas", ConfigValue: "2"}}}}}, nil
}
func (a *fakeAdmin) DescribeACLs(_ context.Context, request *kafka.DescribeACLsRequest) (*kafka.DescribeACLsResponse, error) {
	a.filteredPrincipal = request.Filter.PrincipalFilter
	r := &kafka.DescribeACLsResponse{}
	for _, x := range a.acls {
		r.Resources = append(r.Resources, kafka.ACLResource{ResourceType: x.ResourceType, ResourceName: x.ResourceName, PatternType: x.ResourcePatternType, ACLs: []kafka.ACLDescription{{Principal: x.Principal, Host: x.Host, Operation: x.Operation, PermissionType: x.PermissionType}}})
	}
	return r, nil
}
func (a *fakeAdmin) CreateTopics(_ context.Context, r *kafka.CreateTopicsRequest) (*kafka.CreateTopicsResponse, error) {
	a.creates++
	x := r.Topics[0]
	if a.configs == nil {
		a.configs = make(map[string]map[string]string)
	}
	a.configs[x.Topic] = make(map[string]string)
	t := Topic{Name: x.Topic, Partitions: x.NumPartitions}
	for _, c := range x.ConfigEntries {
		a.configs[x.Topic][c.ConfigName] = c.ConfigValue
		switch c.ConfigName {
		case "cleanup.policy":
			t.Cleanup = c.ConfigValue
		case "retention.ms":
			t.Retention, _ = strconv.ParseInt(c.ConfigValue, 10, 64)
		}
	}
	a.topics[t.Name] = t
	if a.lostCreate {
		return nil, errors.New("synthetic response loss")
	}
	return &kafka.CreateTopicsResponse{Errors: map[string]error{x.Topic: nil}}, nil
}
func (a *fakeAdmin) CreateACLs(_ context.Context, r *kafka.CreateACLsRequest) (*kafka.CreateACLsResponse, error) {
	a.grants++
	a.acls = append(a.acls, r.ACLs...)
	return &kafka.CreateACLsResponse{Errors: make([]error, len(r.ACLs))}, nil
}

func testPlan(t *testing.T) Plan {
	t.Helper()
	p, e := NewPlan([]byte(testManifest), manifestDigest(testManifest), "acme", "User:spiffe://kanz.internal/ns/tenant-acme/sa/risk-engine", "cluster-a")
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestBrokerFailureCodeNeverCopiesErrorText(t *testing.T) {
	conflict := conflictCause{"config-count"}
	if FailureCode(conflict) != "config-count" || conflict.Error() != ErrConflict.Error() || !errors.Is(conflict, ErrConflict) {
		t.Fatal("conflict classification changed refusal")
	}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{kafka.ClusterAuthorizationFailed, "kafka-31"}, {context.DeadlineExceeded, "deadline"},
		{errors.New("secret-error-sentinel"), "unknown"},
		{io.ErrUnexpectedEOF, "unexpected-eof"},
		{x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired, Detail: "SECRET"}, "tls-expired"},
		{protocol.Errorf("SECRET"), "protocol"},
	} {
		err := brokerCause{tc.err}
		if FailureCode(err) != tc.code || err.Error() != ErrBroker.Error() || !errors.Is(err, ErrBroker) {
			t.Fatal("closed broker classification differs")
		}
	}
	if FailureCode(ErrInput) != "" || FailureCode(nil) != "" {
		t.Fatal("non-broker error classified")
	}
}

func TestApplyReadsAllBeforeGrantsAndRerunsWithoutWrites(t *testing.T) {
	p := testPlan(t)
	a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}}
	r, e := Run(context.Background(), a, p, "plan")
	if e != nil || r.Complete || a.creates != 0 || a.grants != 0 {
		t.Fatal("plan mutated or claimed application")
	}
	r, e = Run(context.Background(), a, p, "apply")
	if e != nil || !r.Complete || a.creates != len(p.Topics) || a.grants != 1 {
		t.Fatal("apply incomplete")
	}
	creates := a.creates
	r, e = Run(context.Background(), a, p, "apply")
	if e != nil || !r.Complete || a.creates != creates || a.grants != 1 {
		t.Fatal("rerun repeated mutation")
	}
}

func TestExistingMismatchAndExtraGrantRefuseBeforeMutation(t *testing.T) {
	p := testPlan(t)
	for _, kind := range []string{"retention", "acl"} {
		t.Run(kind, func(t *testing.T) {
			a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}}
			if kind == "retention" {
				x := p.Topics[len(p.Topics)-1]
				x.Retention = 1
				a.topics[x.Name] = x
			} else {
				x := p.ACLs[0]
				x.ResourceName = "other."
				a.acls = []kafka.ACLEntry{x}
			}
			_, e := Run(context.Background(), a, p, "apply")
			if !errors.Is(e, ErrConflict) || a.creates != 0 || a.grants != 0 {
				t.Fatal("mismatch did not refuse before mutations")
			}
		})
	}
}

func TestLostCreateResponseRetainsPartialStateForExplicitReadback(t *testing.T) {
	p := testPlan(t)
	a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}, lostCreate: true}
	r, e := Run(context.Background(), a, p, "apply")
	if !errors.Is(e, ErrBroker) || !r.PartialOrUnknown || r.Complete || len(a.topics) != 1 || a.grants != 0 {
		t.Fatal("ambiguous create misrepresented")
	}
	a.lostCreate = false
	r, e = Run(context.Background(), a, p, "apply")
	if e != nil || !r.Complete || a.creates != len(p.Topics) {
		t.Fatal("explicit retry failed to preserve existing topic")
	}
}

func TestClusterIdentityRefusesUnknownWrongAndChangedEstate(t *testing.T) {
	p := testPlan(t)
	for _, mode := range []string{"plan", "verify", "apply"} {
		for _, id := range []string{"", "cluster-b"} {
			a := &fakeAdmin{clusterID: id, topics: map[string]Topic{}}
			r, err := Run(context.Background(), a, p, mode)
			if !errors.Is(err, ErrConflict) || a.creates != 0 || a.grants != 0 || r.Complete || r.ClusterID != "cluster-a" {
				t.Fatal("unbound estate accepted")
			}
		}
	}
	for _, at := range []int{len(p.Topics) + 1, len(p.Topics) + 2, 2*len(p.Topics) + 1} {
		a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}, changeAt: at}
		r, err := Run(context.Background(), a, p, "apply")
		if !errors.Is(err, ErrConflict) || a.grants != 0 || r.Complete {
			t.Fatal("changed estate accepted")
		}
		if at == len(p.Topics)+1 && a.creates != 0 {
			t.Fatal("created after changed prewrite identity")
		}
		if a.creates > 0 && !r.PartialOrUnknown {
			t.Fatal("prior effects hidden")
		}
	}
	p.ClusterID = ""
	a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}}
	if _, err := Run(context.Background(), a, p, "plan"); !errors.Is(err, ErrInput) || a.metadataCalls != 0 {
		t.Fatal("missing expected identity reached broker")
	}
}

func TestMalformedTopologyCannotClaimReplicationOrISR(t *testing.T) {
	p := testPlan(t)
	mutations := map[string]func(*kafka.MetadataResponse){
		"duplicate-replica": func(m *kafka.MetadataResponse) {
			m.Topics[0].Partitions[0].Replicas[1] = m.Topics[0].Partitions[0].Replicas[0]
		},
		"duplicate-isr": func(m *kafka.MetadataResponse) { m.Topics[0].Partitions[0].Isr[1] = m.Topics[0].Partitions[0].Isr[0] },
		"foreign-isr": func(m *kafka.MetadataResponse) {
			b := kafka.Broker{ID: 3, Host: "broker-3", Port: 9094}
			m.Brokers = append(m.Brokers, b)
			m.Topics[0].Partitions[0].Isr[0] = b
		},
		"unknown-replica":  func(m *kafka.MetadataResponse) { m.Topics[0].Partitions[0].Replicas[0] = kafka.Broker{} },
		"unknown-leader":   func(m *kafka.MetadataResponse) { m.Topics[0].Partitions[0].Leader = kafka.Broker{} },
		"leader-not-isr":   func(m *kafka.MetadataResponse) { m.Topics[0].Partitions[0].Isr = m.Topics[0].Partitions[0].Isr[1:] },
		"duplicate-broker": func(m *kafka.MetadataResponse) { m.Brokers[1] = m.Brokers[0] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a := &fakeAdmin{clusterID: "cluster-a", topics: map[string]Topic{}, mutateMetadata: mutate}
			for _, topic := range p.Topics {
				a.topics[topic.Name] = topic
			}
			r, e := Run(context.Background(), a, p, "apply")
			if !errors.Is(e, ErrConflict) || r.Complete || a.creates != 0 || a.grants != 0 {
				t.Fatal("malformed topology conferred acceptance")
			}
		})
	}
}
