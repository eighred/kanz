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
var clusterIdentity = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Plan struct {
	ClusterID   string
	Tenant      string
	Principal   string
	ManifestSHA string
	Topics      []Topic
	ACLs        []kafka.ACLEntry
}

// NewPlan accepts the existing two tenant principal shapes. System topics can
// be parsed for inventory parity, but this tenant-onboarding slice cannot grant
// a prefix for the unprefixed system namespace.
func NewPlan(raw []byte, digest, tenant, principal, clusterID string) (Plan, error) {
	if !clusterIdentity.MatchString(clusterID) || !tenantName.MatchString(tenant) || !strings.HasPrefix(principal, "User:") {
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
	p := Plan{ClusterID: clusterID, Tenant: tenant, Principal: principal, ManifestSHA: digest, Topics: topics}
	for _, op := range []kafka.ACLOperationType{kafka.ACLOperationTypeWrite, kafka.ACLOperationTypeRead, kafka.ACLOperationTypeDescribe, kafka.ACLOperationTypeCreate} {
		p.ACLs = append(p.ACLs, kafka.ACLEntry{ResourceType: kafka.ResourceTypeTopic, ResourceName: tenant + ".", ResourcePatternType: kafka.PatternTypePrefixed, Principal: principal, Host: "*", Operation: op, PermissionType: kafka.ACLPermissionTypeAllow})
	}
	for _, op := range []kafka.ACLOperationType{kafka.ACLOperationTypeRead, kafka.ACLOperationTypeDescribe} {
		p.ACLs = append(p.ACLs, kafka.ACLEntry{ResourceType: kafka.ResourceTypeGroup, ResourceName: tenant + ".", ResourcePatternType: kafka.PatternTypePrefixed, Principal: principal, Host: "*", Operation: op, PermissionType: kafka.ACLPermissionTypeAllow})
	}
	return p, nil
}
