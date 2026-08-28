package arch

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eighred/kanz/internal/tenantgen"
)

// A TENANT'S OMS STARTS DENY-BY-DEFAULT, OR IT TRADES WITH NOTHING EVALUATED (#779).
//
// WHAT WAS WRONG. infra/deploy/tenants/acme/oms-acme.yaml shipped
// OMS_REQUIRE_MANDATE=false, carried through from the platform base. With it
// false, an order for a portfolio no mandate governs takes the UNGOVERNED branch
// in internal/compliance's PreTradeGate and comes back Allowed:true — the order
// is admitted with NO compliance constraint evaluated at all. Concentration
// limits, restricted lists, issuer exclusions, leverage caps and buying-power
// limits are inert for that portfolio, and the audit trail records the order as
// having passed pre-trade compliance. The only trace is a counter and a WARN
// that fires once per portfolio for the life of the process.
//
// WHY THE BASE IS RIGHT TO SHIP FALSE AND A TENANT IS NOT. oms-deploy.yaml's own
// paragraph states the reason: arming it refuses every order for every portfolio
// nobody has run kanz-mandate for yet — "a trading outage dressed as a control".
// That is true of the pre-tenancy __system__ book, whose portfolios are older
// than the control. A tenant provisioned today has no such portfolios, so the
// grandfathering buys nothing and costs a client's first orders going out
// unchecked.
//
// WHAT THIS CHECKS, and it is deliberately BOTH ends of the pipe:
//
//   - the DECLARATION — internal/tenantgen.Services declares the override, so
//     the NEXT tenant rendered gets it too. A guard that only read the committed
//     acme manifest would pass for a repository whose generator had stopped
//     emitting the value, right up until someone onboarded a client.
//   - the ARTIFACT — every committed per-tenant OMS manifest actually carries
//     OMS_REQUIRE_MANDATE=true on the OMS container. The declaration is what
//     renders; the manifest is what deploys, and infra/deploy is raw-synced.
//
// It PARSES the manifest rather than grepping it. A grep for the string "true"
// near the variable name would also match the prose in the base's paragraph that
// the render carries through as comments — the failure mode
// a-guard-that-matches-prose-checks-nothing names, where a guard passes with the
// checked thing deleted.
//
// WHAT IT DOES NOT CHECK, deliberately: that a mandate EXISTS for any portfolio.
// That is a cluster fact, not a repository one, and seeding one from a script
// would be worse than the gap — a zero-rule mandate published by a robot records
// "somebody decided to constrain nothing" in the audit trail for a decision no
// human made, and silences the ungoverned counter while changing nothing about
// what the order does. See tenantgen.Services' SetEnv entry.
//
// RELATED, NOT DUPLICATED. deny_by_default_is_visible_test.go checks that every
// *_REQUIRE_* key a service reads is NAMED in some deploy manifest — that an
// operator can see the control exists. It says nothing about the value, and
// notes so explicitly. This guard is the one place a value is asserted, and only
// for per-tenant renders.
func TestPerTenantOMSRequiresAMandate(t *testing.T) {
	const key = "OMS_REQUIRE_MANDATE"
	const want = "true"

	root := moduleRoot(t)

	// 1. THE DECLARATION. Read from tenantgen.Services, which is what
	//    cmd/kanz-tenantgen and test/arch/tenant_compute_test.go both render
	//    from, so this fails the day the entry is deleted rather than the day a
	//    tenant is onboarded without it.
	oms, ok := tenantgen.ServiceByName("oms")
	if !ok {
		t.Fatal("internal/tenantgen.Services declares no service named \"oms\" — either the order path " +
			"is no longer rendered per tenant (in which case this guard and #779's fix both need " +
			"rewriting against whatever replaced it), or the name changed and this guard has " +
			"silently stopped checking anything")
	}
	declared := ""
	for _, o := range oms.SetEnv {
		if o.Name == key {
			declared = o.Value
		}
	}
	switch declared {
	case want:
		// The generator arms the control for every tenant it renders.
	case "":
		t.Fatalf("internal/tenantgen.Services' %q entry declares no SetEnv override for %s. Without it a "+
			"rendered tenant inherits the platform base's %s=false, and every order for a portfolio "+
			"under no mandate is ADMITTED WITH NO COMPLIANCE CONSTRAINT EVALUATED while the trail "+
			"records it as having passed pre-trade compliance (#779). The base's stated reason for "+
			"false is about __system__ portfolios that predate mandates; a tenant provisioned today "+
			"has none.", oms.Name, key, key)
	default:
		t.Fatalf("internal/tenantgen.Services declares %s=%q for a rendered tenant, not %q. Anything but "+
			"%q leaves the ungoverned branch reachable: internal/compliance's gate reads this as a "+
			"POSTURE, and only the true posture turns a portfolio nobody has put under mandate into a "+
			"MANDATE_MISSING refusal instead of an unconstrained admission (#779).",
			key, declared, want, want)
	}

	// 2. THE ARTIFACT. Every committed per-tenant OMS manifest, parsed.
	tenantsDir := filepath.Join(root, "infra", "deploy", "tenants")
	entries, err := os.ReadDir(tenantsDir)
	if err != nil {
		t.Fatalf("read %s: %v", tenantsDir, err)
	}

	checked := 0
	var problems []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tenant := e.Name()
		manifest := filepath.Join(root, filepath.FromSlash(oms.ManifestPath(tenant)))
		b, err := os.ReadFile(manifest)
		if err != nil {
			// A tenant directory with no OMS manifest is tenant_compute_test.go's
			// finding, not this one's — it re-renders every declared service for
			// every tenant directory and reports the missing file with the right
			// vocabulary. Reporting it twice, differently, sends the reader to the
			// wrong repair.
			continue
		}
		got, found, err := containerEnvValue(b, oms.Container, key)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", oms.ManifestPath(tenant), err))
		case !found:
			problems = append(problems, fmt.Sprintf(
				"%s does not set %s on container %q. Unset is the PERMISSIVE posture — "+
					"services/oms/internal/config.Load reads it as os.Getenv(...) == \"true\", so an "+
					"absent flag is false — and this tenant's OMS then admits orders for portfolios "+
					"no mandate governs, with no rule evaluated (#779). Regenerate: "+
					"go run ./cmd/kanz-tenantgen -tenant %s",
				oms.ManifestPath(tenant), key, oms.Container, tenant))
		case got != want:
			// EXACTLY "true", not merely truthy. config.Load compares the raw string
			// (os.Getenv(...) == "true"), so "True", "1" and " true" are all read as
			// FALSE — a control an operator believes they armed, silently off. That
			// parsing is a defect in its own right (#783); what this guard can do is
			// refuse to render a spelling that would hit it.
			problems = append(problems, fmt.Sprintf(
				"%s sets %s=%q, want %q. With %q this tenant trades UNCONSTRAINED against every "+
					"portfolio nobody has published a mandate for, and the audit trail records those "+
					"orders as having passed pre-trade compliance (#779). This file is generated — do "+
					"not hand-patch it: go run ./cmd/kanz-tenantgen -tenant %s",
				oms.ManifestPath(tenant), key, got, want, got, tenant))
		}
		checked++
	}

	// NON-VACUOUS BY DESIGN, the SEC-M5/ONBOARD-M1 rule this package holds
	// everywhere: a guard that walked zero manifests reports PASS, and would go
	// on reporting PASS through the entire window in which the first tenant is
	// onboarded with the control off. Finding none is a failure.
	if checked == 0 {
		t.Fatalf("no per-tenant OMS manifest was checked under %s — this guard verified NOTHING. Either "+
			"the render path moved (update oms.ManifestPath's use here) or every tenant directory "+
			"lost its OMS manifest; both are failures, not a clean run.", tenantsDir)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("per-tenant OMS manifests do not require a mandate (%d checked):\n  - %s",
			checked, joinProblems(problems))
	}
}

