package refdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
)

// HTTPSource reads the golden security master over datamaster's
// GET /v1/securities/{id}.
//
// # Why this hop exists at all
//
// datamaster is the only writer of golden_records and the resolution is a
// projection, not a request-time computation — so the classification a mandate
// needs is already resolved, in another service's database, behind an
// authenticated read. Reproducing survivorship in each consumer would be the
// second implementation of the concept CLAUDE.md names as the way a fix stops
// spreading.
//
// # Identity on this hop
//
// datamaster serves ONE tenant per instance and refuses any caller whose
// X-Kanz-Principal-Tenant is not that tenant (auth.RequireCallerTenantIs). This
// is a SERVICE ACTING ON ITS OWN BEHALF — there is no end user behind a refresh
// cycle — so the subject is the calling service's own name and the tenant is
// the tenant that service is deployed for. The headers are written through
// auth.SetPrincipalHeaders and never by hand: pkg/auth carries the constant and
// the enforcement together precisely so a new caller cannot adopt the name and
// reinvent the check.
//
// That trust is sound only while a NetworkPolicy keeps datamaster's port closed
// to everyone else; infra/security/runtime/network-policies.yaml carries the
// rule that admits these three callers and nothing more.
type HTTPSource struct {
	baseURL string
	tenant  string
	subject string
	timeout time.Duration
	client  *http.Client
}

// DefaultFetchTimeout bounds ONE reference lookup. It is deliberately short:
// the caller is a background refresh cycle, a slow answer costs the cycle its
// remaining budget, and a cold cache refuses rather than waits — so there is
// nothing to be gained by being patient here.
const DefaultFetchTimeout = 5 * time.Second

// maxBodyBytes bounds what one response may cost this process. A golden record
// is a few hundred bytes; anything approaching this is a misrouted response or
// a compromised peer, and an unbounded io.ReadAll on a shared refresh path is
// how one bad answer becomes the whole pod's memory.
const maxBodyBytes = 1 << 20

// NewHTTPSource builds the source. baseURL is datamaster's root
// ("http://datamaster.kanz-services.svc:8080"); tenant is the tenant this
// deployment serves; subject names the CALLING SERVICE, and appears in
// datamaster's logs as who asked.
func NewHTTPSource(baseURL, tenant, subject string) (*HTTPSource, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("refdata: NewHTTPSource needs datamaster's base URL")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("refdata: %q is not a usable datamaster base URL (want scheme://host[:port])", baseURL)
	}
	// BOTH ARE REQUIRED AND NEITHER HAS A DEFAULT. An empty tenant would send a
	// header datamaster answers 404 to, and a 404 is this package's word for "the
	// master does not hold this instrument" — so a misconfigured tenant would
	// present as a security master that knows nothing, evict every resident
	// record, and be indistinguishable from an estate whose reference data was
	// never loaded. It fails at construction instead, which is a refusal to start.
	if strings.TrimSpace(tenant) == "" {
		return nil, errors.New("refdata: NewHTTPSource needs the tenant this deployment serves; an " +
			"empty one is refused by datamaster as a 404, which this package would read as " +
			"\"no such instrument\" for every instrument")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, errors.New("refdata: NewHTTPSource needs a subject naming the calling service")
	}
	return &HTTPSource{
		baseURL: baseURL,
		tenant:  tenant,
		subject: subject,
		timeout: DefaultFetchTimeout,
		// A CLIENT THIS PACKAGE OWNS, never http.DefaultClient: that global can be
		// retuned by any package in the binary, from somewhere no reader of this
		// file would look, and its transport caps idle connections per host at 2 —
		// connection churn on exactly this fan-out. No Timeout field either; the
		// bound is the per-call context deadline below, which the request context
		// can tighten and a client timeout could not.
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}},
	}, nil
}

