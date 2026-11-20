package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web_bff/internal/session"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestDailyReturnBFFBrowser(t *testing.T) {
	if os.Getenv("RETURN_PILOT_BFF_CHILD") != "1" {
		t.Skip("requires the isolated pilot gateway process")
	}
	var input struct {
		Gateway, Issuer string
		Key             jose.JSONWebKey
		Claims          map[string]string
		Expected        json.RawMessage
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil || len(raw) >= 64<<10 || json.Unmarshal(raw, &input) != nil || !input.Key.Valid() || !input.Key.IsPublic() {
		t.Fatal("invalid fixture input")
	}
	for _, endpoint := range []string{input.Gateway, input.Issuer} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() != "127.0.0.1" {
			t.Fatal("loopback fixture required")
		}
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 || endpoint != "http://127.0.0.1:"+strconv.FormatUint(port, 10) {
			t.Fatal("loopback fixture required")
		}
	}
	base := bffWithIdentity(t, "http://127.0.0.1:1", input.Gateway)
	defer base.Close()
	app, err := New(&Readiness{}, Options{Identity: base.identity, ClientIP: base.clientIP, Sessions: base.sessions, GatewayURL: input.Gateway, StaticDir: os.Getenv("TEST_MFA_STATIC_DIR")})
	if err != nil || app.static == nil {
		t.Fatal("Vue distribution and actual BFF required")
	}
	defer app.Close()
	site := httptest.NewServer(app)
	defer site.Close()
	cookies := map[string]string{}
	for _, name := range []string{"authorized", "foreign", "excluded", "unfunded"} {
		tenant := "basis-test"
		if name == "foreign" {
			tenant += "-other"
		}
		token := input.Claims[name]
		signed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal("fixture identity refused")
		}
		var standard jwt.Claims
		var scope struct {
			Tenant string `json:"tenant"`
		}
		if signed.Claims(input.Key.Key, &standard, &scope) != nil {
			t.Fatal("fixture identity signature refused")
		}
		const actor = "daily-return-analyst"
		if standard.Validate(jwt.Expected{Issuer: input.Issuer, Subject: actor, AnyAudience: jwt.Audience{"kanz-api"}, Time: time.Now()}) != nil || standard.Expiry == nil {
			t.Fatal("fixture identity claims refused")
		}
		if scope.Tenant != tenant {
			t.Fatal("fixture identity scope refused")
		}
		cookies[name], err = app.sessions.Create(t.Context(), session.Session{Subject: standard.Subject, Tenant: tenant, AccessToken: token, Expiry: time.Now().Add(2 * time.Minute)})
		if err != nil {
			t.Fatal("synthetic BFF session failed")
		}
	}
	wire, err := json.Marshal(map[string]any{"base": site.URL, "cookies": cookies, "expected": input.Expected})
	if err != nil {
		t.Fatal("cannot encode synthetic browser input")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("TEST_MFA_NODE"), os.Getenv("RETURN_PILOT_BROWSER_SCRIPT"))
	cmd.Stdin = bytes.NewReader(wire)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if marker := regexp.MustCompile("return-browser-stage=(?:input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|reload)-(?:response|payload|row|details))").Find(output); marker != nil {
			t.Fatal(string(marker))
		}
		t.Fatal("Chromium probe could not start")
	}
	t.Log("daily return browser passed")
}
