// Command kanz is the Eighred institutional risk & portfolio terminal client:
// a Claude-Code-style REPL over the delivered api-gateway /v1 edge. It signs in
// against the platform identity provider (internal/identityclient), persists
// the token, and drives natural-language turns through the copilot and slash
// commands through the risk endpoints. It is a pure client — no backend, no
// mocks; every turn hits the running gateway, which validates the token.
package main

import (
	"context"
	"errors"
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
	"github.com/eighred/kanz/cmd/kanz/internal/panes/halt"
	"github.com/eighred/kanz/cmd/kanz/internal/repl"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/internal/identityclient"
	"github.com/eighred/kanz/internal/tui/app"
	"github.com/eighred/kanz/internal/tui/gateway"
	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/portfolio"
	"github.com/eighred/kanz/internal/tui/universe"
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
		auth, aerr := newAuthenticator(cfg, os.Stdout)
		if aerr != nil {
			return aerr
		}
		return repl.New(cfg, store, auth, envCredential{}, os.Stdin, os.Stdout).Run(ctx)
	}

	// One writer, shared: the REPL prints into it and the Copilot pane renders
	// from it. Two writers here would render an always-empty pane while the
	// output went to the terminal underneath the frame.
	out := copilot.NewBuffer()
	auth, err := newAuthenticator(cfg, out)
	if err != nil {
		return err
	}
	// stdin is unused by the shell — bubbletea owns the keyboard and the pane
	// feeds lines to Dispatch — so the REPL is given an empty reader rather than
	// os.Stdin, which would otherwise be read from two places at once.
	r := repl.New(cfg, store, auth, envCredential{}, strings.NewReader(""), out)

	// The estate panes (#66) connect LAZILY, on first visit, with whatever token
	// exists then. universe.NewGatewaySource refuses an empty one, and the shell
	// opens before the operator has signed in — building here would mean a
	// signed-out operator cannot open kanz at all, losing the Copilot pane that
	// /login lives on.
	shared := estate.NewShared(newEstateBuilder(cfg, store))

	panes := []pane.Pane{copilot.New(r, out)}
	panes = append(panes, estate.All(shared)...)
	// The book: positions, PnL and risk (#84). A GATEWAY pane, so it runs in this
	// process on the operator's own token — it reads two services (tv-sync's
	// projection for the book, the risk engine for the measures) and degrades to
	// whichever one is answering rather than blanking on either.
	panes = append(panes, portfolio.New(newBookBuilder(cfg, store), portfolio.Config{
		Account:   cfg.BookAccount,
		Portfolio: cfg.BookPortfolio,
	}))
	panes = append(panes,
		// Bus plane: their own SPIFFE identity, so their own process (#65).
		//
		// --gateway is passed so the child follows the SHELL'S resolved endpoint
		// rather than independently re-reading KANZ_GATEWAY_URL and hoping the two
		// agree (#67). Both read the same env var today, so this changes nothing
		// now — it is what keeps them agreeing when kanz gains a flag or a config
		// file and the child does not.
		//
		// THE TOKEN IS NOT PASSED ON argv, deliberately. Command lines are
		// world-readable in the process table; the child inherits the environment
		// and resolves KANZ_TOKEN there, which is the same path it uses standalone.
		pane.NewExecPane("monitor", "Monitor", "kanz-monitor", "--gateway", cfg.GatewayURL),
		// The kill switch gathers its attribution FIRST (#171). kanz-halt requires
		// --by, --reason and --tenant, so an ExecPane launching it bare could only
		// ever fail — and a pane that ran on selection put the kill switch one
		// `tab` from the Copilot prompt. This one is shown, and acts only when
		// three fields are filled and enter is pressed.
		halt.New(),
	)
	reg, err := pane.NewRegistry(panes...)
	if err != nil {
		return err
	}
	model, err := app.New(reg)
	if err != nil {
		return err
	}
	// WithMouseCellMotion turns on click reporting, which is what the tab bar and
	// the input field resolve through bubblezone.
	//
	// THE COST IS REAL AND WORTH STATING: with mouse reporting on, the terminal
	// stops handling drag-to-select itself, so copying text out of the shell needs
	// the terminal's own modifier (shift in most, option in iTerm2). An operator
	// who does not know that reads it as "I cannot copy from kanz". The help
	// overlay names click as a gesture, which is the closest this gets to telling
	// them; a fuller answer belongs wherever the shell documents its keys.
	_, err = tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx)).Run()
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

