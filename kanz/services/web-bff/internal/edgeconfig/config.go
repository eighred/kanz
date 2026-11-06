// Package edgeconfig builds the same-pod Envoy front end for the existing BFF.
// It grants no identity: the BFF and gateway retain their authentication duties.
package edgeconfig

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
)

// Options are bounded listener ports and a per-process admission budget. This
// budget supplements the identity service's existing per-caller login limiter.
type Options struct {
	ListenPort, BFFPort, AdminPort int
	RequestsPerSecond              int
	// AdminLoopback supports a local process fixture without a pod network.
	AdminLoopback bool
}

func Default() Options {
	return Options{ListenPort: 8083, BFFPort: 8084, AdminPort: 19001, RequestsPerSecond: 200}
}

// Build keeps the application listener and upstream on loopback. Admin exposes
// only readiness and metrics on the pod address for kubelet/controlled scraping;
// the deployment must retain ingress denial and must create no Service/Ingress.
func Build(o Options) ([]byte, error) {
	seen := map[int]bool{}
	for _, port := range []int{o.ListenPort, o.BFFPort, o.AdminPort} {
		if port < 1024 || port > 65535 || seen[port] {
			return nil, errors.New("edge ports must be distinct unprivileged TCP ports")
		}
		seen[port] = true
	}
	if o.RequestsPerSecond < 1 || o.RequestsPerSecond > 10000 {
		return nil, errors.New("edge request budget must be between 1 and 10000")
	}
	// Use the canonical principal names, not another declaration of mesh identity.
	remove := []string{auth.HeaderPrincipalSubject, auth.HeaderPrincipalTenant,
		auth.HeaderPrincipalRoles, auth.HeaderPrincipalPortfolios,
		"x-forwarded-client-cert", "x-envoy-internal", "x-envoy-force-trace",
		"x-envoy-upstream-rq-timeout-ms", "x-envoy-upstream-rq-per-try-timeout-ms",
		"x-envoy-max-retries", "x-envoy-retry-on", "x-envoy-retry-grpc-on",
		"x-envoy-retriable-header-names", "x-envoy-retriable-status-codes",
		"x-envoy-hedge-on-per-try-timeout", "x-envoy-upstream-alt-stat-name"}
	for i := range remove {
		remove[i] = strings.ToLower(remove[i])
	}
	headers, err := json.Marshal(remove)
	if err != nil {
		return nil, err
	}
	adminAddress := "0.0.0.0"
	if o.AdminLoopback {
		adminAddress = "127.0.0.1"
	}
	// No retry, mirroring, body buffering, host rewrite, or request access log is
	// configured. Encoded identifiers and long-lived SSE retain their BFF shape.
	raw := fmt.Sprintf(`{
  "admin":{"access_log_path":"/dev/null","address":{"socket_address":{"address":"%s","port_value":%d}},"allow_paths":[{"exact":"/ready"},{"exact":"/stats/prometheus"}]},
  "static_resources":{
    "listeners":[{"name":"web_edge","address":{"socket_address":{"address":"127.0.0.1","port_value":%d}},"filter_chains":[{"filters":[{"name":"envoy.filters.network.http_connection_manager","typed_config":{
      "@type":"type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
      "stat_prefix":"web_edge","codec_type":"AUTO","use_remote_address":true,"skip_xff_append":true,"internal_address_config":{"cidr_ranges":[]},
      "forward_client_cert_details":"SANITIZE","max_request_headers_kb":64,"request_headers_timeout":"10s","stream_idle_timeout":"300s",
      "normalize_path":false,"merge_slashes":false,"path_with_escaped_slashes_action":"KEEP_UNCHANGED",
      "common_http_protocol_options":{"headers_with_underscores_action":"REJECT_REQUEST","idle_timeout":"300s"},
      "upgrade_configs":[{"upgrade_type":"websocket"}],
      "route_config":{"name":"bff_only","request_headers_to_remove":%s,"virtual_hosts":[{"name":"same_origin","domains":["*"],"routes":[
        {"match":{"safe_regex":{"google_re2":{},"regex":"^/api/v1/broker/accounts/[^/]+/stream$"},"headers":[{"name":":method","string_match":{"exact":"GET"}}]},"route":{"cluster":"bff","timeout":"0s","idle_timeout":"0s"}},
        {"match":{"prefix":"/"},"route":{"cluster":"bff","timeout":"0s"}}
      ]}]},
      "http_filters":[
        {"name":"envoy.filters.http.local_ratelimit","typed_config":{"@type":"type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit","stat_prefix":"web_edge_budget","token_bucket":{"max_tokens":%d,"tokens_per_fill":%d,"fill_interval":"1s"},"filter_enabled":{"default_value":{"numerator":100,"denominator":"HUNDRED"}},"filter_enforced":{"default_value":{"numerator":100,"denominator":"HUNDRED"}}}},
        {"name":"envoy.filters.http.router","typed_config":{"@type":"type.googleapis.com/envoy.extensions.filters.http.router.v3.Router","suppress_envoy_headers":true}}
      ]
    }}]}]}],
    "clusters":[{"name":"bff","type":"STATIC","connect_timeout":"2s","load_assignment":{"cluster_name":"bff","endpoints":[{"lb_endpoints":[{"endpoint":{"address":{"socket_address":{"address":"127.0.0.1","port_value":%d}}}}]}]},"circuit_breakers":{"thresholds":[{"max_connections":256,"max_pending_requests":128,"max_requests":1024,"max_retries":0}]}}]
  }
}`, adminAddress, o.AdminPort, o.ListenPort, headers, o.RequestsPerSecond, o.RequestsPerSecond, o.BFFPort)
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	return json.MarshalIndent(value, "", "  ")
}

// Write replaces a complete configuration inside an already existing directory.
// Init-container retries may replace the file, but never follow its old symlink
// or expose a partially written configuration to the proxy.
func Write(path string, o Options) error {
	b, err := Build(o)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	name := ".edge-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }() // Renaming consumes this invocation's random name.
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(name, filepath.Base(path))
}
