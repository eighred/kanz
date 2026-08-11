// Package repl is the kanz terminal client's interactive loop: a
// Claude-Code-style REPL that signs in against the platform identity provider,
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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/gateway"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/internal/identityclient"
)

// Authenticator exchanges an operator's subject and credential for a session
// token at the platform identity provider. It is an interface so the REPL can
// be unit-tested without real network I/O.
//
// IT TAKES THE CREDENTIAL RATHER THAN COLLECTING IT (#364). Where a secret can
// safely be typed differs by surface — a piped stdin has no terminal to mask,
// and under the TUI bubbletea owns the keyboard — so gathering it is the
// composition root's job and this seam stays about the exchange.
type Authenticator interface {
	Login(ctx context.Context, subject, credential string) (*identityclient.Token, error)
}

// CredentialSource yields the secret for a sign-in. It is separate from
// Authenticator because the two answer different questions: this one is "how
// does this surface ask for a secret", which has no single answer, and the
// other is "how is a secret exchanged for a token", which has exactly one.
//
// Implementations MUST NOT echo the secret, and must not retain it — the REPL
// hands it straight to Login and keeps no copy.
type CredentialSource interface {
	Credential(ctx context.Context, subject string) (string, error)
}

// REPL is the interactive terminal session.
type REPL struct {
	in    *bufio.Scanner
	out   io.Writer
	store *tokenstore.Store
	auth  Authenticator
	creds CredentialSource
	gw    *gateway.Client
	now   func() time.Time

	token *identityclient.Token // current session token; nil ⇒ not logged in

	// devSession marks a session held by a pre-minted KANZ_TOKEN rather than a
	// sign-in. It changes what /login, /logout and /whoami can honestly say,
	// and it is printed in the header — a static bearer that nobody can see they
	// are using is the only genuinely dangerous version of this feature.
	devSession bool
}

// New builds a REPL over the config, token store, authenticator, credential
// source and I/O. It loads any persisted token so an unexpired session resumes
// without a login.
func New(cfg config.Config, store *tokenstore.Store, auth Authenticator, creds CredentialSource, in io.Reader, out io.Writer) *REPL {
	r := &REPL{
		in:    bufio.NewScanner(in),
		out:   out,
		store: store,
		auth:  auth,
		creds: creds,
		now:   time.Now,
	}
	r.in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// The gateway client reads the bearer through a closure so a mid-session
	// /login is picked up without rebuilding the client.
	//
	// The signing secret is passed too (#198): without it every call to a gateway
	// that enforces signatures came back 401, which reads as an expired session
	// and sent the operator to /login — which could not fix it.
	r.gw = gateway.New(cfg.GatewayURL, r.accessToken, cfg.SigningSecret)

	// A PRE-MINTED TOKEN IS ADOPTED, NEVER PERSISTED. config.Load has already
	// refused the case where both this and an identity URL are set, so reaching
	// here with a DevToken means there is nowhere to sign in against.
	//
	// It is deliberately NOT written to the token store: the store is where a
	// real session lives across runs, and a bearer from the environment that
	// outlived the environment is exactly the stale-credential surprise this
	// feature must not create. Unset KANZ_TOKEN and the session is gone.
	if cfg.DevToken != "" {
		// Zero Expiry: tokenstore.Valid treats that as non-expiring, which is
		// right — the gateway is the authority on this token's lifetime, and
		// inventing one here would refuse a perfectly good bearer.
		r.token = &identityclient.Token{Token: cfg.DevToken}
		r.devSession = true
		return r
	}
	if tok, err := store.Load(); err == nil {
		r.token = tok
	}
	return r
}

