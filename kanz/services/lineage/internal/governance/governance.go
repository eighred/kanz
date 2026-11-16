// Package governance is the LIN-01c PII tagging + access-governance layer. It
// classifies datasets by sensitivity and gates access to PII lineage through the
// AUTH-01b authorizer (deny-by-default), logging every PII access decision into
// the observation stream via the AUTH-01d recorder — so "who looked at whose PII
// lineage" is itself auditable. The tenant scope is the MT-01d boundary: a
// graph record's independently stored owner is checked before classification;
// public and PII data both remain confined to that owner's tenant.
package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// ActionPIIRead is the authorization action governing PII lineage reads. A role
// must be granted it in the AUTH-01b policy bundle; it is deny-by-default.
const ActionPIIRead auth.Action = "lineage.pii.read"

// ResourceDataset is the authz resource type for a lineage dataset.
const ResourceDataset = "dataset"

// Sensitivity is a dataset's data classification.
type Sensitivity string

const (
	SensitivityUnknown Sensitivity = "unknown"
	SensitivityPublic  Sensitivity = "public"
	SensitivityPII     Sensitivity = "pii"
)

// Config declares which datasets carry PII. A dataset is PII if its name or
// id matches Datasets, or its schema ref contains any SchemaRefSubstrings entry.
// Sourced from a mounted file (the same ConfigMap shape as the auth policy), so
// classification changes ship without a rebuild.
type Config struct {
	Datasets            []string `json:"pii_datasets"`
	SchemaRefSubstrings []string `json:"pii_schema_ref_substrings"`
}

// LoadConfig decodes a JSON classification config.
func LoadConfig(r io.Reader) (*Config, error) {
	var c Config
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("governance: decode config: %w", err)
	}
	return &c, nil
}

// LoadConfigFile loads the classification config from a file.
func LoadConfigFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return LoadConfig(f)
}

// Classifier tags datasets by sensitivity from a Config.
type Classifier struct {
	datasets map[string]struct{}
	refSubs  []string
}

func NewClassifier(c *Config) *Classifier {
	cl := &Classifier{datasets: map[string]struct{}{}}
	if c != nil {
		for _, d := range c.Datasets {
			cl.datasets[d] = struct{}{}
		}
		cl.refSubs = append(cl.refSubs, c.SchemaRefSubstrings...)
	}
	return cl
}

// Classify returns the dataset's sensitivity.
func (c *Classifier) Classify(ds graph.DatasetID, schemaRef string) Sensitivity {
	if _, ok := c.datasets[ds.String()]; ok {
		return SensitivityPII
	}
	if _, ok := c.datasets[ds.Name]; ok {
		return SensitivityPII
	}
	for _, sub := range c.refSubs {
		if sub != "" && strings.Contains(schemaRef, sub) {
			return SensitivityPII
		}
	}
	return SensitivityPublic
}

// Governor gates dataset access by sensitivity. Non-PII datasets are public —
// allowed without an authz round-trip or a log entry. PII datasets go through
// the authorizer (wrap it with auth.AuditedAuthorizer so every decision is
// recorded — that is the access-logging half of LIN-01c).
type Governor struct {
	classifier *Classifier
	authz      auth.Authorizer
}

func NewGovernor(classifier *Classifier, authz auth.Authorizer) *Governor {
	return &Governor{classifier: classifier, authz: authz}
}

// CheckOwner is the unconditional isolation gate. Public means non-PII within
// an owned scope, never tenant-agnostic. A caller-constructed/zero Info has no
// independently stored owner and fails closed even under a wildcard authorizer.
func (g *Governor) CheckOwner(p *auth.Principal, info graph.Info) auth.Decision {
	if p == nil {
		return auth.Decision{Code: auth.DenyNoPrincipal, Reason: "no authenticated principal"}
	}
	if p.Tenant == "" {
		return auth.Decision{Code: auth.DenyPrincipalNoTenant, Reason: "principal tenant unknown"}
	}
	if info.Owner() == "" {
		return auth.Decision{Code: auth.DenyResourceTenantUnresolved, Reason: "stored dataset owner unknown"}
	}
	if string(info.Owner()) != p.Tenant {
		return auth.Decision{Code: auth.DenyCrossTenant, Reason: "stored dataset owner differs from principal tenant"}
	}
	return auth.Decision{Allow: true, Reason: "stored owner matches"}
}

// CheckAccess checks ownership BEFORE classification, public allowance or RBAC.
// The resource tenant comes only from the admitted record, not the principal.
func (g *Governor) CheckAccess(ctx context.Context, p *auth.Principal, info graph.Info) (auth.Decision, Sensitivity) {
	if d := g.CheckOwner(p, info); !d.Allow {
		return d, SensitivityUnknown
	}
	sens := g.classifier.Classify(info.ID, info.SchemaRef)
	if sens != SensitivityPII {
		return auth.Decision{Allow: true, Reason: "owned public dataset"}, sens
	}
	return g.authz.Authorize(ctx, auth.Request{
		Principal: p, Action: ActionPIIRead,
		Resource: auth.Resource{Type: ResourceDataset, ID: info.ID.String(), Tenant: string(info.Owner())},
	}), sens
}
