// Package repl is the kanz terminal client's interactive loop: a
// Claude-Code-style REPL that authenticates via the Eighred SSO device flow,
// persists the token, and drives turns against the api-gateway /v1 edge —
// natural-language questions through the copilot, and slash commands through
// the structured risk endpoints. No mocks: every turn hits the running gateway.
package repl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/gateway"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/pkg/deviceauth"
)

// Authenticator runs the Eighred SSO device flow and returns a token. It is an
// interface so the REPL can be unit-tested without real network I/O.
type Authenticator interface {
	Login(ctx context.Context) (*deviceauth.Token, error)
}

// REPL is the interactive terminal session.
type REPL struct {
	in    *bufio.Scanner
	out   io.Writer
	store *tokenstore.Store
	auth  Authenticator
	gw    *gateway.Client
	now   func() time.Time

	token *deviceauth.Token // current session token; nil ⇒ not logged in
}

// New builds a REPL over the config, token store, authenticator, and I/O. It
// loads any persisted token so an unexpired session resumes without a login.
func New(cfg config.Config, store *tokenstore.Store, auth Authenticator, in io.Reader, out io.Writer) *REPL {
	r := &REPL{
		in:    bufio.NewScanner(in),
		out:   out,
		store: store,
		auth:  auth,
		now:   time.Now,
	}
	r.in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// The gateway client reads the bearer through a closure so a mid-session
	// /login is picked up without rebuilding the client.
	r.gw = gateway.New(cfg.GatewayURL, r.accessToken)
	if tok, err := store.Load(); err == nil {
		r.token = tok
	}
	return r
}

func (r *REPL) accessToken() string {
	if r.token == nil {
		return ""
	}
	return r.token.AccessToken
}

// Run renders the header and processes input until EOF or /quit. It returns nil
// on a clean exit; an I/O error on the input stream is surfaced.
func (r *REPL) Run(ctx context.Context) error {
	r.header()
	for {
		fmt.Fprint(r.out, "\nkanz› ")
		if !r.in.Scan() {
			break // EOF (Ctrl-D) or read error
		}
		line := strings.TrimSpace(r.in.Text())
		if line == "" {
			continue
		}
		if stop := r.Dispatch(ctx, line); stop {
			break
		}
	}
	return r.in.Err()
}

// Dispatch handles one input line, returning true when the session should stop.
//
// EXPORTED SO THE TUI SHELL CAN DRIVE IT (#65). Run owns a blocking read loop on
// stdin, which a bubbletea pane cannot host — bubbletea owns the terminal and
// delivers keys as messages. Dispatch is the half that does the work, and it is
// already independent of where the line came from: it reads a string and writes
// to r.out.
//
// So the shell reuses the REPL rather than reimplementing its commands. A second
// copy of this switch is the failure CLAUDE.md names — the one where a fix lands
// in one of them.
//
// It BLOCKS: /ask reaches the gateway. A caller inside a bubbletea Update must
// run it in a tea.Cmd, or the whole shell freezes for the length of the request.
func (r *REPL) Dispatch(ctx context.Context, line string) (stop bool) {
	if !strings.HasPrefix(line, "/") {
		r.ask(ctx, line)
		return false
	}
	cmd, rest := splitFirst(line)
	switch cmd {
	case "/quit", "/exit":
		return true
	case "/help":
		r.help()
	case "/login":
		if err := r.login(ctx); err != nil {
			r.errf("login failed: %v", err)
		}
	case "/logout":
		r.logout()
	case "/whoami":
		r.whoami()
	case "/exposure":
		id, args := splitFirst(rest)
		r.requireArg(ctx, id, "usage: /exposure <portfolio-id> [as_of-rfc3339]", func() {
			asOf, _ := splitFirst(args)
			r.renderStruct(r.gw.Exposure(ctx, id, asOf))
		})
	case "/measures":
		id, args := splitFirst(rest)
		r.requireArg(ctx, id, "usage: /measures <portfolio-id> [measure ...]", func() {
			r.renderStruct(r.gw.Measures(ctx, id, strings.Fields(args), ""))
		})
	case "/scenario":
		id, body := splitFirst(rest)
		r.requireArg(ctx, id, "usage: /scenario <portfolio-id> <json-body>", func() {
			if strings.TrimSpace(body) == "" {
				r.errf("scenario body (JSON) is required")
				return
			}
			r.renderStruct(r.gw.Scenario(ctx, id, json.RawMessage(body)))
		})
	default:
		r.errf("unknown command %q — try /help", cmd)
	}
	return false
}

// requireArg runs fn only when id is present and the caller is authenticated,
// otherwise printing usage / triggering login.
func (r *REPL) requireArg(ctx context.Context, id, usage string, fn func()) {
	if id == "" {
		r.errf("%s", usage)
		return
	}
	if !r.ensureAuth(ctx) {
		return
	}
	fn()
}

