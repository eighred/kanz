package provision

import (
	"regexp"
	"strings"

	"github.com/eighred/kanz/internal/tenantgen"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/segmentio/kafka-go"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Plan retains the exact requested settings. No apply path changes existing
// retention, partitions, replicas or unrelated grants to make this plan fit.
var clusterIdentity = regexp.MustCompile(`^(redpanda\.)?[A-Za-z0-9_-]+$`)

// ValidClusterID supports Kafka's safe identifier form and Redpanda's explicit
// prefix with the same bounded safe suffix. Other configured Redpanda strings
// are unsupported; no normalization changes the caller's exact estate pin.
func ValidClusterID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && clusterIdentity.MatchString(id) &&
		id != "redpanda.initializing" && id != "redpanda.unknown"
}

type Plan struct {
	ClusterID       string
	Tenant          string
	Principal       string
	BrokerPrincipal string
	ManifestSHA     string
	Topics          []Topic
	ACLs            []kafka.ACLEntry
}

// NewPlan accepts the existing two tenant principal shapes. System topics can
// be parsed for inventory parity, but this tenant-onboarding slice cannot grant
// a prefix for the unprefixed system namespace.
func NewPlan(raw []byte, digest, tenant, principal, clusterID string) (Plan, error) {
	if !ValidClusterID(clusterID) || !tenantName.MatchString(tenant) || !strings.HasPrefix(principal, "User:") {
		return Plan{}, ErrInput
	}
	id, err := spiffeid.FromString(strings.TrimPrefix(principal, "User:"))
	if err != nil || id.TrustDomain().String() != transport.TrustDomain {
		return Plan{}, ErrInput
	}
	prefix := "/ns/tenant-" + tenant + "/sa/"
	valid := strings.HasPrefix(id.Path(), prefix) && tenantName.MatchString(strings.TrimPrefix(id.Path(), prefix))
	for _, name := range tenantgen.KafkaProducerNames() {
		svc, ok := tenantgen.ServiceByName(name)
		if ok && id.String() == svc.ComputeSPIFFEID(tenant) {
			valid = true
		}
	}
	if !valid {
		return Plan{}, ErrInput
	}
	topics, err := ParseTable(raw, digest, tenant)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{ClusterID: clusterID, Tenant: tenant, Principal: principal, BrokerPrincipal: principal, ManifestSHA: digest, Topics: topics}
	for _, op := range []kafka.ACLOperationType{kafka.ACLOperationTypeWrite, kafka.ACLOperationTypeRead, kafka.ACLOperationTypeDescribe, kafka.ACLOperationTypeCreate} {
		p.ACLs = append(p.ACLs, kafka.ACLEntry{ResourceType: kafka.ResourceTypeTopic, ResourceName: tenant + ".", ResourcePatternType: kafka.PatternTypePrefixed, Principal: principal, Host: "*", Operation: op, PermissionType: kafka.ACLPermissionTypeAllow})
	}
	for _, op := range []kafka.ACLOperationType{kafka.ACLOperationTypeRead, kafka.ACLOperationTypeDescribe} {
		p.ACLs = append(p.ACLs, kafka.ACLEntry{ResourceType: kafka.ResourceTypeGroup, ResourceName: tenant + ".", ResourcePatternType: kafka.PatternTypePrefixed, Principal: principal, Host: "*", Operation: op, PermissionType: kafka.ACLPermissionTypeAllow})
	}
	return p, nil
}

// WithPrincipalEncoding selects a wire representation without changing the
// canonical workload identity. Redpanda's rule replacement preserves escaped
// slashes; this profile must match the independently configured broker rules.
// No profile probes the broker, adopts its response, or changes an existing ACL.
func WithPrincipalEncoding(p Plan, encoding, peer string) (Plan, error) {
	if p.Principal == "" || p.BrokerPrincipal != p.Principal || len(p.ACLs) != 6 {
		return Plan{}, ErrInput
	}
	switch encoding {
	case "spiffe-uri":
		return p, nil
	case "redpanda-rule-escaped-uri":
		if peer != "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda" {
			return Plan{}, ErrInput
		}
	default:
		return Plan{}, ErrInput
	}
	p.BrokerPrincipal = strings.ReplaceAll(p.Principal, "/", `\/`)
	p.ACLs = append([]kafka.ACLEntry(nil), p.ACLs...)
	for i := range p.ACLs {
		if p.ACLs[i].Principal != p.Principal {
			return Plan{}, ErrInput
		}
		p.ACLs[i].Principal = p.BrokerPrincipal
	}
	return p, nil
}
