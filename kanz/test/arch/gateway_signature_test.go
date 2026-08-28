package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/gatewaysig"
)

// ONE STATEMENT OF THE GATEWAY REQUEST SIGNATURE (#781).
//
// The gateway rejects an unsigned request BEFORE authentication runs whenever
// API_GATEWAY_SIGNING_SECRET_FILE is set, which every deployment manifest does.
// THREE CALLERS SHIPPED WITHOUT IT, one after another — the Copilot REPL's
// client (#198), provision-tenant.sh's isolation probes (#774), and
// services/web-bff (#777) — and each was repaired by re-deriving the signature
// at the call site, which is why there was a next one.
//
// WHY IT RECURS RATHER THAN BEING LEARNED. The failure presents as an
// authentication error, because it IS one: middleware.Signing answers 401 before
// Auth. #774's operator was told the token was bad and minted three fresh ones,
// each reproducing the identical 401; the plausible fourth step is to conclude
// the isolation gate is broken and hand a tenant over without it. #777's web app
// authenticated perfectly and 401'd on every screen carrying data, because
// /auth/* never crosses the gateway — while the Go suite, 110 Vitest tests, the
// image build and the deployability check all passed.
//
// WHAT THIS CHECKS, in two arms that cover different populations:
//
//	Go     no file outside internal/gatewaysig names the header literally, so a
//	       caller cannot construct a signature without going through the one
//	       implementation the gateway's own verifier is built from.
//	shell  infra/onboarding/provision-tenant.sh — a caller no Go guard can see —
//	       produces BYTE-IDENTICAL output to gatewaysig.Sign, proven by running
//	       it, not by matching its text.
//
// It shares stripGoComments with order_digest_covers_submit_test.go — the package
// already had one, and this guard needs it for the same reason that one does: the
// prose describing a rule names every token the rule is about.
//
// WHY THE HEADER NAME AND NOT THE HMAC. A guard keyed on "does this file compute
// hmac.New(sha256.New, ...)" has to distinguish four unrelated HMACs already in
// the tree — devtoken, Binance and OKX exchange auth, webhook-ingest — from this
// one, and it would do it by inspecting what gets written to the hash, which is
// the part a new caller is free to spell differently. The HEADER is the
// narrow waist: a signature nothing sends under X-Signature is not a gateway
// signature, and a caller that names the header has to name the value too.
func TestTheGatewaySignatureHasOneImplementation(t *testing.T) {
	root := moduleRoot(t)

	// The one place allowed to name it, and the tests that assert about it.
	const home = "internal/gatewaysig/"

	// A DIFFERENT CONTRACT THAT HAPPENS TO SHARE THE HEADER NAME.
	//
	// webhook-ingest reads an INBOUND X-Signature from external producers
	// (TradingView and friends) and verifies it against a per-source secret in
	// internal/ingest/auth.go. That is not this platform's signature to the
	// gateway; it is somebody else's signature to us, over a canonicalization we
	// do not choose and cannot change.
	//
	// ROUTING IT THROUGH gatewaysig.Header WOULD BE THE BUG, not the fix: it would
	// couple two unrelated contracts, so renaming the gateway's header — a thing
	// this platform may do — would silently change what we expect from an external
	// system that never heard about it. The collision is in the name only.
	//
	// THIS EXEMPTION DOES NOT RETIRE. It is a permanent distinction, not a repair
	// somebody owes; the dead-entry check below is what keeps it honest if
	// webhook-ingest ever stops naming the header.
	exempt := map[string]string{
		"services/webhook-ingest/internal/server/server.go": "reads an INBOUND webhook signature " +
			"from an external producer, not this platform's signature TO the gateway",
	}
	matched := map[string]bool{}

	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "testdata" || info.Name() == ".git" ||
				info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, home) {
			return nil
		}
		// TESTS MAY NAME IT. A test asserting that a proxy stamped the header has
		// to read the header, and forcing it through the constant would only move
		// the literal — the constant is imported from the same package the
		// production code uses, so a test cannot drift the CONTRACT, only its own
		// assertion. Production code is where a second spelling becomes a 401.
		if strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		// COMMENTS STRIPPED FIRST. This guard's own prose names the header
		// repeatedly, and so do the packages it guards — a raw scan would match
		// the explanation rather than the code, which is a guard that checks
		// nothing.
		for _, line := range strings.Split(stripGoComments(string(b)), "\n") {
			if strings.Contains(line, `"`+gatewaysig.Header+`"`) {
				if _, ok := exempt[rel]; ok {
					matched[rel] = true
					break
				}
				offenders = append(offenders, fmt.Sprintf(
					"%s names %q as a literal. Use gatewaysig.Header, and produce the value with "+
						"gatewaysig.Sign or SignRequest — the gateway's own verifier is built from "+
						"them, so a caller that goes through this package is correct by construction "+
						"and one that does not is a 401 that reads as an expired token (#781)",
					rel, gatewaysig.Header))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// NON-VACUOUS BY DESIGN: a walk that scanned nothing reports PASS.
	if scanned == 0 {
		t.Fatal("no Go file was scanned — this guard verified NOTHING")
	}
	// A DEAD EXEMPTION IS A LIE THAT PASSES. An entry naming a file that no longer
	// spells the header records a reason for behaviour that has stopped happening,
	// and it would go on excusing that path if the file ever named it again for a
	// different reason. Same rule the digest and drop-env exemptions are held to.
	for rel, why := range exempt {
		if !matched[rel] {
			offenders = append(offenders, fmt.Sprintf(
				"%s is exempted (%s) and no longer names %q. Delete the entry — an exemption that "+
					"matches nothing excuses a file nobody has checked", rel, why, gatewaysig.Header))
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("the gateway signature is stated in more than one place (%d files scanned):\n  - %s",
			scanned, strings.Join(offenders, "\n  - "))
	}
}

// TestTheShellSignerAgreesWithTheGoSigner is the arm no Go-only guard can have.
//
// infra/onboarding/provision-tenant.sh signs its isolation probes in shell —
// openssl, base64, tr — and it is one of the three callers that shipped broken
// (#774). Nothing about it is visible to the compiler, so the only honest check
// is to RUN it and compare bytes.
//
// The structural half always runs; the execution half runs where a POSIX shell
// and openssl exist, which is CI and every developer machine this repository
// targets. When the tools are absent the structural assertions still hold, so
// the guard is never vacuous — but it is weaker, and it says so rather than
// reporting a clean pass.
func TestTheShellSignerAgreesWithTheGoSigner(t *testing.T) {
	root := moduleRoot(t)
	script := filepath.Join(root, "infra", "onboarding", "provision-tenant.sh")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	src := string(b)

	fn := signArgsFunc(t, src)

	// --- structural: the four ways this canonicalization is got wrong ---------
	//
	// Each of these is a real mistake with a real symptom, and each produces a
	// signature that is wrong on EVERY request rather than an unlucky subset.
	for _, want := range []struct{ frag, why string }{
		{`'%s\n%s\n'`, "the trailing newline before the (empty) body. printf '%s\\n%s' drops it " +
			"and every signature is wrong by one byte"},
		{"-sha256", "the digest the gateway uses"},
		{"-hmac", "an HMAC, not a bare digest"},
		{`tr '+/' '-_'`, "base64URL's alphabet; standard base64 differs on the same inputs"},
		{`tr -d '=`, "unpadded. A SHA-256 signature is always padded under standard base64, so " +
			"leaving it on is wrong every time"},
	} {
		if !strings.Contains(fn, want.frag) {
			t.Errorf("provision-tenant.sh's signer does not contain %s — %s", want.frag, want.why)
		}
	}

	// --- execution: the bytes themselves --------------------------------------
	sh, shErr := exec.LookPath("sh")
	ssl, sslErr := exec.LookPath("openssl")
	if shErr != nil || sslErr != nil {
		t.Logf("STRUCTURAL ONLY: sh (%v) or openssl (%v) not on PATH, so the shell signer's "+
			"OUTPUT was not compared against gatewaysig.Sign — only its shape. CI runs both.",
			shErr, sslErr)
		return
	}
	_ = ssl

	const (
		key    = "s3cr3t"
		method = "GET"
		path   = "/v1/portfolios/PF1/exposure"
	)
	// Run the script's OWN function, lifted verbatim, rather than a
	// reimplementation of it here — a copy in this file would be the fifth
	// statement of the rule and would pass while the script drifted.
	prog := fn + "\nVERIFY_SIGNING_SECRET='" + key + "' sign_args " + method + " " + path + "\n"
	out, err := exec.Command(sh, "-c", prog).Output()
	if err != nil {
		t.Fatalf("running provision-tenant.sh's sign_args: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := gatewaysig.Sign([]byte(key), method, path, nil)
	if got != want {
		t.Fatalf("the shell signer and the Go signer disagree.\n  shell: %q\n  go:    %q\n\n"+
			"provision-tenant.sh's isolation probes would be refused by the gateway with a 401 "+
			"that names the TOKEN — which is #774 exactly, and its operator response is to hand a "+
			"tenant over without the cross-tenant check that script exists to run.", got, want)
	}
}

// signArgsFunc lifts the sign_args shell function out of the script, from its
// declaration to its closing brace at column 0.
func signArgsFunc(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "sign_args() {")
	if start < 0 {
		t.Fatal("provision-tenant.sh no longer declares sign_args. Either the isolation probes " +
			"stopped signing — which is #774 returning — or the function was renamed and this " +
			"guard is now checking nothing")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of sign_args")
	}
	return src[start : start+end+3]
}

// AND EVERY BINARY THAT DIALS THE GATEWAY CAN ACTUALLY SIGN (#781).
//
// The guard above stops a caller re-deriving the signature. It does NOT stop the
// failure that actually happened three times, which is simpler: a component
// dials the gateway and sends no signature at all. Nothing is duplicated in that
// case — there is just nothing there, and nothing is what every signal reports.
//
// So this asks the BUILD GRAPH. For every main package whose dependencies
// include something that names a gateway base URL, internal/gatewaysig must also
// be in those dependencies. It is a reachability question and `go list -deps`
// answers it exactly: transitively, through however many packages the binary
// composes, with no guessing about which file "is" the client.
//
// WHAT IT CANNOT SEE, stated rather than implied: whether the signer is
// CALLED on the request that goes out. A binary can import gatewaysig and still
// forget one code path. That residue is what the first guard covers from the
// other side — a caller that forgot AND re-derived is caught there — and what
// services/web-bff/internal/server/proxy_signing_test.go covers for the one
// component whose whole job is proxying. Between the three, the shape that
// shipped three times is closed.
func TestEveryGatewayDialingBinaryCanSign(t *testing.T) {
	const signer = "github.com/eighred/kanz/internal/gatewaysig"

	// A BINARY THAT REACHES THE GATEWAY AND MUST NOT SIGN, with the reason.
	//
	// kanz-monitor polls the gateway's /readyz and nothing else. The signing
	// middleware is mounted on the /v1/ subtree only (api-gateway main.go:
	// mux.Handle("/v1/", chain(gwMux))), so /readyz is outside it and a signature
	// there would be neither required nor checked. The day this tool reads a /v1
	// path it becomes the fourth instance, and deleting this entry is the change
	// that makes it sign.
	exempt := map[string]string{
		"github.com/eighred/kanz/cmd/kanz-monitor": "polls /readyz only, which is mounted " +
			"outside the signed /v1/ chain",
	}
	matched := map[string]bool{}

	// Packages that name a gateway base URL — the honest marker for "this
	// component talks to the gateway", because every one of them has to be told
	// where it is.
	dialers := packagesNamingGatewayURL(t, moduleRoot(t))
	if len(dialers) == 0 {
		t.Fatal("no package names a *GATEWAY_URL env var — either the convention changed or the " +
			"walk is broken. Finding none is a failure, not a pass: this guard would then hold " +
			"for every binary vacuously")
	}

	var problems []string
	checked := 0
	for _, main := range goListMainPackages(t, moduleRoot(t)) {
		deps := goListDeps(t, moduleRoot(t), main)
		dials := false
		for _, d := range deps {
			if dialers[d] {
				dials = true
				break
			}
		}
		if !dials {
			continue
		}
		checked++
		if why, ok := exempt[main]; ok {
			matched[main] = true
			_ = why
			continue
		}
		signs := false
		for _, d := range deps {
			if d == signer {
				signs = true
				break
			}
		}
		if !signs {
			problems = append(problems, fmt.Sprintf(
				"%s dials the api-gateway and nothing in its build graph can produce a request "+
					"signature. Every request it sends to /v1 is refused with a 401 BEFORE "+
					"authentication runs, which reads as an expired token and has now been "+
					"misdiagnosed three times (#198, #774, #777). Import %s, or add an exemption "+
					"here stating why this binary reaches the gateway without signing",
				main, signer))
		}
	}

	if checked == 0 {
		t.Fatal("no main package was found to dial the gateway — this guard verified NOTHING")
	}
	for main, why := range exempt {
		if !matched[main] {
			problems = append(problems, fmt.Sprintf(
				"%s is exempted (%s) and no longer dials the gateway at all. Delete the entry — an "+
					"exemption that matches nothing excuses a binary nobody has checked", main, why))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("gateway callers that cannot sign (%d dialing binaries checked):\n  - %s",
			checked, strings.Join(problems, "\n  - "))
	}
}

// packagesNamingGatewayURL returns the import paths of packages whose
// non-test source names a *GATEWAY_URL environment variable.
func packagesNamingGatewayURL(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "testdata" || info.Name() == ".git" ||
				info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !gatewayURLEnv.MatchString(stripGoComments(string(b))) {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		out["github.com/eighred/kanz/"+filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gatewayURLEnv matches the env var every gateway caller is told the address
// through: KANZ_GATEWAY_URL, WEB_BFF_GATEWAY_URL, and whatever the next one is
// called. Matched inside a string literal, after comments are stripped.
var gatewayURLEnv = regexp.MustCompile(`"[A-Z0-9_]*GATEWAY_URL"`)

// goListMainPackages is named around consumer_goroutine_join_test.go's
// mainPackages, which globs DIRECTORIES for a different question. This one
// needs IMPORT PATHS, because `go list -deps` takes those.
func goListMainPackages(t *testing.T, root string) []string {
	t.Helper()
	// RUN FROM THE MODULE ROOT. A Go test's working directory is its own package
	// directory, so `go list ./...` here would enumerate test/arch and nothing
	// else — the guard then finds no main package, and without the non-vacuity
	// check below it would have reported a clean pass over an empty set.
	cmd := exec.Command("go", "list", "-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`, "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list main packages: %v", err)
	}
	var pkgs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pkgs = append(pkgs, l)
		}
	}
	return pkgs
}

func goListDeps(t *testing.T, root, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
