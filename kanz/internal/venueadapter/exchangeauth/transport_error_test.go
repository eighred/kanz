package exchangeauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type accountDiagnosticRoundTrip func(*http.Request) (*http.Response, error)

func (f accountDiagnosticRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type accountDiagnosticBody struct{ closed bool }

func (*accountDiagnosticBody) Read([]byte) (int, error) { return 0, errors.New("dummy-body-canary") }
func (b *accountDiagnosticBody) Close() error           { b.closed = true; return nil }

func TestAccountTransportFailuresNeverExposeCredentials(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		for _, phase := range []string{"construct", "transport", "body"} {
			t.Run(venue+"/"+phase, func(t *testing.T) {
				body := &accountDiagnosticBody{}
				client := &http.Client{Transport: accountDiagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
					if phase == "body" {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
					}
					return nil, fmt.Errorf("dummy-peer-canary %s %v", req.URL.String(), req.Header)
				})}
				base := "https://fixture.invalid"
				if phase == "construct" {
					base = "http://[invalid"
				}
				id, err := AccountID(context.Background(), venue, Credential{APIKey: "dummy-key-canary", APISecret: "dummy-secret-canary", Passphrase: "dummy-passphrase-canary"}, Options{BaseURL: base, HTTPClient: client, OKXTrading: OKXDemo, Now: func() time.Time { return time.UnixMilli(1700000000000) }})
				if id != "" || err == nil {
					t.Fatal("failed identity call produced an account")
				}
				for depth := 0; err != nil; depth++ {
					if depth > 10 {
						t.Fatal("unbounded error chain")
					}
					text := fmt.Sprintf("%v | %+v | %#v", err, err, err)
					if strings.Contains(text, "canary") || strings.Contains(text, "signature=") || strings.Contains(text, "fixture.invalid") {
						t.Fatal("identity failure retained request or peer material")
					}
					err = errors.Unwrap(err)
				}
				if phase == "body" && !body.closed {
					t.Fatal("identity error left response open")
				}
			})
		}
	}
}

func TestAccountTransportCancellationIdentityAndModeRemainIntact(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(venue+"/"+cause.Error(), func(t *testing.T) {
				client := &http.Client{Transport: accountDiagnosticRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("dummy-peer-canary: %w", cause) })}
				_, err := AccountID(context.Background(), venue, Credential{}, Options{BaseURL: "https://fixture.invalid", HTTPClient: client, OKXTrading: OKXDemo})
				if !errors.Is(err, cause) {
					t.Fatal("identity call lost context cause")
				}
			})
		}
	}
	calls := 0
	client := &http.Client{Transport: accountDiagnosticRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"0","data":[{"uid":"fixture-account"}]}`)), Header: make(http.Header)}, nil
	})}
	_, err := AccountID(context.Background(), "okx", Credential{}, Options{BaseURL: "https://fixture.invalid", HTTPClient: client})
	if !errors.Is(err, ErrUnknownOKXTradingMode) || calls != 0 {
		t.Fatal("unknown trading mode no longer refuses before transport")
	}
}
