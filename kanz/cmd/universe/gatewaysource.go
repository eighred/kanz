package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
)

// gatewaySource drives the estate through the api-gateway's /v1/control routes
// (OPS-M2c).
//
// IT REPLACED A DIRECT gRPC DIAL, and the replacement is the point. The TUI used to
// dial operator.v1 in plaintext at localhost:9090, which only worked inside a
// `kubectl port-forward` the human had to start. So the operator tool required a
// kubeconfig, a cluster credential, and enough Kubernetes knowledge to know that
// port-forwarding was the missing step — for routine work like rotating a venue key.
//
// Now the identity is the operator's OWN: a bearer token the gateway validates
// against the same authority as every other client (OIDC in production, the HS256
// dev validator on a rig). The operator surface is behind the platform's single
// identity authority rather than beside it, and the TUI needs no cluster access at
// all — it is an ordinary API client that happens to render a table.
//
// RESPONSES ARE protojson INTO THE GENERATED TYPES, not into hand-written structs.
// The gateway forwards the operator's own messages, so decoding them into anything
// else would be a second definition of the same contract, free to drift.
type gatewaySource struct {
	base    string
	token   string
	signKey []byte
	hc      *http.Client
}

// gatewayConfig is what the TUI needs to reach the control plane. All three come
// from flags/env in main.go — none of them is a kubeconfig.
type gatewayConfig struct {
	// BaseURL of the gateway, e.g. https://api.eighred.com.
	BaseURL string
	// Token is the bearer credential identifying the HUMAN. The gateway decides
	// what it may do (the operator role carries authz.Operate); this tool asserts
	// nothing about its own authority.
	Token string
	// SigningSecret is the shared HMAC key when the deployment sets
	// API_GATEWAY_SIGNING_SECRET. Empty is valid — the gateway's Signing middleware
	// is a no-op when it holds no secret, so a deployment without one needs none
	// here either.
	SigningSecret string

	// THERE IS DELIBERATELY NO Timeout HERE. Every call is bounded by the context its
	// caller passes (Config.CallTimeout for ordinary reads, testConnTimeout for Test
	// Connection) — see the http.Client construction in newGatewaySource.
}

func newGatewaySource(cfg gatewayConfig) (*gatewaySource, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("no gateway URL: set --gateway-url or KANZ_GATEWAY_URL " +
			"(e.g. https://api.eighred.com). The TUI reaches the estate through the API " +
			"gateway; it does not talk to the cluster directly")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gateway URL %q is not a valid absolute URL (want scheme://host)", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("no token: set --token-file or KANZ_TOKEN. The gateway " +
			"authenticates a PERSON — this tool holds no authority of its own")
	}
	return &gatewaySource{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		signKey: []byte(cfg.SigningSecret),
		// NO Timeout ON THIS CLIENT, ON PURPOSE. Every call site sets an explicit
		// context deadline, and http.Client.Timeout is enforced INDEPENDENTLY of the
		// request context: a client with both bounds is bounded by min(the two), so the
		// smaller silently wins and the layer ordering can no longer be read off the
		// constants that declare it. That is not theoretical — a 30s client timeout sat
		// underneath the 60s the operator needs to run a probe Job, so Test Connection
		// died client-side reporting "Client.Timeout exceeded while awaiting headers"
		// and pointed the operator at the gateway and the network instead of the host
		// they were probing. The per-call context is the single source of truth for how
		// long a call may take; test/arch asserts this literal stays bare.
		hc: &http.Client{},
	}, nil
}

