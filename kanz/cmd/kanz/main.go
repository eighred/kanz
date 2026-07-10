// Command kanz is the Eighred institutional risk & portfolio terminal client:
// a Claude-Code-style REPL over the delivered api-gateway /v1 edge. It signs in
// via the Eighred SSO device flow (pkg/deviceauth), persists the token, and
// drives natural-language turns through the copilot and slash commands through
// the risk endpoints. It is a pure client — no backend, no mocks; every turn
// hits the running gateway, which validates the SSO-issued token.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kanz-eng/kanz/cmd/kanz/internal/config"
	"github.com/kanz-eng/kanz/cmd/kanz/internal/repl"
	"github.com/kanz-eng/kanz/cmd/kanz/internal/tokenstore"
	"github.com/kanz-eng/kanz/pkg/deviceauth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kanz: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := tokenstore.New()
	if err != nil {
		return err
	}
	dev, err := deviceauth.New(deviceauth.Config{
		Issuer:   cfg.Issuer,
		ClientID: cfg.ClientID,
		Scope:    cfg.Scope,
	})
	if err != nil {
		return err
	}

	// The signal context cancels an in-flight login poll or gateway request on
	// Ctrl-C, so the terminal exits promptly rather than hanging on the network.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	auth := &deviceLogin{client: dev, out: os.Stdout}
	r := repl.New(cfg, store, auth, os.Stdin, os.Stdout)
	return r.Run(ctx)
}

// deviceLogin adapts deviceauth.Client to repl.Authenticator, printing the
// user_code and verification URL when the flow begins so the user knows where
// to approve the sign-in.
type deviceLogin struct {
	client *deviceauth.Client
	out    *os.File
}

func (d *deviceLogin) Login(ctx context.Context) (*deviceauth.Token, error) {
	return d.client.Authorize(ctx, func(p deviceauth.Prompt) {
		fmt.Fprintln(d.out, "\nTo sign in, open the URL below and enter the code:")
		if p.VerificationURIComplete != "" {
			fmt.Fprintf(d.out, "  %s\n", p.VerificationURIComplete)
		} else {
			fmt.Fprintf(d.out, "  %s\n", p.VerificationURI)
		}
		fmt.Fprintf(d.out, "  code: %s\n\nWaiting for approval…\n", p.UserCode)
	})
}
