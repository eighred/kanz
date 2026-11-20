package refdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/transport"
)

const SectorHistoryPath = "/v1/sector-classifications"
const SectorHistoryLimit = 64

// DatedSector is Datamaster's resolved classification for one exact UTC day.
// An empty taxonomy/code pair is an explicit unknown observation.
type DatedSector struct {
	InstrumentID  string    `json:"instrument_id"`
	EffectiveDate string    `json:"effective_date"`
	SourceAsOf    time.Time `json:"source_as_of"`
	KnownAt       time.Time `json:"known_at"`
	Taxonomy      string    `json:"taxonomy"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Vendor        string    `json:"vendor"`
}

// SectorHistory reads from a tenant-pinned Datamaster instance.
type SectorHistory struct {
	endpoint *url.URL
	tenant   string
	client   *http.Client
}

func NewSectorHistory(base, tenant string, mesh *transport.Mesh) (*SectorHistory, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || tenant == "" || tenant == "__system__" {
		return nil, errors.New("refdata: explicit Datamaster history endpoint and tenant required")
	}
	tr := &http.Transport{MaxIdleConnsPerHost: 4}
	switch u.Scheme {
	case "https":
		if !mesh.Enabled() {
			return nil, errors.New("refdata: Datamaster history HTTPS requires a workload identity")
		}
		peer, err := transport.ServiceID("kanz-services", "datamaster")
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = transport.ClientTLSConfig(mesh.Source, transport.AuthorizeServices(peer))
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() || mesh.Enabled() {
			return nil, errors.New("refdata: plaintext Datamaster history requires literal local loopback")
		}
	default:
		return nil, errors.New("refdata: invalid Datamaster history scheme")
	}
	u.Path = SectorHistoryPath
	return &SectorHistory{endpoint: u, tenant: tenant, client: &http.Client{
		Transport: tr, Timeout: DefaultFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// At resolves one whole batch at the effective day and knowledge cut. An
// unavailable service is an error, distinct from absent or unknown rows.
func (s *SectorHistory) At(ctx context.Context, tenant, date string, asOf time.Time, ids []string) ([]DatedSector, error) {
	if tenant != s.tenant || len(ids) == 0 || len(ids) > SectorHistoryLimit {
		return nil, errors.New("refdata: sector history request scope invalid")
	}
	u := *s.endpoint
	q := url.Values{"effective_date": {date}, "as_of": {asOf.UTC().Format(time.RFC3339Nano)}, "instrument_id": ids}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	auth.SetPrincipalHeaders(req.Header, "accounting", tenant, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("refdata: Datamaster history unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refdata: Datamaster history HTTP %d", resp.StatusCode)
	}
	var rows []DatedSector
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&rows); err != nil || len(rows) > len(ids) {
		return nil, errors.New("refdata: invalid dated classification response")
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !slices.Contains(ids, row.InstrumentID) || seen[row.InstrumentID] || row.EffectiveDate != date ||
			row.KnownAt.IsZero() || row.KnownAt.After(asOf) || row.SourceAsOf.IsZero() || row.SourceAsOf.After(row.KnownAt) ||
			(row.Taxonomy == "") != (row.Code == "") {
			return nil, errors.New("refdata: dated classification response has invalid scope or time")
		}
		seen[row.InstrumentID] = true
	}
	slices.SortFunc(rows, func(a, b DatedSector) int { return strings.Compare(a.InstrumentID, b.InstrumentID) })
	return rows, nil
}
