package main

import (
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
	// -spiffe-socket is read in Task 3 when Plaintext is false.
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	p := tea.NewProgram(newModel(cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
