package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AN ADDRESS THAT NAMES A PORT NOTHING PUBLISHES IS A DEAD FLOW (#762).
//
// # The failure this was written for, twice
//
// This estate has shipped the same defect two separate times, and both are
// recorded in the manifests themselves.
//
// risk-engine's Service published only 8081 while the gateway was configured for
// risk-engine.kanz-services.svc:9090, so every read route — exposure, measures,
// the lot — was dead at the network layer. The Service comment says so: "It was
// never exposed ... so every one of those routes was dead at the network layer."
// The NetworkPolicy had the mirror-image version of it, naming 8081 for a flow
// the gateway makes on 9090: "the policy permitted a flow nobody makes and
// denied the one that matters."
//
// Then services/mcp shipped with MCP_RISK_QUERY_ADDR pointing at
// risk-engine.kanz-services.svc:9000 — the port the VENUE ADAPTERS publish, not
// a port risk-engine has ever had. The read plane could not have answered a
// single query, and nothing said so.
//
// # Why nothing caught it
//
// Both halves are individually valid YAML and individually plausible. The defect
// lives in the GAP between two documents — an address in one workload's env and
// a port list in another workload's Service — which is exactly the shape a
// per-document check cannot see, and the same argument
// submit_fields_reach_state_test.go makes for two proto messages.
//
// A deployment does not fail either: the pod starts, the dial hangs until its
// deadline, and the symptom is a slow upstream rather than a wrong number.
//
// # What this checks
//
// Every `<service>.kanz-services.svc:<port>` address in the infra tree names a
// port that service's own Service object publishes. Both spellings are matched —
// bare `host:port` and `http://host:port` — because both are in use.
//
// It is deliberately limited to kanz-services. A cross-namespace address
// (operator lives in kanz-operator) names a Service this tree may not define at
// all, and asserting on one it cannot see would fail for the wrong reason.
const infraDir = "infra"

// addrPattern matches a kanz-services address with an explicit port, with or
// without a scheme in front of it.
var addrPattern = regexp.MustCompile(`([a-z0-9-]+)\.kanz-services\.svc:(\d+)`)

// serviceAddressExempt names an address permitted to point at an unpublished
// port, and the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An address naming a port nothing serves is a
// flow that cannot complete; there is no configuration in which that is correct.
// An entry here is a claim that some dial is meant to fail, which has to be
// argued in writing before it is true in code.
var serviceAddressExempt = map[string]string{}

func TestEveryServiceAddressNamesAPublishedPort(t *testing.T) {
	root := moduleRoot(t)
	published := publishedServicePorts(t, filepath.Join(root, infraDir))

	// NON-VACUITY, first half. A parse that found no Services would find nothing
	// to contradict and report a clean estate having checked nothing.
	if len(published) < 10 {
		t.Fatalf("found %d Service object(s) under %s — the estate defines far more, so the parse "+
			"is broken rather than the manifests", len(published), infraDir)
	}

	type addr struct{ file, service, port string }
	var found []addr
	walkYAML(t, filepath.Join(root, infraDir), func(rel, body string) {
		for _, m := range addrPattern.FindAllStringSubmatch(body, -1) {
			found = append(found, addr{file: rel, service: m[1], port: m[2]})
		}
	})

	// NON-VACUITY, second half. Zero addresses means the pattern stopped
	// matching — a rename, a format change — and every flow would look fine.
	if len(found) == 0 {
		t.Fatalf("no %s addresses found under %s — the address format changed and this guard is "+
			"checking nothing", "*.kanz-services.svc:<port>", infraDir)
	}

	var problems []string
	seenExempt := map[string]bool{}
	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].service+found[i].port < found[j].service+found[j].port
	})
	for _, a := range found {
		ports, known := published[a.service]
		if !known {
			// A Service this tree does not define. Reported rather than skipped:
			// an address for a workload with no Service object is the same dead
			// flow by another route.
			problems = append(problems, fmt.Sprintf(
				"%s dials %s.kanz-services.svc:%s but no Service named %q is defined under %s",
				a.file, a.service, a.port, a.service, infraDir))
			continue
		}
		if ports[a.port] {
			continue
		}
		key := a.service + ":" + a.port
		if reason, ok := serviceAddressExempt[key]; ok {
			seenExempt[key] = true
			t.Logf("%s: unpublished port, tracked — %s", key, reason)
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s dials %s.kanz-services.svc:%s, but the %s Service publishes %v — the dial hangs "+
				"until its deadline and reads as a slow upstream rather than a wrong port",
			a.file, a.service, a.port, a.service, sortedPortList(ports)))
	}
	// DEAD-ENTRY CHECK: an exemption matching nothing has outlived its repair.
	for key := range serviceAddressExempt {
		if !seenExempt[key] {
			problems = append(problems, "exemption for "+key+" matches nothing — delete the entry")
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// publishedServicePorts maps a Service name to the ports it publishes, read from
// every Service object in the tree.
func publishedServicePorts(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	nameRe := regexp.MustCompile(`(?m)^  name: (\S+)`)
	portRe := regexp.MustCompile(`\bport: (\d+)`)
	walkYAML(t, dir, func(_ string, body string) {
		for _, doc := range strings.Split(body, "\n---") {
			if !strings.Contains(doc, "kind: Service") {
				continue
			}
			// The ports list belongs to spec; a Service has no other `port:` key.
			n := nameRe.FindStringSubmatch(doc)
			if n == nil {
				continue
			}
			ports := out[n[1]]
			if ports == nil {
				ports = map[string]bool{}
				out[n[1]] = ports
			}
			for _, p := range portRe.FindAllStringSubmatch(doc, -1) {
				ports[p[1]] = true
			}
		}
	})
	return out
}

func walkYAML(t *testing.T, dir string, fn func(rel, body string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		fn(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func sortedPortList(ports map[string]bool) []string {
	out := make([]string, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
