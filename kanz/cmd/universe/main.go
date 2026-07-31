// universe is the operator TUI: it lists the Kubernetes estate (nodes, clusters),
// provisions new nodes, drives node lifecycle (cordon/drain/region) and manages
// venue API credentials.
//
// THE PANES THEMSELVES NOW LIVE IN internal/tui/universe, so the kanz shell can
// mount them too (#66). This binary is the standalone entry point and stays
// until the shell is proven: flags, secret loading, and a bubbletea program
// around the same Model the shell embeds. Nothing here is duplicated — if this
// file and the shell ever disagree about what a pane does, the pane moved.
//
// IT REACHES THE ESTATE THROUGH THE API GATEWAY, and holds no cluster access of any
// kind (OPS-M2c). It used to dial operator.v1 directly in plaintext at
// localhost:9090, which only worked inside a `kubectl port-forward` the human had to
// start — so an operator rotating a venue key needed a kubeconfig, a cluster
// credential, and enough Kubernetes knowledge to know that port-forwarding was the
// missing step. The operator's identity is now their own bearer token, validated by
// the same authority as every other client of the platform.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/universe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "universe: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		cfg       universe.Config
		gwURL     string
		tokenFile string
		signFile  string
	)
	fs := flag.NewFlagSet("universe", flag.ContinueOnError)
	fs.StringVar(&gwURL, "gateway-url", os.Getenv("KANZ_GATEWAY_URL"),
		"api-gateway base URL, e.g. https://api.eighred.com (env KANZ_GATEWAY_URL)")
	fs.StringVar(&tokenFile, "token-file", os.Getenv("KANZ_TOKEN_FILE"),
		"file holding the bearer token identifying you (env KANZ_TOKEN_FILE, or KANZ_TOKEN directly)")
	fs.StringVar(&signFile, "signing-secret-file", os.Getenv("KANZ_SIGNING_SECRET_FILE"),
		"file holding the gateway's request-signing secret, if the deployment sets one "+
			"(env KANZ_SIGNING_SECRET_FILE, or KANZ_SIGNING_SECRET directly)")
	fs.DurationVar(&cfg.PollInterval, "poll", 3*time.Second, "estate refresh interval")
	fs.DurationVar(&cfg.CallTimeout, "timeout", universe.DefaultCallTimeout,
		"timeout for one ordinary control-plane call (the estate poll and the "+
			"cordon/drain/move/add/set-keys actions). Test Connection is NOT bounded by this: "+
			"it waits on the operator running a probe Job and keeps its own, much longer bound")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	// A FILE IS PREFERRED OVER AN ENV VAR for both secrets, the same ordering
	// pkg-level config uses elsewhere (SEC-01d): an env var is visible in the
	// process table and inherited by anything this shell spawns.
	token, err := secretFrom(tokenFile, "KANZ_TOKEN")
	if err != nil {
		return err
	}
	signing, err := secretFrom(signFile, "KANZ_SIGNING_SECRET")
	if err != nil {
		return err
	}

	src, err := universe.NewGatewaySource(universe.GatewayConfig{
		BaseURL:       gwURL,
		Token:         token,
		SigningSecret: signing,
	})
	if err != nil {
		return err
	}

	p := tea.NewProgram(universe.NewModel(cfg, src), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// secretFrom reads a secret from a file if given, else from env. Returns empty
// (no error) when neither is set — the caller decides whether that is fatal, since
// the token is required and the signing secret is not.
func secretFrom(path, envKey string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv(envKey)), nil
}
