package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/web_bff/internal/session"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The gateway harness runs this test binary as a child and supplies the two
// synthetic claims over stdin. Both sessions use the normal BFF proxy.
func TestNativeSyntheticBlotterBrowser(t *testing.T) {
	if os.Getenv("SYNTHETIC_BLOTTER_PROBE") != "1" {
		t.Skip("requires the isolated native OMS/gateway qualification process")
	}
	var input struct {
		Gateway, Tenant, Portfolio, Authorized, Foreign string
		State                                           []byte
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if err != nil || len(raw) >= 2<<20 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("invalid synthetic browser input")
	}
	u, err := url.Parse(input.Gateway)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || input.Tenant == "" || input.Portfolio == "" || input.Authorized == "" || input.Foreign == "" {
		t.Fatal("a loopback gateway and synthetic sessions are required")
	}
	state := new(orderpb.OrderState)
	if proto.Unmarshal(input.State, state) != nil || state.GetOrderId() == "" || state.GetPortfolioId() != input.Portfolio {
		t.Fatal("missing native aggregate")
	}
	view, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(state)
	if err != nil {
		t.Fatal("invalid native aggregate")
	}
	base := bffWithIdentity(t, "http://127.0.0.1:1", input.Gateway)
	defer base.Close()
	app, err := New(&Readiness{}, Options{Identity: base.identity, ClientIP: base.clientIP, Sessions: base.sessions, GatewayURL: input.Gateway, StaticDir: os.Getenv("TEST_MFA_STATIC_DIR")})
	if err != nil || app.static == nil {
		t.Fatal("built Vue distribution and normal BFF required")
	}
	defer app.Close()
	site := httptest.NewServer(app)
	defer site.Close()
	cookie := func(tenant, credential string) string {
		value, err := app.sessions.Create(t.Context(), session.Session{Subject: "blotter-reader", Tenant: tenant, AccessToken: credential, Expiry: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal("synthetic BFF session failed")
		}
		return value
	}
	browserInput, err := json.Marshal(map[string]any{"base": site.URL, "authorized": cookie(input.Tenant, input.Authorized), "foreign": cookie(input.Tenant+"-other", input.Foreign), "state": json.RawMessage(view)})
	if err != nil {
		t.Fatal("cannot encode synthetic browser request")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("TEST_MFA_NODE"), os.Getenv("TEST_NATIVE_BLOTTER_SCRIPT"))
	cmd.Stdin = bytes.NewReader(browserInput)
	if err := cmd.Run(); err != nil {
		t.Fatal("actual Chromium blotter checks failed")
	}
	t.Log("native synthetic blotter browser passed")
}
