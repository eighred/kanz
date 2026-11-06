// Package edgeconfig builds the same-pod Envoy front end for the existing BFF.
// It grants no identity: the BFF and gateway retain their authentication duties.
package edgeconfig

import (
	"crypto/rand"
	"encoding/json"
	"errors"
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
	adminAddress := "0.0.0.0"
	if o.AdminLoopback {
		adminAddress = "127.0.0.1"
	}
	// No retry, mirroring, body buffering, host rewrite, or request access log is
	// configured. Encoded identifiers and long-lived SSE retain their BFF shape.
	// Encode strings as JSON values so a future canonical header name cannot
	// alter the proxy configuration structure.
	value := map[string]any{
		"admin": map[string]any{
			"access_log_path": "/dev/null",
			"address": map[string]any{
				"socket_address": map[string]any{
					"address":    adminAddress,
					"port_value": o.AdminPort,
				},
			},
			"allow_paths": []any{
				map[string]any{
					"exact": "/ready",
				},
				map[string]any{
					"exact": "/stats/prometheus",
				},
			},
		},
		"static_resources": map[string]any{
			"listeners": []any{
				map[string]any{
					"name": "web_edge",
					"address": map[string]any{
						"socket_address": map[string]any{
							"address":    "127.0.0.1",
							"port_value": o.ListenPort,
						},
					},
					"filter_chains": []any{
						map[string]any{
							"filters": []any{
								map[string]any{
									"name": "envoy.filters.network.http_connection_manager",
									"typed_config": map[string]any{
										"@type":              "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
										"stat_prefix":        "web_edge",
										"codec_type":         "AUTO",
										"use_remote_address": true,
										"skip_xff_append":    true,
										"internal_address_config": map[string]any{
											"cidr_ranges": []any{},
										},
										"forward_client_cert_details":      "SANITIZE",
										"max_request_headers_kb":           64,
										"request_headers_timeout":          "10s",
										"stream_idle_timeout":              "300s",
										"normalize_path":                   false,
										"merge_slashes":                    false,
										"path_with_escaped_slashes_action": "KEEP_UNCHANGED",
										"common_http_protocol_options": map[string]any{
											"headers_with_underscores_action": "REJECT_REQUEST",
											"idle_timeout":                    "300s",
										},
										"upgrade_configs": []any{
											map[string]any{
												"upgrade_type": "websocket",
											},
										},
										"route_config": map[string]any{
											"name":                      "bff_only",
											"request_headers_to_remove": remove,
											"virtual_hosts": []any{
												map[string]any{
													"name": "same_origin",
													"domains": []any{
														"*",
													},
													"routes": []any{
														map[string]any{
															"match": map[string]any{
																"safe_regex": map[string]any{
																	"google_re2": map[string]any{},
																	"regex":      "^/api/v1/broker/accounts/[^/]+/stream$",
																},
																"headers": []any{
																	map[string]any{
																		"name": ":method",
																		"string_match": map[string]any{
																			"exact": "GET",
																		},
																	},
																},
															},
															"route": map[string]any{
																"cluster":      "bff",
																"timeout":      "0s",
																"idle_timeout": "0s",
															},
														},
														map[string]any{
															"match": map[string]any{
																"prefix": "/",
															},
															"route": map[string]any{
																"cluster": "bff",
																"timeout": "0s",
															},
														},
													},
												},
											},
										},
										"http_filters": []any{
											map[string]any{
												"name": "envoy.filters.http.local_ratelimit",
												"typed_config": map[string]any{
													"@type":       "type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit",
													"stat_prefix": "web_edge_budget",
													"token_bucket": map[string]any{
														"max_tokens":      o.RequestsPerSecond,
														"tokens_per_fill": o.RequestsPerSecond,
														"fill_interval":   "1s",
													},
													"filter_enabled": map[string]any{
														"default_value": map[string]any{
															"numerator":   100,
															"denominator": "HUNDRED",
														},
													},
													"filter_enforced": map[string]any{
														"default_value": map[string]any{
															"numerator":   100,
															"denominator": "HUNDRED",
														},
													},
												},
											},
											map[string]any{
												"name": "envoy.filters.http.router",
												"typed_config": map[string]any{
													"@type":                  "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
													"suppress_envoy_headers": true,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			"clusters": []any{
				map[string]any{
					"name":            "bff",
					"type":            "STATIC",
					"connect_timeout": "2s",
					"load_assignment": map[string]any{
						"cluster_name": "bff",
						"endpoints": []any{
							map[string]any{
								"lb_endpoints": []any{
									map[string]any{
										"endpoint": map[string]any{
											"address": map[string]any{
												"socket_address": map[string]any{
													"address":    "127.0.0.1",
													"port_value": o.BFFPort,
												},
											},
										},
									},
								},
							},
						},
					},
					"circuit_breakers": map[string]any{
						"thresholds": []any{
							map[string]any{
								"max_connections":      256,
								"max_pending_requests": 128,
								"max_requests":         1024,
								"max_retries":          0,
							},
						},
					},
				},
			},
		},
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