// call performs one control-plane request. req may be nil for bodiless calls; resp
// may be nil when the response is not needed.
func (g *gatewaySource) call(ctx context.Context, method, path string, req, resp proto.Message) error {
	// The HTTP client deliberately carries no Timeout of its own, so the caller's context is
	// the ONLY thing bounding this request. A call site that forgets a deadline would
	// therefore hang the TUI forever on an unresponsive gateway — the worst failure mode in
	// an operator tool, because it looks like a frozen program rather than an error. Refuse
	// it instead: this is a programming mistake, and it should be loud and immediate the
	// first time it is exercised rather than a hang someone has to bisect.
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("internal: %s %s was called with no context deadline; every "+
			"control-plane call must set one (the HTTP client sets no timeout of its own)",
			method, path)
	}

	var body []byte
	if req != nil {
		var err error
		body, err = protojson.Marshal(req)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, g.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.token)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// X-API-Version is deliberately NOT sent. The gateway validates it only when
	// present, and the constant lives in an internal/ package this binary cannot
	// import — sending a hardcoded copy would be a second spelling of the version,
	// free to drift into a 406 that looks like an outage.
	if len(g.signKey) > 0 {
		httpReq.Header.Set("X-Signature", sign(g.signKey, method, path, body))
	}

	res, err := g.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("gateway unreachable at %s: %w", g.base, err)
	}
	defer func() { _ = res.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return gatewayError(res.StatusCode, payload)
	}
	if resp == nil || len(payload) == 0 {
		return nil
	}
	// The gateway may add fields this build predates; that is a forward-compatible
	// server, not a malformed response, so the TUI tolerates them. (The gateway
	// does NOT do this for REQUESTS — see control.decode.)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// sign reproduces the gateway's Signing middleware: HMAC-SHA256 over
// method \n path \n body, base64 raw-url encoded.
func sign(key []byte, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(method + "\n" + path + "\n"))
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// gatewayError turns a non-2xx into a message an operator can act on WITHOUT
// knowing how the platform is wired. Each of these was previously diagnosable only
// by knowing about port-forwards, roles or middleware.
func gatewayError(status int, payload []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &body)
	detail := strings.TrimSpace(body.Error)

	switch status {
	case http.StatusUnauthorized:
		if strings.Contains(detail, "signature") {
			return fmt.Errorf("the gateway requires signed requests and this one was not "+
				"signed or did not match: set --signing-secret-file or KANZ_SIGNING_SECRET to the "+
				"same value as the gateway's API_GATEWAY_SIGNING_SECRET (%s)", detail)
		}
		return fmt.Errorf("the gateway rejected the token — it is expired, malformed, or "+
			"issued by another authority (%s)", detail)
	case http.StatusForbidden:
		return fmt.Errorf("your account is authenticated but lacks the operator role: the " +
			"control plane requires the role the gateway is configured with as " +
			"API_GATEWAY_OPERATOR_ROLE")
	case http.StatusNotFound:
		return fmt.Errorf("this gateway fronts no control plane (404): it was started without " +
			"API_GATEWAY_OPERATOR_ADDR, so the /v1/control routes are not registered")
	case http.StatusNotImplemented:
		return fmt.Errorf("the operator is running but this capability is switched off in its "+
			"deployment (%s)", detail)
	case http.StatusPreconditionFailed:
		// S4b: the exchange itself refused the credential.
		return fmt.Errorf("%s", detail)
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("the gateway could not reach the control plane (%d): the operator "+
			"may be down or the network policy between them may be missing", status)
	}
	if detail == "" {
		detail = strings.TrimSpace(string(payload))
	}
	return fmt.Errorf("gateway returned %d: %s", status, detail)
}

// --- nodeSource ------------------------------------------------------------

func (g *gatewaySource) fetch(ctx context.Context) (fetchMsg, error) {
	var nodes operatorpb.ListNodesResponse
	if err := g.call(ctx, http.MethodGet, "/v1/control/nodes", nil, &nodes); err != nil {
		return fetchMsg{}, err
	}
	var clusters operatorpb.ListClustersResponse
	if err := g.call(ctx, http.MethodGet, "/v1/control/clusters", nil, &clusters); err != nil {
		return fetchMsg{}, err
	}
	// Secondary reads stay best-effort, exactly as the gRPC source had them: a
	// transient failure degrades one pane for one tick rather than the whole poll.
	provisions, _ := g.listProvisions(ctx)
	venues, _ := g.listVenueKeys(ctx)
	return fetchMsg{
		nodes:      toNodeRows(nodes.GetNodes()),
		clusters:   toClusterRows(clusters.GetClusters()),
		provisions: provisions,
		venues:     venues,
	}, nil
}

