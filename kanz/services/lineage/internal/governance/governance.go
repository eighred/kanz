// Package governance is the LIN-01c PII tagging + access-governance layer. It
// classifies datasets by sensitivity and gates access to PII lineage through the
// AUTH-01b authorizer (deny-by-default), logging every PII access decision into
// the observation stream via the AUTH-01d recorder — so "who looked at whose PII
// lineage" is itself auditable. The tenant scope is the MT-01d boundary: a
// principal's tenant is bound onto the resource, so the authorizer's cross-tenant
// guard runs on PII access too.
package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/services/lineage/internal/graph"
)

// ActionPIIRead is the authorization action governing PII lineage reads. A role
// must be granted it in the AUTH-01b policy bundle; it is deny-by-default.
const ActionPIIRead auth.Action = "lineage.pii.read"

// ResourceDataset is the authz resource type for a lineage dataset.
const ResourceDataset = "dataset"

// Sensitivity is a dataset's data classification.
type Sensitivity string

const (
	SensitivityPublic Sensitivity = "public"
	SensitivityPII    Sensitivity = "pii"
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

// CheckAccess decides whether principal p may read dataset ds. It returns the
// dataset's sensitivity alongside the decision so callers can redact vs. expose.
// A nil principal is denied on PII (no identity, no access); public is always
// allowed.
func (g *Governor) CheckAccess(ctx context.Context, p *auth.Principal, ds graph.DatasetID, schemaRef string) (auth.Decision, Sensitivity) {
	sens := g.classifier.Classify(ds, schemaRef)
	if sens != SensitivityPII {
		return auth.Decision{Allow: true, Reason: "public dataset"}, sens
	}
	tenant := ""
	if p != nil {
		tenant = p.Tenant
	}
	d := g.authz.Authorize(ctx, auth.Request{
		Principal: p,
		Action:    ActionPIIRead,
		Resource:  auth.Resource{Type: ResourceDataset, ID: ds.String(), Tenant: tenant},
	})
	return d, sens
}