// identityLogin adapts identityclient.Client to repl.Authenticator.
//
// THE clientIP ARGUMENT IS EMPTY, AND THAT IS CORRECT HERE. It exists so the
// web-BFF can tell the identity service which browser an attempt came from —
// without it every forwarded login arrives from the BFF, so the limiter keys
// them all together and one user's failed attempts throttle everybody. The CLI
// is not a forwarder: it opens the connection itself, so the address the
// service already sees IS the caller's, and sending a header would only let a
// client choose which rate-limit bucket it lands in.
type identityLogin struct{ client *identityclient.Client }

func (i *identityLogin) Login(ctx context.Context, subject, credential string) (*identityclient.Token, error) {
	return i.client.Login(ctx, subject, credential, "")
}

// envCredential reads the sign-in secret from KANZ_CREDENTIAL.
//
// WHY THE ENVIRONMENT, AND WHAT IS STILL MISSING. Where a secret can be typed
// depends on the surface, and neither of this binary's two surfaces can take
// one today: the line REPL runs precisely when stdin is NOT a terminal (it is
// the fallback for a pipe), so there is nothing to mask; and under the TUI
// bubbletea owns the keyboard, and the Copilot pane reaches the REPL through a
// one-way Dispatch(line) with no way to say "the next line is a secret".
//
// So this is the honest floor rather than the finished answer: it serves
// scripted and CI callers exactly, and an interactive masked prompt — a login
// form beside the halt form, which is the shape this shell already uses for
// gathering fields — is still owed. Until then /login says so rather than
// failing with something vague.
type envCredential struct{}

func (envCredential) Credential(context.Context, string) (string, error) {
	if c := os.Getenv("KANZ_CREDENTIAL"); c != "" {
		return c, nil
	}
	return "", errors.New("no credential to sign in with: set KANZ_CREDENTIAL. " +
		"This client cannot yet prompt for one — the line REPL only runs when stdin is a pipe, " +
		"and under the shell bubbletea owns the keyboard, so a masked prompt needs a login form " +
		"of its own (#364)")
}

// newEstateBuilder returns the lazy constructor the estate panes call on their
// first visit (#66).
//
// EXTRACTED FROM main SO IT CAN BE TESTED. As an inline closure it shipped a nil
// dereference that panicked the whole shell on the first Tab to the Nodes tab,
// and nothing could reach it: main's wiring had no test, and the estate tests
// supply their own build func.
// bearerFor picks the session token the panes present to the gateway.
//
// ONE IMPLEMENTATION, because two panes must never disagree about whether there
// is a session. The REPL adopts KANZ_TOKEN and never persists it (deliberately),
// so a store-only lookup would leave a pane reporting "not signed in" while the
// Copilot pane beside it was answering questions — one shell disagreeing with
// itself.
//
// tokenstore.Valid is the existing answer to all three ways there is no usable
// session — nil, empty, and expired — so it is called rather than re-derived.
// The expiry half matters on its own: without it an expired token reaches the
// gateway and comes back 401, which tells the operator far less than "your
// session expired".
func bearerFor(cfg config.Config, tok *identityclient.Token) (string, error) {
	switch {
	case cfg.DevToken != "":
		return cfg.DevToken, nil
	case tokenstore.Valid(tok, time.Now()):
		return tok.Token, nil
	}
	return "", errors.New(
		"not signed in (or the session expired) — run /login on the Copilot pane, then return here")
}

// newBookBuilder defers the book pane's source until first use, for the same
// reason the estate builder does: the shell opens before anyone has signed in.
func newBookBuilder(cfg config.Config, store *tokenstore.Store) func() (*portfolio.Source, error) {
	return func() (*portfolio.Source, error) {
		tok, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("could not read the saved session: %w", err)
		}
		bearer, err := bearerFor(cfg, tok)
		if err != nil {
			return nil, err
		}
		c, err := gateway.New(gateway.Config{
			BaseURL: cfg.GatewayURL,
			Token:   gateway.StaticToken(bearer),
			// A deployment concern, not a session one — the gateway's Signing
			// middleware is a no-op when it holds no secret.
			SigningSecret: cfg.SigningSecret,
		})
		if err != nil {
			return nil, err
		}
		return portfolio.NewSource(c), nil
	}
}

