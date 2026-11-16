package binance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/server"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const queryCanary = "dummy-query-canary"
const headerCanary = "dummy-header-canary"
const peerCanary = "dummy-transport-canary"

type diagnosticRoundTrip func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type diagnosticBody struct{ closed bool }

func (*diagnosticBody) Read([]byte) (int, error) { return 0, errors.New(peerCanary) }
func (b *diagnosticBody) Close() error           { b.closed = true; return nil }

func diagnosticResponse(r *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func assertDiagnosticRedacted(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refused request")
	}
	secrets = append(secrets, queryCanary, headerCanary, peerCanary, "signature=")
	for depth := 0; err != nil; depth++ {
		if depth > 10 {
			t.Fatal("error chain is not bounded")
		}
		rendered := fmt.Sprintf("%v | %+v | %#v", err, err, err)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(rendered, secret) {
				t.Fatal("diagnostic retained request or transport material")
			}
		}
		err = errors.Unwrap(err)
	}
}

func newDiagnosticREST(client *http.Client, base string) *binanceREST {
	return newBinanceREST(restConfig{BaseURL: base, APIKey: headerCanary, APISecret: "dummy-signing-secret", HTTPClient: client, Bucket: newWeightBucket(1200, time.Minute, nil), Now: func() time.Time { return time.UnixMilli(1700000000000) }})
}

func diagnosticCalls() map[string]func(context.Context, *http.Client, string) error {
	return map[string]func(context.Context, *http.Client, string) error{
		"identity": func(ctx context.Context, client *http.Client, base string) error {
			_, err := newDiagnosticREST(client, base).ExchangeAccountID(ctx)
			return err
		},
		"signed": func(ctx context.Context, client *http.Client, base string) error {
			r := newDiagnosticREST(client, base)
			_, err := r.signedOrderCall(ctx, http.MethodGet, "/api/v3/order", url.Values{"symbol": {"BTCUSDT"}, "note": {queryCanary}}, 1)
			return err
		},
		"public": func(ctx context.Context, client *http.Client, base string) error {
			r := newDiagnosticREST(client, base)
			_, err := r.tickerPrice(ctx, queryCanary)
			return err
		},
	}
}

func TestRESTFailureIsRedactedAtQueryRPCAndLog(t *testing.T) {
	client := &http.Client{Transport: diagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("%s %s %v", peerCanary, req.URL.String(), req.Header)
	})}
	rest := newDiagnosticREST(client, "https://fixture.invalid")
	venue := &BinanceVenue{mic: "BINANCE", rest: rest, symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, now: time.Now}
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	rpc := server.New(venue, nil, nil, execution.AccountProof{}, nil, logger)
	_, err := rpc.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: &orderpb.OrderState{OrderId: "fixture-order", InstrumentId: "BTC-USD"}})
	if status.Code(err) != codes.Unavailable {
		t.Fatal("transport failure changed RPC status")
	}
	assertDiagnosticRedacted(t, err)
	logger.Error("fixture query failure", "err", err)
	for _, secret := range []string{queryCanary, headerCanary, peerCanary, "signature="} {
		if strings.Contains(log.String(), secret) {
			t.Fatal("structured diagnostic exposed transport material")
		}
	}
}

func TestRESTTransportDiagnosticsAreRedacted(t *testing.T) {
	for name, call := range diagnosticCalls() {
		for _, stage := range []string{"construct", "round_trip", "body"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				body := &diagnosticBody{}
				var secrets []string
				calls := 0
				client := &http.Client{Transport: diagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					secrets = append(secrets, req.Header.Get("X-MBX-APIKEY"), req.Header.Get("OK-ACCESS-KEY"), req.Header.Get("OK-ACCESS-SIGN"), req.URL.Query().Get("signature"))
					if stage == "body" {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
					}
					return nil, fmt.Errorf("%s %s %v", peerCanary, req.URL.String(), req.Header)
				})}
				base := "https://fixture.invalid"
				if stage == "construct" {
					base = "http://[invalid"
				}
				err := call(context.Background(), client, base)
				assertDiagnosticRedacted(t, err, secrets...)
				if stage == "construct" && calls != 0 {
					t.Fatal("malformed request reached transport")
				}
				if stage == "body" && !body.closed {
					t.Fatal("failed response body was not closed")
				}
				var network net.Error
				if stage == "body" && errors.As(err, &network) {
					t.Fatal("ordinary body failure became a network error")
				}
			})
		}
	}
}

