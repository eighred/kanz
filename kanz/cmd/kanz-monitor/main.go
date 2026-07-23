package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-monitor: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var cfg Config
	fs := flag.NewFlagSet("kanz-monitor", flag.ContinueOnError)
	fs.StringVar(&cfg.NATSURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "spine URL to subscribe to (read-only)")
	fs.StringVar(&cfg.GatewayURL, "gateway", envOr("KANZ_GATEWAY_URL", "http://localhost:8080"), "api-gateway base URL for /v1 reads + /metrics")
	fs.StringVar(&cfg.Token, "token", os.Getenv("KANZ_TOKEN"), "HS256 bearer for the gateway (see kanz-devtoken)")
	fs.StringVar(&cfg.Tenant, "tenant", envOr("KANZ_TENANT", "__system__"), "tenant whose book to monitor")
	fs.DurationVar(&cfg.PollInterval, "poll", 2*time.Second, "poll interval for /metrics and /v1 reads")
	fs.BoolVar(&cfg.Plaintext, "nats-plaintext", true, "dial NATS without TLS (dev rig). false ⇒ mesh mTLS")
	fs.StringVar(&cfg.SPIFFESocket, "spiffe-socket", os.Getenv("SPIFFE_ENDPOINT_SOCKET"), "SPIFFE Workload API socket for the mesh SVID (required when --nats-plaintext=false)")
	fs.StringVar(&cfg.MetricsURL, "metrics-url", os.Getenv("KANZ_OMS_METRICS_URL"), "OMS /metrics endpoint for the incident counters (its own registry, distinct from --gateway-url)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if err := validate(cfg); err != nil {
		return err
	}

	p := tea.NewProgram(newModel(cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// validate rejects only the one silent-degrade footgun in Config:
// transport.NewMesh(ctx, "") returns a DISABLED mesh with a nil Client that
// dials NATS in plaintext regardless of what the caller asked for. If the
// operator requested mTLS (!Plaintext) but supplied no workload socket to
// obtain an SVID from, that call would silently degrade to plaintext instead
// of failing — the wrong direction for a security posture, so refuse to
// start rather than mislead.
//
// An empty MetricsURL is NOT rejected here: the poller skips the counter
// scrape when it is unset, which is a valid bus-feed-plus-health mode (see
// poller.go's Config.MetricsURL comment).
func validate(cfg Config) error {
	if !cfg.Plaintext && cfg.SPIFFESocket == "" {
		return errors.New("--nats-plaintext=false requires --spiffe-socket (or $SPIFFE_ENDPOINT_SOCKET): mTLS was requested but there is no workload socket to obtain an SVID from, and dialing without one silently degrades to plaintext")
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