// ask puts a natural-language question to the copilot and renders the governed
// answer with its citations and any refusal/injection warnings.
func (r *REPL) ask(ctx context.Context, question string) {
	if !r.ensureAuth(ctx) {
		return
	}
	res, err := r.gw.Ask(ctx, question)
	if err != nil {
		r.errf("%v", err)
		return
	}
	fmt.Fprintf(r.out, "\n%s\n", strings.TrimSpace(res.Answer))
	if len(res.Citations) > 0 {
		fmt.Fprintln(r.out, "\nSources:")
		for _, c := range res.Citations {
			fmt.Fprintf(r.out, "  • %s\n", c)
		}
	}
	if res.Refused {
		fmt.Fprintln(r.out, "\n⚠ the copilot refused this request (out of policy or ungrounded)")
	}
	if res.InjectionFlagged {
		fmt.Fprintln(r.out, "⚠ a prompt-injection attempt was flagged in the retrieved context")
	}
	if !res.Grounded && !res.Refused {
		fmt.Fprintln(r.out, "⚠ this answer is not grounded in retrieved sources — treat with caution")
	}
}

// ensureAuth guarantees a usable token, running the device flow if the current
// one is missing or expired. It returns false (having printed an error) when
// login fails, so callers can abort the turn.
func (r *REPL) ensureAuth(ctx context.Context) bool {
	if tokenstore.Valid(r.token, r.now()) {
		return true
	}
	if err := r.login(ctx); err != nil {
		r.errf("login required but failed: %v", err)
		return false
	}
	return true
}

// login runs the device flow, persists the token, and adopts it for the
// session.
func (r *REPL) login(ctx context.Context) error {
	tok, err := r.auth.Login(ctx)
	if err != nil {
		return err
	}
	r.token = tok
	if err := r.store.Save(tok); err != nil {
		// A persistence failure is non-fatal: the session token still works,
		// the user just re-logs-in next run. Surface it, don't abort.
		r.errf("warning: could not persist token: %v", err)
	}
	fmt.Fprintln(r.out, "✓ signed in")
	return nil
}

func (r *REPL) logout() {
	r.token = nil
	if err := r.store.Delete(); err != nil {
		r.errf("logout: %v", err)
		return
	}
	fmt.Fprintln(r.out, "✓ signed out")
}

// whoami decodes (without verifying — display only) the access token's claims
// and prints subject, tenant, and expiry. The gateway is the authority that
// verifies the token; this is a convenience readout.
func (r *REPL) whoami() {
	if r.token == nil {
		fmt.Fprintln(r.out, "not signed in — a question or command will start the device flow")
		return
	}
	claims := decodeClaims(r.token.AccessToken)
	sub, _ := claims["sub"].(string)
	tenant, _ := claims["tenant"].(string)
	fmt.Fprintf(r.out, "subject: %s\n", orNA(sub))
	fmt.Fprintf(r.out, "tenant:  %s\n", orNA(tenant))
	if !r.token.Expiry.IsZero() {
		fmt.Fprintf(r.out, "expires: %s (%s)\n", r.token.Expiry.Format(time.RFC3339), humanUntil(r.token.Expiry.Sub(r.now())))
	}
}

// renderStruct pretty-prints a structured (protojson) response, or reports the
// error. A single place so every structured command renders consistently.
func (r *REPL) renderStruct(raw json.RawMessage, err error) {
	if err != nil {
		r.errf("%v", err)
		return
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		// Not indentable JSON — print verbatim rather than swallow the response.
		fmt.Fprintf(r.out, "\n%s\n", string(raw))
		return
	}
	fmt.Fprintf(r.out, "\n%s\n", buf.String())
}

func (r *REPL) header() {
	fmt.Fprintln(r.out, "╭───────────────────────────────────────────────╮")
	fmt.Fprintln(r.out, "│                KANZ TERMINAL                  │")
	fmt.Fprintln(r.out, "│   institutional risk & portfolio copilot      │")
	fmt.Fprintln(r.out, "╰───────────────────────────────────────────────╯")
	fmt.Fprintln(r.out, "Ask a question in plain language, or use a slash command. /help for commands, /quit to exit.")
}

func (r *REPL) help() {
	fmt.Fprint(r.out, `
Commands:
  <question>                     ask the copilot in natural language
  /exposure <id> [as_of]         portfolio exposure (as_of RFC3339, default latest)
  /measures <id> [measure ...]   named risk measures (e.g. VaR99 Delta)
  /scenario <id> <json>          evaluate a scenario (JSON EvaluateScenarioRequest)
  /login                         sign in via the Eighred SSO device flow
  /logout                        forget the persisted token
  /whoami                        show the current identity and token expiry
  /help                          this help
  /quit                          exit
`)
}

func (r *REPL) errf(format string, args ...any) {
	fmt.Fprintf(r.out, "✗ "+format+"\n", args...)
}

// --- small helpers ---

// splitFirst splits s into its first whitespace-delimited token and the
// remainder (trimmed). Missing parts are "".
func splitFirst(s string) (first, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexFunc(s, isSpace); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }

func orNA(s string) string {
	if s == "" {
		return "(n/a)"
	}
	return s
}

func humanUntil(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	d = d.Round(time.Minute)
	return "in " + d.String()
}

// decodeClaims base64url-decodes a JWT's payload segment WITHOUT verifying the
// signature — this is a display-only readout, never an authorization decision.
func decodeClaims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}