// Fetch reads one instrument's golden record.
//
// 404 is NOT an error: it is datamaster's definitive answer that this tenant's
// master does not hold the instrument, and the cache acts on it (it evicts).
// Every other non-200 IS an error, so a 500 or a 403 leaves the resident record
// in place rather than quietly withdrawing a classification because the peer
// was unwell.
func (s *HTTPSource) Fetch(ctx context.Context, instrumentID string) (Record, bool, error) {
	if strings.TrimSpace(instrumentID) == "" {
		return Record{}, false, errors.New("refdata: cannot fetch an empty instrument id")
	}
	// The refresh cycle's context has the cycle's budget; this bounds the ONE
	// call inside it, so a single unresponsive lookup cannot spend the whole
	// cycle and starve every instrument behind it.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// PathEscape, not string concatenation: an instrument id is vendor-supplied
	// (a RIC carries a dot, a symbol can carry a slash), and an id containing "/"
	// or ".." would otherwise address a different route on datamaster.
	endpoint := s.baseURL + "/v1/securities/" + url.PathEscape(instrumentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Record{}, false, fmt.Errorf("refdata: build request: %w", err)
	}
	auth.SetPrincipalHeaders(req.Header, s.subject, s.tenant, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return Record{}, false, fmt.Errorf("refdata: GET %s: %w", endpoint, err)
	}
	defer func() {
		// Drained before Close so the connection returns to the idle pool instead
		// of being torn down — on a refresh that fetches hundreds of instruments,
		// an undrained body is a new TCP handshake per record.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Record{}, false, nil
	case resp.StatusCode != http.StatusOK:
		return Record{}, false, fmt.Errorf("refdata: datamaster answered %s for %s", resp.Status, instrumentID)
	}

	var body securityBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return Record{}, false, fmt.Errorf("refdata: decode %s: %w", instrumentID, err)
	}
	rec, err := body.record(instrumentID)
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// securityBody mirrors datamaster's handleSecurity response. Only the
// classification half is decoded — the identifiers and the provenance are that
// surface's business, and decoding fields this package does not use would make
// it break on changes that do not concern it.
type securityBody struct {
	InstrumentID string `json:"instrument_id"`
	AssetClass   string `json:"asset_class"`
	IssuerID     string `json:"issuer_id"`
	Sector       struct {
		Taxonomy string `json:"taxonomy"`
		Code     string `json:"code"`
		Name     string `json:"name"`
	} `json:"sector"`
	AsOf string `json:"as_of"`
}

// record converts the wire body, refusing anything self-contradictory.
func (b securityBody) record(requested string) (Record, error) {
	// A BODY FOR A DIFFERENT INSTRUMENT IS A REFUSAL, not a record. It would mean
	// a misrouted response or a proxy serving a cached answer for another key,
	// and installing it would classify one instrument under another's sector —
	// silently, and for as long as the entry lived.
	if b.InstrumentID != "" && b.InstrumentID != requested {
		return Record{}, fmt.Errorf("refdata: asked datamaster for %q and it answered for %q",
			requested, b.InstrumentID)
	}
	rec := Record{
		InstrumentID: requested,
		AssetClass:   b.AssetClass,
		IssuerID:     b.IssuerID,
		Sector:       Sector{Taxonomy: b.Sector.Taxonomy, Code: b.Sector.Code, Name: b.Sector.Name},
	}
	if b.AsOf != "" {
		t, err := time.Parse(time.RFC3339, b.AsOf)
		if err != nil {
			// An unparseable as_of is refused rather than zeroed. A zero as_of means
			// "undated" to Cache.Lookup, which is a real state with its own
			// point-in-time behaviour; silently manufacturing it out of a decode
			// failure would hide a wire-format break behind a legitimate-looking
			// answer.
			return Record{}, fmt.Errorf("refdata: %s: as_of %q is not RFC3339: %w", requested, b.AsOf, err)
		}
		rec.AsOf = t
	}
	return rec, nil
}

var _ Source = (*HTTPSource)(nil)
