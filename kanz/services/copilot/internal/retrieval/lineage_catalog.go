package retrieval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
)

// Mesh identity headers — the SVCWIRE-01c trusted-header contract. Duplicated
// here (not imported) because the gateway's proxy package that defines them is
// service-internal to api-gateway; these three strings are a wire contract, not
// a shared type.
const (
	headerPrincipalSubject = "X-Kanz-Principal-Subject"
	headerPrincipalTenant  = "X-Kanz-Principal-Tenant"
	headerPrincipalRoles   = "X-Kanz-Principal-Roles" // comma-separated
)

// LineageCatalog is the production Catalog (PARITY-04b): it resolves a governed
// source event id to its LIN-01 lineage dataset node by querying the lineage
// service's `GET /v1/lineage/event/{id}` endpoint, propagating the calling
// principal as the trusted mesh-identity headers so the lineage service applies
// its PII governance for the END USER — not the copilot's SVID. It retires
// IdentityCatalog (which echoed the raw event id as its own node), so a citation
// names the governed dataset the number came from.
//
// The *http.Client is injected so this package stays SPIFFE-free (the SVCWIRE-01c
// stance): the composition root builds the mTLS client from the copilot SVID, or
// a plaintext client for dev.
type LineageCatalog struct {
	client  *http.Client
	baseURL string
}

// NewLineageCatalog builds a Catalog over client (nil ⇒ http.DefaultClient) and
// the lineage service base URL.
func NewLineageCatalog(client *http.Client, baseURL string) *LineageCatalog {
	if client == nil {
		client = http.DefaultClient
	}
	return &LineageCatalog{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

var _ Catalog = (*LineageCatalog)(nil)

// Resolve returns the lineage dataset node ("namespace.name") that produced
// sourceEventID. ok=false — the caller falls back to citing the raw event id
// (Citation.String already does this) — when the id is empty, unknown (404),
// PII-access-denied (403), the lineage service is unreachable, or the response
// is malformed. A resolution failure must never break an answer: grounding does
// not depend on the lineage node, only the display of the citation does.
func (c *LineageCatalog) Resolve(ctx context.Context, sourceEventID string) (string, bool) {
	if sourceEventID == "" {
		return "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/lineage/event/"+url.PathEscape(sourceEventID), nil)
	if err != nil {
		return "", false
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok && p != nil {
		req.Header.Set(headerPrincipalSubject, p.Subject)
		req.Header.Set(headerPrincipalTenant, p.Tenant)
		if len(p.Roles) > 0 {
			req.Header.Set(headerPrincipalRoles, strings.Join(p.Roles, ","))
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var prov struct {
		Target struct {
			Dataset struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"dataset"`
		} `json:"target"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prov); err != nil {
		return "", false
	}
	ns, name := prov.Target.Dataset.Namespace, prov.Target.Dataset.Name
	switch {
	case ns == "" && name == "":
		return "", false
	case ns == "":
		return name, true
	default:
		return ns + "." + name, true // matches graph.DatasetID.String()
	}
}