func (g *gatewaySource) addNode(ctx context.Context, in addNodeInput) (string, error) {
	req := &operatorpb.AddNodeRequest{
		Hostname: in.hostname, Ip: in.ip, SshPort: in.sshPort, SshUser: in.sshUser, SshPrivateKey: in.sshKey,
		SshHostKey: in.sshHostKey,
	}
	var resp operatorpb.AddNodeResponse
	if err := g.call(ctx, http.MethodPost, "/v1/control/nodes", req, &resp); err != nil {
		return "", err
	}
	return resp.GetProvisionId(), nil
}

func (g *gatewaySource) listProvisions(ctx context.Context) ([]provisionRow, error) {
	var resp operatorpb.ListProvisionsResponse
	if err := g.call(ctx, http.MethodGet, "/v1/control/provisions", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]provisionRow, 0, len(resp.GetProvisions()))
	for _, p := range resp.GetProvisions() {
		out = append(out, provisionRow{id: p.GetId(), hostname: p.GetHostname(), status: provLabel(p.GetStatus()), message: p.GetMessage()})
	}
	return out, nil
}

func (g *gatewaySource) testConnection(ctx context.Context, ip string, port int32) (testConnResult, error) {
	var resp operatorpb.TestConnectionResponse
	req := &operatorpb.TestConnectionRequest{Ip: ip, SshPort: port}
	if err := g.call(ctx, http.MethodPost, "/v1/control/test-connection", req, &resp); err != nil {
		return testConnResult{}, err
	}
	return testConnResult{reachable: resp.GetReachable(), latencyMs: resp.GetLatencyMs(), message: resp.GetMessage()}, nil
}

func (g *gatewaySource) cordon(ctx context.Context, name string) error {
	return g.call(ctx, http.MethodPost, "/v1/control/nodes/"+url.PathEscape(name)+"/cordon", nil, nil)
}

func (g *gatewaySource) uncordon(ctx context.Context, name string) error {
	return g.call(ctx, http.MethodPost, "/v1/control/nodes/"+url.PathEscape(name)+"/uncordon", nil, nil)
}

func (g *gatewaySource) drain(ctx context.Context, name string) error {
	return g.call(ctx, http.MethodPost, "/v1/control/nodes/"+url.PathEscape(name)+"/drain", nil, nil)
}

func (g *gatewaySource) setRegion(ctx context.Context, name, region string) error {
	// Name rides the PATH — the gateway takes the node's identity from there and
	// ignores any in the body, so sending it twice would be two spellings of one
	// fact. Region is the only thing the body carries.
	req := &operatorpb.SetNodeRegionRequest{Region: region}
	return g.call(ctx, http.MethodPost, "/v1/control/nodes/"+url.PathEscape(name)+"/region", req, nil)
}

func (g *gatewaySource) setVenueKeys(ctx context.Context, venue string, keys venueKeys) (string, error) {
	// The credential rides the body over TLS and is never placed in the path, a
	// query string, or a log line — any of which would put it in a proxy access log.
	req := &operatorpb.SetVenueKeysRequest{ApiKey: keys.apiKey, ApiSecret: keys.apiSecret, Passphrase: keys.passphrase}
	var resp operatorpb.SetVenueKeysResponse
	if err := g.call(ctx, http.MethodPut, "/v1/control/venues/"+url.PathEscape(venue)+"/keys", req, &resp); err != nil {
		return "", err
	}
	return resp.GetExchangeAccountId(), nil
}

func (g *gatewaySource) listVenueKeys(ctx context.Context) ([]venueRow, error) {
	var resp operatorpb.ListVenueKeysResponse
	if err := g.call(ctx, http.MethodGet, "/v1/control/venues", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]venueRow, 0, len(resp.GetVenues()))
	for _, v := range resp.GetVenues() {
		out = append(out, venueRow{venue: v.GetVenue(), configured: v.GetConfigured()})
	}
	return out, nil
}

// compile-time assertion: the gateway source IS the estate source.
var _ nodeSource = (*gatewaySource)(nil)