// containerEnvValue returns the value a multi-document manifest sets for env var
// key on the named container, and whether it set one at all.
//
// It walks the DECODED documents. The alternative — a regex over the raw bytes —
// would match the variable's name inside the long comment blocks the base
// carries into every render, and those comments discuss both values of this flag
// at length; a guard reading them cannot tell a posture from a paragraph.
//
// An env entry sourced from valueFrom returns an error rather than "not found":
// the two are different repairs, and calling a secret-backed value "unset" would
// send an operator to add a literal beside a reference the API server would then
// reject.
func containerEnvValue(manifest []byte, container, key string) (string, bool, error) {
	type envVar struct {
		Name      string     `yaml:"name"`
		Value     *string    `yaml:"value"`
		ValueFrom *yaml.Node `yaml:"valueFrom"`
	}
	type doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `yaml:"name"`
						Env  []envVar `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	sawContainer := false
	for {
		var d doc
		err := dec.Decode(&d)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, fmt.Errorf("decode manifest: %w", err)
		}
		// Deployment AND Rollout: a workload rendered as an Argo Rollout carries
		// the identical pod template, and a guard that only knew one kind would
		// pass silently the day a service moved to the other.
		if d.Kind != "Deployment" && d.Kind != "Rollout" {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name != container {
				continue
			}
			sawContainer = true
			for _, e := range c.Env {
				if e.Name != key {
					continue
				}
				if e.ValueFrom != nil {
					return "", false, fmt.Errorf("%s is supplied from a valueFrom reference on container %q; "+
						"a control this guard must read the VALUE of cannot be indirected through a "+
						"secret or ConfigMap, because nothing in this repository can then say what it "+
						"is set to", key, container)
				}
				if e.Value == nil {
					return "", true, nil
				}
				return *e.Value, true, nil
			}
		}
	}
	if !sawContainer {
		return "", false, fmt.Errorf("no container named %q in any Deployment or Rollout document", container)
	}
	return "", false, nil
}

// joinProblems renders one problem per line, indented to match the caller's
// bullet.
func joinProblems(problems []string) string {
	var b bytes.Buffer
	for i, p := range problems {
		if i > 0 {
			b.WriteString("\n  - ")
		}
		b.WriteString(p)
	}
	return b.String()
}
