// universe is the read-only operator TUI (F0+S1): it lists the Kubernetes
// estate (nodes, clusters) by dialing the in-cluster operator.v1 service over a
// kubeconfig-gated `kubectl port-forward`. Read-only — no add/edit/delete/ssh.
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
		fmt.Fprintln(os.Stderr, "universe: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var cfg Config
	fs := flag.NewFlagSet("universe", flag.ContinueOnError)
	fs.StringVar(&cfg.OperatorAddr, "operator-addr", envOr("KANZ_OPERATOR_ADDR", "localhost:9090"),
		"operator.v1 gRPC address (typically a `kubectl port-forward` target)")
	fs.DurationVar(&cfg.PollInterval, "poll", 3*time.Second, "estate refresh interval")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	src, closeConn, err := dialOperator(cfg.OperatorAddr)
	if err != nil {
		return err
	}
	defer func() { _ = closeConn() }()

	p := tea.NewProgram(newModel(cfg, src), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
