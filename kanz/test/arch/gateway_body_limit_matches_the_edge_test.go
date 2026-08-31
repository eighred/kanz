package arch

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// THE GATEWAY'S BODY CEILING AND THE EDGE'S MUST BE THE SAME NUMBER (#887).
//
// `middleware.MaxRequestBody` is not an invented threshold. It is derived from
// `nginx.ingress.kubernetes.io/proxy-body-size` in
// infra/deploy/api-gateway-ingress.yaml, on the argument that a body the edge
// already refuses cannot be one the gateway is obliged to accept.
//
// A derivation that nothing checks is just a comment. These two live in
// different languages, in different directories, and are changed by different
// people for different reasons — raising one and not the other is the ordinary
// outcome, and it fails in both directions:
//
//   - edge raised, gateway not: the estate believes it accepts larger bodies and
//     the gateway answers 413 for requests nginx happily forwarded;
//   - gateway raised, edge not: the gateway's own bound becomes untested prose,
//     because nothing over the edge's smaller limit ever reaches it on the public
//     path — and it silently becomes the ONLY bound on the two paths that skip
//     the Ingress (the browser route through web-bff, and any in-cluster peer the
//     NetworkPolicy admits to :8080).
//
// The second is the dangerous one, because it looks like an improvement.
//
// # This guard checks agreement, not a particular value
//
// Either number may change; they may not disagree. That keeps the guard from
// becoming a second place a threshold is written down, which would be the defect
// it exists to prevent (derive the set from the source of truth, never restate
// it).
func TestTheGatewayBodyLimitMatchesTheEdge(t *testing.T) {
	root := moduleRoot(t)

	ingress := readFile(t, filepath.Join(root, "infra", "deploy", "api-gateway-ingress.yaml"))
	m := regexp.MustCompile(`nginx\.ingress\.kubernetes\.io/proxy-body-size:\s*"([0-9]+)([kKmMgG]?)"`).
		FindStringSubmatch(ingress)
	if m == nil {
		t.Fatalf("no proxy-body-size annotation in infra/deploy/api-gateway-ingress.yaml.\n\n" +
			"That annotation is where middleware.MaxRequestBody's value comes from. If the edge " +
			"stopped bounding the body, the gateway's constant is no longer a derivation and " +
			"needs its own stated justification — do not delete this guard to make that go away.")
	}
	edge := parseByteSize(t, m[1], m[2])

	src := readFile(t, filepath.Join(root, "services", "api-gateway", "internal", "middleware", "bodylimit.go"))
	// The declaration, not any mention of it: this file's own doc comment
	// discusses the constant at length, and a looser match would read the prose.
	c := regexp.MustCompile(`(?m)^const MaxRequestBody = (\d+) << (\d+)`).FindStringSubmatch(src)
	if c == nil {
		t.Fatalf("could not find `const MaxRequestBody = N << M` in middleware/bodylimit.go.\n\n" +
			"This guard reads the declaration itself rather than a mention of it. If the constant " +
			"moved or changed shape, move this matcher with it rather than dropping the check.")
	}
	mant, _ := strconv.ParseInt(c[1], 10, 64)
	shift, _ := strconv.ParseInt(c[2], 10, 64)
	gateway := mant << uint(shift)

	if gateway != edge {
		t.Fatalf("middleware.MaxRequestBody = %d bytes but the Ingress allows %d (%s%s).\n\n"+
			"These are the same bound expressed twice and they have drifted. The gateway's "+
			"constant is DERIVED from the edge's — a body nginx already refuses is not one the "+
			"gateway must accept — so the two cannot disagree without one of them being "+
			"unjustified.\n\n"+
			"If the intent was to raise the ceiling, raise both. If the intent was for the "+
			"gateway to be STRICTER than the edge, that is a real design and it needs saying "+
			"out loud in bodylimit.go, because it changes what this guard should check.",
			gateway, edge, m[1], m[2])
	}

	t.Logf("gateway MaxRequestBody = ingress proxy-body-size = %d bytes", gateway)
}

// parseByteSize turns nginx's size shorthand ("1m", "512k", "1048576") into
// bytes. nginx accepts k/m/g and treats them as binary multiples.
func parseByteSize(t *testing.T, digits, unit string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("proxy-body-size %q is not a number: %v", digits, err)
	}
	switch strings.ToLower(unit) {
	case "":
		return n
	case "k":
		return n << 10
	case "m":
		return n << 20
	case "g":
		return n << 30
	}
	t.Fatalf("unrecognised proxy-body-size unit %q — extend this parser rather than letting the "+
		"comparison silently use the wrong scale", unit)
	return 0
}
