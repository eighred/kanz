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
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/panes/copilot"
	"github.com/eighred/kanz/cmd/kanz/internal/panes/estate"
	"github.com/eighred/kanz/cmd/kanz/internal/repl"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/internal/tui/app"
	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/universe"
	"github.com/eighred/kanz/pkg/deviceauth"
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

	// NO-FLAG kanz OPENS THE TUI (#65). The REPL is not gone — it is the shell's
	// Copilot pane, driven through repl.Dispatch, so /login, /ask and the rest
	// have one implementation rather than two.
	//
	// EXCEPT WHEN THERE IS NO TERMINAL TO DRAW ON. bubbletea needs a TTY; with
	// stdin piped (`echo /whoami | kanz`, a CI step, a script) it fails at
	// startup. Falling back to the line REPL keeps those callers working, and
	// keeps this binary usable in a pipeline — a TUI that cannot be scripted is
	// a worse tool, not a better one.
	if !stdinIsTerminal() {
		auth := &deviceLogin{client: dev, out: os.Stdout}
		return repl.New(cfg, store, auth, os.Stdin, os.Stdout).Run(ctx)
	}

	// One writer, shared: the REPL prints into it and the Copilot pane renders
	// from it. Two writers here would render an always-empty pane while the
	// output went to the terminal underneath the frame.
	out := copilot.NewBuffer()
	auth := &deviceLogin{client: dev, out: out}
	// stdin is unused by the shell — bubbletea owns the keyboard and the pane
	// feeds lines to Dispatch — so the REPL is given an empty reader rather than
	// os.Stdin, which would otherwise be read from two places at once.
	r := repl.New(cfg, store, auth, strings.NewReader(""), out)

	// The estate panes (#66) connect LAZILY, on first visit, with whatever token
	// exists then. universe.NewGatewaySource refuses an empty one, and the shell
	// opens before the operator has signed in — building here would mean a
	// signed-out operator cannot open kanz at all, losing the Copilot pane that
	// /login lives on.
	shared := estate.NewShared(func() (universe.Model, error) {
		// Read the token from the STORE, not from the REPL. The REPL persists on
		// /login, so the store is current — and it is the only source both halves
		// of the shell agree on, rather than a second copy that can go stale.
		tok, terr := store.Load()
		if terr != nil {
			return universe.Model{}, fmt.Errorf("not signed in: %w", terr)
		}
		src, serr := universe.NewGatewaySource(universe.GatewayConfig{
			BaseURL: cfg.GatewayURL,
			Token:   tok.AccessToken,
			// The signing secret is a deployment concern, not a session one: the
			// gateway's Signing middleware is a no-op when it holds no secret, so
			// an unset value here is valid rather than missing.
			SigningSecret: os.Getenv("KANZ_SIGNING_SECRET"),
		})
		if serr != nil {
			return universe.Model{}, serr
		}
		return universe.NewModel(universe.Config{
			PollInterval: 3 * time.Second,
			CallTimeout:  universe.DefaultCallTimeout,
		}, src), nil
	})

	panes := []pane.Pane{copilot.New(r, out)}
	panes = append(panes, estate.All(shared)...)
	panes = append(panes,
		// Bus plane: their own SPIFFE identity, so their own process (#65).
		pane.NewExecPane("monitor", "Monitor", "kanz-monitor"),
		pane.NewExecPane("halt", "Halt", "kanz-halt"),
	)
	reg, err := pane.NewRegistry(panes...)
	if err != nil {
		return err
	}
	model, err := app.New(reg)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

// stdinIsTerminal reports whether stdin is an interactive terminal.
//
// Uses the ModeCharDevice bit rather than a term library: it is stdlib, it is
// what the term packages check on both POSIX and Windows consoles, and adding a
// direct dependency to answer one boolean is the kind of creep this shell is
// meant to avoid.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		// Cannot tell. Choose the line REPL: it works on a terminal too, so the
		// wrong guess degrades the interface instead of failing to start.
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// deviceLogin adapts deviceauth.Client to repl.Authenticator, printing the
// user_code and verification URL when the flow begins so the user knows where
// to approve the sign-in.
type deviceLogin struct {
	client *deviceauth.Client
	// io.Writer, not *os.File: under the TUI this is the Copilot pane's buffer,
	// because writing the device-code prompt straight to the terminal would
	// paint over the frame and be erased by the next render — the operator would
	// be asked to approve a sign-in they never saw.
	out io.Writer
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
