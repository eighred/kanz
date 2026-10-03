package capital

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// HistoryReader reads the authenticated accounting domain API. Production uses
// HistoryClient through the gateway's Fund-capability and portfolio checks.
type HistoryReader interface {
	ReadCashCommits(context.Context, string, string, int64, int64, int) (cashview.CommitPage, error)
}

type HistoryClient struct {
	base  *url.URL
	http  *http.Client
	token func(context.Context) (string, error)
}

// NewHistoryClient requires HTTPS and refuses redirects: an upstream redirect
// must never move a financial evidence request or its bearer to another route.
// The token callback reads the deployment's current credential at request time,
// allowing rotation without retaining an expired bearer in the client.
func NewHistoryClient(endpoint string, client *http.Client, token func(context.Context) (string, error)) (*HistoryClient, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Path != "" && base.Path != "/") || token == nil {
		return nil, errors.New("capital: history requires an HTTPS gateway origin and credential provider")
	}
	var bounded http.Client
	if client != nil {
		bounded = *client
	}
	if bounded.Transport == nil {
		bounded.Transport = &http.Transport{
			Proxy:        http.ProxyFromEnvironment,
			DialContext:  (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns: 32, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 8,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		}
	}
	bounded.Timeout = 0 // request context owns the complete call budget
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HistoryClient{base: base, http: &bounded, token: token}, nil
}

// CloseIdleConnections releases pooled sockets after the caller has stopped
// and joined its consumers. It does not interrupt an active evidence request.
func (c *HistoryClient) CloseIdleConnections() { c.http.CloseIdleConnections() }

func (c *HistoryClient) ReadCashCommits(ctx context.Context, portfolio, currency string, after, through int64, limit int) (cashview.CommitPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var page cashview.CommitPage
	if bus.TenantIDFromContext(ctx) == "" || !validID(portfolio) || !validID(currency) || after < 0 || through < 0 || through != 0 && after > through || limit < 1 || limit > 64 {
		return page, ErrInvalid
	}
	endpoint := *c.base
	endpoint.Path = "/v1/portfolios/" + portfolio + "/cash-commits"
	endpoint.RawPath = "/v1/portfolios/" + url.PathEscape(portfolio) + "/cash-commits"
	query := url.Values{"currency": {currency}, "after": {strconv.FormatInt(after, 10)}, "through": {strconv.FormatInt(through, 10)}, "limit": {strconv.Itoa(limit)}}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return page, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return page, errors.New("capital: history credential unavailable")
	}
	if strings.TrimSpace(token) == "" || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
		return page, errors.New("capital: history credential unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		// Transport implementations can include request headers in their errors.
		// No diagnostic from that boundary may copy a bearer into a DLQ or log.
		return page, errors.New("capital: history transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return page, fmt.Errorf("capital: history request refused (HTTP %d)", response.StatusCode)
	}
	// 4 MiB raw payloads expand under base64. Bound the complete JSON before
	// decoding, and independently validate raw bytes and record counts below.
	const maxResponse = 6 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		return page, errors.New("capital: history response read failed")
	}
	if len(body) > maxResponse {
		return page, ErrInvalid
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, ErrInvalid
	}
	if err := validateHistoryPage(page, bus.TenantIDFromContext(ctx), portfolio, currency, after, through, limit); err != nil {
		return cashview.CommitPage{}, err
	}
	return page, nil
}

func validateHistoryPage(page cashview.CommitPage, tenant, portfolio, currency string, after, through int64, limit int) error {
	if tenant == "" || page.TenantID != tenant || page.PortfolioID != portfolio || page.Currency != currency || page.Through <= 0 ||
		through != 0 && page.Through != through || page.Next < after || page.Next > page.Through ||
		page.HasMore != (page.Next < page.Through) || len(page.Records) > limit ||
		page.Next-after != int64(len(page.Records)) || page.HasMore && len(page.Records) == 0 {
		return ErrInvalid
	}
	bytes := 0
	for index, record := range page.Records {
		bytes += len(record.Payload)
		if record.Revision != after+int64(index)+1 || len(record.Payload) == 0 || len(record.Payload) > 512<<10 || bytes > 4<<20 {
			return ErrInvalid
		}
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(record.Payload, &msg); err != nil || msg.GetPortfolioId() != portfolio || msg.GetBaseCurrency() != currency || msg.GetCashCommit().GetRevision() != record.Revision {
			return ErrInvalid
		}
	}
	return nil
}

// RecoverCashPrefix repairs a transport gap from retained accounting evidence.
// It never rewrites receipts, invents an opening balance, or clears quarantine.
// A failed page leaves a durable prefix that a fresh process can resume.
func RecoverCashPrefix(ctx context.Context, pool *pgxpool.Pool, source HistoryReader, portfolio, currency string, through int64) error {
	if source == nil || !validID(portfolio) || !validID(currency) || through <= 0 || bus.TenantIDFromContext(ctx) == "" {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var after int64
	err := pool.QueryRow(ctx, `SELECT revision FROM capital_balances WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency).Scan(&after)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// Large catch-up is an explicit reconciliation operation, not an unbounded
	// broker handler that holds its delivery until AckWait has expired.
	if through-after > 4096 {
		return fmt.Errorf("capital: retained prefix exceeds automatic recovery bound")
	}
	for after < through {
		page, err := source.ReadCashCommits(ctx, portfolio, currency, after, through, 64)
		if err != nil {
			return err
		}
		if err := validateHistoryPage(page, bus.TenantIDFromContext(ctx), portfolio, currency, after, through, 64); err != nil {
			return err
		}
		for _, record := range page.Records {
			if err := ApplyPayload(ctx, pool, record.Payload); err != nil {
				return err
			}
		}
		after = page.Next
	}
	return nil
}