func newEstateBuilder(cfg config.Config, store *tokenstore.Store) func() (universe.Model, error) {
	return func() (universe.Model, error) {
		// Read the token from the STORE, not from the REPL. The REPL persists on
		// /login, so the store is current — and it is the only source both halves
		// of the shell agree on, rather than a second copy that can go stale.
		tok, err := store.Load()
		if err != nil {
			return universe.Model{}, fmt.Errorf("could not read the saved session: %w", err)
		}

		// "NOT SIGNED IN" IS NOT AN ERROR FROM Load. tokenstore.Load returns
		// (nil, nil) for a missing file AND for a corrupt one — deliberately, so
		// the CLI re-authenticates instead of wedging. An earlier version checked
		// only the error and then read tok.AccessToken, so the common case (never
		// signed in) dereferenced nil and took the shell down on the first Tab.
		//
		// tokenstore.Valid is the existing answer to all three ways there is no
		// usable session — nil, empty, and expired — so it is called rather than
		// re-derived. The expiry half matters on its own: without it an expired
		// token reaches the gateway and comes back 401, which tells the operator
		// far less than "your session expired".
		bearer, err := bearerFor(cfg, tok)
		if err != nil {
			return universe.Model{}, err
		}

		src, err := universe.NewGatewaySource(universe.GatewayConfig{
			BaseURL: cfg.GatewayURL,
			Token:   bearer,
			// The signing secret is a deployment concern, not a session one: the
			// gateway's Signing middleware is a no-op when it holds no secret, so
			// an unset value here is valid rather than missing.
			SigningSecret: cfg.SigningSecret,
		})
		if err != nil {
			return universe.Model{}, err
		}
		return universe.NewModel(universe.Config{
			PollInterval: 3 * time.Second,
			CallTimeout:  universe.DefaultCallTimeout,
		}, src), nil
	}
}

// newAuthenticator builds the sign-in path, or a placeholder when there is none.
//
// THE DEV-TOKEN BRANCH STAYS, AND THE REASON IS UNCHANGED BY #364. A KANZ_TOKEN
// session has no identity URL by construction — config.Load rejects configuring
// both — so building a client unconditionally would refuse to start at all. That
// is not hypothetical; it is what the first version of the dev-token path did,
// against the SSO client this replaces:
//
//	$ KANZ_GATEWAY_URL=... KANZ_TOKEN=... kanz
//	kanz: deviceauth: issuer is required
//
// The unit tests missed it because they construct the REPL directly and never
// run this wiring — the same gap that hid the estate builder's nil dereference.
// Extracted so it has a test of its own.
//
// out is no longer used: the device flow had to PRINT a code and a URL for the
// operator to approve, and a credential exchange has nothing to show. It is kept
// in the signature because the caller's choice of writer is still the thing that
// differs between the two surfaces, and a login form will need it.
func newAuthenticator(cfg config.Config, _ io.Writer) (repl.Authenticator, error) {
	if cfg.DevToken != "" {
		// Never called: repl.login refuses before reaching the Authenticator in a
		// dev session. It is a real value rather than nil so that if that ever
		// stops being true, the result is an error naming the cause instead of a
		// nil-pointer panic in the middle of a turn.
		return unavailableAuth{}, nil
	}
	// The timeout is left to identityclient's own default, and that default is
	// load-bearing: the identity service verifies with Argon2id, deliberately
	// slowly, and pays that cost even for an unknown subject as its timing
	// defence. A tighter timeout chosen here would turn every sign-in into a
	// client-side cancellation.
	return &identityLogin{client: identityclient.New(cfg.IdentityURL, "", 0)}, nil
}

// unavailableAuth stands in where there is nowhere to sign in against.
type unavailableAuth struct{}

func (unavailableAuth) Login(context.Context, string, string) (*identityclient.Token, error) {
	return nil, errors.New("this session uses KANZ_TOKEN and has no KANZ_IDENTITY_URL to sign in against")
}