func (r *REPL) accessToken() string {
	if r.token == nil {
		return ""
	}
	return r.token.Token
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
		if err := r.login(ctx, strings.TrimSpace(rest)); err != nil {
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

// ensureAuth reports whether there is a usable token, telling the operator what
// to do when there is not. It returns false (having printed) so callers can
// abort the turn.
//
// IT NO LONGER SIGNS IN ON THE OPERATOR'S BEHALF (#364), AND THAT IS FORCED BY
// THE IDENTITY PROVIDER RATHER THAN CHOSEN. The device flow it replaced needed
// no secret from the caller, so an expired session could be renewed silently
// mid-turn. Credential sign-in needs a subject and a secret, and the identity
// service issues NO refresh token — there is nothing to renew from. Prompting
// for a password in the middle of an unrelated command is the worst possible
// place to ask for one: it trains an operator to type their credential whenever
// the terminal asks, which is precisely the reflex a phishing pane would want.
//
// So an expired session is REPORTED, and /login is the only thing that asks.
func (r *REPL) ensureAuth(ctx context.Context) bool {
	if tokenstore.Valid(r.token, r.now()) {
		return true
	}
	if r.devSession {
		// Unreachable while KANZ_TOKEN is non-empty (New adopts it with a zero
		// expiry, which Valid accepts), so this can only mean the gateway's own
		// authority rejected it — say where the credential came from rather than
		// sending the operator to a /login that refuses dev sessions.
		r.errf("the KANZ_TOKEN bearer from the environment is not usable — it is the gateway that " +
			"decides that, so check the token, not this client")
		return false
	}
	if r.token == nil {
		r.errf("not signed in — run /login <subject> first")
		return false
	}
	r.errf("your session has expired — run /login <subject> to sign in again " +
		"(the identity provider issues no refresh token, so this is a fresh sign-in)")
	return false
}

// login exchanges a credential for a session token, persists it, and adopts it
// for the session. The secret is read from the CredentialSource and handed
// straight to the Authenticator — this function keeps no copy of it.
func (r *REPL) login(ctx context.Context, subject string) error {
	if r.devSession {
		return errors.New("this session uses the pre-minted KANZ_TOKEN, and there is no identity " +
			"provider to sign in against. Unset KANZ_TOKEN and set KANZ_IDENTITY_URL to sign in")
	}
	if subject == "" {
		return errors.New("usage: /login <subject> (the account you were invited as, e.g. user:alice)")
	}
	if r.creds == nil {
		return errors.New("this surface cannot ask for a credential")
	}
	cred, err := r.creds.Credential(ctx, subject)
	if err != nil {
		return err
	}
	tok, err := r.auth.Login(ctx, subject, cred)
	if err != nil {
		return err
	}
	r.token = tok
	if err := r.store.Save(tok); err != nil {
		// A persistence failure is non-fatal: the session token still works,
		// the user just signs in again next run. Surface it, don't abort.
		r.errf("warning: could not persist token: %v", err)
	}
	fmt.Fprintf(r.out, "✓ signed in as %s\n", orNA(tok.Subject))
	return nil
}

func (r *REPL) logout() {
	if r.devSession {
		// Clearing r.token would be a lie: the next New reads KANZ_TOKEN again,
		// so the session would come back on restart while /whoami claimed it was
		// gone. Say where the credential actually lives.
		r.errf("this session comes from KANZ_TOKEN in the environment, not a stored sign-in — " +
			"unset that variable to end it")
		return
	}
	r.token = nil
	if err := r.store.Delete(); err != nil {
		r.errf("logout: %v", err)
		return
	}
	fmt.Fprintln(r.out, "✓ signed out")
}

// whoami prints the session's subject, tenant, and expiry.
//
// IT PREFERS WHAT THE IDENTITY SERVICE SAID over what the token claims. A
// sign-in returns subject and tenant alongside the JWT, and those are the
// service's own answer; decoding the token here would be re-deriving them from
// an UNVERIFIED payload, which is a worse source for the same fact. The decode
// survives only for a KANZ_TOKEN session, which arrived as a bare bearer string
// with nothing beside it — and it is display-only either way, because the
// gateway is the authority that verifies anything.
func (r *REPL) whoami() {
	if r.token == nil {
		fmt.Fprintln(r.out, "not signed in — run /login <subject>")
		return
	}
	if r.devSession {
		fmt.Fprintln(r.out, "session: KANZ_TOKEN from the environment (not a sign-in)")
	}
	sub, tenant := r.token.Subject, r.token.Tenant
	if sub == "" || tenant == "" {
		claims := decodeClaims(r.token.Token)
		if sub == "" {
			sub, _ = claims["sub"].(string)
		}
		if tenant == "" {
			tenant, _ = claims["tenant"].(string)
		}
	}
	fmt.Fprintf(r.out, "subject: %s\n", orNA(sub))
	fmt.Fprintf(r.out, "tenant:  %s\n", orNA(tenant))
	if !r.token.Expires.IsZero() {
		fmt.Fprintf(r.out, "expires: %s (%s)\n", r.token.Expires.Format(time.RFC3339), humanUntil(r.token.Expires.Sub(r.now())))
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
	if r.devSession {
		// VISIBLE EVERY SESSION, not once. The failure mode of a static bearer is
		// forgetting you are on one, so this is printed where it cannot be
		// scrolled past before the first command.
		fmt.Fprintln(r.out, "⚠ using KANZ_TOKEN from the environment — not a sign-in. /login is unavailable.")
	}
}

func (r *REPL) help() {
	fmt.Fprint(r.out, `
Commands:
  <question>                     ask the copilot in natural language
  /exposure <id> [as_of]         portfolio exposure (as_of RFC3339, default latest)
  /measures <id> [measure ...]   named risk measures (e.g. VaR99 Delta)
  /scenario <id> <json>          evaluate a scenario (JSON EvaluateScenarioRequest)
  /login <subject>               sign in at the identity provider (e.g. /login user:alice)
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
