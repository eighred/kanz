package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE GATEWAY'S MANDATE PATHS AND COMPLIANCE'S MUST BE THE SAME STRINGS (#606).
//
// # The hole this closes
//
// api.go's routes() says the mandate routes "are 1:1 with the gateway's, so the
// gateway path IS the upstream path and nothing rewrites". Nothing enforced that.
// Both sides spell the same path as a STRING LITERAL in two different files, and
// two string literals are exactly the thing that drifts.
//
// Each side's tests pin its OWN literal, and only its own. So a bare rename is
// caught — but a rename done the way a person actually does one is not. Change
// compliance's path, update compliance's tests along with it (nobody leaves their
// own suite red), and leave the gateway file alone, and this is the result,
// measured rather than assumed:
//
//	services/compliance/...   ok   (4 packages)
//	services/api-gateway/...  ok   (8 packages)
//
// Twelve green packages, both halves internally consistent, and every request a
// signatory makes in production answers 404. The gateway tests cannot see it:
// they forward to a FAKE backend that answers any path at all, so they never
// learn the real handler moved.
//
// That is #606's own defect one layer out — a route that exists, is fronted,
// demands the right capability, and still cannot be reached — and it is the third
// time this shape has cost real time (#539).
//
// # Why set equality rather than "the gateway's are a subset"
//
// A path compliance serves that the gateway does not front is a handler no client
// can reach — the state #606 was filed on. A path the gateway fronts that
// compliance does not serve is a 404 the client cannot distinguish from "that
// proposal is not yours", because notFoundBody is deliberately the answer to
// both. Both directions are defects, so the assertion is equality.
//
// # What it does not check
//
// That the handler behind each path is the RIGHT one — only that both sides agree
// a path exists. The capability each demands is held by
// authz.TestTheWholeRouteTableIsDeclared; that a signatory's request actually
// arrives is held by TestAMandateSignatoryReachesAllFourRoutes.

var (
	// complianceMuxRouteRe matches compliance's own registrations.
	complianceMuxRouteRe = regexp.MustCompile(`s\.mux\.HandleFunc\("([A-Z]+ /[^"]+)"`)
	// gatewayComplianceRouteRe matches a gateway registration whose handler
	// forwards to the compliance service. (?s) so the two-line call shape matches.
	gatewayComplianceRouteRe = regexp.MustCompile(
		`(?s)mux\.Handle\(authz\.[A-Za-z]+,\s*"([A-Z]+ /[^"]+)",\s*h\.handle\(ServiceCompliance,`)
	// routeParityCommentRe strips // comments, so the guard cannot satisfy itself
	// from the prose that describes these very routes — both files list every path
	// in a doc comment, and a guard that reads its own documentation checks
	// nothing (learned on body_identity, three guards deep).
	routeParityCommentRe = regexp.MustCompile(`(?m)//.*$`)
)

func routeParityMatches(t *testing.T, path string, re *regexp.Regexp) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := routeParityCommentRe.ReplaceAllString(string(b), "")
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out = append(out, strings.Join(strings.Fields(m[1]), " "))
	}
	sort.Strings(out)
	return out
}

func TestTheMandateRoutesAreOneToOneAcrossTheGateway(t *testing.T) {
	root := moduleRoot(t)

	compliance := routeParityMatches(t,
		filepath.Join(root, "services", "compliance", "internal", "api", "api.go"),
		complianceMuxRouteRe)
	gateway := routeParityMatches(t,
		filepath.Join(root, "services", "api-gateway", "internal", "proxy", "proxy.go"),
		gatewayComplianceRouteRe)

	// NON-VACUITY. Two empty sets are equal, and a broken regex produces two empty
	// sets — so without this the guard is loudest exactly when it has stopped
	// working. Four routes exist as of #606: propose, approve, the queue, and the
	// by-id read.
	if len(compliance) < 4 {
		t.Fatalf("found %d route(s) on compliance's mux, expected at least 4 — the scan is not "+
			"finding s.mux.HandleFunc registrations, so the comparison below proves nothing:\n  %v",
			len(compliance), compliance)
	}
	if len(gateway) < 4 {
		t.Fatalf("found %d compliance route(s) fronted by the gateway, expected at least 4 — the "+
			"scan is not finding the mux.Handle(...ServiceCompliance...) registrations, so the "+
			"comparison below proves nothing:\n  %v", len(gateway), gateway)
	}

	if strings.Join(compliance, "\n") == strings.Join(gateway, "\n") {
		return
	}

	inGatewayOnly := routeParityMissing(gateway, compliance)
	inComplianceOnly := routeParityMissing(compliance, gateway)

	var b strings.Builder
	b.WriteString("the gateway and compliance no longer agree on the mandate paths.\n\n")
	if len(inComplianceOnly) > 0 {
		b.WriteString("compliance SERVES these and the gateway fronts nothing for them:\n  " +
			strings.Join(inComplianceOnly, "\n  ") + "\n\n" +
			"That is a handler no client can reach — the exact state #606 was filed on, where the " +
			"code claimed a mandate \"is fetched by proposal id\" and no route existed.\n\n")
	}
	if len(inGatewayOnly) > 0 {
		b.WriteString("the gateway FRONTS these and compliance serves nothing for them:\n  " +
			strings.Join(inGatewayOnly, "\n  ") + "\n\n" +
			"Every call answers 404 from the upstream — which a client cannot distinguish from " +
			"\"that proposal is not yours\", because notFoundBody is deliberately the answer to " +
			"both.\n\n")
	}
	b.WriteString("Nothing rewrites between them: the gateway path IS the upstream path, so these " +
		"two literals have to be the same string. Do not assume the service suites would have " +
		"caught this — a rename that updates its own side's tests leaves all twelve compliance and " +
		"api-gateway packages green, because the gateway's forward to a fake backend that answers " +
		"any path.")
	t.Error(b.String())
}

func routeParityMissing(from, in []string) []string {
	have := make(map[string]bool, len(in))
	for _, s := range in {
		have[s] = true
	}
	var out []string
	for _, s := range from {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}
