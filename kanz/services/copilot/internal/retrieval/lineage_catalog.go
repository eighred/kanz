package retrieval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
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

// NewLineageCatalog builds a Catalog over client and the lineage service base
// URL.
//
// A nil client gets a private one, NOT http.DefaultClient (#235). copilot passes
// nil today, so every citation lookup this service makes ran on a global that any
// package in the process can retune — and on DefaultTransport, whose 2 idle
// connections per host throttle a path taken once per cited reading. It carries no
// Timeout: the bound is the caller's context, set in Resolve.
func NewLineageCatalog(client *http.Client, baseURL string) *LineageCatalog {
	if client == nil {
		client = &http.Client{Transport: &http.Transport{}}
	}
	return &LineageCatalog{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// lineageResolveTimeout bounds one citation lookup. Small on purpose: this is a
// display detail on an answer the user is waiting for, and Citation.String already
// renders the raw event id when it fails. Slow here is worse than absent.
const lineageResolveTimeout = 3 * time.Second

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
	// BOUND THE CALL, NOT THE CLIENT (#235). This is a cosmetic lookup on the
	// answer path — it decorates a citation — and it had no deadline of its own,
	// so a wedged lineage service held the whole answer for as long as the
	// caller's context allowed, once per cited reading. A failure here is already
	// defined as harmless, which makes giving up early strictly better than
	// waiting. It goes here rather than on the http.Client because a client-level
	// timeout is enforced independently of the context and would win invisibly
	// over the caller's own budget.
	ctx, cancel := context.WithTimeout(ctx, lineageResolveTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/lineage/event/"+url.PathEscape(sourceEventID), nil)
	if err != nil {
		return "", false
	}
	// The SVCWIRE-01c trusted-header contract, minted in exactly one place
	// (#258): copilot is not the identity authority, it FORWARDS the end user's
	// principal so lineage applies PII governance to the human rather than to
	// copilot's SVID.
	if p, ok := auth.PrincipalFromContext(ctx); ok && p != nil {
		auth.SetPrincipalHeaders(req.Header, p.Subject, p.Tenant, p.Roles)
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