func TestRESTTransportContextAndTimeoutIdentity(t *testing.T) {
	for name, call := range diagnosticCalls() {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(name+"/"+cause.Error(), func(t *testing.T) {
				client := &http.Client{Transport: diagnosticRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("%s: %w", peerCanary, cause) })}
				err := call(context.Background(), client, "https://fixture.invalid")
				if !errors.Is(err, cause) {
					t.Fatal("context identity was lost")
				}
				assertDiagnosticRedacted(t, err)
			})
		}
		t.Run(name+"/client_timeout", func(t *testing.T) {
			client := &http.Client{Timeout: 20 * time.Millisecond, Transport: diagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			})}
			err := call(context.Background(), client, "https://fixture.invalid")
			var timed net.Error
			if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &timed) || !timed.Timeout() {
				t.Fatal("HTTP client timeout identity was lost")
			}
			assertDiagnosticRedacted(t, err)
		})
	}
}

func TestRESTRedirectPolicyAndSignedRequestRemainIntact(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprint(refuse), func(t *testing.T) {
			calls, redirects := 0, 0
			client := &http.Client{Transport: diagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if req.Method != http.MethodGet || req.URL.Path != "/api/v3/order" || req.Header.Get("X-MBX-APIKEY") != headerCanary || req.URL.Query().Get("note") != queryCanary || req.URL.Query().Get("timestamp") != "1700000000000" || req.URL.Query().Get("recvWindow") != "5000" || req.URL.Query().Get("signature") == "" {
						t.Fatal("signed request fields changed")
					}
					resp := diagnosticResponse(req, 307, `{"orderId":7}`)
					resp.Header.Set("Location", "/accepted?note="+queryCanary)
					return resp, nil
				}
				if req.URL.Path != "/accepted" {
					t.Fatal("redirect destination changed")
				}
				return diagnosticResponse(req, 200, `{"orderId":7}`), nil
			}), CheckRedirect: func(*http.Request, []*http.Request) error {
				redirects++
				if refuse {
					return errors.New(peerCanary)
				}
				return nil
			}}
			err := diagnosticCalls()["signed"](context.Background(), client, "https://fixture.invalid")
			if redirects != 1 {
				t.Fatal("redirect policy was bypassed")
			}
			if refuse {
				assertDiagnosticRedacted(t, err)
				if calls != 1 {
					t.Fatal("refused redirect sent another request")
				}
			} else if err != nil || calls != 2 {
				t.Fatal("valid redirect no longer succeeds")
			}
		})
	}
}

func TestRESTLoopbackSuccessPreservesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"orderId":7}`)
	}))
	defer srv.Close()
	if err := diagnosticCalls()["signed"](context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal("loopback success no longer decodes")
	}
}

func TestRESTHTTPAndBusinessClassificationsRemainIntact(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: diagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
				return diagnosticResponse(req, status, `{"code":-2010,"msg":"fixture business refusal"}`), nil
			})}
			r := newDiagnosticREST(client, "https://fixture.invalid")
			_, err := r.signedOrderCall(context.Background(), http.MethodGet, "/api/v3/order", url.Values{"note": {queryCanary}}, 1)
			switch status {
			case 200, 400:
				var api *APIError
				if !errors.As(err, &api) || api.Code != -2010 || api.Msg != "fixture business refusal" {
					t.Fatal("business API error changed")
				}
			case 401, 403:
				if !errors.Is(err, ErrEgressDenied) {
					t.Fatal("auth refusal classification changed")
				}
				assertDiagnosticRedacted(t, err)
			case 429:
				if !errors.Is(err, ErrRateLimited) {
					t.Fatal("rate-limit classification changed")
				}
			case 500:
				assertDiagnosticRedacted(t, err)
				if !strings.Contains(err.Error(), "500") {
					t.Fatal("HTTP failure lost status context")
				}
			}
		})
	}
}
